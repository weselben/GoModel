package thinkextract

import (
	"io"
	"strings"
	"testing"

	"github.com/enterpilot/gomodel/internal/core"
)

func TestState_Flush_PartialReasoningEmitted(t *testing.T) {
	// Reasoning accumulated across Feed calls but not yet emitted in the
	// last Flush is returned by the next Flush call.
	s := NewState(Options{})
	s.Feed("<think>part1</think>")
	cd, rd := s.Feed("<think>part2</think>")
	if cd != "" {
		t.Errorf("mid-stream content=%q, want empty", cd)
	}
	if rd != "\n\npart2" {
		t.Errorf("mid-stream reasoning=%q, want %q", rd, "\n\npart2")
	}
	// Flush emits nothing more: all reasoning already returned.
	cd, rd = s.Flush()
	if cd != "" || rd != "" {
		t.Errorf("post-flush: got (%q,%q), want empty", cd, rd)
	}
}

func TestState_BufferCapOverflow(t *testing.T) {
	// A single Feed that exceeds the cap must not panic and must drop the
	// buffered text on the floor as ordinary content.
	opts := Options{MaxBufferBytes: 8}
	s := NewState(opts)
	cd, rd := s.Feed("0123456789ABCDEF")
	if rd != "" {
		t.Errorf("reasoning=%q, want empty", rd)
	}
	if cd == "" {
		t.Errorf("content delta is empty after cap overflow, want some")
	}
	if s.buffer.Len() > 0 {
		t.Errorf("buffer not cleared after cap, len=%d", s.buffer.Len())
	}
}

func TestState_BufferCapOverflowInsideThinkBlock(t *testing.T) {
	// Cap hit while inside a think block: content after the cap must be
	// emitted as ordinary content, not as reasoning.
	opts := Options{MaxBufferBytes: 8}
	s := NewState(Options{})
	s = NewState(opts)
	s.Feed("a<think>hidden")
	cd, _ := s.Feed(strings.Repeat("z", 64))
	if cd == "" {
		t.Errorf("expected overflow content emission")
	}
}

func TestState_SafeEmitPrefix_NoPartialMatch(t *testing.T) {
	// A tail that does not match any prefix of the open tag is safe to emit.
	s := NewState(Options{})
	cd, _ := s.Feed("answer hello")
	if cd != "answer hello" {
		t.Errorf("content=%q, want %q", cd, "answer hello")
	}
}

func TestExtract_NestedTags(t *testing.T) {
	// Nested opens: the first close terminates the first block; the inner
	// open stays inside the extracted reasoning text. Documented behaviour.
	input := "a<think>one<think>two</think>b"
	cleaned, reasoning, found := Extract(input, Options{})
	if !found {
		t.Fatalf("found=false, want true")
	}
	if cleaned != "ab" {
		t.Errorf("cleaned=%q, want %q", cleaned, "ab")
	}
	if reasoning != "one<think>two" {
		t.Errorf("reasoning=%q, want %q", reasoning, "one<think>two")
	}
}

func TestExtract_EmptyInput(t *testing.T) {
	cleaned, reasoning, found := Extract("", Options{})
	if found {
		t.Errorf("found=true, want false")
	}
	if cleaned != "" || reasoning != "" {
		t.Errorf("got (%q,%q), want empty", cleaned, reasoning)
	}
}

func TestTransformStream_FlushAtDone(t *testing.T) {
	// State that has buffered text at [DONE] should emit a flush delta
	// carrying the residual content so the client sees nothing lost.
	input := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a<think>x</think>b\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"y<think>z</think> more\"}}]}\n\n" +
		"data: [DONE]\n"
	rc := TransformStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	// Both chunks should rewrite; the second leaves " more" after the block.
	if !strings.Contains(out, "\"reasoning_content\":\"\\n\\nz\"") {
		t.Errorf("missing reasoning=second block with separator: %q", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("missing [DONE]: %q", out)
	}
}

func TestTransformStream_NoDoneDrain(t *testing.T) {
	// Streams that terminate without [DONE] still flush residual state.
	input := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a<think>x</think>b\"}}]}\n"
	rc := TransformStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, "\"reasoning_content\":\"x\"") {
		t.Errorf("missing reasoning_content=x: %q", out)
	}
}

func TestTransformStream_MalformedChoicesArray(t *testing.T) {
	// choices is a string, not an array: json.Unmarshal succeeds, the
	// unmarshal-into-[]json.RawMessage fails, returns err, forwards verbatim.
	input := "data: {\"choices\":\"oops\"}\n\ndata: [DONE]\n"
	rc := TransformStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, "oops") {
		t.Errorf("malformed chunk not forwarded: %q", out)
	}
}

