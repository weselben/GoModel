package streaming

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"

	"github.com/goccy/go-json"
)

// RepetitionGuardStream watches an upstream SSE stream for a text unit (a
// single token or a short token chain) that starts repeating consecutively.
// When the unit repeats limit times in a row, the guard closes the upstream
// source and ends the stream with the dialect's native termination: chat
// completions get a synthetic finish_reason "stop" chunk plus data: [DONE],
// Anthropic messages get message_delta(stop_reason "end_turn") plus
// message_stop, and the Responses API gets a response.completed event. In
// every case the client sees a normal turn end, not a broken connection.
//
// The guard is purely observational: bytes pass through to the caller as they
// arrive and are never rewritten or held back. Detection runs eagerly on the
// decoded delta text of each complete SSE event, so an accepted leak of up to
// limit x maxPattern tokens may already have been emitted when the guard fires.
//
// A TokenCounter is resolved lazily for the model on the first content delta;
// unknown models fall back to a byte-period detector. Skip heuristics keep
// fenced code, base64/hex blobs, markdown tables, long whitespace runs, and
// tool_calls/function_call deltas from ever tripping the guard. With limit <= 0
// the source is returned unchanged, so a disabled guard has zero overhead.
//
// Usage accounting limitation: the guard closes the upstream before the
// provider's final usage chunk arrives, so a triggered stream emits no usage
// entry even though the provider still bills the emitted tokens. The cut
// consumes up to limit consecutive repeats of the unit beyond what usage
// logs show. Synthesizing a usage entry from the emitted deltas would
// require a callback into the usage observer that the guard does not
// currently have; until then the accounting gap is documented, not patched.
type RepetitionGuardStream struct {
	source          io.ReadCloser
	limit           int
	maxPattern      int
	model           string
	counter         TokenCounter
	counterResolved bool

	pending    []byte
	out        StreamBuffer
	scratch    []byte
	sourceDone bool
	readErr    error
	closed     bool
	triggered  bool
	closeOnce  sync.Once
	closeErr   error
	onTrigger  func()
	zeroReads  int

	mu sync.Mutex

	// sawJSONEvent tracks that at least one JSON event has been decoded from
	// the stream; the trigger() chat-completions branch emits the synthetic
	// terminal chunk only when it saw one, so streams that never spoke JSON
	// (plain test payloads) end with just the done marker.
	sawJSONEvent bool
	envID        string
	envObject    string
	envModel     string
	envCreated   float64

	// dialect pins the stream shape (chat completions, Anthropic messages,
	// or the Responses API) from the first recognizable event; the
	// termination the guard synthesizes on trigger matches that dialect.
	dialect streamDialect

	choices map[int]*choiceState
}

// streamDialect identifies which SSE wire shape a stream speaks.
type streamDialect int

const (
	dialectUnknown streamDialect = iota
	dialectChatCompletions
	dialectAnthropicMessages
	dialectResponses
)

// GuardOption customizes the guard at construction time.
type GuardOption func(*RepetitionGuardStream)

// WithTriggerCallback registers fn to run exactly once when the guard
// triggers. Callers use it to increment metrics without coupling the
// streaming package to the metrics registry.
func WithTriggerCallback(fn func()) GuardOption {
	return func(g *RepetitionGuardStream) { g.onTrigger = fn }
}

type choiceState struct {
	// fenced tracks ``` parity per choice: true while inside a fenced block.
	fenced bool
	// tokenTail is the rolling tail of the last limit*maxPattern token IDs.
	tokenTail []int
	// byteTail is the rolling tail of recently observed content bytes.
	byteTail []byte
}

