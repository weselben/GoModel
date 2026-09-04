package streaming

// Dialect tests: the guard must detect repetition in all three supported
// SSE wire shapes and terminate each with its native protocol — chat
// completions via a finish_reason "stop" chunk + [DONE], Anthropic messages
// via message_delta(stop_reason end_turn) + message_stop (no [DONE]),
// Responses API via response.completed (no [DONE]). See the wayfinder
// decision in #57/#63 and the stop_reason tables in the respective API docs.

import (
	"io"
	"strings"
	"testing"
)

// anthropicEvent builds one Anthropic Messages SSE event block.
func anthropicEvent(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// responsesEvent builds one Responses API SSE event block.
func responsesEvent(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

func TestDialect_AnthropicNormalPassthrough(t *testing.T) {
	body := anthropicEvent("message_start", `{"type":"message_start","message":{"id":"msg_1","model":"claude-x","role":"assistant"}}`) +
		anthropicEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`) +
		anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello there."}}`) +
		anthropicEvent("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		anthropicEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`) +
		anthropicEvent("message_stop", `{"type":"message_stop"}`)

	src := newSource(body)
	sink := &counterSink{}
	stream := NewRepetitionGuardStream(src, 3, 8, "claude-x", WithTriggerCallback(sink.inc))
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if string(out) != body {
		t.Fatalf("anthropic passthrough altered\nwant: %q\ngot:  %q", body, string(out))
	}
	if sink.n.Load() != 0 {
		t.Fatalf("trigger fired on clean anthropic stream")
	}
}

func TestDialect_AnthropicLoopCut(t *testing.T) {
	loop := anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"and then "}}`)
	body := anthropicEvent("message_start", `{"type":"message_start","message":{"id":"msg_1","model":"claude-x","role":"assistant"}}`) +
		anthropicEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`) +
		anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Answer: "}}`) +
		strings.Repeat(loop, 20) + // upstream never sends message_stop
		anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"never reached"}}`)

	src := newSource(body)
	sink := &counterSink{}
	stream := NewRepetitionGuardStream(src, 3, 8, "claude-x", WithTriggerCallback(sink.inc))
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	output := string(out)

	if src.closeCount != 1 {
		t.Fatalf("expected upstream closed once, got %d", src.closeCount)
	}
	if n := sink.n.Load(); n != 1 {
		t.Fatalf("trigger callback fired %d times, want 1", n)
	}
	if !strings.Contains(output, `event: content_block_stop`) ||
		!strings.Contains(output, `"type":"content_block_stop","index":0`) {
		t.Fatalf("missing content_block_stop before message_delta in output:\n%s", tail(output, 300))
	}
	if !strings.Contains(output, `event: message_delta`) ||
		!strings.Contains(output, `"stop_reason":"end_turn"`) ||
		!strings.Contains(output, `event: message_stop`) {
		t.Fatalf("missing anthropic termination events in output:\n%s", tail(output, 300))
	}
	// The synthetic message_delta must not carry a usage key: the guard
	// cannot know the real token count, and a hardcoded 0 makes
	// usage-accumulating clients under-count.
	if strings.Contains(output[strings.LastIndex(output, "event: message_delta"):], `"usage"`) {
		t.Fatalf("synthetic message_delta must omit the usage key:\n%s", tail(output, 200))
	}
	if strings.Contains(output, "data: [DONE]") {
		t.Fatalf("anthropic streams must not receive a [DONE] marker:\n%s", tail(output, 200))
	}
	if strings.Contains(output, "never reached") {
		t.Fatalf("upstream leaked past the cut:\n%s", tail(output, 200))
	}
	if !strings.HasSuffix(output, `data: {"type":"message_stop"}`+"\n\n") {
		t.Fatalf("output does not end with message_stop: tail=%q", tail(output, 120))
	}
}

func TestDialect_AnthropicThinkingDeltasNeverInspect(t *testing.T) {
	// thinking_delta repeats structurally; only text_delta is inspected.
	think := anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm "}}`)
	body := anthropicEvent("message_start", `{"type":"message_start","message":{"id":"msg_2","model":"claude-x","role":"assistant"}}`) +
		anthropicEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`) +
		strings.Repeat(think, 40) +
		anthropicEvent("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		anthropicEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`) +
		anthropicEvent("message_stop", `{"type":"message_stop"}`)

	src := newSource(body)
	sink := &counterSink{}
	stream := NewRepetitionGuardStream(src, 3, 8, "claude-x", WithTriggerCallback(sink.inc))
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if string(out) != body {
		t.Fatalf("thinking loop must pass through unmodified\nwant len %d, got len %d", len(body), len(out))
	}
	if sink.n.Load() != 0 {
		t.Fatalf("trigger fired on thinking deltas")
	}
}

