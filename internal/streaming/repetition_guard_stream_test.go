package streaming

import (
	"bytes"
	"io"
	"strconv"
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

// stopEvent is the synthetic terminal chunk the guard appends on trigger
// before [DONE]. Test payloads carry no envelope (id/object/created/model),
// so the chunk is the minimal choices-only shape the guard emits for
// envelope-less streams.
func stopEvent(index int) string {
	return `data: {"choices":[{"delta":{},"finish_reason":"stop","index":` + strconv.Itoa(index) + "}]}\n\n"
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
	want := chatEvent("x") + chatEvent("a") + chatEvent("a") + chatEvent("a") + stopEvent(0) + doneEvent()

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
	want := chatEvent("hello ") + chatEvent("ab") + chatEvent("ab") + stopEvent(0) + doneEvent()

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
	want := chatEvent("prefix") + chatEvent(payload) + stopEvent(0) + doneEvent()

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
	want := chatEvent(payload) + stopEvent(0) + doneEvent()

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
	want := chatEvent(payload) + stopEvent(0) + doneEvent()

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

// ---------------------------------------------------------------------------
// WithTriggerCallback coverage
// ---------------------------------------------------------------------------

func TestRepetitionGuardStream_TriggerCallbackFiresOnce(t *testing.T) {
	callbackCount := 0
	opts := []GuardOption{WithTriggerCallback(func() { callbackCount++ })}

	limit := 3
	input := chatEvent("x") + chatEvent("a") + chatEvent("a") + chatEvent("a") + doneEvent()
	src := newSource(input)
	stream := NewRepetitionGuardStream(src, limit, 8, "gpt-4o", opts...)
	if stream == src {
		t.Fatal("expected wrapper with WithTriggerCallback, got original source")
	}

	_, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if callbackCount != 1 {
		t.Fatalf("callbackCount = %d, want 1", callbackCount)
	}
	if src.closeCount != 1 {
		t.Fatalf("expected source closed once, got %d", src.closeCount)
	}
}

func TestRepetitionGuardStream_NoTriggerCallbackNoFire(t *testing.T) {
	callbackCount := 0
	opts := []GuardOption{WithTriggerCallback(func() { callbackCount++ })}

	input := chatEvent("the ") + chatEvent("quick ") + chatEvent("brown ") + doneEvent()
	src := newSource(input)
	stream := NewRepetitionGuardStream(src, 3, 8, "gpt-4o", opts...)
	if stream == src {
		t.Fatal("expected wrapper, got original source")
	}

	_, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if callbackCount != 0 {
		t.Fatalf("callback fired on clean stream, count = %d", callbackCount)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	if src.closeCount != 1 {
		t.Fatalf("expected one source close, got %d", src.closeCount)
	}
}

// ---------------------------------------------------------------------------
// NewRepetitionGuardStream — nil source / disabled path coverage
// ---------------------------------------------------------------------------

func TestNewRepetitionGuardStream_NilSourceReturnsSource(t *testing.T) {
	var src io.ReadCloser = nil
	stream := NewRepetitionGuardStream(src, 3, 8, "gpt-4o")
	if stream != nil {
		t.Fatalf("expected nil for nil source, got %v", stream)
	}
}

func TestNewRepetitionGuardStream_DisabledPathNoOpts(t *testing.T) {
	src := newSource(chatEvent("hello"))
	stream := NewRepetitionGuardStream(src, 0, 8, "gpt-4o")
	if stream != src {
		t.Fatal("expected original source for limit=0")
	}
	stream = NewRepetitionGuardStream(src, -1, 8, "gpt-4o")
	if stream != src {
		t.Fatal("expected original source for limit=-1")
	}
}

// ---------------------------------------------------------------------------
// newGuardWithCounter — nil source / disabled path coverage
// ---------------------------------------------------------------------------

func TestNewGuardWithCounter_NilSourceReturnsSource(t *testing.T) {
	var src io.ReadCloser = nil
	stream := newGuardWithCounter(src, 3, 8, nil)
	if stream != nil {
		t.Fatalf("expected nil for nil source, got %v", stream)
	}
}

func TestNewGuardWithCounter_DisabledPath(t *testing.T) {
	src := newSource(chatEvent("hello"))
	stream := newGuardWithCounter(src, 0, 8, nil)
	if stream != src {
		t.Fatal("expected original source for limit=0")
	}
	stream = newGuardWithCounter(src, -5, 8, nil)
	if stream != src {
		t.Fatal("expected original source for limit=-5")
	}
}

// ---------------------------------------------------------------------------
// Read — missing branches
// ---------------------------------------------------------------------------

func TestRepetitionGuardStream_Read_EmptyBuffer(t *testing.T) {
	stream := newGuardWithCounter(newSource(chatEvent("x")), 3, 8, nil)
	n, err := stream.Read(nil)
	if n != 0 || err != nil {
		t.Fatalf("Read(nil) = (%d, %v), want (0, nil)", n, err)
	}
}

func TestRepetitionGuardStream_Read_SourceErrorPropagated(t *testing.T) {
	err := io.ErrNoProgress
	// Use a reader that returns err on first Read.
	srcReader := &errOnFirstRead{Reader: strings.NewReader(""), err: err}
	src := &recordingReadCloser{Reader: srcReader}
	stream := newGuardWithCounter(src, 3, 8, nil)
	_, errRead := io.ReadAll(stream)
	if errRead != err {
		t.Fatalf("expected error %v, got %v", err, errRead)
	}
}

// errOnFirstRead returns the given err on the first Read, then EOF.
type errOnFirstRead struct {
	io.Reader
	err  error
	done bool
}

func (r *errOnFirstRead) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		return 0, r.err
	}
	return r.Reader.Read(p)
}

func TestRepetitionGuardStream_Read_TriggeredReturnsEOF(t *testing.T) {
	// Force the guard into triggered state without reading the whole stream.
	payload := periodicRunPayload(3)
	input := chatEvent(payload) + doneEvent()
	src := newSource(input)
	stream := newGuardWithCounter(src, 3, 8, nil)

	// Drain to trigger.
	_, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}

	// Next read should return EOF.
	buf := make([]byte, 8)
	n, err := stream.Read(buf)
	if n != 0 || err != io.EOF {
		t.Fatalf("Read after trigger = (%d, %v), want (0, io.EOF)", n, err)
	}
}

