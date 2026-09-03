package streaming

// Residual coverage tests for repetition_guard_stream.go and tokenizer.go.
// Targets the small set of branches the main coverage suite did not hit:
// non-EOF source errors, runaway unbounded events, single-line non-data
// events, observe's empty-triggered early returns, trigger double-call,
// the byte-fallback keep > fallbackMinRunBytes branch when limit > 3, and
// the omnitoken error paths (resolved encoding with a missing factory).

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goccy/go-json"

	"github.com/ron2111/omnitoken"
)

// readStep controls a readerFunc's next return — pairs of (n, err, payload).
type readStep struct {
	n       int
	err     error
	payload []byte
}

// steppedReader yields one readStep per call; once the steps are exhausted it
// returns io.EOF so a test that triggers cancellation deterministically exits.
type steppedReader struct {
	steps []readStep
	idx   atomic.Int32
}

func (r *steppedReader) Read(p []byte) (int, error) {
	i := int(r.idx.Add(1)) - 1
	if i >= len(r.steps) {
		return 0, io.EOF
	}
	step := r.steps[i]
	if step.n > 0 {
		if step.payload != nil {
			copy(p, step.payload[:step.n])
		} else {
			copy(p, bytes.Repeat([]byte{byte('a')}, step.n))
		}
	}
	return step.n, step.err
}

func (r *steppedReader) Close() error { return nil }

// chatEventRaw mirrors chatEvent but returns a string for tests that need it.
func chatEventRaw(content string) string {
	encoded, _ := json.Marshal(content)
	return "data: {\"choices\":[{\"delta\":{\"content\":" + string(encoded) + "}}]}\n\n"
}