func TestDialect_ResponsesNormalPassthrough(t *testing.T) {
	body := responsesEvent("response.created", `{"type":"response.created","response":{"id":"resp_1","object":"response","created_at":1700000000,"model":"codex-mini","status":"in_progress"}}`) +
		responsesEvent("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"item_0"}}`) +
		responsesEvent("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"item_0","output_index":0,"delta":"Hello world"}`) +
		responsesEvent("response.output_text.done", `{"type":"response.output_text.done","item_id":"item_0","output_index":0,"text":"Hello world"}`) +
		responsesEvent("response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"item_0","status":"completed"}}`) +
		responsesEvent("response.completed", `{"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed"}}`)

	src := newSource(body)
	sink := &counterSink{}
	stream := NewRepetitionGuardStream(src, 3, 8, "codex-mini", WithTriggerCallback(sink.inc))
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if string(out) != body {
		t.Fatalf("responses passthrough altered\nwant: %q\ngot:  %q", body, string(out))
	}
	if sink.n.Load() != 0 {
		t.Fatalf("trigger fired on clean responses stream")
	}
}

func TestDialect_ResponsesLoopCut(t *testing.T) {
	loop := responsesEvent("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"item_0","output_index":0,"delta":"looping "}`)
	body := responsesEvent("response.created", `{"type":"response.created","response":{"id":"resp_9","object":"response","created_at":1700000000,"model":"codex-mini","status":"in_progress"}}`) +
		responsesEvent("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"item_0","output_index":0,"delta":"Start: "}`) +
		strings.Repeat(loop, 30) // upstream never sends response.completed

	src := newSource(body)
	sink := &counterSink{}
	stream := NewRepetitionGuardStream(src, 3, 8, "codex-mini", WithTriggerCallback(sink.inc))
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	output := string(out)

	if src.closeCount != 1 {
		t.Fatalf("expected upstream closed once, got %d", src.closeCount)
	}
	if n := sink.n.Load(); n != 1 {
		t.Fatalf("trigger callback fired %d times, want 1", n)
	}
	if !strings.Contains(output, `event: response.completed`) ||
		!strings.Contains(output, `"status":"completed"`) {
		t.Fatalf("missing synthetic response.completed in output:\n%s", tail(output, 300))
	}
	if strings.Contains(output, "data: [DONE]") {
		t.Fatalf("responses streams must not receive a [DONE] marker:\n%s", tail(output, 200))
	}
	// Envelope echo: the synthetic completion carries the observed id/model.
	if !strings.Contains(output, `"id":"resp_9"`) || !strings.Contains(output, `"model":"codex-mini"`) {
		t.Fatalf("synthetic completion does not echo the response envelope:\n%s", tail(output, 300))
	}
	if !strings.HasSuffix(output, "\n\n") {
		t.Fatalf("output does not end with an SSE boundary")
	}
}

func TestDialect_ChatTerminationEchoesEnvelope(t *testing.T) {	chunk := func(content string) string {
		return `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"glm-x","choices":[{"index":0,"delta":{"content":"` + content + `"}}]}` + "\n\n"
	}
	body := chunk("Once ") + chunk("Twice ") + strings.Repeat(chunk("again "), 20)

	src := newSource(body)
	sink := &counterSink{}
	stream := NewRepetitionGuardStream(src, 4, 8, "glm-x", WithTriggerCallback(sink.inc))
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	output := string(out)

	if n := sink.n.Load(); n != 1 {
		t.Fatalf("trigger callback fired %d times, want 1", n)
	}
	// The synthetic terminal chunk echoes the observed envelope and carries
	// finish_reason "stop", exactly like a normal upstream finish.
	want := `data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}],"created":1700000000,"id":"chatcmpl-1","model":"glm-x","object":"chat.completion.chunk"}` + "\n\n"
	if !strings.Contains(output, want) {
		t.Fatalf("synthetic terminal chunk missing or envelope not echoed:\nwant: %s\ntail: %s", want, tail(output, 300))
	}
	if !strings.HasSuffix(output, doneEvent()) {
		t.Fatalf("chat stream must still end with [DONE]: tail=%q", tail(output, 120))
	}
}

// TestDialect_MalformedDeltasIgnored covers the guard clauses in the
// dialect extractors: anthropic text_delta with missing/empty text and a
// responses output_text.delta whose delta is not a string. Both events are
// forwarded untouched and never inspected.
func TestDialect_MalformedDeltasIgnored(t *testing.T) {
	body := anthropicEvent("message_start", `{"type":"message_start","message":{"id":"msg_3","model":"claude-x","role":"assistant"}}`) +
		anthropicEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`) +
		anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta"}}`) +
		anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`) +
		anthropicEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`) +
		responsesEvent("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"item_0","output_index":0,"delta":123}`) +
		responsesEvent("response.completed", `{"type":"response.completed","response":{"id":"resp_3","object":"response","status":"completed"}}`)

	src := newSource(body)
	sink := &counterSink{}
	stream := NewRepetitionGuardStream(src, 3, 8, "claude-x", WithTriggerCallback(sink.inc))
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if string(out) != body {
		t.Fatalf("malformed deltas altered\nwant: %q\ngot:  %q", body, string(out))
	}
	if sink.n.Load() != 0 {
		t.Fatalf("trigger fired on malformed/tool deltas")
	}
}


// TestDialect_ResponsesInterleavedItemsNeverMerge covers the Responses API
// case where two output items interleave: each item streams 5-byte chunks
// in lock-step, so the merged tail is one long periodic run of
// "abcdefghij" while each item's own run stays below the byte fallback's
// fallbackMinRunBytes ceiling (96). Keying state by the composite
// output/content index keeps the items separate; sharing one choice state
// would falsely trip the guard on the merged run.
func TestDialect_ResponsesInterleavedItemsNeverMerge(t *testing.T) {
	item0 := responsesEvent("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"item_0","output_index":0,"content_index":0,"delta":"abcde"}`)
	item1 := responsesEvent("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"item_1","output_index":1,"content_index":0,"delta":"fghij"}`)
	var body string
	for i := 0; i < 10; i++ {
		body += item0 + item1
	}
	body += responsesEvent("response.completed", `{"type":"response.completed","response":{"id":"resp_4","object":"response","status":"completed"}}`)

	src := newSource(body)
	sink := &counterSink{}
	// Unknown model forces the byte-period fallback; "claude-x" would
	// resolve a tokenizer and detect the per-item token runs instead.
	stream := NewRepetitionGuardStream(src, 2, 8, "guard-test-unknown-model", WithTriggerCallback(sink.inc))
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if string(out) != body {
		t.Fatalf("interleaved items altered\nwant: %q\ngot:  %q", body, string(out))
	}
	if sink.n.Load() != 0 {
		t.Fatalf("trigger fired on interleaved output items")
	}
}
