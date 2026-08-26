package thinkextract

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// TransformResponsesStream wraps an SSE stream of OpenAI Responses API events
// and rewrites any output_text delta carrying legacy think-block tags into a
// synthesized reasoning item plus reasoning_text deltas. Non-Responses events
// pass through unchanged. A data: [DONE] sentinel terminates the stream.
//
// The transformer tracks the current output item per response. On the first
// delta that opens a reasoning block it synthesizes a reasoning output item
// (response.output_item.added with type=reasoning, id=rs_*, status=in_progress),
// emits response.reasoning_text.delta events for the reasoning body, closes the
// item with response.output_item.done, and re-emits the remaining text delta as
// response.output_text.delta on the original item. Subsequent reasoning blocks
// in the same item reuse the same synthesized reasoning item ID so the client
// sees one contiguous reasoning span.
//
// A reasoning block that opens but never closes within the stream is
// forwarded verbatim — the tag stays in the output text so nothing is lost.
// When the upstream stream ends without a [DONE] sentinel the transformer
// flushes any still-open reasoning item as ordinary text so the client sees
// the bytes the model produced.
func TransformResponsesStream(in io.ReadCloser, opts Options) io.ReadCloser {
	o := opts.withDefaults()
	pr, pw := io.Pipe()
	go transformResponsesLoop(in, pw, o)
	return pr
}

// responsesStreamState tracks the current output item so the transformer
// can route reasoning deltas to a synthesized reasoning item and the
// remaining text to the original message item.
type responsesStreamState struct {
	opts    Options
	// state is the tag-scanner state for the currently open message item.
	// A new state is created whenever the output item changes (the Responses
	// protocol emits output_item.added before deltas).
	state   *State
	// itemID is the ID of the current output item from the upstream stream.
	itemID  string
	// itemIndex is the output_index of the current output item.
	itemIndex int
	// reasoningItemID is the ID assigned to the synthesized reasoning item.
	reasoningItemID string
	// reasoningOpen is true while a reasoning block is being synthesized
	// (between output_item.added and output_item.done).
	reasoningOpen bool
	// reasoningText accumulates the reasoning body so output_item.done can
	// carry the full text in a single reasoning_text part.
	reasoningText strings.Builder
}

// transformResponsesLoop reads SSE lines from in and rewrites any event
// carrying legacy think-block tags. It is the Responses API counterpart of
// transformLoop.
func transformResponsesLoop(in io.ReadCloser, pw *io.PipeWriter, o Options) {
	defer pw.Close()
	defer in.Close()

	st := &responsesStreamState{opts: o}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		trimmed := bytes.TrimLeft(line, " \t")
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			if !writeLine(pw, line) {
				return
			}
			continue
		}
		payload := bytes.TrimPrefix(trimmed, []byte("data:"))
		payload = bytes.TrimPrefix(payload, []byte(" "))

		if string(payload) == "[DONE]" {
			flushResponsesState(pw, st)
			if !writeLine(pw, line) {
				return
			}
			continue
		}

		events, err := rewriteResponsesEvent(payload, st)
		if err != nil {
			if !writeLine(pw, line) {
				return
			}
			continue
		}
		for _, ev := range events {
			if !writeEvent(pw, ev) {
				return
			}
		}
	}
	flushResponsesState(pw, st)
}

// responsesEvent is the parsed shape of a Responses API SSE event. Only the
// fields the transformer reads are typed; everything else is preserved as
// raw JSON so re-emission is byte-exact for unmodified events.
type responsesEvent struct {
	Type          string             `json:"type"`
	ItemID        string             `json:"item_id,omitempty"`
	OutputIndex   int                `json:"output_index,omitempty"`
	ContentIndex  int                `json:"content_index,omitempty"`
	Delta         string             `json:"delta,omitempty"`
	Item          *responsesItemInfo `json:"item,omitempty"`
	RawExtra      json.RawMessage    `json:"-"` // rest of the event, untouched
}

// responsesItemInfo is the subset of the `item` payload on
// response.output_item.added / done that the transformer reads.
type responsesItemInfo struct {
	ID     string          `json:"id,omitempty"`
	Type   string          `json:"type,omitempty"`
	Status string          `json:"status,omitempty"`
	Role   string          `json:"role,omitempty"`
	Content []responsesContentPart `json:"content,omitempty"`
}

// responsesContentPart is one element of an item's content array.
type responsesContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// rewriteResponsesEvent decodes one Responses API SSE event payload and
// returns the replacement events to emit. A single event can yield 0..N
// output events (synthesized reasoning item events plus the original event
// with cleaned text, or just the original event unchanged).
func rewriteResponsesEvent(payload []byte, st *responsesStreamState) ([][]byte, error) {
	var ev responsesEvent
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, err
	}

	switch ev.Type {
	case "response.output_item.added":
		// Track the current item so deltas know which state to use.
		if ev.Item != nil {
			st.itemID = ev.Item.ID
			st.itemIndex = ev.OutputIndex
			st.state = NewState(st.opts)
		}
		return [][]byte{payload}, nil

	case "response.output_item.done":
		// The upstream item is closing. If we synthesized a reasoning item
		// for it, emit the reasoning item's done event first, then forward
		// the upstream done.
		if st.reasoningOpen {
			done := buildReasoningDoneEvent(st)
			st.reasoningOpen = false
			st.reasoningItemID = ""
			st.reasoningText.Reset()
			return [][]byte{done, payload}, nil
		}
		return [][]byte{payload}, nil

	case "response.output_text.delta":
		return rewriteTextDelta(ev, payload, st), nil

	default:
		return [][]byte{payload}, nil
	}
}