const (
	minLimit          = 2
	defaultMaxPattern = 8
	maxMaxPattern     = 64

	// Byte fallback bounds: the detector only trusts runs of at least
	// fallbackMinRunBytes with a period of at most fallbackMaxUnitBytes.
	// 64 matches the fallback reference recorded in the wayfinder map (#62).
	fallbackMaxUnitBytes = 64
	fallbackMinRunBytes  = 96

	// Encoded-blob heuristic window (base64/hex density + entropy).
	encodedWindowBytes = 64

	// maxConsecutiveZeroReads caps how many (0, nil) source reads the
	// guard tolerates before surfacing io.ErrNoProgress, matching the
	// stdlib io.Copy busy-spin cutoff.
	maxConsecutiveZeroReads = 100
)

// NewRepetitionGuardStream returns source unchanged when limit is zero or
// negative, keeping the default behavior identical to not having the guard.
//
// maxPattern bounds the inspected repetition period in tokens (<= 0 means the
// default of 8, clamped to 1..64); limit is the repeat count that trips the
// guard (clamped to at least 2). model is resolved lazily into a TokenCounter
// on the first content delta; an unknown model falls back to the byte-period
// detector.
func NewRepetitionGuardStream(source io.ReadCloser, limit, maxPattern int, model string, opts ...GuardOption) io.ReadCloser {
	if source == nil || limit <= 0 {
		return source
	}
	limit, maxPattern = clampGuardParams(limit, maxPattern)
	g := &RepetitionGuardStream{
		source:     source,
		limit:      limit,
		maxPattern: maxPattern,
		model:      model,
		choices:    make(map[int]*choiceState),
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

func clampGuardParams(limit, maxPattern int) (int, int) {
	if limit < minLimit {
		limit = minLimit
	}
	// Cap limit so limit*maxPattern (detectTokenRun) and
	// fallbackMaxUnitBytes*limit (detectByteRun) never overflow int.
	if limit > math.MaxInt/maxMaxPattern {
		limit = math.MaxInt / maxMaxPattern
	}
	if maxPattern <= 0 {
		maxPattern = defaultMaxPattern
	}
	if maxPattern > maxMaxPattern {
		maxPattern = maxMaxPattern
	}
	return limit, maxPattern
}

func (s *RepetitionGuardStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		s.mu.Lock()
		if s.out.Len() > 0 {
			n := s.out.Read(p)
			s.mu.Unlock()
			return n, nil
		}
		if s.triggered || s.sourceDone || s.closed {
			s.mu.Unlock()
			return 0, io.EOF
		}
		if s.scratch == nil {
			s.scratch = make([]byte, 32*1024)
		}
		scratch := s.scratch
		s.mu.Unlock()

		// The source read blocks, so it runs outside the mutex; Close may
		// be waiting on the mutex while this goroutine is blocked here.
		n, err := s.source.Read(scratch)

		s.mu.Lock()
		if n > 0 {
			s.zeroReads = 0
			s.observe(scratch[:n])
			s.mu.Unlock()
			continue
		}
		if err != nil {
			// n == 0 here: the n > 0 branch above never reaches this one.
			if errors.Is(err, io.EOF) {
				if len(s.pending) > 0 {
					// The source ended mid-event (no blank-line separator
					// after the final payload): forward the buffered bytes
					// before reporting EOF so the client never loses the
					// last delta.
					s.out.AppendBytes(s.pending)
					s.pending = s.pending[:0]
					s.mu.Unlock()
					continue
				}
				s.sourceDone = true
				s.mu.Unlock()
				return 0, io.EOF
			}
			s.readErr = err
			s.sourceDone = true
			s.mu.Unlock()
			return 0, err
		}
		// (0, nil): the source made no progress and reported no error.
		// Surface io.ErrNoProgress after repeated empty reads so io.Copy
		// callers cannot busy-spin on a stalled source.
		s.zeroReads++
		if s.zeroReads >= maxConsecutiveZeroReads {
			s.mu.Unlock()
			return 0, io.ErrNoProgress
		}
		s.mu.Unlock()
		return 0, nil
	}
}

// Close idempotently closes the upstream source. It is safe to call after a
// repetition trigger (the source is already closed) and returns the recorded
// close error on repeat calls. Close is safe to call concurrently with Read:
// the upstream close happens outside the mutex (so a Read blocked in the
// source is unblocked) and the state teardown runs under it.
func (s *RepetitionGuardStream) Close() error {
	_ = s.closeSource()
	s.mu.Lock()
	s.closed = true
	s.out.Release()
	s.pending = nil
	s.mu.Unlock()
	return s.closeErr
}

func (s *RepetitionGuardStream) closeSource() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.source.Close()
	})
	return s.closeErr
}