func TestRepetitionGuardStream_Read_AfterCloseReturnsEOF(t *testing.T) {
	payload := periodicRunPayload(3)
	input := chatEvent(payload) + doneEvent()
	src := newSource(input)
	stream := newGuardWithCounter(src, 3, 8, nil)

	_, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}

	buf := make([]byte, 8)
	n, err := stream.Read(buf)
	if n != 0 || err != io.EOF {
		t.Fatalf("Read after Close = (%d, %v), want (0, io.EOF)", n, err)
	}
}

func TestRepetitionGuardStream_Read_SourceAlreadyDone(t *testing.T) {
	// Source returns EOF on first read with n==0.
	src := &recordingReadCloser{Reader: strings.NewReader("")}
	stream := newGuardWithCounter(src, 3, 8, nil)
	n, err := stream.Read(make([]byte, 8))
	if n != 0 || err != io.EOF {
		t.Fatalf("Read on empty source = (%d, %v), want (0, io.EOF)", n, err)
	}
}

// ---------------------------------------------------------------------------
// observe — runaway event, empty events, multi-events
// ---------------------------------------------------------------------------

func TestRepetitionGuardStream_Observe_RunawayEvent(t *testing.T) {
	// Build a single event whose data payload exceeds maxPendingEventBytes (16 KB).
	large := strings.Repeat("a", maxPendingEventBytes+1024)
	// Wrap in an SSE event (data: ...\n\n).
	event := "data: \"" + escapeJSONString(large) + "\"\n\n"
	src := newSource(event)
	stream := newGuardWithCounter(src, 3, 8, nil)
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	// The runaway event should be forwarded unchanged (no trigger, the content
	// is just repeated 'a' but the guard emits it without inspecting since no
	// event boundary was found until it was flushed).
	if !bytes.Contains(out, []byte(large)) {
		t.Fatalf("runaway event not forwarded, got: %q", string(out))
	}
}

