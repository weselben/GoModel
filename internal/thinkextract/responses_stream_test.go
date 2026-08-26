package thinkextract

import (
	"io"
	"strings"
	"testing"
)

const responsesStreamBasic = `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message","status":"in_progress"}}

data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"answer<think>x</think> rest"}

data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","status":"completed","content":[{"type":"output_text","text":"answer<think>x</think> rest"}]}}

data: [DONE]
`

func TestTransformResponsesStream_Basic(t *testing.T) {
	rc := TransformResponsesStream(io.NopCloser(strings.NewReader(responsesStreamBasic)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	// Reasoning item added, text delta with cleaned text, reasoning delta,
	// reasoning done, original done, [DONE].
	if !strings.Contains(out, `"type":"reasoning"`) {
		t.Errorf("reasoning item not synthesized: %q", out)
	}
	if !strings.Contains(out, `"reasoning_text"`) {
		t.Errorf("reasoning_text not emitted: %q", out)
	}
	if !strings.Contains(out, `"delta":"answer rest"`) && !strings.Contains(out, `"delta":"answer rest"`) {
		t.Errorf("cleaned text delta missing: %q", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("missing [DONE]: %q", out)
	}
}

func TestTransformResponsesStream_NoTags(t *testing.T) {
	input := `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message"}}

data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"plain text"}

data: [DONE]
`
	rc := TransformResponsesStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, "plain text") {
		t.Errorf("plain text lost: %q", out)
	}
	if strings.Contains(out, `"type":"reasoning"`) {
		t.Errorf("reasoning item synthesized for plain text: %q", out)
	}
}

func TestTransformResponsesStream_NonDeltaEventsPassThrough(t *testing.T) {
	input := `data: {"type":"response.created","response":{"id":"resp_1"}}

data: {"type":"response.in_progress","response":{"id":"resp_1"}}

data: [DONE]
`
	rc := TransformResponsesStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, "response.created") {
		t.Errorf("created event lost: %q", out)
	}
	if !strings.Contains(out, "response.in_progress") {
		t.Errorf("in_progress event lost: %q", out)
	}
}

func TestTransformResponsesStream_OnlyReasoning(t *testing.T) {
	input := `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message"}}

data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"<think>all</think>"}

data: {"type":"response.output_item.done","output_index":0,"item":{"id":"msg_1","type":"message","status":"completed","content":[{"type":"output_text","text":"<think>all</think>"}]}}

data: [DONE]
`
	rc := TransformResponsesStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, `"delta":"all"`) && !strings.Contains(out, `"text":"all"`) {
		t.Errorf("reasoning body missing: %q", out)
	}
	// Empty content delta is dropped; the original done event still flows.
	if !strings.Contains(out, `"type":"response.output_item.done"`) {
		t.Errorf("done event lost: %q", out)
	}
}

func TestTransformResponsesStream_EmptyDeltaSkipped(t *testing.T) {
	input := `data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":""}

data: [DONE]
`
	rc := TransformResponsesStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("missing [DONE]: %q", out)
	}
}

func TestTransformResponsesStream_MalformedForwarded(t *testing.T) {
	input := "data: {not json}\n\ndata: [DONE]\n"
	rc := TransformResponsesStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, "{not json}") {
		t.Errorf("malformed event not forwarded: %q", out)
	}
}

func TestTransformResponsesStream_NonDataLinesForwarded(t *testing.T) {
	input := ": comment\n\nevent: response.created\nid: 1\n\ndata: [DONE]\n"
	rc := TransformResponsesStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	for _, want := range []string{": comment", "event: response.created", "id: 1", "[DONE]"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q: %q", want, out)
		}
	}
}

func TestTransformResponsesStream_UnclosedTagFlushed(t *testing.T) {
	input := `data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message"}}

data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"answer <think>unclosed"}

data: [DONE]
`
	rc := TransformResponsesStream(io.NopCloser(strings.NewReader(input)), Options{})
	defer rc.Close()
	out, err := ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(out, "unclosed") {
		t.Errorf("unclosed reasoning dropped: %q", out)
	}
	if strings.Contains(out, `"reasoning"`) {
		t.Errorf("reasoning synthesized for unclosed tag: %q", out)
	}
}