// observe forwards complete SSE events to the output unchanged and inspects
// their decoded delta content. Events are emitted before they are inspected:
// the guard never rewrites or holds bytes, it can only stop forwarding and
// append a terminating [DONE] once a run is found.
func (s *RepetitionGuardStream) observe(data []byte) {
	if s.triggered || len(data) == 0 {
		return
	}
	s.pending = append(s.pending, data...)

	for len(s.pending) > 0 {
		idx, sepLen := nextEventBoundary(s.pending)
		if idx == -1 {
			if len(s.pending) > maxPendingEventBytes {
				// Runaway event with no boundary: forward it unobserved
				// rather than buffering without bound.
				s.out.AppendBytes(s.pending)
				s.pending = s.pending[:0]
			}
			return
		}

		event := s.pending[:idx]
		sep := s.pending[idx : idx+sepLen]
		s.pending = s.pending[idx+sepLen:]
		if len(event) == 0 {
			continue
		}

		// Re-append the matched separator so a CRLF upstream stream passes
		// through byte-identical; we never replace it with the LF constant.
		s.out.AppendBytes(event)
		s.out.AppendBytes(sep)
		s.inspectEvent(event)
		if s.triggered {
			s.pending = s.pending[:0]
			return
		}
	}
}

// inspectEvent parses one SSE event's data payload and runs any content
// deltas through the detector. Non-data, [DONE], non-JSON, and content-free
// events are ignored.
func (s *RepetitionGuardStream) inspectEvent(event []byte) {
	payload := eventPayload(event)
	if payload == nil || bytes.Equal(payload, donePayload) {
		return
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		// Not JSON we can inspect; the bytes were forwarded unchanged.
		return
	}
	s.captureEnvelope(decoded)

	deltas := contentDeltas(decoded)
	if len(deltas) == 0 {
		return
	}

	if !s.counterResolved {
		s.counterResolved = true
		counter, err := NewTokenCounter(s.model)
		if err != nil {
			slog.Debug("stream repetition guard tokenizer unavailable; using byte fallback",
				"model", s.model, "error", err)
			counter = nil
		}
		s.counter = counter
	}

	for _, d := range deltas {
		if s.inspectDelta(d.choiceIndex, d.content) {
			s.trigger(d.choiceIndex)
			return
		}
	}
}

// eventPayload joins the data lines of an event into one payload, mirroring
// the SSE multi-line data rule. It returns nil for events without data lines.
func eventPayload(event []byte) []byte {
	if bytes.IndexByte(event, '\n') == -1 {
		jsonData, ok := parseDataLine(event)
		if !ok {
			return nil
		}
		return jsonData
	}

	lines := bytes.Split(event, []byte("\n"))
	var payloadLines [][]byte
	for _, line := range lines {
		if jsonData, ok := parseDataLine(line); ok {
			payloadLines = append(payloadLines, jsonData)
		}
	}
	if len(payloadLines) == 0 {
		return nil
	}
	return bytes.Join(payloadLines, []byte("\n"))
}

func (s *RepetitionGuardStream) choiceState(index int) *choiceState {
	st, ok := s.choices[index]
	if !ok {
		st = &choiceState{}
		s.choices[index] = st
	}
	return st
}

