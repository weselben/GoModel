package thinkextract

import (
	"encoding/json"
	"testing"

	"github.com/enterpilot/gomodel/internal/core"
)

func newResp(content string, extra core.UnknownJSONFields) *core.ChatResponse {
	return &core.ChatResponse{
		Choices: []core.Choice{{
			Message: core.ResponseMessage{
				Role:        "assistant",
				Content:     content,
				ExtraFields: extra,
			},
		}},
	}
}

func TestTransformChatResponse_NoTags(t *testing.T) {
	resp := newResp("hello world", core.UnknownJSONFields{})
	if got := TransformChatResponse(resp, Options{}); got != 0 {
		t.Fatalf("rewritten=%d, want 0", got)
	}
	if resp.Choices[0].Message.Content != "hello world" {
		t.Errorf("content mutated: %v", resp.Choices[0].Message.Content)
	}
	if len(resp.Choices[0].Message.ExtraFields.Lookup(FieldReasoning)) > 0 {
		t.Errorf("reasoning unexpectedly set")
	}
}

func TestTransformChatResponse_StringContent(t *testing.T) {
	resp := newResp("answer<think>hidden</think> rest", core.UnknownJSONFields{})
	if got := TransformChatResponse(resp, Options{}); got != 1 {
		t.Fatalf("rewritten=%d, want 1", got)
	}
	if content, _ := resp.Choices[0].Message.Content.(string); content != "answer rest" {
		t.Errorf("content=%q, want %q", content, "answer rest")
	}
	raw := resp.Choices[0].Message.ExtraFields.Lookup(FieldReasoning)
	if len(raw) == 0 {
		t.Fatalf("reasoning_content missing")
	}
	var got string
	if err := jsonUnmarshalString(raw, &got); err != nil {
		t.Fatalf("unmarshal reasoning: %v", err)
	}
	if got != "hidden" {
		t.Errorf("reasoning=%q, want %q", got, "hidden")
	}
}

func TestTransformChatResponse_PreservesExistingExtra(t *testing.T) {
	resp := newResp("a<think>b</think>c", core.UnknownJSONFields{})
	// Add a known extra field first.
	merged, err := core.MergeUnknownJSONFields(resp.Choices[0].Message.ExtraFields, map[string]json.RawMessage{
		"x_custom": json.RawMessage(`"keepme"`),
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	resp.Choices[0].Message.ExtraFields = merged

	if got := TransformChatResponse(resp, Options{}); got != 1 {
		t.Fatalf("rewritten=%d, want 1", got)
	}
	raw := resp.Choices[0].Message.ExtraFields.Lookup("x_custom")
	if len(raw) == 0 {
		t.Fatalf("x_custom lost during rewrite")
	}
	var got string
	if err := jsonUnmarshalString(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != "keepme" {
		t.Errorf("x_custom=%q, want %q", got, "keepme")
	}
}

func TestTransformChatResponse_UpstreamReasoningWins(t *testing.T) {
	resp := newResp("a<think>b</think>c", core.UnknownJSONFields{})
	merged, _ := core.MergeUnknownJSONFields(resp.Choices[0].Message.ExtraFields, map[string]json.RawMessage{
		FieldReasoning: json.RawMessage(`"upstream"`),
	})
	resp.Choices[0].Message.ExtraFields = merged

	if got := TransformChatResponse(resp, Options{}); got != 0 {
		t.Fatalf("rewritten=%d, want 0 (upstream reasoning preserved)", got)
	}
	if content, _ := resp.Choices[0].Message.Content.(string); content != "a<think>b</think>c" {
		t.Errorf("content mutated: %q", content)
	}
}

func TestTransformChatResponse_ContentParts(t *testing.T) {
	resp := &core.ChatResponse{
		Choices: []core.Choice{{
			Message: core.ResponseMessage{
				Role: "assistant",
				Content: []core.ContentPart{
					{Type: "text", Text: "before<think>x</think>"},
					{Type: "image_url", ImageURL: &core.ImageURLContent{URL: "data:image/png;base64,xxx"}},
					{Type: "text", Text: "<think>y</think>after"},
				},
			},
		}},
	}
	if got := TransformChatResponse(resp, Options{}); got != 1 {
		t.Fatalf("rewritten=%d, want 1", got)
	}
	parts := resp.Choices[0].Message.Content.([]core.ContentPart)
	if parts[0].Text != "before" {
		t.Errorf("part[0].Text=%q, want %q", parts[0].Text, "before")
	}
	if parts[1].Type != "image_url" {
		t.Errorf("part[1] type=%q, want image_url", parts[1].Type)
	}
	if parts[2].Text != "after" {
		t.Errorf("part[2].Text=%q, want %q", parts[2].Text, "after")
	}
	raw := resp.Choices[0].Message.ExtraFields.Lookup(FieldReasoning)
	if len(raw) == 0 {
		t.Fatalf("reasoning missing")
	}
	var got string
	if err := jsonUnmarshalString(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != "x\n\ny" {
		t.Errorf("reasoning=%q, want %q", got, "x\n\ny")
	}
}

func TestTransformChatResponse_NilResponse(t *testing.T) {
	if got := TransformChatResponse(nil, Options{}); got != 0 {
		t.Errorf("nil response: got=%d, want 0", got)
	}
}

func TestTransformChatResponse_MultipleChoices(t *testing.T) {
	resp := &core.ChatResponse{
		Choices: []core.Choice{
			{Message: core.ResponseMessage{Role: "assistant", Content: "a<think>x</think>b"}},
			{Message: core.ResponseMessage{Role: "assistant", Content: "plain"}},
			{Message: core.ResponseMessage{Role: "assistant", Content: "c<think>y</think>d"}},
		},
	}
	if got := TransformChatResponse(resp, Options{}); got != 2 {
		t.Fatalf("rewritten=%d, want 2", got)
	}
	for i, want := range []string{"ab", "plain", "cd"} {
		if c, _ := resp.Choices[i].Message.Content.(string); c != want {
			t.Errorf("choice[%d].Content=%q, want %q", i, c, want)
		}
	}
}

// jsonUnmarshalString unmarshals a JSON string into a string value.
// Standard for unmarshalling json.RawMessage that contains a JSON string.
func jsonUnmarshalString(raw []byte, out *string) error {
	return json.Unmarshal(raw, out)
}