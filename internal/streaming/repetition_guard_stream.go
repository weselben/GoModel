package streaming

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"sync"

	"github.com/goccy/go-json"
)

// RepetitionGuardStream detects an upstream SSE stream that has started
// repeating the same text unit (a single token or a chain of tokens). When the
// unit repeats StreamRepetitionLimit times consecutively, the guard closes the
// upstream source, strips the repeated tail, and ends the stream cleanly with a
// [DONE] marker. With a limit <= 0 the source is returned unchanged so there is
// zero overhead when the feature is disabled.
//
// The prototype operates on the OpenAI chat-completion streaming shape
// (choices[i].delta.content). Other SSE payloads are forwarded untouched.
type RepetitionGuardStream struct {
	io.ReadCloser

	limit      int
	maxUnit    int
	holdback   int
	scratch    []byte
	pending    []byte
	out        bytes.Buffer
	sourceDone bool
	closed     bool
	triggered  bool
	doneOnce   sync.Once

	choices map[int]*choiceState
	queue   []queuedEvent
}

type choiceState struct {
	text []byte
}

type queuedEvent struct {
	raw     []byte
	payload []byte // nil for opaque events
	deltas  []textDelta
}

type textDelta struct {
	choiceIndex int
	start       int // byte offset in choiceState.text before this event
	end         int // byte offset in choiceState.text after this event
}

const (
	defaultMaxUnitBytes = 256
	minRepetitionLimit  = 2
)

// NewRepetitionGuardStream returns source unchanged when limit is zero or
// negative, keeping the default behavior identical to not having the guard.
func NewRepetitionGuardStream(source io.ReadCloser, limit int) io.ReadCloser {
	if source == nil || limit <= 0 {
		return source
	}
	if limit < minRepetitionLimit {
		limit = minRepetitionLimit
	}
	maxUnit := defaultMaxUnitBytes
	return &RepetitionGuardStream{
		ReadCloser: source,
		limit:      limit,
		maxUnit:    maxUnit,
		holdback:   limit * maxUnit,
		choices:    make(map[int]*choiceState),
	}
}

