package streaming

import (
	"bytes"
	"log/slog"
	"strconv"
	"strings"

	"github.com/goccy/go-json"
)

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
