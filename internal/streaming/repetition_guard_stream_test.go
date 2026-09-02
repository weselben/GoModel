package streaming

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

// recordingReadCloser wraps an io.Reader and counts Close calls.
type recordingReadCloser struct {
	io.Reader
	closeCount int
	closeErr   error
}

func (r *recordingReadCloser) Close() error {
	r.closeCount++
	return r.closeErr
}

func newSource(data string) *recordingReadCloser {
	return &recordingReadCloser{Reader: strings.NewReader(data)}
}

// chatEvent builds a single-line SSE chat-completion event carrying content as
// delta.content (JSON-escaped).
func chatEvent(content string) string {
	encoded, _ := json.Marshal(content)
	return "data: {\"choices\":[{\"delta\":{\"content\":" + string(encoded) + "}}]}\n\n"
}

func doneEvent() string {
	return "data: [DONE]\n\n"
}

// highEntropyUnit is a 32-char alnum unit (~5.0 bits/char entropy) whose
// repeats avoid the encoded-blob skip heuristic, so they reach the byte-period
// detector.
const highEntropyUnit = "abcdefghijklmnopqrstuvwxyz123456"

func periodicRunPayload(reps int) string {
	return strings.Repeat(highEntropyUnit, reps)
}

// toolCallEvent builds an SSE event whose delta carries only tool_calls.
func toolCallEvent(args string) string {
	return `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"do","arguments":"` + args + `"}}]}}]}` + "\n\n"
}

// functionCallEvent builds an SSE event whose delta carries only function_call.
func functionCallEvent(args string) string {
	return `data: {"choices":[{"index":0,"delta":{"function_call":{"name":"do","arguments":"` + args + `"}}}]}` + "\n\n"
}

// newTestCounter maps fixed strings to fixed token IDs (exact match, copied).
type newTestCounter map[string][]int

func (c newTestCounter) Tokens(text string) []int {
	ids, ok := c[text]
	if !ok {
		return nil
	}
	out := make([]int, len(ids))
	copy(out, ids)
	return out
}

func TestRepetitionGuardStream_DisabledReturnsOriginal(t *testing.T) {
	for _, limit := range []int{0, -5} {
		data := chatEvent("hello") + chatEvent("world") + doneEvent()
		src := newSource(data)
		stream := NewRepetitionGuardStream(src, limit, 8, "gpt-4o")
		if stream != src {
			t.Fatalf("limit=%d: expected original source, got wrapper", limit)
		}
		out, err := io.ReadAll(stream)
		if err != nil {
			t.Fatalf("limit=%d: ReadAll error: %v", limit, err)
		}
		if string(out) != data {
			t.Fatalf("limit=%d: passthrough mismatch\nwant: %q\ngot:  %q", limit, data, string(out))
		}
	}
}

func TestRepetitionGuardStream_TokenStutter(t *testing.T) {
	// limit=3, single token 'a'. Accepted leak = 3 copies (no holdback).
	limit := 3
	input := chatEvent("x") + chatEvent("a") + chatEvent("a") + chatEvent("a") + doneEvent()
	want := chatEvent("x") + chatEvent("a") + chatEvent("a") + chatEvent("a") + doneEvent()

	src := newSource(input)
	stream := newGuardWithCounter(src, limit, 8, newTestCounter{"x": {1}, "a": {5}})
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(out) != want {
		t.Fatalf("stutter mismatch\nwant: %q\ngot:  %q", want, string(out))
	}
	if src.closeCount != 1 {
		t.Fatalf("expected source closed once on trigger, got %d", src.closeCount)
	}
}

func TestRepetitionGuardStream_ChainLoop(t *testing.T) {
	// limit=2, unit 'ab' as two tokens.
	limit := 2
	input := chatEvent("hello ") + chatEvent("ab") + chatEvent("ab") + chatEvent("ab") + doneEvent()
	want := chatEvent("hello ") + chatEvent("ab") + chatEvent("ab") + doneEvent()

	src := newSource(input)
	stream := newGuardWithCounter(src, limit, 8, newTestCounter{"hello ": {1}, "ab": {2, 3}})
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(out) != want {
		t.Fatalf("chain mismatch\nwant: %q\ngot:  %q", want, string(out))
	}
	if src.closeCount != 1 {
		t.Fatalf("expected source closed once, got %d", src.closeCount)
	}
}

