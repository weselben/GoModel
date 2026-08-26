package thinkextract

import (
	"strings"
	"testing"
)

func TestDefaultTagPairs_HasThinkTag(t *testing.T) {
	pairs := DefaultTagPairs()
	if len(pairs) == 0 {
		t.Fatalf("DefaultTagPairs returned empty list")
	}
	var found bool
	for _, p := range pairs {
		if p.Open == "<think>" && p.Close == "</think>" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("DefaultTagPairs missing the canonical <think> pair")
	}
}

func TestParseTagPairs_Empty(t *testing.T) {
	if got := ParseTagPairs(""); got != nil {
		t.Errorf("ParseTagPairs(\"\") = %+v, want nil", got)
	}
	if got := ParseTagPairs("   "); got != nil {
		t.Errorf("ParseTagPairs(\"   \") = %+v, want nil", got)
	}
}

func TestParseTagPairs_Single(t *testing.T) {
	got := ParseTagPairs("<think>...</think>")
	if len(got) != 1 || got[0].Open != "<think>" || got[0].Close != "</think>" {
		t.Errorf("got %+v, want single <think> pair", got)
	}
}

func TestParseTagPairs_Multiple(t *testing.T) {
	got := ParseTagPairs("<think>...</think>,<thinking>...</thinking>")
	if len(got) != 2 {
		t.Fatalf("got %+v, want 2 pairs", got)
	}
	if got[0] != (TagPair{Open: "<think>", Close: "</think>"}) {
		t.Errorf("pair[0]=%+v", got[0])
	}
	if got[1] != (TagPair{Open: "<thinking>", Close: "</thinking>"}) {
		t.Errorf("pair[1]=%+v", got[1])
	}
}

func TestParseTagPairs_SkipsMalformed(t *testing.T) {
	// Missing "..." separator, or empty open/close: skipped silently so
	// one bad entry never breaks the whole list.
	got := ParseTagPairs("<think>...</think>,malformed,<thinking>...</thinking>,...close-only")
	if len(got) != 2 {
		t.Errorf("got %+v, want 2 pairs (malformed entries skipped)", got)
	}
}

func TestExtract_AlternateTag(t *testing.T) {
	opts := Options{TagPairs: []TagPair{{Open: "<thinking>", Close: "</thinking>"}}}
	cleaned, reasoning, found := Extract("a<thinking>b</thinking>c", opts)
	if !found {
		t.Fatalf("found=false, want true")
	}
	if cleaned != "ac" {
		t.Errorf("cleaned=%q, want %q", cleaned, "ac")
	}
	if reasoning != "b" {
		t.Errorf("reasoning=%q, want %q", reasoning, "b")
	}
}

func TestExtract_DefaultPairs_MatchesMultiple(t *testing.T) {
	// Default list matches both <think> and <thinking> blocks in one input.
	input := "<think>a</think> middle <thinking>b</thinking> end"
	cleaned, reasoning, found := Extract(input, Options{})
	if !found {
		t.Fatalf("found=false, want true")
	}
	if cleaned != "middle  end" {
		t.Errorf("cleaned=%q, want %q", cleaned, "middle  end")
	}
	if reasoning != "a\n\nb" {
		t.Errorf("reasoning=%q, want %q", reasoning, "a\n\nb")
	}
}

func TestExtract_EarliestOpenWins(t *testing.T) {
	// When two configured open tags are both present, the earliest absolute
	// position is the one used.
	opts := Options{TagPairs: []TagPair{
		{Open: "<a>", Close: "</a>"},
		{Open: "<b>", Close: "</b>"},
	}}
	cleaned, reasoning, found := Extract("x<a>first</a>y<b>second</b>z", opts)
	if !found {
		t.Fatalf("found=false")
	}
	if cleaned != "xyz" {
		t.Errorf("cleaned=%q, want %q", cleaned, "xyz")
	}
	if reasoning != "first\n\nsecond" {
		t.Errorf("reasoning=%q, want %q", reasoning, "first\n\nsecond")
	}
}

func TestExtract_UnclosedAlternateTag(t *testing.T) {
	// Unclosed alternate tag: treated as ordinary content (no rewrite).
	opts := Options{TagPairs: []TagPair{{Open: "<thinking>", Close: "</thinking>"}}}
	input := "before<thinking>open"
	cleaned, reasoning, found := Extract(input, opts)
	if found {
		t.Errorf("found=true, want false on unclosed alternate tag")
	}
	if cleaned != input {
		t.Errorf("cleaned=%q, want %q", cleaned, input)
	}
	if reasoning != "" {
		t.Errorf("reasoning=%q, want empty", reasoning)
	}
}

func TestState_AlternateTag(t *testing.T) {
	opts := Options{TagPairs: []TagPair{{Open: "<thinking>", Close: "</thinking>"}}}
	s := NewState(opts)
	cd, rd := s.Feed("a<thinking>x</thinking>b")
	if cd != "ab" {
		t.Errorf("content=%q, want %q", cd, "ab")
	}
	if rd != "x" {
		t.Errorf("reasoning=%q, want %q", rd, "x")
	}
}

func TestState_DefaultPairs_StreamBothKinds(t *testing.T) {
	s := NewState(Options{})
	cd, rd := s.Feed("a<think>x</think>middle<thinking>y</thinking>end")
	if cd != "amiddleend" {
		t.Errorf("content=%q, want %q", cd, "amiddleend")
	}
	if rd != "x\n\ny" {
		t.Errorf("reasoning=%q, want %q", rd, "x\n\ny")
	}
}

func TestState_BufferCapOverflowMultiPair(t *testing.T) {
	opts := Options{MaxBufferBytes: 8, TagPairs: []TagPair{{Open: "<a>", Close: "</a>"}}}
	s := NewState(opts)
	cd, _ := s.Feed(strings.Repeat("z", 64))
	if cd == "" {
		t.Errorf("expected overflow content emission")
	}
}

func TestSafeEmitPrefixAll_EmptyPairs(t *testing.T) {
	if got := safeEmitPrefixAll("text", nil); got != 4 {
		t.Errorf("got %d, want 4", got)
	}
}

func TestEnabledFor_PerSurfaceDefaults(t *testing.T) {
	// Per-surface pointers unset falls back to enabled for any surface.
	o := Options{}
	if !o.EnabledFor(SurfaceChat) {
		t.Errorf("EnabledFor(chat)=false, want true")
	}
	if !o.EnabledFor(SurfaceMessages) {
		t.Errorf("EnabledFor(messages)=false, want true")
	}
	if !o.EnabledFor("") {
		t.Errorf("EnabledFor(empty)=false, want true")
	}
}

func TestEnabledFor_ExplicitDisable(t *testing.T) {
	off := false
	on := true
	o := Options{ChatEnabled: &off, MessagesEnabled: &on}
	if o.EnabledFor(SurfaceChat) {
		t.Errorf("chat should be disabled")
	}
	if !o.EnabledFor(SurfaceMessages) {
		t.Errorf("messages should be enabled")
	}
}