// inspectDelta applies the skip heuristics and feeds inspectable content to
// the active detector. It reports whether a repetition was detected.
func (s *RepetitionGuardStream) inspectDelta(index int, content []byte) bool {
	st := s.choiceState(index)

	// Fenced-code parity: ``` toggles flip the per-choice fence state, and
	// any delta that touches a fence or lands inside one is never inspected.
	if toggles := bytes.Count(content, codeFenceMarker); toggles > 0 {
		if toggles%2 == 1 {
			st.fenced = !st.fenced
		}
		slog.Debug("stream repetition guard skipped delta", "choice", index, "reason", "code_fence_marker")
		return false
	}
	if st.fenced {
		slog.Debug("stream repetition guard skipped delta", "choice", index, "reason", "inside_code_fence")
		return false
	}
	if isMarkdownTableRow(content) {
		slog.Debug("stream repetition guard skipped delta", "choice", index, "reason", "markdown_table_row")
		return false
	}
	if hasLongWhitespaceRun(content) {
		slog.Debug("stream repetition guard skipped delta", "choice", index, "reason", "whitespace_run")
		return false
	}
	if looksLikeEncodedBlob(content) {
		slog.Debug("stream repetition guard skipped delta", "choice", index, "reason", "encoded_blob")
		return false
	}

	if s.counter != nil {
		return st.detectTokenRun(content, s.counter, s.limit, s.maxPattern)
	}
	return st.detectByteRun(content, s.limit)
}

// captureEnvelope remembers id/object/created/model from the latest decoded
// chunk so the synthetic terminal chunk indistinguishably completes the
// same conversation, and pins the stream dialect from the payload shape so
// trigger() can speak the right wire protocol. Every call also flips
// sawJSONEvent so trigger() knows at least one JSON event has been seen.
func (s *RepetitionGuardStream) captureEnvelope(decoded map[string]any) {
	if s.dialect == dialectUnknown {
		s.dialect = detectDialect(decoded)
	}
	s.sawJSONEvent = true
	// Responses API events nest the envelope under "response".
	envelope := decoded
	if inner, ok := decoded["response"].(map[string]any); ok {
		envelope = inner
	}
	if v, ok := envelope["id"].(string); ok {
		s.envID = v
	}
	if v, ok := envelope["object"].(string); ok {
		s.envObject = v
	}
	if v, ok := envelope["model"].(string); ok {
		s.envModel = v
	}
	if v, ok := envelope["created"].(float64); ok {
		s.envCreated = v
	}
	if v, ok := envelope["created_at"].(float64); ok {
		s.envCreated = v
	}
}

// detectDialect identifies the SSE wire shape from a decoded payload:
// Anthropic messages events carry type "message_*"/"content_block_*",
// Responses API events carry type "response.*", and chat completions carry
// choices. Defaults to chat completions when nothing else matches.
func detectDialect(decoded map[string]any) streamDialect {
	t, _ := decoded["type"].(string)
	switch {
	case strings.HasPrefix(t, "message_") || strings.HasPrefix(t, "content_block_"):
		return dialectAnthropicMessages
	case strings.HasPrefix(t, "response."):
		return dialectResponses
	default:
		return dialectChatCompletions
	}
}

// terminalChunk builds the data payload the guard appends on trigger: a
// chat-completion chunk whose only delta is empty and whose finish_reason is
// "stop", exactly like the final chunk of a successful upstream stream.
// Envelope fields are omitted when the stream never carried them.
func (s *RepetitionGuardStream) terminalChunk(index int) []byte {
	choice := map[string]any{
		"index":         index,
		"delta":         map[string]any{},
		"finish_reason": "stop",
	}
	chunk := map[string]any{"choices": []any{choice}}
	if s.envID != "" {
		chunk["id"] = s.envID
	}
	if s.envObject != "" {
		chunk["object"] = s.envObject
	}
	if s.envModel != "" {
		chunk["model"] = s.envModel
	}
	if s.envCreated != 0 {
		chunk["created"] = s.envCreated
	}
	b, _ := json.Marshal(chunk)
	return b
}

