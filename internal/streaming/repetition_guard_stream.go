package streaming

import (
	"errors"
	"io"
	"math"
	"sync"
)

// RepetitionGuardStream watches an upstream SSE stream for a text unit (a
// single token or a short token chain) that starts repeating consecutively.
// When the unit repeats limit times in a row, the guard closes the upstream
// source and ends the stream with the dialect's native termination: chat
// completions get a synthetic finish_reason "stop" chunk plus data: [DONE],
// Anthropic messages get message_delta(stop_reason "end_turn") plus
// message_stop, and the Responses API gets a response.completed event. In
// every case the client sees a normal turn end, not a broken connection.
//
// The guard is purely observational: bytes pass through to the caller as they
// arrive and are never rewritten or held back. Detection runs eagerly on the
// decoded delta text of each complete SSE event, so an accepted leak of up to
// limit x maxPattern tokens may already have been emitted when the guard fires.
//
// A TokenCounter is resolved lazily for the model on the first content delta;
// unknown models fall back to a byte-period detector. Skip heuristics keep
// fenced code, base64/hex blobs, markdown tables, long whitespace runs, and
// tool_calls/function_call deltas from ever tripping the guard. With limit <= 0
// the source is returned unchanged, so a disabled guard has zero overhead.
//
// Usage accounting limitation: the guard closes the upstream before the
// provider's final usage chunk arrives, so a triggered stream emits no usage
// entry even though the provider still bills the emitted tokens. The cut
// consumes up to limit consecutive repeats of the unit beyond what usage
// logs show. Synthesizing a usage entry from the emitted deltas would
// require a callback into the usage observer that the guard does not
// currently have; until then the accounting gap is documented, not patched.
type RepetitionGuardStream struct {
	source          io.ReadCloser
	limit           int
	maxPattern      int
	model           string
	counter         TokenCounter
	counterResolved bool

	pending    []byte
	out        StreamBuffer
	scratch    []byte
	sourceDone bool
	readErr    error
	closed     bool
	triggered  bool
	closeOnce  sync.Once
	closeErr   error
	onTrigger  func()
	zeroReads  int

	mu sync.Mutex

	// sawJSONEvent tracks that at least one JSON event has been decoded from
	// the stream; the trigger() chat-completions branch emits the synthetic
	// terminal chunk only when it saw one, so streams that never spoke JSON
	// (plain test payloads) end with just the done marker.
	sawJSONEvent bool
	envID        string
	envObject    string
	envModel     string
	envCreated   float64

	// dialect pins the stream shape (chat completions, Anthropic messages,
	// or the Responses API) from the first recognizable event; the
	// termination the guard synthesizes on trigger matches that dialect.
	dialect streamDialect

	choices map[int]*choiceState
}

// streamDialect identifies which SSE wire shape a stream speaks.
type streamDialect int

const (
	dialectUnknown streamDialect = iota
	dialectChatCompletions
	dialectAnthropicMessages
	dialectResponses
)

// GuardOption customizes the guard at construction time.
type GuardOption func(*RepetitionGuardStream)

// WithTriggerCallback registers fn to run exactly once when the guard
// triggers. Callers use it to increment metrics without coupling the
// streaming package to the metrics registry.
func WithTriggerCallback(fn func()) GuardOption {
	return func(g *RepetitionGuardStream) { g.onTrigger = fn }
}

type choiceState struct {
	// fenced tracks ``` parity per choice: true while inside a fenced block.
	fenced bool
	// tokenTail is the rolling tail of the last limit*maxPattern token IDs.
	tokenTail []int
	// byteTail is the rolling tail of recently observed content bytes.
	byteTail []byte
}

const (
	minLimit          = 2
	defaultMaxPattern = 8
	maxMaxPattern     = 64

	// Byte fallback bounds: the detector only trusts runs of at least
	// fallbackMinRunBytes with a period of at most fallbackMaxUnitBytes.
	// 64 matches the fallback reference recorded in the wayfinder map (#62).
	fallbackMaxUnitBytes = 64
	fallbackMinRunBytes  = 96

	// Encoded-blob heuristic window (base64/hex density + entropy).
	encodedWindowBytes = 64

	// maxConsecutiveZeroReads caps how many (0, nil) source reads the
	// guard tolerates before surfacing io.ErrNoProgress, matching the
	// stdlib io.Copy busy-spin cutoff.
	maxConsecutiveZeroReads = 100
)

