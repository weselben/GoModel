package gateway

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/thinkextract"
)

// thinkProvider is a mock provider that returns a fixed response and stream
// for each endpoint. Used to verify the thinkextract orchestrator hooks
// without touching real providers.
type thinkProvider struct {
	chatResponse *core.ChatResponse
	chatStream   io.ReadCloser
	responsesResp *core.ResponsesResponse
	responsesStream io.ReadCloser
}

func (p *thinkProvider) ChatCompletion(_ context.Context, _ *core.ChatRequest) (*core.ChatResponse, error) {
	return p.chatResponse, nil
}

func (p *thinkProvider) StreamChatCompletion(_ context.Context, _ *core.ChatRequest) (io.ReadCloser, error) {
	return p.chatStream, nil
}

func (p *thinkProvider) Responses(_ context.Context, _ *core.ResponsesRequest) (*core.ResponsesResponse, error) {
	return p.responsesResp, nil
}

func (p *thinkProvider) StreamResponses(_ context.Context, _ *core.ResponsesRequest) (io.ReadCloser, error) {
	return p.responsesStream, nil
}

func (p *thinkProvider) ListModels(_ context.Context) (*core.ModelsResponse, error) {
	return &core.ModelsResponse{}, nil
}

func (p *thinkProvider) Embeddings(_ context.Context, _ *core.EmbeddingRequest) (*core.EmbeddingResponse, error) {
	return nil, nil
}

func (p *thinkProvider) GetProviderType(_ string) string {
	return "think"
}

func (p *thinkProvider) GetName() string {
	return "think"
}

func (p *thinkProvider) Supports(_ string) bool {
	return true
}

func TestOrchestratorChatCompletion_ThinkExtracted(t *testing.T) {
	provider := &thinkProvider{
		chatResponse: &core.ChatResponse{
			Choices: []core.Choice{{
				Message: core.ResponseMessage{Role: "assistant", Content: "answer<think>reasoning</think> rest"},
			}},
		},
	}
	o := NewInferenceOrchestrator(InferenceConfig{
		Provider: provider,
		ThinkExtractOptions: &thinkextract.Options{},
	})
	resp, _, _, _, _, err := o.DispatchChatCompletion(context.Background(), nil, &core.ChatRequest{Model: "test"})
	if err != nil {
		t.Fatalf("DispatchChatCompletion: %v", err)
	}
	msg := resp.Choices[0].Message
	if content, _ := msg.Content.(string); content != "answer rest" {
		t.Errorf("content=%q, want %q", content, "answer rest")
	}
	raw := msg.ExtraFields.Lookup("reasoning_content")
	if len(raw) == 0 {
		t.Fatalf("reasoning_content missing")
	}
}

func TestOrchestratorChatCompletion_DisabledOnChatSurface(t *testing.T) {
	provider := &thinkProvider{
		chatResponse: &core.ChatResponse{
			Choices: []core.Choice{{
				Message: core.ResponseMessage{Role: "assistant", Content: "a<think>x</think>b"},
			}},
		},
	}
	off := false
	o := NewInferenceOrchestrator(InferenceConfig{
		Provider: provider,
		ThinkExtractOptions: &thinkextract.Options{ChatEnabled: &off},
	})
	resp, _, _, _, _, err := o.DispatchChatCompletion(thinkextract.WithSurface(context.Background(), thinkextract.SurfaceChat), nil, &core.ChatRequest{Model: "test"})
	if err != nil {
		t.Fatalf("DispatchChatCompletion: %v", err)
	}
	msg := resp.Choices[0].Message
	if content, _ := msg.Content.(string); content != "a<think>x</think>b" {
		t.Errorf("content rewritten despite chat disabled: %q", content)
	}
	if len(msg.ExtraFields.Lookup("reasoning_content")) > 0 {
		t.Errorf("reasoning set despite chat disabled")
	}
}

func TestOrchestratorStreamChatCompletion_ThinkExtracted(t *testing.T) {
	input := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a<think>r</think>b\"}}]}\n\ndata: [DONE]\n"
	provider := &thinkProvider{
		chatStream: io.NopCloser(strings.NewReader(input)),
	}
	o := NewInferenceOrchestrator(InferenceConfig{
		Provider: provider,
		ThinkExtractOptions: &thinkextract.Options{},
	})
	stream, err := o.StreamChatCompletion(context.Background(), nil, &core.ChatRequest{Model: "test", Stream: true})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	defer stream.Stream.Close()
	out, err := io.ReadAll(stream.Stream)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(string(out), "reasoning_content") {
		t.Errorf("stream not rewritten: %q", string(out))
	}
}

