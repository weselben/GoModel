package thinkextract

import (
	"io"
	"strings"
	"testing"
)

func TestExtract_NoThink(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{name: "empty", input: ""},
		{name: "plain text", input: "hello world"},
		{name: "text with angle brackets but no tag", input: "a < b and c > d"},
		{name: "other xml-ish tags", input: "<code>x</code>"},
		{name: "partial open at end", input: "answer<th"},
		{name: "partial close at end", input: "answer</"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleaned, reasoning, found := Extract(tt.input, Options{})
			if found {
				t.Fatalf("Extract(%q) found=true, want false", tt.input)
			}
			if cleaned != tt.input {
				t.Errorf("Extract(%q) cleaned=%q, want %q", tt.input, cleaned, tt.input)
			}
			if reasoning != "" {
				t.Errorf("Extract(%q) reasoning=%q, want empty", tt.input, reasoning)
			}
		})
	}
}

func TestExtract_Simple(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantClean string
		wantReason string
		wantFound bool
	}{
		{name: "single block", input: "answer<think>reasoning</think>more", wantClean: "answermore", wantReason: "reasoning", wantFound: true},
		{name: "leading whitespace stripped", input: "  <think>  x  </think>  y  ", wantClean: "y", wantReason: "x", wantFound: true},
		{name: "trailing whitespace stripped", input: "a<think>b</think>c  ", wantClean: "ac", wantReason: "b", wantFound: true},
		{name: "two blocks", input: "a<think>b</think>c<think>d</think>e", wantClean: "ace", wantReason: "b\n\nd", wantFound: true},
		{name: "multiline reasoning", input: "a<think>line1\nline2</think>b", wantClean: "ab", wantReason: "line1\nline2", wantFound: true},
		{name: "empty block", input: "a<think></think>b", wantClean: "ab", wantReason: "", wantFound: true},
		{name: "whitespace-only block", input: "a<think>   </think>b", wantClean: "ab", wantReason: "", wantFound: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleaned, reasoning, found := Extract(tt.input, Options{})
			if found != tt.wantFound {
				t.Fatalf("Extract(%q) found=%v, want %v", tt.input, found, tt.wantFound)
			}
			if cleaned != tt.wantClean {
				t.Errorf("Extract(%q) cleaned=%q, want %q", tt.input, cleaned, tt.wantClean)
			}
			if reasoning != tt.wantReason {
				t.Errorf("Extract(%q) reasoning=%q, want %q", tt.input, reasoning, tt.wantReason)
			}
		})
	}
}

func TestExtract_UnclosedBlock(t *testing.T) {
	// Unclosed block is treated as ordinary content, not reasoning.
	tests := []struct {
		name  string
		input string
	}{
		{name: "open without close", input: "a<think>reasoning"},
		{name: "open at very end", input: "a<think>"},
		{name: "close before open", input: "a</think>b<think>c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleaned, reasoning, found := Extract(tt.input, Options{})
			if found {
				t.Errorf("Extract(%q) found=true, want false", tt.input)
			}
			if cleaned != tt.input {
				t.Errorf("Extract(%q) cleaned=%q, want %q", tt.input, cleaned, tt.input)
			}
			if reasoning != "" {
				t.Errorf("Extract(%q) reasoning=%q, want empty", tt.input, reasoning)
			}
		})
	}
}

func TestExtract_CustomTags(t *testing.T) {
	opts := Options{TagOpen: "<thinking>", TagClose: "</thinking>"}
	cleaned, reasoning, found := Extract("a<thinking>b</thinking>c", opts)
	if !found {
		t.Fatalf("Extract with custom tags found=false, want true")
	}
	if cleaned != "ac" {
		t.Errorf("cleaned=%q, want %q", cleaned, "ac")
	}
	if reasoning != "b" {
		t.Errorf("reasoning=%q, want %q", reasoning, "b")
	}
}