// NewRepetitionGuardStream returns source unchanged when limit is zero or
// negative, keeping the default behavior identical to not having the guard.
//
// maxPattern bounds the inspected repetition period in tokens (<= 0 means the
// default of 8, clamped to 1..64); limit is the repeat count that trips the
// guard (clamped to at least 2). model is resolved lazily into a TokenCounter
// on the first content delta; an unknown model falls back to the byte-period
// detector.
func NewRepetitionGuardStream(source io.ReadCloser, limit, maxPattern int, model string, opts ...GuardOption) io.ReadCloser {
	if source == nil || limit <= 0 {
		return source
	}
	limit, maxPattern = clampGuardParams(limit, maxPattern)
	g := &RepetitionGuardStream{
		source:     source,
		limit:      limit,
		maxPattern: maxPattern,
		model:      model,
		choices:    make(map[int]*choiceState),
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}

func clampGuardParams(limit, maxPattern int) (int, int) {
	if limit < minLimit {
		limit = minLimit
	}
	// Cap limit so limit*maxPattern (detectTokenRun) and
	// fallbackMaxUnitBytes*limit (detectByteRun) never overflow int.
	if limit > math.MaxInt/maxMaxPattern {
		limit = math.MaxInt / maxMaxPattern
	}
	if maxPattern <= 0 {
		maxPattern = defaultMaxPattern
	}
	if maxPattern > maxMaxPattern {
		maxPattern = maxMaxPattern
	}
	return limit, maxPattern
}

func (s *RepetitionGuardStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		s.mu.Lock()
		if s.out.Len() > 0 {
			n := s.out.Read(p)
			s.mu.Unlock()
			return n, nil
		}
		if s.triggered || s.sourceDone || s.closed {
			s.mu.Unlock()
			return 0, io.EOF
		}
		if s.scratch == nil {
			s.scratch = make([]byte, 32*1024)
		}
		scratch := s.scratch
		s.mu.Unlock()

		// The source read blocks, so it runs outside the mutex; Close may
		// be waiting on the mutex while this goroutine is blocked here.
		n, err := s.source.Read(scratch)

		s.mu.Lock()
		if n > 0 {
			s.zeroReads = 0
			s.observe(scratch[:n])
			s.mu.Unlock()
			continue
		}
		if err != nil {
			// n == 0 here: the n > 0 branch above never reaches this one.
			if errors.Is(err, io.EOF) {
				if len(s.pending) > 0 {
					// The source ended mid-event (no blank-line separator
					// after the final payload): forward the buffered bytes
					// before reporting EOF so the client never loses the
					// last delta.
					s.out.AppendBytes(s.pending)
					s.pending = s.pending[:0]
					s.mu.Unlock()
					continue
				}
				s.sourceDone = true
				s.mu.Unlock()
				return 0, io.EOF
			}
			s.readErr = err
			s.sourceDone = true
			s.mu.Unlock()
			return 0, err
		}
		// (0, nil): the source made no progress and reported no error.
		// Surface io.ErrNoProgress after repeated empty reads so io.Copy
		// callers cannot busy-spin on a stalled source.
		s.zeroReads++
		if s.zeroReads >= maxConsecutiveZeroReads {
			s.mu.Unlock()
			return 0, io.ErrNoProgress
		}
		s.mu.Unlock()
		return 0, nil
	}
}

// Close idempotently closes the upstream source. It is safe to call after a
// repetition trigger (the source is already closed) and returns the recorded
// close error on repeat calls. Close is safe to call concurrently with Read:
// the upstream close happens outside the mutex (so a Read blocked in the
// source is unblocked) and the state teardown runs under it.
func (s *RepetitionGuardStream) Close() error {
	_ = s.closeSource()
	s.mu.Lock()
	s.closed = true
	s.out.Release()
	s.pending = nil
	s.mu.Unlock()
	return s.closeErr
}

func (s *RepetitionGuardStream) closeSource() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.source.Close()
	})
	return s.closeErr
}
