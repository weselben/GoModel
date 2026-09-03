package streaming

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"math"
	"sync"

	"github.com/goccy/go-json"
)

// RepetitionGuardStream watches an upstream SSE stream for a text unit (a
// single token or a short token chain) that starts repeating consecutively.
// When the unit repeats limit times in a row, the guard closes the upstream
// source, appends a data: [DONE]\n\n marker, and ends the stream.
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

	choices map[int]*choiceState
}

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
	fallbackMaxUnitBytes = 32
	fallbackMinRunBytes  = 96

	// Encoded-blob heuristic window (base64/hex density + entropy).
	encodedWindowBytes = 64
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

// newGuardWithCounter builds the guard with an explicit TokenCounter, bypassing
// lazy model resolution. A nil counter selects the byte fallback directly.
func newGuardWithCounter(source io.ReadCloser, limit, maxPattern int, counter TokenCounter) io.ReadCloser {
	if source == nil || limit <= 0 {
		return source
	}
	limit, maxPattern = clampGuardParams(limit, maxPattern)
	g := &RepetitionGuardStream{
		source:          source,
		limit:           limit,
		maxPattern:      maxPattern,
		counter:         counter,
		counterResolved: true,
		choices:         make(map[int]*choiceState),
	}
	return g
}

func clampGuardParams(limit, maxPattern int) (int, int) {
	if limit < minLimit {
		limit = minLimit
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
		if s.out.Len() > 0 {
			return s.out.Read(p), nil
		}
		if s.triggered || s.sourceDone || s.closed {
			return 0, io.EOF
		}
		if s.scratch == nil {
			s.scratch = make([]byte, 32*1024)
		}
		n, err := s.source.Read(s.scratch)
		if n > 0 {
			s.observe(s.scratch[:n])
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.sourceDone = true
				if n == 0 {
					return 0, io.EOF
				}
			}
			s.readErr = err
			s.sourceDone = true
			return 0, err
		}
		if n == 0 {
			return 0, nil
		}
	}
}

// Close idempotently closes the upstream source. It is safe to call after a
// repetition trigger (the source is already closed) and returns the recorded
// close error on repeat calls.
func (s *RepetitionGuardStream) Close() error {
	s.closed = true
	_ = s.closeSource()
	s.out.Release()
	s.pending = nil
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
		s.pending = s.pending[idx+sepLen:]
		if len(event) == 0 {
			continue
		}

		s.out.AppendBytes(event)
		s.out.AppendBytes(lfEventBoundary)
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

// trigger closes the upstream once, appends the terminating [DONE] marker,
// and marks the guard terminated. Everything already emitted stays emitted.
func (s *RepetitionGuardStream) trigger(index int) {
	if s.triggered {
		return
	}
	s.triggered = true

	_ = s.closeSource()

	s.out.AppendString("data: ")
	s.out.AppendBytes(donePayload)
	s.out.AppendBytes(lfEventBoundary)

	slog.Warn("stream repetition guard triggered", "choice", index, "model", s.model)

	if s.onTrigger != nil {
		s.onTrigger()
	}
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

// detectRun is the simplified byte-period detector used by tests and kept for
// direct inspection: it reports whether tail ends in a periodic run with
// period <= fallbackMaxUnitBytes that repeats limit times and spans at least
// fallbackMinRunBytes.
func detectRun(tail []byte, limit int) bool {
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

var codeFenceMarker = []byte("```")

// contentDeltas extracts choices[].delta.content string values from a decoded
// chat-completion payload. Deltas carrying tool_calls or function_call are
// never inspected and therefore never returned.
func contentDeltas(payload map[string]any) []struct {
	choiceIndex int
	content     []byte
} {
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
			continue
		}
		out = append(out, struct {
			choiceIndex int
			content     []byte
		}{choiceIndex: i, content: []byte(content)})
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