// trigger closes the upstream once, appends a dialect-appropriate
// synthetic termination (so clients that read finish_reason / stop_reason /
// response.completed see a normal end), and marks the guard terminated.
// Everything already emitted stays emitted.
//
//   - Chat completions: terminal chunk with finish_reason "stop", then
//     data: [DONE] — mirrors a successful upstream stream.
//   - Anthropic messages: message_delta with stop_reason "end_turn"
//     (no usage key — the guard cannot know the real token count), then
//     message_stop. No [DONE] marker — Anthropic streams end at
//     message_stop.
//   - Responses API: response.completed with status "completed" and the
//     echoed response envelope. No [DONE] marker.
func (s *RepetitionGuardStream) trigger(index int) {
	if s.triggered {
		return
	}
	s.triggered = true

	_ = s.closeSource()

	switch s.dialect {
	case dialectAnthropicMessages:
		// Close the still-open text block first: strict Anthropic clients
		// track per-block state and hang on an unclosed content_block.
		s.out.AppendString("event: content_block_stop\n")
		s.out.AppendString(`data: {"type":"content_block_stop","index":`)
		s.out.AppendString(strconv.Itoa(index))
		s.out.AppendString("}")
		s.out.AppendBytes(lfEventBoundary)
		s.out.AppendString("event: message_delta\n")
		s.out.AppendString(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)
		s.out.AppendBytes(lfEventBoundary)
		s.out.AppendString("event: message_stop\n")
		s.out.AppendString(`data: {"type":"message_stop"}`)
		s.out.AppendBytes(lfEventBoundary)
	case dialectResponses:
		s.out.AppendString("event: response.completed\n")
		s.out.AppendString("data: ")
		s.out.AppendBytes(s.responsesCompletedPayload())
		s.out.AppendBytes(lfEventBoundary)
	default:
		if s.sawJSONEvent {
			s.out.AppendString("data: ")
			s.out.AppendBytes(s.terminalChunk(index))
			s.out.AppendBytes(lfEventBoundary)
		}
		s.out.AppendString("data: ")
		s.out.AppendBytes(donePayload)
		s.out.AppendBytes(lfEventBoundary)
	}

	slog.Warn("stream repetition guard triggered", "choice", index, "model", s.model)

	if s.onTrigger != nil {
		s.onTrigger()
	}
}

// responsesCompletedPayload builds the data payload for the synthetic
// response.completed event: the observed envelope plus status "completed".
// Clients (Codex, the OpenAI SDK, AgentRunKit) treat this event as the
// authoritative end of a Responses stream.
func (s *RepetitionGuardStream) responsesCompletedPayload() []byte {
	resp := map[string]any{
		"object": "response",
		"status": "completed",
	}
	if s.envID != "" {
		resp["id"] = s.envID
	}
	if s.envModel != "" {
		resp["model"] = s.envModel
	}
	if s.envCreated != 0 {
		resp["created_at"] = s.envCreated
	}
	chunk := map[string]any{
		"type":     "response.completed",
		"response": resp,
	}
	b, _ := json.Marshal(chunk)
	return b
}

// detectTokenRun appends the delta's token IDs to the rolling tail (capped at
// limit*maxPattern) and reports whether the smallest period p in 1..maxPattern
// yields limit consecutive copies at the tail.
func (st *choiceState) detectTokenRun(content []byte, counter TokenCounter, limit, maxPattern int) bool {
	st.tokenTail = append(st.tokenTail, counter.Tokens(string(content))...)
	if capacity := limit * maxPattern; len(st.tokenTail) > capacity {
		st.tokenTail = append(st.tokenTail[:0], st.tokenTail[len(st.tokenTail)-capacity:]...)
	}

	tail := st.tokenTail
	for p := 1; p <= maxPattern; p++ {
		need := p * limit
		if len(tail) < need {
			break
		}
		if tokenTailRepeats(tail, p, need) {
			return true
		}
	}
	return false
}