// TestRepetitionGuardStream_NonEOFSourceError — Read propagates a non-EOF
// upstream error; the guard keeps observing bytes it already got, so the
// error surfaces on the read after the last good chunk.
func TestRepetitionGuardStream_NonEOFSourceError(t *testing.T) {
	event := []byte(chatEventRaw("hi"))
	src := &steppedReader{steps: []readStep{
		{n: len(event), payload: event},
		{n: 0, err: errors.New("boom")},
	}}
	stream := NewRepetitionGuardStream(src, 3, 8, "gpt-4o")

	buf := make([]byte, 64)
	if n, err := stream.Read(buf); err != nil || n == 0 {
		t.Fatalf("first read should return the event, got n=%d err=%v", n, err)
	}
	n, err := stream.Read(buf)
	if n != 0 {
		t.Fatalf("want n=0 on error, got %d", n)
	}
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("want non-EOF error, got %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestRepetitionGuardStream_SourceReturnsZeroNoError — covers the (n==0, err==nil)
// indefinite-zero branch in Read.
func TestRepetitionGuardStream_SourceReturnsZeroNoError(t *testing.T) {
	src := &steppedReader{steps: []readStep{
		{n: 0, err: nil},
	}}
	stream := NewRepetitionGuardStream(src, 3, 8, "gpt-4o")

	buf := make([]byte, 16)
	if _, err := stream.Read(buf); err != nil {
		t.Fatalf("want nil error on (0,nil), got %v", err)
	}
}

// TestRepetitionGuardStream_ObserveRunawayEvent — a chunk larger than
// maxPendingEventBytes with no \n\n boundary must be forwarded unobserved
// rather than buffered forever.
func TestRepetitionGuardStream_ObserveRunawayEvent(t *testing.T) {
	guard := newGuardWithCounter(&recordingReadCloser{Reader: strings.NewReader("")}, 3, 8, nil).(*RepetitionGuardStream)
	// Force any size: > maxPendingEventBytes (256 KiB) of plain text, no \n.
	oversize := bytes.Repeat([]byte("a"), maxPendingEventBytes+1024)
	guard.observe(oversize)
	if guard.out.Len() == 0 {
		t.Fatalf("runaway chunk should be forwarded into out, but out is empty")
	}
	if len(guard.pending) != 0 {
		t.Fatalf("pending should be reset after runaway forward, got %d bytes", len(guard.pending))
	}
}

// TestRepetitionGuardStream_ObserveEmptyAndPostTrigger — the guard.observe
// guard clause returns early for nil input or when already triggered.
func TestRepetitionGuardStream_ObserveEmptyAndPostTrigger(t *testing.T) {
	guard := newGuardWithCounter(&recordingReadCloser{Reader: strings.NewReader("")}, 3, 8, nil).(*RepetitionGuardStream)
	guard.observe(nil)
	guard.observe([]byte(""))
	if guard.out.Len() != 0 {
		t.Fatalf("empty observe must be a no-op, out has %d bytes", guard.out.Len())
	}

	// Mark triggered and observe more data — must early-return.
	guard.triggered = true
	guard.observe([]byte("anything"))
	if guard.out.Len() != 0 {
		t.Fatalf("post-trigger observe must early-return, out has %d bytes", guard.out.Len())
	}
}

// TestRepetitionGuardStream_TriggerDoubleCall — trigger's idempotent guard.
func TestRepetitionGuardStream_TriggerDoubleCall(t *testing.T) {
	var calls atomic.Int32
	src := newSource(chatEventRaw("a") + chatEventRaw("a"))
	stream := NewRepetitionGuardStream(src, 2, 8, "gpt-4o",
		WithTriggerCallback(func() { calls.Add(1) }),
	)

	guard := stream.(*RepetitionGuardStream)
	guard.trigger(0)
	// second invocation: callback must not run a second time, output must not
	// append a second [DONE].
	prevOut := guard.out.Len()
	guard.trigger(0)
	if guard.out.Len() != prevOut {
		t.Fatalf("second trigger appended extra bytes: out grew %d -> %d", prevOut, guard.out.Len())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("callback fired %d times, want exactly 1", got)
	}
}

// TestRepetitionGuardStream_EventPayloadSingleLineNonData — parseDataLine fails
// on non-data lines; eventPayload returns nil.
func TestRepetitionGuardStream_EventPayloadSingleLineNonData(t *testing.T) {
	got := eventPayload([]byte("event: ping"))
	if got != nil {
		t.Fatalf("eventPayload(non-data single line) = %q, want nil", got)
	}
	got = eventPayload([]byte(""))
	if got != nil {
		t.Fatalf("eventPayload(empty) = %q, want nil", got)
	}
}

// TestDetectByteRun_HighLimitUsesMaxNeed — when limit * fallbackMaxUnitBytes
// exceeds fallbackMinRunBytes, detectByteRun uses the larger keep value. Drive
// the real guard (not the detectRun helper) with limit=4 so 32*4=128 > 96.
func TestDetectByteRun_HighLimitUsesMaxNeed(t *testing.T) {
	limit := 4
	payload := periodicRunPayload(4) // 128 bytes, period 32
	input := chatEventRaw("start") + chatEventRaw(payload) + chatEventRaw(payload) + chatEventRaw(payload)
	want := chatEventRaw("start") + chatEventRaw(payload) + doneEvent()

	src := newSource(input)
	stream := newGuardWithCounter(src, limit, 8, nil) // nil counter -> byte fallback
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(out) != want {
		t.Fatalf("limit=4 byte fallback mismatch\nwant: %q\ngot:  %q", want, string(out))
	}
	if src.closeCount != 1 {
		t.Fatalf("expected source closed once, got %d", src.closeCount)
	}
}

// TestNewTokenCounter_ForEncodingError — register an encoding whose factory
// fails, then a model pointing at it. ResolveModel succeeds, ForEncoding
// fails. Covers tokenizer.go:35-36 and repetition_guard_stream.go:258.4-261.1.
func TestNewTokenCounter_ForEncodingError(t *testing.T) {
	const fakeEncoding = "kimi-test-failing-encoding"
	const fakeModel = "kimi-test-fake-encoding-model"

	if err := omnitoken.RegisterEncoding(fakeEncoding, func() (omnitoken.ModelEngine, error) {
		return nil, errors.New("engine build failed")
	}); err != nil {
		t.Fatalf("RegisterEncoding: %v", err)
	}
	if err := omnitoken.RegisterModel(fakeModel, fakeEncoding); err != nil {
		t.Fatalf("RegisterModel: %v", err)
	}
	counter, err := NewTokenCounter(fakeModel)
	if err == nil {
		t.Fatalf("NewTokenCounter(%q): expected ForEncoding error, got nil", fakeModel)
	}
	if counter != nil {
		t.Fatalf("NewTokenCounter(%q) returned non-nil counter alongside error", fakeModel)
	}

	// The guard must still function: lazy resolution fails, fallback log fires,
	// byte-period detector takes over. limit=3 with a 128-byte period-32 run
	// triggers on the first payload event.
	payload := periodicRunPayload(4)
	input := chatEventRaw("start") + chatEventRaw(payload) + chatEventRaw(payload) + chatEventRaw(payload)
	want := chatEventRaw("start") + chatEventRaw(payload) + doneEvent()
	src := newSource(input)
	stream := NewRepetitionGuardStream(src, 3, 8, fakeModel)
	out, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(out) != want {
		t.Fatalf("guard with failed tokenizer fell back wrong\nwant: %q\ngot:  %q", want, string(out))
	}
}