func TestRepetitionGuardStream_FallbackStutter(t *testing.T) {
	// Byte fallback: a single delta carrying a 96-byte periodic run (high enough
	// entropy to avoid the encoded-blob skip heuristic) trips the guard.
	limit := 3
	payload := periodicRunPayload(3) // 96 bytes
	input := chatEvent("prefix") + chatEvent(payload) + doneEvent()
	want := chatEvent("prefix") + chatEvent(payload) + doneEvent()

	src := newSource(input)
	stream := newGuardWithCounter(src, limit, 8, nil) // nil counter -> byte fallback
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(out) != want {
		t.Fatalf("fallback stutter mismatch\nwant: %q\ngot:  %q", want, string(out))
	}
	if src.closeCount != 1 {
		t.Fatalf("expected source closed once, got %d", src.closeCount)
	}
}

func TestRepetitionGuardStream_CleanStreamIdentical(t *testing.T) {
	input := chatEvent("the ") + chatEvent("quick ") + chatEvent("brown ") + chatEvent("fox ") + doneEvent()
	src := newSource(input)
	stream := newGuardWithCounter(src, 3, 8, newTestCounter{
		"the ": {1}, "quick ": {2}, "brown ": {3}, "fox ": {4},
	})
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(out) != input {
		t.Fatalf("clean stream mismatch\nwant: %q\ngot:  %q", input, string(out))
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	if src.closeCount != 1 {
		t.Fatalf("expected one source close, got %d", src.closeCount)
	}
}

func TestRepetitionGuardStream_Base64BlobNoTrigger(t *testing.T) {
	// High symbol density + low entropy -> skipped by the encoded-blob heuristic.
	blob := strings.Repeat("QUJD", 64) // 256 bytes, entropy 2.0, density 1.0
	input := chatEvent(blob) + chatEvent(blob) + doneEvent()
	src := newSource(input)
	stream := newGuardWithCounter(src, 3, 8, nil)
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(out) != input {
		t.Fatalf("base64 passthrough mismatch\nwant: %q\ngot:  %q", input, string(out))
	}
	if src.closeCount != 0 {
		t.Fatalf("expected no trigger, got %d close calls", src.closeCount)
	}
}

func TestRepetitionGuardStream_FencedCodeNoTrigger(t *testing.T) {
	longA := strings.Repeat("a", 120)
	input := chatEvent("```\n") + chatEvent(longA) + chatEvent("```\n") + doneEvent()
	src := newSource(input)
	stream := newGuardWithCounter(src, 3, 8, nil)
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(out) != input {
		t.Fatalf("fenced passthrough mismatch\nwant: %q\ngot:  %q", input, string(out))
	}
	if src.closeCount != 0 {
		t.Fatalf("expected no trigger inside fenced code, got %d close calls", src.closeCount)
	}
}

func TestRepetitionGuardStream_MarkdownTableNoTrigger(t *testing.T) {
	row := "| a | b |\n"
	input := chatEvent(row) + chatEvent(row) + chatEvent(row) + chatEvent(row) + doneEvent()
	src := newSource(input)
	stream := newGuardWithCounter(src, 3, 8, nil)
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(out) != input {
		t.Fatalf("table passthrough mismatch\nwant: %q\ngot:  %q", input, string(out))
	}
	if src.closeCount != 0 {
		t.Fatalf("expected no trigger on table rows, got %d close calls", src.closeCount)
	}
}

func TestRepetitionGuardStream_WhitespaceRunNoTrigger(t *testing.T) {
	longNL := strings.Repeat("\n", 100)
	input := chatEvent(longNL) + chatEvent(longNL) + doneEvent()
	src := newSource(input)
	stream := newGuardWithCounter(src, 3, 8, nil)
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(out) != input {
		t.Fatalf("whitespace passthrough mismatch\nwant: %q\ngot:  %q", input, string(out))
	}
	if src.closeCount != 0 {
		t.Fatalf("expected no trigger on whitespace runs, got %d close calls", src.closeCount)
	}
}

func TestRepetitionGuardStream_ToolCallsNeverTrigger(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"tool_calls", toolCallEvent(periodicRunPayload(4))},
		{"function_call", functionCallEvent(periodicRunPayload(4))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := tc.raw + tc.raw + doneEvent()
			src := newSource(input)
			stream := newGuardWithCounter(src, 3, 8, nil)
			out, err := io.ReadAll(stream)
			if err != nil {
				t.Fatalf("ReadAll error: %v", err)
			}
			if string(out) != input {
				t.Fatalf("tool_calls passthrough mismatch\nwant: %q\ngot:  %q", input, string(out))
			}
			if src.closeCount != 0 {
				t.Fatalf("expected no trigger on %s deltas, got %d close calls", tc.name, src.closeCount)
			}
		})
	}
}

