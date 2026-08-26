package thinkextract

import (
	"testing"

	"github.com/enterpilot/gomodel/internal/core"
)

func responsesResp(texts ...string) *core.ResponsesResponse {
	out := &core.ResponsesResponse{}
	for _, text := range texts {
		out.Output = append(out.Output, core.ResponsesOutputItem{
			ID:   "msg_x",
			Type: "message",
			Content: []core.ResponsesContentItem{
				{Type: "output_text", Text: text},
			},
		})
	}
	return out
}

func TestTransformResponsesResponse_NoTags(t *testing.T) {
	resp := responsesResp("plain answer")
	if got := TransformResponsesResponse(resp, Options{}); got != 0 {
		t.Fatalf("rewritten=%d, want 0", got)
	}
	if len(resp.Output) != 1 {
		t.Errorf("output items=%d, want 1", len(resp.Output))
	}
}

func TestTransformResponsesResponse_SingleItem(t *testing.T) {
	resp := responsesResp("answer<think>hidden</think> rest")
	if got := TransformResponsesResponse(resp, Options{}); got != 1 {
		t.Fatalf("rewritten=%d, want 1", got)
	}
	if len(resp.Output) != 2 {
		t.Fatalf("output items=%d, want 2 (reasoning + message)", len(resp.Output))
	}
	if resp.Output[0].Type != "reasoning" {
		t.Errorf("output[0].Type=%q, want reasoning", resp.Output[0].Type)
	}
	if resp.Output[0].Content[0].Type != "reasoning_text" || resp.Output[0].Content[0].Text != "hidden" {
		t.Errorf("reasoning content=%+v, want reasoning_text hidden", resp.Output[0].Content[0])
	}
	if resp.Output[1].Type != "message" {
		t.Errorf("output[1].Type=%q, want message", resp.Output[1].Type)
	}
	if resp.Output[1].Content[0].Text != "answer rest" {
		t.Errorf("message text=%q, want %q", resp.Output[1].Content[0].Text, "answer rest")
	}
}

func TestTransformResponsesResponse_MultipleItems(t *testing.T) {
	resp := responsesResp("a<think>x</think>b", "plain", "c<think>y</think>d")
	if got := TransformResponsesResponse(resp, Options{}); got != 2 {
		t.Fatalf("rewritten=%d, want 2", got)
	}
	// Expect: reasoning, message(ab), message(plain), reasoning, message(cd)
	if len(resp.Output) != 5 {
		t.Fatalf("items=%d, want 5", len(resp.Output))
	}
	if resp.Output[0].Type != "reasoning" || resp.Output[3].Type != "reasoning" {
		t.Errorf("reasoning items misplaced: %+v", resp.Output)
	}
	if resp.Output[1].Content[0].Text != "ab" || resp.Output[4].Content[0].Text != "cd" {
		t.Errorf("message texts wrong: %q %q", resp.Output[1].Content[0].Text, resp.Output[4].Content[0].Text)
	}
	if resp.Output[2].Content[0].Text != "plain" {
		t.Errorf("untouched middle item changed: %q", resp.Output[2].Content[0].Text)
	}
}

func TestTransformResponsesResponse_NonMessageItemUntouched(t *testing.T) {
	resp := &core.ResponsesResponse{
		Output: []core.ResponsesOutputItem{
			{ID: "fc_1", Type: "function_call", Name: "f", Arguments: "{}"},
			{ID: "msg_1", Type: "message", Content: []core.ResponsesContentItem{{Type: "output_text", Text: "a<think>x</think>b"}}},
		},
	}
	if got := TransformResponsesResponse(resp, Options{}); got != 1 {
		t.Fatalf("rewritten=%d, want 1", got)
	}
	if resp.Output[0].Type != "function_call" {
		t.Errorf("function_call item moved or rewritten: %+v", resp.Output[0])
	}
	if resp.Output[1].Type != "reasoning" {
		t.Errorf("reasoning item not inserted before message: %+v", resp.Output[1])
	}
}

func TestTransformResponsesResponse_NonTextContentUntouched(t *testing.T) {
	resp := &core.ResponsesResponse{
		Output: []core.ResponsesOutputItem{{
			ID:   "msg_1",
			Type: "message",
			Content: []core.ResponsesContentItem{
				{Type: "input_image", ImageURL: &core.ImageURLContent{URL: "data:image/png;base64,xxx"}},
			},
		}},
	}
	if got := TransformResponsesResponse(resp, Options{}); got != 0 {
		t.Errorf("rewritten=%d, want 0", got)
	}
}

func TestTransformResponsesResponse_NilResponse(t *testing.T) {
	if got := TransformResponsesResponse(nil, Options{}); got != 0 {
		t.Errorf("nil response: got=%d, want 0", got)
	}
}

func TestTransformResponsesResponse_EmptyBodyBlock(t *testing.T) {
	resp := responsesResp("before<think></think>after")
	if got := TransformResponsesResponse(resp, Options{}); got != 1 {
		t.Fatalf("rewritten=%d, want 1 (empty-body block is still a rewrite)", got)
	}
	// Reasoning item is still prepended with empty text, matching the chat
	// path's behaviour of counting empty-body rewrites.
	if resp.Output[0].Type != "reasoning" {
		t.Errorf("reasoning item missing for empty-body block")
	}
}