// rewriteTextDelta handles one response.output_text.delta event. It feeds the
// delta into the state scanner and emits reasoning item events plus the
// cleaned text delta as needed.
func rewriteTextDelta(ev responsesEvent, original []byte, st *responsesStreamState) [][]byte {
	if st.state == nil {
		st.state = NewState(st.opts)
	}
	cd, rd := st.state.Feed(ev.Delta)
	if cd == "" && rd == "" {
		// Fully buffered mid-tag; emit nothing until more text arrives.
		return nil
	}

	var out [][]byte

	if rd != "" {
		if !st.reasoningOpen {
			st.reasoningItemID = "rs_" + shortID()
			st.reasoningOpen = true
			st.reasoningText.Reset()
			out = append(out, buildReasoningAddedEvent(st))
		}
		st.reasoningText.WriteString(rd)
		out = append(out, buildReasoningTextDeltaEvent(st, rd))
	}

	if cd != "" {
		// Cleaned text delta on the original item.
		var cleanedEv responsesEvent
		if err := json.Unmarshal(original, &cleanedEv); err == nil {
			cleanedEv.Delta = cd
			if encoded, err := json.Marshal(&cleanedEv); err == nil {
				out = append(out, encoded)
			}
		}
	}

	return out
}

// flushResponsesState drains the tag state at stream end. Any unclosed block
// becomes ordinary text on the current item so nothing is dropped.
func flushResponsesState(pw *io.PipeWriter, st *responsesStreamState) {
	if st.state == nil {
		return
	}
	cd, rd := st.state.Flush()
	if rd != "" {
		if !st.reasoningOpen {
			st.reasoningItemID = "rs_" + shortID()
			st.reasoningOpen = true
			if !writeEvent(pw, buildReasoningAddedEvent(st)) {
				return
			}
		}
		if !writeEvent(pw, buildReasoningTextDeltaEvent(st, rd)) {
			return
		}
	}
	if st.reasoningOpen {
		if !writeEvent(pw, buildReasoningDoneEvent(st)) {
			return
		}
		st.reasoningOpen = false
		st.reasoningItemID = ""
		st.reasoningText.Reset()
	}
	if cd != "" {
		// Residual text after any open tag closed. Emit as ordinary text on
		// the current item so the client sees the bytes.
		ev := map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       st.itemID,
			"output_index":  st.itemIndex,
			"content_index": 0,
			"delta":         cd,
		}
		if payload, err := json.Marshal(ev); err == nil {
			_ = writeEvent(pw, payload)
		}
	}
}

// buildReasoningAddedEvent synthesizes response.output_item.added for a
// reasoning item. The summary array is empty by design — the reasoning text
// is the raw model output, not a summary.
func buildReasoningAddedEvent(st *responsesStreamState) []byte {
	ev := map[string]any{
		"type":         "response.output_item.added",
		"output_index": st.itemIndex,
		"item": map[string]any{
			"id":      st.reasoningItemID,
			"type":    "reasoning",
			"status":  "in_progress",
			"summary": []any{},
			"content": []any{},
		},
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return []byte(`{"type":"response.output_item.added"}`)
	}
	return b
}

// buildReasoningTextDeltaEvent synthesizes response.reasoning_text.delta for
// one chunk of reasoning text.
func buildReasoningTextDeltaEvent(st *responsesStreamState, delta string) []byte {
	ev := map[string]any{
		"type":          "response.reasoning_text.delta",
		"item_id":       st.reasoningItemID,
		"output_index":  st.itemIndex,
		"content_index": 0,
		"delta":         delta,
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return []byte(`{"type":"response.reasoning_text.delta","delta":""}`)
	}
	return b
}

// buildReasoningDoneEvent synthesizes response.output_item.done for the
// synthesized reasoning item, carrying the full reasoning text in a single
// reasoning_text part.
func buildReasoningDoneEvent(st *responsesStreamState) []byte {
	ev := map[string]any{
		"type":         "response.output_item.done",
		"output_index": st.itemIndex,
		"item": map[string]any{
			"id":      st.reasoningItemID,
			"type":    "reasoning",
			"status":  "completed",
			"summary": []any{},
			"content": []any{
				map[string]any{"type": "reasoning_text", "text": st.reasoningText.String()},
			},
		},
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return []byte(`{"type":"response.output_item.done"}`)
	}
	return b
}