// tokenTailRepeats reports whether the last need token IDs are limit copies
// of the final p IDs.
func tokenTailRepeats(tail []int, p, need int) bool {
	n := len(tail)
	if n < need {
		return false
	}
	for i := n - need; i < n; i++ {
		if tail[i] != tail[n-p+(i-(n-need))%p] {
			return false
		}
	}
	return true
}

// detectByteRun appends content bytes to the rolling tail (capped so the
// longest checked run always fits) and reports whether a period of at most
// fallbackMaxUnitBytes bytes repeats limit times AND extends for at least
// fallbackMinRunBytes at the tail. The generous minimum run keeps natural
// text repeats from tripping the byte fallback.
func (st *choiceState) detectByteRun(content []byte, limit int) bool {
	st.byteTail = append(st.byteTail, content...)
	keep := fallbackMinRunBytes
	if maxNeed := fallbackMaxUnitBytes * limit; maxNeed > keep {
		keep = maxNeed
	}
	if len(st.byteTail) > keep {
		st.byteTail = append(st.byteTail[:0], st.byteTail[len(st.byteTail)-keep:]...)
	}

	tail := st.byteTail
	for p := 1; p <= fallbackMaxUnitBytes; p++ {
		need := p * limit
		if len(tail) < need {
			break
		}
		if !byteTailRepeats(tail, p, need) {
			continue
		}
		if run := byteRunLength(tail, p); run >= need && run >= fallbackMinRunBytes {
			return true
		}
	}
	return false
}

// byteTailRepeats reports whether the last need bytes are limit copies of the
// final p bytes.
func byteTailRepeats(tail []byte, p, need int) bool {
	n := len(tail)
	if n < need {
		return false
	}
	for i := n - need; i < n; i++ {
		if tail[i] != tail[n-p+(i-(n-need))%p] {
			return false
		}
	}
	return true
}

// byteRunLength measures the maximal periodic suffix run of tail with period
// p, scanning backwards from the end.
func byteRunLength(tail []byte, p int) int {
	n := len(tail)
	if n < p {
		return 0
	}
	run := p
	for i := n - 1 - p; i >= 0; i-- {
		if tail[i] != tail[i+p] {
			break
		}
		run++
	}
	return run
}
var codeFenceMarker = []byte("```")

// contentDeltas extracts (choiceIndex, text) pairs from a decoded SSE
// payload across the three supported wire shapes:
//
//   - Chat completions: choices[].delta.content, skipping tool_calls and
//     function_call deltas.
//   - Anthropic messages: content_block_delta events with
//     delta.type "text_delta" yield delta.text; thinking deltas and
//     input_json_delta (tool use) are never inspected. The content-block
//     index is the choice index.
//   - Responses API: response.output_text.delta events yield the delta
//     string; function-call and reasoning deltas are never inspected.
//     The state key is the composite output/content index, since deltas
//     from different output items can interleave.
//
// Payloads that match no shape yield nil and are ignored.
func contentDeltas(payload map[string]any) []struct {
	choiceIndex int
	content     []byte
} {
	if t, _ := payload["type"].(string); t != "" || payload["choices"] == nil {
		switch {
		case strings.HasPrefix(t, "content_block_"):
			if t != "content_block_delta" {
				return nil
			}
			delta, ok := payload["delta"].(map[string]any)
			if !ok || delta["type"] != "text_delta" {
				return nil
			}
			text, ok := delta["text"].(string)
			if !ok || text == "" {
				return nil
			}
			idx, _ := payload["index"].(float64)
			return []struct {
				choiceIndex int
				content     []byte
			}{{choiceIndex: int(idx), content: []byte(text)}}
		case strings.HasPrefix(t, "response."):
			if t != "response.output_text.delta" {
				return nil
			}
			text, ok := payload["delta"].(string)
			if !ok || text == "" {
				return nil
			}
			// Deltas from different output items can interleave; key the
			// guard state by the composite output/content index so
			// independent tails never merge into a false repetition.
			// Negative keys cannot collide with the non-negative
			// choice/block indices of the other dialects, and the
			// Responses termination ignores the index anyway.
			outIdx, _ := payload["output_index"].(float64)
			contentIdx, _ := payload["content_index"].(float64)
			key := -1 - (int(outIdx)*65536 + int(contentIdx))
			return []struct {
				choiceIndex int
				content     []byte
			}{{choiceIndex: key, content: []byte(text)}}
		default:
			return nil
		}
	}

	choicesRaw, ok := payload["choices"].([]any)
	if !ok {
		return nil
	}
	var out []struct {
		choiceIndex int
		content     []byte
	}
	for i, c := range choicesRaw {
		choiceMap, ok := c.(map[string]any)
		if !ok {
			continue
		}
		// Key the guard state by the choice's own index, not its position
		// in the choices array; the two differ only for out-of-order
		// choice delivery, which OpenAI permits.
		choiceIndex := i
		if idx, ok := choiceMap["index"].(float64); ok {
			choiceIndex = int(idx)
		}
		delta, ok := choiceMap["delta"].(map[string]any)
		if !ok {
			continue
		}
		// tool_calls/function_call deltas are never inspected, even when they
		// also carry a content field.
		if _, ok := delta["tool_calls"]; ok {
			continue
		}
		if _, ok := delta["function_call"]; ok {
			continue
		}
		content, ok := delta["content"].(string)
		if !ok || content == "" {
			// DeepSeek-style reasoning streams carry the visible thinking
			// in delta.reasoning_content; a hang loops there exactly like
			// in content, so inspect it with the same limit.
			content, ok = delta["reasoning_content"].(string)
			if !ok || content == "" {
				continue
			}
		}
		out = append(out, struct {
			choiceIndex int
			content     []byte
		}{choiceIndex: choiceIndex, content: []byte(content)})
	}
	return out
}

