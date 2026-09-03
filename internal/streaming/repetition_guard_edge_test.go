package streaming

// Edge-case tests ported from the brown harness after mining the production
// audit log (forge.weselben.de, 2026-09-01..03) for real traffic shapes:
// CRLF separators, usage final chunks, long repeating units on models the
// tokenizer cannot resolve, and reasoning_content-only streams. The
// reasoning_content case pins the current contract (not inspected) so the
// gap stays visible — see wayfinder #56.

import (
	"io"
	"strings"
	"testing"
)

// TestEdge_CRLFStreamLoop — some upstreams separate SSE events with
// \r\n\r\n; nextEventBoundary handles both shapes, so detection and the
// synthetic termination must work on CRLF streams too.
func TestEdge_CRLFStreamLoop(t *testing.T) {
	chunk := func(content string) string {
		return "data: {\"choices\":[{\"delta\":{\"content\":\"" + content + "\"}}]}\r\n\r\n"
	}
	body := chunk("Start ") + strings.Repeat(chunk("again "), 20)

	src := newSource(body)
	sink := &counterSink{}
	stream := NewRepetitionGuardStream(src, 3, 8, "some-unknown-model", WithTriggerCallback(sink.inc))
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := sink.n.Load(); n != 1 {
		t.Fatalf("trigger callback fired %d times, want 1", n)
	}
	if !strings.HasSuffix(string(out), doneEvent()) {
		t.Fatalf("CRLF loop cut must end with [DONE]: tail=%q", tail(string(out), 80))
	}
}

// TestEdge_UsageFinalChunkPasses — OpenAI streams requested with
// stream_options.include_usage end with a choices:[] chunk carrying usage.
// It has no delta content, so the guard forwards it untouched.
func TestEdge_UsageFinalChunkPasses(t *testing.T) {
	body := chatEvent("Hello ") + chatEvent("world") +
		"data: {\"id\":\"chatcmpl-U\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"gpt-4o\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n" +
		doneEvent()

	src := newSource(body)
	sink := &counterSink{}
	stream := NewRepetitionGuardStream(src, 3, 8, "gpt-4o", WithTriggerCallback(sink.inc))
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if string(out) != body {
		t.Fatalf("usage final chunk altered the stream\nwant: %q\ngot:  %q", body, string(out))
	}
	if sink.n.Load() != 0 {
		t.Fatalf("trigger fired on a stream ending with a usage chunk")
	}
}

// TestEdge_LongUnitByteFallback — after the fallback period cap was raised
// to 64 bytes (wayfinder #62 reference), a 40-byte repeating unit on an
// unresolvable model must still trip the guard; units above the cap stay
// undetected by design.
func TestEdge_LongUnitByteFallback(t *testing.T) {
	unit := "0123456789abcdefghijklmnopqrstuvwxyzABCD" // 40 bytes, minimal period 40
	body := chatEvent("start ") + strings.Repeat(chatEvent(unit), 30)

	src := newSource(body)
	sink := &counterSink{}
	stream := NewRepetitionGuardStream(src, 3, 8, "totally-unknown-model", WithTriggerCallback(sink.inc))
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if n := sink.n.Load(); n != 1 {
		t.Fatalf("trigger callback fired %d times, want 1", n)
	}
	if !strings.HasSuffix(string(out), doneEvent()) {
		t.Fatalf("40-byte unit loop must end with [DONE]: tail=%q", tail(string(out), 80))
	}
}

// TestEdge_ReasoningContentNotInspected — DeepSeek-style streams carry
// delta.reasoning_content alongside delta.content. The guard deliberately
// inspects only message content (wayfinder #56 left reasoning detection
// undecided), so a reasoning-only loop passes through untouched. This pins
// the contract; flip it if #56 decides to inspect reasoning.
func TestEdge_ReasoningContentNotInspected(t *testing.T) {
	reason := `data: {"choices":[{"delta":{"reasoning_content":"thinking... "}}]}` + "\n\n"
	body := `data: {"choices":[{"delta":{"role":"assistant"}}]}` + "\n\n" +
		strings.Repeat(reason, 200) +
		chatEvent("Final answer.") + doneEvent()

	src := newSource(body)
	sink := &counterSink{}
	stream := NewRepetitionGuardStream(src, 4, 8, "deepseek-r1", WithTriggerCallback(sink.inc))
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if string(out) != body {
		t.Fatalf("reasoning_content stream altered\nwant len %d, got len %d", len(body), len(out))
	}
	if sink.n.Load() != 0 {
		t.Fatalf("trigger fired on reasoning_content-only deltas")
	}
}
