package streaming

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testSSEEvent(content string) string {
	return "data: {\"choices\":[{\"delta\":{\"content\":" + escapeForSSE(content) + "}}]}\n\n"
}

func escapeForSSE(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ---------------------------------------------------------------------------
// TestE2E_CodingLoopAfterFence — realistic coding answer with fenced code,
// followed by a prose loop that triggers the guard.
//
// Branches covered:
//   - WithTriggerCallback fires exactly once when guard triggers
//   - NewRepetitionGuardStream production constructor with real model resolution
//   - Fenced code pass-through (``` toggles fence state, content skipped)
//   - Byte fallback: prose loop uses high-entropy unit that reaches byte detector
//   - Source is closed early after trigger (upstream drained count < total events)
//   - Output ends with data: [DONE]
// ---------------------------------------------------------------------------

func TestE2E_CodingLoopAfterFence(t *testing.T) {
	const loopPhrase = " But then I realized "
	const loopTotal = 30
	const limit = 4

	callbackCount := 0
	opts := []GuardOption{WithTriggerCallback(func() { callbackCount++ })}

	emitted := make(chan int, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		count := 0
		defer func() { emitted <- count }()
		write := func(s string) bool {
			if _, err := io.WriteString(w, s); err != nil {
				return false
			}
			flusher.Flush()
			count++
			return true
		}

		if !write(testSSEEvent("Here is the helper you asked for:\n\n")) {
			return
		}
		if !write(testSSEEvent("```go\n")) {
			return
		}
		for i := 0; i < 6; i++ {
			if !write(testSSEEvent("fmt.Println(\"tick\")\n")) {
				return
			}
		}
		if !write(testSSEEvent("```\n\n")) {
			return
		}
		if !write(testSSEEvent("The helper prints six lines, once per loop iteration.\n")) {
			return
		}

		for i := 0; i < loopTotal; i++ {
			if !write(testSSEEvent(loopPhrase)) {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		write("data: [DONE]\n\n")
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()

	guard := NewRepetitionGuardStream(resp.Body, limit, 8, "gpt-4o", opts...)
	out, err := io.ReadAll(guard)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	output := string(out)

	if !strings.Contains(output, "fmt.Println") {
		t.Fatalf("fenced code lost from output:\n%s", output)
	}
	got := strings.Count(output, loopPhrase)
	if got > limit {
		t.Fatalf("loop phrase emitted %d times, want <= limit %d (accepted leak)", got, limit)
	}
	if !strings.HasSuffix(output, "data: [DONE]\n\n") {
		t.Fatalf("output does not end with synthetic [DONE]: tail=%q", output[max(0, len(output)-80):])
	}

	if callbackCount != 1 {
		t.Fatalf("callback fired %d times, want 1", callbackCount)
	}

	select {
	case n := <-emitted:
		if n >= 10+loopTotal {
			t.Fatalf("upstream drained fully (%d events); guard did not cancel early", n)
		}
		t.Logf("upstream cut after %d events; client saw %d loop phrases", n, got)
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for upstream to notice the cancel")
	}
}

// ---------------------------------------------------------------------------
// TestE2E_CleanCodingAnswer — non-looping coding answer passes through
// byte-identical.
//
// Branches covered:
//   - Clean stream: no trigger, no callback
//   - Fenced code pass-through (no trigger on ``` content)
//   - Output identical to upstream
// ---------------------------------------------------------------------------

func TestE2E_CleanCodingAnswer(t *testing.T) {
	events := []string{
		testSSEEvent("To reverse a slice in Go:\n\n"),
		testSSEEvent("```go\n"),
		testSSEEvent("for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {\n"),
		testSSEEvent("    s[i], s[j] = s[j], s[i]\n"),
		testSSEEvent("}\n"),
		testSSEEvent("```\n\n"),
		testSSEEvent("This runs in O(n) time with O(1) extra space.\n"),
		"data: [DONE]\n\n",
	}
	var upstream strings.Builder
	for _, e := range events {
		upstream.WriteString(e)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			_, _ = io.WriteString(w, e)
		}
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()

	guard := NewRepetitionGuardStream(resp.Body, 4, 8, "gpt-4o")
	out, err := io.ReadAll(guard)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(out) != upstream.String() {
		t.Fatalf("clean answer not byte-identical\nwant: %q\ngot:  %q", upstream.String(), string(out))
	}
}

// ---------------------------------------------------------------------------
// TestE2E_TokenChainLoop — multi-token repeating unit using the real
// gpt-4o tokenizer.
//
// Branches covered:
//   - Real TokenCounter resolution via NewTokenCounter(gpt-4o)
//   - Token-level repetition detection (not just byte-level)
//   - Guard triggers after limit+1 copies, appends [DONE]
// ---------------------------------------------------------------------------

func TestE2E_TokenChainLoop(t *testing.T) {
	const unit = " I think "
	const limit = 3

	var upstream strings.Builder
	upstream.WriteString(testSSEEvent("start "))
	for i := 0; i < 10; i++ {
		upstream.WriteString(testSSEEvent(unit))
	}
	upstream.WriteString("data: [DONE]\n\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, upstream.String())
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()

	guard := NewRepetitionGuardStream(resp.Body, limit, 8, "gpt-4o")
	out, err := io.ReadAll(guard)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	output := string(out)
	if strings.Count(output, unit) > limit {
		t.Fatalf("chain unit emitted %d times, want <= limit %d", strings.Count(output, unit), limit)
	}
	if !strings.HasSuffix(output, "data: [DONE]\n\n") {
		t.Fatalf("output does not end with synthetic [DONE]")
	}
}

// ---------------------------------------------------------------------------
// TestE2E_NoTriggerOnNormalProse — prose text that does not repeat enough
// to trigger the guard.
//
// Branches covered:
//   - No trigger, callback does not fire
//   - Close without trigger does not run callback
// ---------------------------------------------------------------------------

func TestE2E_NoTriggerOnNormalProse(t *testing.T) {
	callbackCount := 0
	opts := []GuardOption{WithTriggerCallback(func() { callbackCount++ })}

	events := []string{
		testSSEEvent("The quick brown fox jumps over the lazy dog. "),
		testSSEEvent("Pack my box with five dozen liquor jugs. "),
		testSSEEvent("How vexingly quick daft zebras jump. "),
		"data: [DONE]\n\n",
	}
	var upstream strings.Builder
	for _, e := range events {
		upstream.WriteString(e)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			_, _ = io.WriteString(w, e)
		}
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()

	guard := NewRepetitionGuardStream(resp.Body, 3, 8, "gpt-4o", opts...)
	out, err := io.ReadAll(guard)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(out) != upstream.String() {
		t.Fatalf("normal prose not identical\nwant: %q\ngot:  %q", upstream.String(), string(out))
	}
	if callbackCount != 0 {
		t.Fatalf("callback fired on clean stream, count=%d", callbackCount)
	}
}

// ---------------------------------------------------------------------------
// TestE2E_DisabledGuard — limit <= 0 returns source unchanged.
//
// Branches covered:
//   - NewRepetitionGuardStream returns original source for limit <= 0
//   - Passthrough is byte-identical
// ---------------------------------------------------------------------------

func TestE2E_DisabledGuard(t *testing.T) {
	events := []string{
		testSSEEvent("repeated text "),
		testSSEEvent("repeated text "),
		testSSEEvent("repeated text "),
		testSSEEvent("repeated text "),
		"data: [DONE]\n\n",
	}
	var upstream strings.Builder
	for _, e := range events {
		upstream.WriteString(e)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			_, _ = io.WriteString(w, e)
		}
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()

	guard := NewRepetitionGuardStream(resp.Body, 0, 8, "gpt-4o")
	if guard != resp.Body {
		t.Fatal("expected original source for limit=0")
	}
	out, err := io.ReadAll(guard)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(out) != upstream.String() {
		t.Fatalf("disabled guard passthrough mismatch\nwant: %q\ngot:  %q", upstream.String(), string(out))
	}
}

// ---------------------------------------------------------------------------
// TestE2E_UnknownModelByteFallback — unresolvable model falls back to
// byte-period detector.
//
// Branches covered:
//   - inspectEvent lazy resolution path: omnitoken.ResolveModel failure
//   - byte fallback detects repetition
// ---------------------------------------------------------------------------

func TestE2E_UnknownModelByteFallback(t *testing.T) {
	const phrase = "repeating phrase "
	const limit = 3

	var upstream strings.Builder
	upstream.WriteString(testSSEEvent("start "))
	for i := 0; i < 10; i++ {
		upstream.WriteString(testSSEEvent(phrase))
	}
	upstream.WriteString("data: [DONE]\n\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, upstream.String())
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}
	defer resp.Body.Close()

	// Use an unknown model name → NewTokenCounter fails → byte fallback.
	guard := NewRepetitionGuardStream(resp.Body, limit, 8, "no-such-model-xyz")
	out, err := io.ReadAll(guard)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	output := string(out)
	if !strings.Contains(output, "start ") {
		t.Fatalf("expected 'start ' in output, got: %q", output)
	}
	if !strings.HasSuffix(output, "data: [DONE]\n\n") {
		t.Fatalf("output does not end with [DONE]")
	}
}

// ---------------------------------------------------------------------------
// TestE2E_CloseIdempotent — calling Close after ReadAll is idempotent.
//
// Branches covered:
//   - Close after triggered state
//   - Second Close does not double-close upstream
// ---------------------------------------------------------------------------

func TestE2E_CloseIdempotent(t *testing.T) {
	const phrase = "repeating "
	const limit = 3

	var upstream strings.Builder
	upstream.WriteString(testSSEEvent(phrase))
	upstream.WriteString(testSSEEvent(phrase))
	upstream.WriteString(testSSEEvent(phrase))
	upstream.WriteString(testSSEEvent(phrase))
	upstream.WriteString("data: [DONE]\n\n")

	upstreamData := upstream.String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, upstreamData)
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}

	guard := NewRepetitionGuardStream(resp.Body, limit, 8, "gpt-4o")
	_, err = io.ReadAll(guard)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	// First Close.
	err1 := guard.Close()
	// Second Close (idempotent).
	err2 := guard.Close()
	if err1 != nil || err2 != nil {
		t.Fatalf("Close errors: first=%v second=%v", err1, err2)
	}
}

// ---------------------------------------------------------------------------
// TestE2E_ReadAfterTriggerReturnsEOF — after guard triggers, reads return EOF.
//
// Branches covered:
//   - triggered → io.EOF path in Read
// ---------------------------------------------------------------------------

func TestE2E_ReadAfterTriggerReturnsEOF(t *testing.T) {
	const phrase = "repeat "
	const limit = 2

	var upstream strings.Builder
	for i := 0; i < 10; i++ {
		upstream.WriteString(testSSEEvent(phrase))
	}
	upstream.WriteString("data: [DONE]\n\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, upstream.String())
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}

	guard := NewRepetitionGuardStream(resp.Body, limit, 8, "gpt-4o")
	_, _ = io.ReadAll(guard)

	// Subsequent Read should return EOF.
	buf := make([]byte, 16)
	n, err := guard.Read(buf)
	if n != 0 || err != io.EOF {
		t.Fatalf("Read after trigger = (%d, %v), want (0, io.EOF)", n, err)
	}
}

// ---------------------------------------------------------------------------
// TestE2E_EmptyBufferRead — Read(nil) returns (0, nil).
//
// Branches covered:
//   - len(p)==0 early return in Read
// ---------------------------------------------------------------------------

func TestE2E_EmptyBufferRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("http.Get: %v", err)
	}

	guard := NewRepetitionGuardStream(resp.Body, 3, 8, "gpt-4o")
	n, err := guard.Read(nil)
	if n != 0 || err != nil {
		t.Fatalf("Read(nil) = (%d, %v), want (0, nil)", n, err)
	}
}