func (s *RepetitionGuardStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	for {
		if s.out.Len() > 0 {
			return s.out.Read(p)
		}
		if s.triggered {
			return 0, io.EOF
		}

		// If the upstream has finished, flush whatever is still held and
		// terminate naturally without synthesizing a [DONE].
		if s.sourceDone {
			s.flushAll()
			if s.out.Len() == 0 {
				if s.closed {
					return 0, io.EOF
				}
				return 0, io.EOF
			}
			continue
		}

		tmp := s.scratch[:cap(s.scratch)]
		if len(tmp) == 0 {
			tmp = make([]byte, 32*1024)
			s.scratch = tmp
		}
		n, err := s.ReadCloser.Read(tmp)
		if n > 0 {
			s.feed(tmp[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.sourceDone = true
				continue
			}
			return 0, err
		}
	}
}

func (s *RepetitionGuardStream) Close() error {
	var closeErr error
	s.doneOnce.Do(func() {
		s.closed = true
		s.triggered = true
		s.sourceDone = true
		closeErr = s.ReadCloser.Close()
	})
	return closeErr
}

func (s *RepetitionGuardStream) feed(data []byte) {
	if s.triggered {
		return
	}
	s.pending = append(s.pending, data...)
	s.processPending()
}

func (s *RepetitionGuardStream) processPending() {
	for len(s.pending) > 0 {
		idx, sepLen := nextEventBoundary(s.pending)
		if idx == -1 {
			// Wait for a complete event before looking at it.
			return
		}

		event := s.pending[:idx]
		s.pending = s.pending[idx+sepLen:]

		if len(event) == 0 {
			continue
		}
		s.processEvent(event)
		s.releaseSafeEvents()
	}
}

func (s *RepetitionGuardStream) processEvent(event []byte) {
	// Fast path: single-line events dominate chat SSE streams.
	if bytes.IndexByte(event, '\n') == -1 {
		jsonData, ok := parseDataLine(event)
		if !ok {
			// Non-data line; pass through as opaque.
			s.enqueueOpaque(event)
			return
		}
		s.processPayload(event, jsonData)
		return
	}

	lines := bytes.Split(event, []byte("\n"))
	var payloadLines [][]byte
	for _, line := range lines {
		jsonData, ok := parseDataLine(line)
		if !ok {
			continue
		}
		payloadLines = append(payloadLines, jsonData)
	}
	if len(payloadLines) == 0 {
		s.enqueueOpaque(event)
		return
	}
	payload := bytes.Join(payloadLines, []byte("\n"))
	s.processPayload(event, payload)
}

func (s *RepetitionGuardStream) processPayload(raw, payload []byte) {
	if bytes.Equal(payload, donePayload) {
		s.queue = append(s.queue, queuedEvent{raw: raw, payload: payload})
		return
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		// Not JSON we can inspect; preserve bytes unchanged.
		s.enqueueOpaque(raw)
		return
	}

	deltas := contentDeltas(decoded)
	if len(deltas) == 0 {
		// JSON event without inspectable content; preserve unchanged.
		s.queue = append(s.queue, queuedEvent{raw: raw, payload: payload})
		return
	}

	var tds []textDelta
	for _, d := range deltas {
		st := s.choiceState(d.choiceIndex)
		start := len(st.text)
		st.text = append(st.text, d.content...)
		tds = append(tds, textDelta{
			choiceIndex: d.choiceIndex,
			start:       start,
			end:         len(st.text),
		})
	}

	ev := queuedEvent{raw: raw, payload: payload, deltas: tds}
	s.queue = append(s.queue, ev)
	s.checkForRepetition()
}

func (s *RepetitionGuardStream) choiceState(index int) *choiceState {
	st, ok := s.choices[index]
	if !ok {
		st = &choiceState{}
		s.choices[index] = st
	}
	return st
}

func (s *RepetitionGuardStream) checkForRepetition() {
	if s.triggered {
		return
	}
	for choiceIndex, st := range s.choices {
		unitLen, runStart, ok := detectRun(st.text, s.limit, s.maxUnit)
		if !ok {
			continue
		}
		keepEnd := runStart + unitLen
		s.trigger(choiceIndex, keepEnd)
		return
	}
}

func (s *RepetitionGuardStream) trigger(choiceIndex, keepEnd int) {
	if s.triggered {
		return
	}
	s.triggered = true

	_ = s.ReadCloser.Close()

	// Emit everything safely before the cut, rewrite the straddling event,
	// then append a synthesized [DONE].
	cutEventIdx := -1
	for i, ev := range s.queue {
		var evEnd int
		for _, d := range ev.deltas {
			if d.choiceIndex == choiceIndex {
				if d.end > evEnd {
					evEnd = d.end
				}
			}
		}
		if evEnd > keepEnd {
			cutEventIdx = i
			break
		}
		s.out.Write(ev.raw)
		s.out.Write(lfEventBoundary)
	}

	if cutEventIdx >= 0 {
		ev := s.queue[cutEventIdx]
		rewritten := rewriteEventAtCut(ev, choiceIndex, keepEnd, s.choices[choiceIndex].text)
		s.out.Write(rewritten)
		s.out.Write(lfEventBoundary)
	}

	s.out.Write([]byte("data: "))
	s.out.Write(donePayload)
	s.out.Write(lfEventBoundary)

	slog.Warn("stream repetition guard triggered",
		"choice", choiceIndex,
		"kept_bytes", keepEnd,
	)

	// Drop remaining queued events; the upstream is already closed.
	s.queue = s.queue[:0]
	s.pending = s.pending[:0]
}

func (s *RepetitionGuardStream) releaseSafeEvents() {
	if s.triggered {
		return
	}
	safePos := s.safePosition()
	for len(s.queue) > 0 {
		ev := s.queue[0]
		if !eventIsSafe(ev, safePos) {
			break
		}
		s.out.Write(ev.raw)
		s.out.Write(lfEventBoundary)
		s.queue = s.queue[1:]
	}
}

func (s *RepetitionGuardStream) safePosition() int {
	minPos := -1
	for _, st := range s.choices {
		pos := len(st.text) - s.holdback
		if pos < 0 {
			pos = 0
		}
		if minPos == -1 || pos < minPos {
			minPos = pos
		}
	}
	if minPos < 0 {
		return 0
	}
	return minPos
}

func eventIsSafe(ev queuedEvent, safePos int) bool {
	if len(ev.deltas) == 0 {
		return true
	}
	for _, d := range ev.deltas {
		if d.end > safePos {
			return false
		}
	}
	return true
}

func (s *RepetitionGuardStream) enqueueOpaque(raw []byte) {
	// An event we cannot inspect breaks ordering guarantees, so flush
	// everything currently held before forwarding it.
	s.flushQueue()
	s.out.Write(raw)
	s.out.Write(lfEventBoundary)
}

func (s *RepetitionGuardStream) flushQueue() {
	for _, ev := range s.queue {
		s.out.Write(ev.raw)
		s.out.Write(lfEventBoundary)
	}
	s.queue = s.queue[:0]
}

func (s *RepetitionGuardStream) flushAll() {
	if s.triggered {
		return
	}
	s.flushQueue()
	if len(s.pending) > 0 {
		s.out.Write(s.pending)
	}
	s.pending = s.pending[:0]
}

func detectRun(text []byte, limit, maxUnit int) (unitLen, runStart int, ok bool) {
	if len(text) < limit {
		return 0, 0, false
	}
	maxP := len(text) / limit
	if maxP > maxUnit {
		maxP = maxUnit
	}
	for p := 1; p <= maxP; p++ {
		need := p * limit
		unit := text[len(text)-p:]
		if isRepeatedSuffix(text, unit, need) {
			start := len(text) - need
			for start >= p && bytes.Equal(text[start-p:start], unit) {
				start -= p
			}
			return p, start, true
		}
	}
	return 0, 0, false
}

func isRepeatedSuffix(text, unit []byte, need int) bool {
	if len(text) < need {
		return false
	}
	suffix := text[len(text)-need:]
	p := len(unit)
	for i := 0; i < need; i++ {
		if suffix[i] != unit[i%p] {
			return false
		}
	}
	return true
}

func contentDeltas(payload map[string]any) []struct {
	choiceIndex int
	content     []byte
} {
	choicesRaw, ok := payload["choices"].([]any)
	if !ok {
		return nil
	}
	var out []struct {
		choiceIndex int
		content     []byte
	}
	for i, c := range choicesRaw {
		choiceMap, ok := c.(map[string]any)
		if !ok {
			continue
		}
		delta, ok := choiceMap["delta"].(map[string]any)
		if !ok {
			continue
		}
		content, ok := delta["content"].(string)
		if !ok || content == "" {
			continue
		}
		out = append(out, struct {
			choiceIndex int
			content     []byte
		}{choiceIndex: i, content: []byte(content)})
	}
	return out
}

func rewriteEventAtCut(ev queuedEvent, choiceIndex, keepEnd int, text []byte) []byte {
	if len(ev.deltas) == 0 {
		return ev.raw
	}

	var decoded map[string]any
	if err := json.Unmarshal(ev.payload, &decoded); err != nil {
		return ev.raw
	}
	choicesRaw, ok := decoded["choices"].([]any)
	if !ok {
		return ev.raw
	}

	for _, d := range ev.deltas {
		if d.choiceIndex != choiceIndex {
			continue
		}
		if d.end <= keepEnd {
			// Event ends before the cut; nothing to rewrite.
			continue
		}
		start := d.start
		if start > keepEnd {
			start = keepEnd
		}
		newContent := string(text[start:keepEnd])
		if d.choiceIndex < len(choicesRaw) {
			choiceMap, ok := choicesRaw[d.choiceIndex].(map[string]any)
			if ok {
				delta, ok := choiceMap["delta"].(map[string]any)
				if ok {
					delta["content"] = newContent
				}
			}
		}
	}

	rewritten, err := json.Marshal(decoded)
	if err != nil {
		return ev.raw
	}
	return append([]byte("data: "), rewritten...)
}