func escapeJSONString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(byte(r))
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func TestRepetitionGuardStream_Observe_EmptyEventSkipped(t *testing.T) {
	// Two valid events separated by a blank line (empty event boundary).
	input := chatEvent("hello") + "\n\n" + chatEvent("world") + doneEvent()
	src := newSource(input)
	stream := newGuardWithCounter(src, 3, 8, nil)
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	// Both events should pass through.
	if !bytes.Contains(out, []byte("hello")) || !bytes.Contains(out, []byte("world")) {
		t.Fatalf("expected both events, got: %q", string(out))
	}
}

func TestRepetitionGuardStream_Observe_MultipleEventsInOneChunk(t *testing.T) {
	// Two SSE events packed back-to-back in a single source Read.
	input := chatEvent("a") + chatEvent("b") + doneEvent()
	src := newSource(input)
	stream := newGuardWithCounter(src, 3, 8, nil)
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	want := input
	if string(out) != want {
		t.Fatalf("multi-event mismatch\nwant: %q\ngot:  %q", want, string(out))
	}
}

// ---------------------------------------------------------------------------
// eventPayload — multi-line, comments, non-data events
// ---------------------------------------------------------------------------

func TestEventPayload_MultiLineEvent(t *testing.T) {
	// Two data: lines joined with \n.
	event := "data: hello\n" +
		"data: world\n\n"
	got := eventPayload([]byte(event))
	want := []byte("hello\nworld")
	if !bytes.Equal(got, want) {
		t.Fatalf("multi-line event payload\nwant: %q\ngot:  %q", want, got)
	}
}

func TestEventPayload_CommentOnlyReturnsNil(t *testing.T) {
	event := ": comment line\n\n"
	got := eventPayload([]byte(event))
	if got != nil {
		t.Fatalf("comment-only event payload = %q, want nil", got)
	}
}

func TestEventPayload_SingleLineNonDataEventReturnsNil(t *testing.T) {
	event := ": just a comment\n\n"
	got := eventPayload([]byte(event))
	if got != nil {
		t.Fatalf("single-line comment payload = %q, want nil", got)
	}
}

// ---------------------------------------------------------------------------
// inspectEvent — non-JSON payload, [DONE], missing choices
// ---------------------------------------------------------------------------

func TestRepetitionGuardStream_InspectEvent_NonJSONPayload(t *testing.T) {
	event := "data: not-json\n\n"
	src := newSource(event)
	stream := newGuardWithCounter(src, 3, 8, nil)
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	// Non-JSON should pass through unchanged.
	if !bytes.Contains(out, []byte("not-json")) {
		t.Fatalf("expected non-JSON passed through, got: %q", string(out))
	}
}

func TestRepetitionGuardStream_InspectEvent_MalformedJSON(t *testing.T) {
	event := "data: {broken json\n\n"
	src := newSource(event)
	stream := newGuardWithCounter(src, 3, 8, nil)
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if !bytes.Contains(out, []byte("broken json")) {
		t.Fatalf("expected malformed JSON passed through, got: %q", string(out))
	}
}

func TestRepetitionGuardStream_InspectEvent_NoChoicesKey(t *testing.T) {
	event := "data: {\"message\":\"hello\"}\n\n"
	src := newSource(event)
	stream := newGuardWithCounter(src, 3, 8, nil)
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if !bytes.Contains(out, []byte("hello")) {
		t.Fatalf("expected no-choices payload passed through, got: %q", string(out))
	}
}

func TestRepetitionGuardStream_InspectEvent_ChoicesNotArray(t *testing.T) {
	event := "data: {\"choices\":\"string\"}\n\n"
	src := newSource(event)
	stream := newGuardWithCounter(src, 3, 8, nil)
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if !bytes.Contains(out, []byte("choices")) {
		t.Fatalf("expected choices-string passed through, got: %q", string(out))
	}
}

func TestRepetitionGuardStream_InspectEvent_ChoicesArrayWithNonMap(t *testing.T) {
	event := "data: {\"choices\":[\"not-a-map\"]}\n\n"
	src := newSource(event)
	stream := newGuardWithCounter(src, 3, 8, nil)
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if !bytes.Contains(out, []byte("not-a-map")) {
		t.Fatalf("expected choices-non-map passed through, got: %q", string(out))
	}
}

// ---------------------------------------------------------------------------
// trigger — double-call no-op (observable via Close count)
// ---------------------------------------------------------------------------