// isMarkdownTableRow reports whether content, after leading whitespace, starts
// with '|': a markdown table row, which repeats structurally.
func isMarkdownTableRow(content []byte) bool {
	trimmed := bytes.TrimLeft(content, " \t")
	return len(trimmed) > 0 && trimmed[0] == '|'
}

// hasLongWhitespaceRun reports whether content contains a run of at least 8
// consecutive whitespace bytes.
func hasLongWhitespaceRun(content []byte) bool {
	run := 0
	for _, b := range content {
		if isSpaceByte(b) {
			run++
			if run >= 8 {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}

func isSpaceByte(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

// looksLikeEncodedBlob reports whether the trailing encodedWindowBytes-byte
// window of content looks like base64 or hex: symbol density >= 0.85 and
// Shannon entropy < 4.5 bits/char. Structured encodings repeat tokens by
// construction and must not trip the guard.
func looksLikeEncodedBlob(content []byte) bool {
	if len(content) < encodedWindowBytes {
		return false
	}
	window := content[len(content)-encodedWindowBytes:]
	if encodedSymbolDensity(window) < 0.85 {
		return false
	}
	return shannonEntropy(window) < 4.5
}

func encodedSymbolDensity(window []byte) float64 {
	symbols := 0
	for _, b := range window {
		if isEncodedSymbol(b) {
			symbols++
		}
	}
	return float64(symbols) / float64(len(window))
}

// isEncodedSymbol matches the base64 alphabet (and its url-safe variant) plus
// '=' padding; hex digits are a subset.
func isEncodedSymbol(b byte) bool {
	switch {
	case b >= '0' && b <= '9', b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z':
		return true
	case b == '+', b == '/', b == '-', b == '_', b == '=':
		return true
	}
	return false
}

// shannonEntropy returns the per-byte Shannon entropy of window in bits.
func shannonEntropy(window []byte) float64 {
	var freq [256]int
	for _, b := range window {
		freq[b]++
	}
	n := float64(len(window))
	entropy := 0.0
	for _, count := range freq {
		if count == 0 {
			continue
		}
		p := float64(count) / n
		entropy -= p * math.Log2(p)
	}
	return entropy
}