func TestOrchestratorResponses_ThinkExtracted(t *testing.T) {
	provider := &thinkProvider{
		responsesResp: &core.ResponsesResponse{
			Output: []core.ResponsesOutputItem{{
				ID:   "msg_1",
				Type: "message",
				Content: []core.ResponsesContentItem{
					{Type: "output_text", Text: "a<think>x</think>b"},
				},
			}},
		},
	}
	o := NewInferenceOrchestrator(InferenceConfig{
		Provider: provider,
		ThinkExtractOptions: &thinkextract.Options{},
	})
	resp, _, _, _, _, err := o.DispatchResponses(context.Background(), nil, &core.ResponsesRequest{Model: "test"})
	if err != nil {
		t.Fatalf("DispatchResponses: %v", err)
	}
	if len(resp.Output) != 2 {
		t.Fatalf("output items=%d, want 2 (reasoning + message)", len(resp.Output))
	}
	if resp.Output[0].Type != "reasoning" {
		t.Errorf("output[0].Type=%q, want reasoning", resp.Output[0].Type)
	}
}

func TestOrchestratorStreamResponses_ThinkExtracted(t *testing.T) {
	input := "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"a<think>x</think>b\"}\n\n" +
		"data: [DONE]\n"
	provider := &thinkProvider{
		responsesStream: io.NopCloser(strings.NewReader(input)),
	}
	o := NewInferenceOrchestrator(InferenceConfig{
		Provider: provider,
		ThinkExtractOptions: &thinkextract.Options{},
	})
	stream, err := o.StreamResponses(context.Background(), nil, &core.ResponsesRequest{Model: "test", Stream: true})
	if err != nil {
		t.Fatalf("StreamResponses: %v", err)
	}
	defer stream.Stream.Close()
	out, err := io.ReadAll(stream.Stream)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(string(out), `"type":"reasoning"`) {
		t.Errorf("reasoning item not synthesized: %q", string(out))
	}
}

func TestOrchestratorChatCompletion_NilThinkExtractOptions(t *testing.T) {
	provider := &thinkProvider{
		chatResponse: &core.ChatResponse{
			Choices: []core.Choice{{
				Message: core.ResponseMessage{Role: "assistant", Content: "a<think>x</think>b"},
			}},
		},
	}
	o := NewInferenceOrchestrator(InferenceConfig{Provider: provider})
	resp, _, _, _, _, err := o.DispatchChatCompletion(context.Background(), nil, &core.ChatRequest{Model: "test"})
	if err != nil {
		t.Fatalf("DispatchChatCompletion: %v", err)
	}
	msg := resp.Choices[0].Message
	if content, _ := msg.Content.(string); content != "a<think>x</think>b" {
		t.Errorf("content rewritten with nil options: %q", content)
	}
}

func TestOrchestratorChatCompletion_MessagesSurfaceDefaultsOff(t *testing.T) {
	provider := &thinkProvider{
		chatResponse: &core.ChatResponse{
			Choices: []core.Choice{{
				Message: core.ResponseMessage{Role: "assistant", Content: "a<think>x</think>b"},
			}},
		},
	}
	o := NewInferenceOrchestrator(InferenceConfig{
		Provider: provider,
		ThinkExtractOptions: &thinkextract.Options{},
	})
	ctx := thinkextract.WithSurface(context.Background(), thinkextract.SurfaceMessages)
	resp, _, _, _, _, err := o.DispatchChatCompletion(ctx, nil, &core.ChatRequest{Model: "test"})
	if err != nil {
		t.Fatalf("DispatchChatCompletion: %v", err)
	}
	msg := resp.Choices[0].Message
	if content, _ := msg.Content.(string); content != "a<think>x</think>b" {
		t.Errorf("content rewritten on messages surface despite default-off policy: %q", content)
	}
}

func TestOrchestratorChatCompletion_MessagesSurfaceUnsignedPolicy(t *testing.T) {
	provider := &thinkProvider{
		chatResponse: &core.ChatResponse{
			Choices: []core.Choice{{
				Message: core.ResponseMessage{Role: "assistant", Content: "a<think>x</think>b"},
			}},
		},
	}
	o := NewInferenceOrchestrator(InferenceConfig{
		Provider: provider,
		ThinkExtractOptions: &thinkextract.Options{MessagesPolicy: "unsigned"},
	})
	ctx := thinkextract.WithSurface(context.Background(), thinkextract.SurfaceMessages)
	resp, _, _, _, _, err := o.DispatchChatCompletion(ctx, nil, &core.ChatRequest{Model: "test"})
	if err != nil {
		t.Fatalf("DispatchChatCompletion: %v", err)
	}
	msg := resp.Choices[0].Message
	if content, _ := msg.Content.(string); content != "ab" {
		t.Errorf("content not rewritten on messages surface with unsigned policy: %q", content)
	}
	if len(msg.ExtraFields.Lookup(thinkextract.SynthesizedMarkerKey)) == 0 {
		t.Errorf("synthesized marker missing on messages surface")
	}
}