func TestRepetitionGuardStream_TriggerDoubleCallNoOp(t *testing.T) {
	callbackCount := 0
	opts := []GuardOption{WithTriggerCallback(func() { callbackCount++ })}

	payload := periodicRunPayload(3)
	input := chatEvent(payload) + doneEvent()
	src := newSource(input)
	stream := NewRepetitionGuardStream(src, 3, 8, "gpt-4o", opts...)

	// First trigger via ReadAll.
	_, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if callbackCount != 1 {
		t.Fatalf("first trigger: callbackCount = %d, want 1", callbackCount)
	}
	firstClose := src.closeCount

	// Calling trigger again (e.g. via a second Close) must be a no-op.
	_ = stream.Close() // first Close calls closeSource
	_ = stream.Close() // second Close must not double-close
	if src.closeCount != firstClose {
		t.Fatalf("Close doubled source close: first=%d second=%d",
			firstClose, src.closeCount)
	}
}

// ---------------------------------------------------------------------------
// detectTokenRun — tail capacity trimming
// ---------------------------------------------------------------------------

func TestRepetitionGuardStream_DetectTokenRun_TailTrimming(t *testing.T) {
	// Use a counter that maps "x" → {1}. limit=3, maxPattern=8 → capacity=24.
	// Push 100 tokens so the tail is trimmed, then continue to exercise the
	// trim-on-each-iteration path.
	counter := newTestCounter{"x": {1}}
	// Force inspection by feeding a repeating pattern that triggers.
	payload := strings.Repeat(chatEvent("x"), 50)
	input := payload + doneEvent()
	src2 := newSource(input)
	stream2 := newGuardWithCounter(src2, 3, 8, counter)
	// The tail is trimmed each iteration; the guard should still detect the run.
	_, err := io.ReadAll(stream2)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if src2.closeCount != 1 {
		t.Fatalf("expected trigger after trimming, got closeCount=%d", src2.closeCount)
	}
}

// ---------------------------------------------------------------------------
// byteTailRepeats — n < need early return
// ---------------------------------------------------------------------------

func TestByteTailRepeats_NeedTooLarge(t *testing.T) {
	tail := []byte{1, 2, 3}
	// need=9 > len(tail)=3 → n < need → false
	if byteTailRepeats(tail, 1, 9) {
		t.Fatal("expected false when need > len(tail)")
	}
}

// ---------------------------------------------------------------------------
// byteRunLength — n < p path (called indirectly via detectRun)
// ---------------------------------------------------------------------------