func TestRepetitionGuardStream_UnknownModelFallsBack(t *testing.T) {
	// Production constructor with an unresolvable model triggers via byte fallback.
	payload := periodicRunPayload(3) // 96 bytes, period 32
	input := chatEvent(payload) + chatEvent(payload) + doneEvent()
	want := chatEvent(payload) + doneEvent()

	src := newSource(input)
	stream := NewRepetitionGuardStream(src, 3, 8, "no-such-model-xyz")
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(out) != want {
		t.Fatalf("unknown-model fallback mismatch\nwant: %q\ngot:  %q", want, string(out))
	}
	if src.closeCount != 1 {
		t.Fatalf("expected one source close, got %d", src.closeCount)
	}
}

func TestRepetitionGuardStream_CloseIdempotent(t *testing.T) {
	payload := periodicRunPayload(3)
	input := chatEvent(payload) + doneEvent()
	src := newSource(input)
	stream := newGuardWithCounter(src, 3, 8, nil)

	if _, err := io.ReadAll(stream); err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	// Trigger already closed the source once.
	if src.closeCount != 1 {
		t.Fatalf("expected 1 close after trigger, got %d", src.closeCount)
	}
	// Explicit Close must not double-close.
	if err := stream.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second Close error: %v", err)
	}
	if src.closeCount != 1 {
		t.Fatalf("expected exactly 1 source close (idempotent), got %d", src.closeCount)
	}
}

func TestRepetitionGuardStream_TriggerBeforeUpstreamEOF(t *testing.T) {
	// Trigger fires early; trailing upstream bytes are dropped, output ends in [DONE].
	payload := periodicRunPayload(3)
	input := chatEvent(payload) + chatEvent("never-reached") + doneEvent()
	want := chatEvent(payload) + doneEvent()

	src := newSource(input)
	stream := newGuardWithCounter(src, 3, 8, nil)
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(out) != want {
		t.Fatalf("early-trigger mismatch\nwant: %q\ngot:  %q", want, string(out))
	}
	if !bytes.HasSuffix(out, []byte(doneEvent())) {
		t.Fatalf("expected output to end with [DONE], got %q", string(out))
	}
	if strings.Contains(string(out), "never-reached") {
		t.Fatalf("expected trailing bytes to be dropped after trigger")
	}
	if src.closeCount != 1 {
		t.Fatalf("expected source closed once, got %d", src.closeCount)
	}
	// Subsequent reads return EOF.
	buf := make([]byte, 16)
	if _, err := stream.Read(buf); err != io.EOF {
		t.Fatalf("expected io.EOF after termination, got %v", err)
	}
}

func TestDetectRun(t *testing.T) {
	cases := []struct {
		name string
		tail string
		want bool
	}{
		{"96 a's limit 3", strings.Repeat("a", 96), true},
		{"95 a's below min run", strings.Repeat("a", 95), false},
		{"ab*60 limit 2", strings.Repeat("ab", 60), true},
		{"ab*30 below min run", strings.Repeat("ab", 30), false},
		{"xyz*32 limit 3", strings.Repeat("xyz", 32), true},
		{"xyz*30 below min run", strings.Repeat("xyz", 30), false},
		{"prefixed ab run", "pre" + strings.Repeat("ab", 50), true},
		{"no repeat", "abcdefgh", false},
		{"short tail", "ab", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectRun([]byte(tc.tail), 3); got != tc.want {
				t.Fatalf("detectRun(%q) = %v, want %v", tc.tail, got, tc.want)
			}
		})
	}
}