func TestExtract_TagInsideCode(t *testing.T) {
	// Tags inside markdown code blocks are still rewritten. This is a
	// documented caveat of the feature: the package cannot know whether the
	// text is code.
	input := "```\n<think>x</think>\n```"
	cleaned, reasoning, found := Extract(input, Options{})
	if !found {
		t.Fatalf("found=false, want true")
	}
	if cleaned != "```\n\n```" {
		t.Errorf("cleaned=%q, want %q", cleaned, "```\n\n```")
	}
	if reasoning != "x" {
		t.Errorf("reasoning=%q, want %q", reasoning, "x")
	}
}

func TestState_Feed_FullBlock(t *testing.T) {
	s := NewState(Options{})
	cd, rd := s.Feed("a<think>x</think>b")
	if cd != "ab" {
		t.Errorf("content delta=%q, want %q", cd, "ab")
	}
	if rd != "x" {
		t.Errorf("reasoning delta=%q, want %q", rd, "x")
	}
	cd, rd = s.Flush()
	if cd != "" || rd != "" {
		t.Errorf("Flush after full block: got (%q,%q), want empty", cd, rd)
	}
}

func TestState_Feed_PartialTagAcrossChunks(t *testing.T) {
	s := NewState(Options{})
	cd, rd := s.Feed("answer <th")
	if cd != "answer " {
		t.Errorf("after partial open: content=%q, want %q", cd, "answer ")
	}
	if rd != "" {
		t.Errorf("after partial open: reasoning=%q, want empty", rd)
	}

	cd, rd = s.Feed("ink>reasoning")
	if cd != "" {
		t.Errorf("after tag open: content=%q, want empty", cd)
	}
	if rd != "" {
		t.Errorf("after tag open: reasoning=%q, want empty", rd)
	}

	cd, rd = s.Feed("</think> more")
	if cd != " more" {
		t.Errorf("after close: content=%q, want %q", cd, " more")
	}
	if rd != "reasoning" {
		t.Errorf("after close: reasoning=%q, want %q", rd, "reasoning")
	}
}

func TestState_Feed_MultipleBlocks(t *testing.T) {
	s := NewState(Options{})
	cd, rd := s.Feed("a<think>one</think>b<think>two</think>c")
	if cd != "abc" {
		t.Errorf("content=%q, want %q", cd, "abc")
	}
	if rd != "one\n\ntwo" {
		t.Errorf("reasoning=%q, want %q", rd, "one\n\ntwo")
	}
}

func TestState_Flush_UnclosedBlock(t *testing.T) {
	s := NewState(Options{})
	s.Feed("a<think>partial")
	cd, rd := s.Flush()
	// Unclosed block: the buffer is inThink, so flush emits as content.
	if cd != "<think>partial" {
		t.Errorf("flush content=%q, want %q", cd, "<think>partial")
	}
	if rd != "" {
		t.Errorf("flush reasoning=%q, want empty", rd)
	}
}

func TestState_Flush_PartialOpenTagTail(t *testing.T) {
	s := NewState(Options{})
	// Feed emits the safe prefix; the partial tag tail stays buffered.
	cd, _ := s.Feed("answer <th")
	if cd != "answer " {
		t.Errorf("feed content=%q, want %q", cd, "answer ")
	}
	// Flush emits the partial tail verbatim — a partial tag that can no
	// longer be completed is literal content and must not be dropped.
	cd, rd := s.Flush()
	if cd != "<th" {
		t.Errorf("flush content=%q, want %q", cd, "<th")
	}
	if rd != "" {
		t.Errorf("flush reasoning=%q, want empty", rd)
	}
}

func TestState_ExistingReasoningNotOverwritten(t *testing.T) {
	s := NewState(Options{})
	// Feed text without tags and confirm no reasoning emitted.
	cd, rd := s.Feed("plain text")
	if cd != "plain text" {
		t.Errorf("content=%q, want %q", cd, "plain text")
	}
	if rd != "" {
		t.Errorf("reasoning=%q, want empty", rd)
	}
}

