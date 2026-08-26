package thinkextract

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/enterpilot/gomodel/internal/core"
)

func TestTransformStream_UnclosedBlockFlushedAtDone(t *testing.T) {
	// A think block that never closes is re-emitted at [DONE] as literal
	// content (open tag + body) so no bytes are dropped.
	input := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"text <think>unclosed\"}}]}\n\ndata: [DONE]\n"
	rc := TransformStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, "think\\u003eunclosed") {
		t.Errorf("unclosed block body dropped: %q", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("missing [DONE]: %q", out)
	}
}

func TestTransformStream_ChoiceDeltaNonObject(t *testing.T) {
	// delta as a string instead of an object: choice decode fails, chunk is
	// forwarded verbatim.
	input := "data: {\"choices\":[{\"index\":0,\"delta\":\"oops\"}]}\n\ndata: [DONE]\n"
	rc := TransformStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, "oops") {
		t.Errorf("non-object delta chunk not forwarded: %q", out)
	}
}

func TestTransformStream_EarlyReaderClose(t *testing.T) {
	// Closing the output reader before the upstream is drained must not
	// deadlock the transformer goroutine.
	input := strings.Repeat("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}]}\n\n", 100)
	rc := TransformStream(io.NopCloser(strings.NewReader(input)), Options{})
	buf := make([]byte, 64)
	_, _ = rc.Read(buf)
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Give the goroutine a moment to notice the closed pipe and exit. The
	// test passes as long as it terminates without hanging.
	time.Sleep(50 * time.Millisecond)
}

func TestSafeEmitPrefix_EmptyTag(t *testing.T) {
	if got := safeEmitPrefix("abc", ""); got != 3 {
		t.Errorf("safeEmitPrefix with empty tag: got %d, want 3", got)
	}
}

func TestState_Feed_CapWhileInThink(t *testing.T) {
	// Cap overflow while inside a think block: buffered reasoning text is
	// flushed as ordinary content so nothing is silently dropped.
	opts := Options{MaxBufferBytes: 8}
	s := NewState(opts)
	s.Feed("<think>")
	cd, _ := s.Feed(strings.Repeat("y", 64))
	if cd == "" {
		t.Errorf("expected overflow content emission while in think block")
	}
}

func TestState_Feed_EmptyChunk(t *testing.T) {
	s := NewState(Options{})
	cd, rd := s.Feed("")
	if cd != "" || rd != "" {
		t.Errorf("empty chunk produced (%q,%q), want empty", cd, rd)
	}
}

func TestExtract_CustomBufferCapOption(t *testing.T) {
	// Options plumbing: a custom cap is respected and the default pair list
	// fills the rest.
	opts := Options{MaxBufferBytes: 16}
	s := NewState(opts)
	if s.opts.MaxBufferBytes != 16 {
		t.Errorf("cap=%d, want 16", s.opts.MaxBufferBytes)
	}
	if len(s.pairs) == 0 || s.pairs[0].Open != "<think>" {
		t.Errorf("default pairs not applied: %+v", s.pairs)
	}
}

func TestTransformChatResponse_OnlyUnclosedTextParts(t *testing.T) {
	// ContentParts where the only text has an unclosed block: Extract returns
	// found=false for each part, !changed holds, no rewrite.
	resp := &core.ChatResponse{
		Choices: []core.Choice{{Message: core.ResponseMessage{
			Role: "assistant",
			Content: []core.ContentPart{
				{Type: "text", Text: "a<think>unclosed"},
			},
		}}},
	}
	if got := TransformChatResponse(resp, Options{}); got != 0 {
		t.Errorf("rewritten=%d, want 0 (unclosed block keeps content intact)", got)
	}
}

func TestTransformChatResponse_NonTextPartOnly(t *testing.T) {
	// ContentParts with only non-text parts: no rewrite.
	resp := &core.ChatResponse{
		Choices: []core.Choice{{Message: core.ResponseMessage{
			Role: "assistant",
			Content: []core.ContentPart{
				{Type: "image_url", ImageURL: &core.ImageURLContent{URL: "data:image/png;base64,xxx"}},
			},
		}}},
	}
	if got := TransformChatResponse(resp, Options{}); got != 0 {
		t.Errorf("rewritten=%d, want 0", got)
	}
}

func TestTransformStream_LongOutputForcesGoroutineExit(t *testing.T) {
	// Stream enough events that the internal pipe fills; then close the
	// reader. The transformer goroutine must observe the closed pipe and
	// exit without leaking.
	pr, pw := io.Pipe()
	defer pw.Close()
	input := strings.Repeat("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\""+strings.Repeat("x", 1024)+"\"}}]}\n\n", 256)
	src := io.NopCloser(strings.NewReader(input))
	go transformLoop(src, pw, Options{}, false)
	// Read a small chunk then close.
	buf := make([]byte, 256)
	_, _ = pr.Read(buf)
	_ = pr.Close()
	// Give the goroutine a chance to wake up.
	time.Sleep(50 * time.Millisecond)
}