func TestTokenTailRepeats(t *testing.T) {
	cases := []struct {
		name string
		tail []int
		p    int
		need int
		want bool
	}{
		{"three copies of single token", []int{1, 5, 5, 5}, 1, 3, true},
		{"two copies of 2-token unit", []int{1, 2, 3, 2, 3}, 2, 4, true},
		{"mismatch in suffix", []int{1, 5, 5, 6}, 1, 3, false},
		{"not enough length", []int{5, 5}, 1, 3, false},
		{"partial run at tail only", []int{1, 2, 3, 4}, 1, 3, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokenTailRepeats(tc.tail, tc.p, tc.need); got != tc.want {
				t.Fatalf("tokenTailRepeats(%v, %d, %d) = %v, want %v",
					tc.tail, tc.p, tc.need, got, tc.want)
			}
		})
	}
}

func TestSkipHeuristics(t *testing.T) {
	t.Run("isMarkdownTableRow", func(t *testing.T) {
		cases := []struct {
			in   string
			want bool
		}{
			{"| a | b |", true},
			{"  | a | b |", true},
			{"a | b |", false},
			{"", false},
			{"   ", false},
		}
		for _, tc := range cases {
			if got := isMarkdownTableRow([]byte(tc.in)); got != tc.want {
				t.Fatalf("isMarkdownTableRow(%q) = %v, want %v", tc.in, got, tc.want)
			}
		}
	})

	t.Run("hasLongWhitespaceRun", func(t *testing.T) {
		cases := []struct {
			in   string
			want bool
		}{
			{"a        b", true}, // 8 spaces
			{"abc defg", false},
			{"\t\t\t\t\t\t\t\t", true},   // 8 tabs
			{"a\n\n\n\n\n\n\n\nb", true}, // 8 newlines
			{"", false},
			{"a   b   c", false},
		}
		for _, tc := range cases {
			if got := hasLongWhitespaceRun([]byte(tc.in)); got != tc.want {
				t.Fatalf("hasLongWhitespaceRun(%q) = %v, want %v", tc.in, got, tc.want)
			}
		}
	})

	t.Run("looksLikeEncodedBlob", func(t *testing.T) {
		cases := []struct {
			name string
			in   string
			want bool
		}{
			{"base64-like", strings.Repeat("QUJD", 32), true},
			{"repeated a", strings.Repeat("aaaa", 32), true},
			{"high-entropy alnum", strings.Repeat("abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWX", 1), false}, // dense but high entropy -> not a blob
			{"short input", "hello", false},
		}
		for _, tc := range cases {
			if got := looksLikeEncodedBlob([]byte(tc.in)); got != tc.want {
				t.Fatalf("%s: looksLikeEncodedBlob(len=%d) = %v, want %v",
					tc.name, len(tc.in), got, tc.want)
			}
		}
	})
}

func TestClampGuardParams(t *testing.T) {
	cases := []struct {
		inLimit     int
		inPattern   int
		wantLimit   int
		wantPattern int
	}{
		{3, 8, 3, 8},
		{1, 8, 2, 8},
		{0, 0, 2, 8},
		{5, 0, 5, 8},
		{5, 100, 5, 64},
		{2, 1, 2, 1},
		{2, 64, 2, 64},
		{10, -3, 10, 8},
	}
	for _, tc := range cases {
		gotLimit, gotPattern := clampGuardParams(tc.inLimit, tc.inPattern)
		if gotLimit != tc.wantLimit || gotPattern != tc.wantPattern {
			t.Fatalf("clampGuardParams(%d,%d) = (%d,%d), want (%d,%d)",
				tc.inLimit, tc.inPattern, gotLimit, gotPattern, tc.wantLimit, tc.wantPattern)
		}
	}
}