func TestByteRunLength_ShortTail(t *testing.T) {
	// tail is 2 bytes, period is 3 → n < p → returns 0
	if got := byteRunLength([]byte("ab"), 3); got != 0 {
		t.Fatalf("byteRunLength([\"ab\"], 3) = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// contentDeltas — missing/invalid delta fields
// ---------------------------------------------------------------------------

func TestRepetitionGuardStream_ContentDeltas_MissingDelta(t *testing.T) {
	event := "data: {\"choices\":[{} ]}\n\n"
	src := newSource(event)
	stream := newGuardWithCounter(src, 3, 8, nil)
	_, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
}

func TestRepetitionGuardStream_ContentDeltas_DeltaNotMap(t *testing.T) {
	event := "data: {\"choices\":[{\"delta\":\"string\"}]}\n\n"
	src := newSource(event)
	stream := newGuardWithCounter(src, 3, 8, nil)
	_, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
}

func TestRepetitionGuardStream_ContentDeltas_ToolCallsSkipped(t *testing.T) {
	event := toolCallEvent("bigpayload")
	src := newSource(event)
	stream := newGuardWithCounter(src, 3, 8, nil)
	_, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if src.closeCount != 0 {
		t.Fatalf("tool_calls should not trigger, closeCount=%d", src.closeCount)
	}
}

func TestRepetitionGuardStream_ContentDeltas_FunctionCallSkipped(t *testing.T) {
	event := functionCallEvent("bigpayload")
	src := newSource(event)
	stream := newGuardWithCounter(src, 3, 8, nil)
	_, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if src.closeCount != 0 {
		t.Fatalf("function_call should not trigger, closeCount=%d", src.closeCount)
	}
}

func TestRepetitionGuardStream_ContentDeltas_ContentNonString(t *testing.T) {
	event := "data: {\"choices\":[{\"delta\":{\"content\":123}}]}\n\n"
	src := newSource(event)
	stream := newGuardWithCounter(src, 3, 8, nil)
	_, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
}

func TestRepetitionGuardStream_ContentDeltas_ContentEmptyString(t *testing.T) {
	event := "data: {\"choices\":[{\"delta\":{\"content\":\"\"}}]}\n\n"
	src := newSource(event)
	stream := newGuardWithCounter(src, 3, 8, nil)
	_, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// looksLikeEncodedBlob — density < 0.85 false path
// ---------------------------------------------------------------------------

func TestLooksLikeEncodedBlob_LowDensity(t *testing.T) {
	// 64 bytes with only a few encoded symbols mixed in → density < 0.85.
	// isEncodedSymbol returns true for a-z, A-Z, 0-9, so "abcdefghijklmnop"
	// is all encoded symbols → density = 1.0. Need to include non-symbols.
	mixed := "abcdefghijklmnop" + " .!?;" // space, period, exclaim, question are NOT encoded symbols
	window := mixed
	for len(window) < encodedWindowBytes {
		window += mixed
	}
	window = window[:encodedWindowBytes]
	if looksLikeEncodedBlob([]byte(window)) {
		t.Fatalf("expected false for low-density window, got true")
	}
}

// ---------------------------------------------------------------------------
// looksLikeEncodedBlob — density ≥ 0.85 AND entropy ≥ 4.5 false path
// ---------------------------------------------------------------------------

func TestLooksLikeEncodedBlob_HighEntropyAlnum(t *testing.T) {
	// All alphanumeric → density=1.0, but Shannon entropy ≥ 4.5 bits/char.
	// A 64-byte string with near-uniform distribution over 62 alnum chars.
	// Use the already-tested highEntropyUnit repeated.
	unit := "abcdefghijklmnopqrstuvwxyz012345" // 32 chars
	blob := unit + unit // 64 bytes, density=1.0, entropy ~4.9
	if looksLikeEncodedBlob([]byte(blob)) {
		t.Fatalf("expected false for high-entropy alnum, got true")
	}
}

// ---------------------------------------------------------------------------
// isEncodedSymbol — symbol branches
// ---------------------------------------------------------------------------

func TestIsEncodedSymbol_SymbolBranches(t *testing.T) {
	cases := []struct {
		b    byte
		want bool
	}{
		{'+', true},
		{'/', true},
		{'-', true},
		{'_', true},
		{'=', true},
	}
	for _, tc := range cases {
		if got := isEncodedSymbol(tc.b); got != tc.want {
			t.Fatalf("isEncodedSymbol(%q) = %v, want %v", tc.b, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// detectTokenRun — n < need early break (not enough tokens)
// ---------------------------------------------------------------------------

// nonRepeatingCounter returns unique IDs per call to avoid false triggers.
type nonRepeatingCounter struct {
	next int
}

func (c *nonRepeatingCounter) Tokens(text string) []int {
	c.next++
	return []int{c.next}
}

func TestDetectTokenRun_NeedTooLarge(t *testing.T) {
	// Append 4 times → 4 tokens. period=4, limit=3 → need=12.
	// Tail < need so the loop breaks early.
	counter := &nonRepeatingCounter{}
	payload := ""
	for i := 0; i < 4; i++ {
		payload += chatEvent("test")
	}
	payload += doneEvent()
	src2 := newSource(payload)
	stream2 := newGuardWithCounter(src2, 3, 4, counter)
	_, err := io.ReadAll(stream2)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if src2.closeCount != 0 {
		t.Fatalf("expected no trigger with insufficient tokens, got %d", src2.closeCount)
	}
}

// ---------------------------------------------------------------------------
// detectByteRun — runLength < need false path
// ---------------------------------------------------------------------------

func TestDetectByteRun_RunLengthBelowNeed(t *testing.T) {
	// tail = "xyxyx" (5 bytes, period 2, 2.5 copies) → periodic suffix run = 5.
	// need for p=2, limit=3 → need=6. runLength=5 < 6 → false.
	// Use detectRun directly for precision.
	if got := detectRun([]byte("xyxyx"), 3); got {
		t.Fatalf("detectRun(\"xyxyx\", 3) = true, want false (runLength 5 < need 6)")
	}
}