func TestState_BufferCap(t *testing.T) {
	opts := Options{MaxBufferBytes: 32}
	s := NewState(opts)
	// Feed enough text to exceed the cap without a complete tag.
	big := strings.Repeat("x", 100)
	cd, _ := s.Feed(big + "<think>" + strings.Repeat("y", 100))
	// Should not panic, and should not have stuck everything in buffer.
	if s.buffer.Len() > 1000 {
		t.Errorf("buffer.Len()=%d after cap overflow, want bounded", s.buffer.Len())
	}
	_ = cd
}

func TestTransformStream_NoTags(t *testing.T) {
	input := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n"
	rc := TransformStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("output does not contain 'hello': %q", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("output missing [DONE]: %q", out)
	}
}

func TestTransformStream_SingleChunkWithTag(t *testing.T) {
	input := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a<think>r</think>b\"}}]}\n\ndata: [DONE]\n"
	rc := TransformStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, "\"reasoning_content\":\"r\"") {
		t.Errorf("output missing reasoning_content=r: %q", out)
	}
	if !strings.Contains(out, "\"content\":\"ab\"") {
		t.Errorf("output missing content=ab: %q", out)
	}
}

func TestTransformStream_PartialTagAcrossChunks(t *testing.T) {
	input := strings.Join([]string{
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer <th\"}}]}",
		"",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ink>reason\"}}]}",
		"",
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ing</think> end\"}}]}",
		"",
		"data: [DONE]",
		"",
	}, "\n")
	rc := TransformStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, "reasoning_content") {
		t.Errorf("output missing reasoning_content: %q", out)
	}
	if strings.Contains(out, "<think>") {
		t.Errorf("output still contains <think> literal: %q", out)
	}
}

func TestTransformStream_PreservesNonDeltaFields(t *testing.T) {
	input := "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"a<think>r</think>b\"},\"finish_reason\":null}],\"model\":\"m\"}\n\ndata: [DONE]\n"
	rc := TransformStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, "\"id\":\"x\"") {
		t.Errorf("output dropped id: %q", out)
	}
	if !strings.Contains(out, "\"model\":\"m\"") {
		t.Errorf("output dropped model: %q", out)
	}
	if !strings.Contains(out, "\"role\":\"assistant\"") {
		t.Errorf("output dropped role: %q", out)
	}
}

func TestTransformStream_UpstreamReasoningPreserved(t *testing.T) {
	input := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a<think>r</think>b\",\"reasoning_content\":\"upstream\"}}]}\n\ndata: [DONE]\n"
	rc := TransformStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, "\"reasoning_content\":\"upstream\"") {
		t.Errorf("upstream reasoning_content stomped: %q", out)
	}
	// And the <think> text should still be in content (untouched).
	if !strings.Contains(out, "<think>") {
		t.Errorf("upstream reasoning chunk should not be rewritten: %q", out)
	}
}

func TestTransformStream_InvalidJSONForwarded(t *testing.T) {
	input := "data: {not json}\n\ndata: [DONE]\n"
	rc := TransformStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, "{not json}") {
		t.Errorf("invalid JSON not forwarded: %q", out)
	}
}

func TestTransformStream_NonDataLinesForwarded(t *testing.T) {
	input := ": comment\n\nevent: test\nid: 1\n\ndata: [DONE]\n"
	rc := TransformStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	for _, want := range []string{": comment", "event: test", "id: 1", "[DONE]"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q: %q", want, out)
		}
	}
}

func TestTransformStream_MultipleChoices(t *testing.T) {
	input := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a<think>x</think>b\"}},{\"index\":1,\"delta\":{\"content\":\"c<think>y</think>d\"}}]}\n\ndata: [DONE]\n"
	rc := TransformStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, "\"reasoning_content\":\"x\"") {
		t.Errorf("missing choice 0 reasoning: %q", out)
	}
	if !strings.Contains(out, "\"reasoning_content\":\"y\"") {
		t.Errorf("missing choice 1 reasoning: %q", out)
	}
}

func TestReadAll_ClosesReader(t *testing.T) {
	rc := TransformStream(io.NopCloser(strings.NewReader("data: [DONE]\n")), Options{})
	_, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
}