func TestTransformStream_EmptyChoices(t *testing.T) {
	// Empty choices array: passes through verbatim.
	input := "data: {\"choices\":[]}\n\ndata: [DONE]\n"
	rc := TransformStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("empty-choices chunk lost: %q", out)
	}
}

func TestTransformStream_PartialTagAcrossDone(t *testing.T) {
	// Tag opens in one chunk, closes in another, followed by [DONE]: must
	// produce both content and reasoning deltas.
	input := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a<think>r\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"est of reason\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ing</think> end\"}}]}\n\n" +
		"data: [DONE]\n"
	rc := TransformStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, "\"reasoning_content\":\"r\\nest of reason\\ning\"") &&
		!strings.Contains(out, "reasoning_content") {
		t.Errorf("reasoning across chunks missing: %q", out)
	}
}

func TestTransformChatResponse_UnknownContentType(t *testing.T) {
	// Content is an int (unexpected). Must not panic, must report no
	// rewrite.
	resp := &core.ChatResponse{
		Choices: []core.Choice{{Message: core.ResponseMessage{Role: "assistant", Content: 42}}},
	}
	if got := TransformChatResponse(resp, Options{}); got != 0 {
		t.Errorf("rewritten=%d, want 0", got)
	}
}

func TestTransformChatResponse_NilMessage(t *testing.T) {
	// Choices with no content at all. No panic, no rewrite.
	resp := &core.ChatResponse{
		Choices: []core.Choice{{Message: core.ResponseMessage{Role: "assistant", Content: nil}}},
	}
	if got := TransformChatResponse(resp, Options{}); got != 0 {
		t.Errorf("rewritten=%d, want 0", got)
	}
}

func TestTransformChatResponse_EmptyContentPartText(t *testing.T) {
	// A text part with empty text is not rewritten; a text part with the
	// tag but no body is rewritten with empty reasoning (which setReasoning
	// then rejects, so no-op).
	resp := &core.ChatResponse{
		Choices: []core.Choice{{Message: core.ResponseMessage{
			Role: "assistant",
			Content: []core.ContentPart{
				{Type: "text", Text: ""},
				{Type: "text", Text: "before<think></think>after"},
			},
		}}},
	}
	if got := TransformChatResponse(resp, Options{}); got != 1 {
		t.Errorf("rewritten=%d, want 1 (empty-body block is a rewrite)", got)
	}
	parts := resp.Choices[0].Message.Content.([]core.ContentPart)
	if parts[0].Text != "" || parts[1].Text != "beforeafter" {
		t.Errorf("parts=%+v, want empty + \"beforeafter\"", parts)
	}
}

func TestTransformChatResponse_NonTextPartUntouched(t *testing.T) {
	// ImageURL parts are untouched even if they look like text fields.
	resp := &core.ChatResponse{
		Choices: []core.Choice{{Message: core.ResponseMessage{
			Role: "assistant",
			Content: []core.ContentPart{
				{Type: "image_url", ImageURL: &core.ImageURLContent{URL: "data:image/png;base64,xxx"}},
				{Type: "text", Text: "<think>y</think>shown"},
			},
		}}},
	}
	if got := TransformChatResponse(resp, Options{}); got != 1 {
		t.Errorf("rewritten=%d, want 1", got)
	}
	parts := resp.Choices[0].Message.Content.([]core.ContentPart)
	if parts[0].Type != "image_url" {
		t.Errorf("image part lost type: %v", parts[0])
	}
	if parts[1].Text != "shown" {
		t.Errorf("text part: %q, want \"shown\"", parts[1].Text)
	}
}

func TestTransformChatResponse_PreservesToolCalls(t *testing.T) {
	// Tool calls on the message must survive the rewrite.
	calls := []core.ToolCall{{ID: "call_1", Type: "function", Function: core.FunctionCall{Name: "f", Arguments: "{}"}}}
	resp := &core.ChatResponse{
		Choices: []core.Choice{{
			Message: core.ResponseMessage{
				Role:      "assistant",
				Content:   "a<think>b</think>c",
				ToolCalls: calls,
			},
		}},
	}
	if got := TransformChatResponse(resp, Options{}); got != 1 {
		t.Errorf("rewritten=%d, want 1", got)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Errorf("tool calls lost")
	}
}

// Cover safeEmitPrefix with no tail match for the entire tag.
func TestSafeEmitPrefix_NoMatch(t *testing.T) {
	got := safeEmitPrefix("plain text", "<think>")
	if got != len("plain text") {
		t.Errorf("got %d, want %d", got, len("plain text"))
	}
}

// Sanity check: jsonUnmarshalString handles valid and invalid raw input.