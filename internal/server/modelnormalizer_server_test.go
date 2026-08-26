package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/modelnormalizer"
)

// chatCaptureProvider records the chat request it receives and returns a
// minimal response so the test can verify what the provider actually saw.
type chatCaptureProvider struct {
	lastChat *core.ChatRequest
}

func (p *chatCaptureProvider) ChatCompletion(_ context.Context, req *core.ChatRequest) (*core.ChatResponse, error) {
	p.lastChat = req
	return &core.ChatResponse{ID: "chatcmpl-1", Model: req.Model, Provider: "kimicode"}, nil
}
func (p *chatCaptureProvider) StreamChatCompletion(context.Context, *core.ChatRequest) (io.ReadCloser, error) {
	return nil, nil
}
func (p *chatCaptureProvider) ListModels(context.Context) (*core.ModelsResponse, error) {
	return &core.ModelsResponse{Object: "list", Data: []core.Model{
		{ID: "kimi-for-coding", Object: "model", OwnedBy: "kimicode"},
		{ID: "k3", Object: "model", OwnedBy: "kimicode"},
	}}, nil
}
func (p *chatCaptureProvider) Responses(context.Context, *core.ResponsesRequest) (*core.ResponsesResponse, error) {
	return nil, nil
}
func (p *chatCaptureProvider) StreamResponses(context.Context, *core.ResponsesRequest) (io.ReadCloser, error) {
	return nil, nil
}
func (p *chatCaptureProvider) Embeddings(context.Context, *core.EmbeddingRequest) (*core.EmbeddingResponse, error) {
	return nil, nil
}
func (p *chatCaptureProvider) Supports(string) bool            { return true }
func (p *chatCaptureProvider) GetProviderType(string) string   { return "kimicode" }

func TestHandler_ChatCompletionAppliesNormalizer(t *testing.T) {
	provider := &chatCaptureProvider{}
	normalizer := modelnormalizer.New([]modelnormalizer.Rule{
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding", Thinking: modelnormalizer.ThinkingDisabled},
	})
	srv := New(provider, &Config{ModelNormalizer: normalizer})

	body := `{"model":"kimi-k2.6","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.echo.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	require.NotNil(t, provider.lastChat)
	require.Equal(t, "kimi-for-coding", provider.lastChat.Model)
	require.Equal(t, "kimicode", provider.lastChat.Provider)
	thinking := provider.lastChat.ExtraFields.Lookup("thinking")
	require.NotNil(t, thinking, "provider should receive thinking extension")
	require.Contains(t, string(thinking), `"disabled"`)
}

func TestHandler_ChatCompletionUnknownAliasPassesThrough(t *testing.T) {
	provider := &chatCaptureProvider{}
	normalizer := modelnormalizer.New([]modelnormalizer.Rule{
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding"},
	})
	srv := New(provider, &Config{ModelNormalizer: normalizer})

	body := `{"model":"kimicode/kimi-for-coding","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.echo.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	require.NotNil(t, provider.lastChat)
	require.Equal(t, "kimi-for-coding", provider.lastChat.Model)
	require.Equal(t, "kimicode", provider.lastChat.Provider)
	require.Nil(t, provider.lastChat.ExtraFields.Lookup("thinking"))
}

func TestNew_WiresModelNormalizerIntoChatPath(t *testing.T) {
	provider := &chatCaptureProvider{}
	normalizer := modelnormalizer.New([]modelnormalizer.Rule{
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding", Thinking: modelnormalizer.ThinkingDisabled},
	})

	srv := New(provider, &Config{ModelNormalizer: normalizer})

	// Send a chat completion request for the canonical alias; the provider
	// should see the rewritten target model + injected thinking field.
	body := `{"model":"kimi-k2.6","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.echo.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	require.NotNil(t, provider.lastChat)
	require.Equal(t, "kimi-for-coding", provider.lastChat.Model)
	require.Equal(t, "kimicode", provider.lastChat.Provider)
	thinking := provider.lastChat.ExtraFields.Lookup("thinking")
	require.NotNil(t, thinking, "provider should receive thinking extension")
	require.Contains(t, string(thinking), `"disabled"`)
}

func TestNew_ListModelsMergesNormalizerAliases(t *testing.T) {
	provider := &chatCaptureProvider{}
	cw := 262144
	normalizer := modelnormalizer.New([]modelnormalizer.Rule{
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding", Modes: []string{"chat"}, ContextWindow: &cw},
		{Alias: "bge_m3_embed", Target: "kimicode/bge_m3_embed", Modes: []string{"embedding"}},
	})

	srv := New(provider, &Config{ModelNormalizer: normalizer})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.echo.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp core.ModelsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	byID := make(map[string]core.Model, len(resp.Data))
	for _, m := range resp.Data {
		byID[m.ID] = m
	}

	// Provider models are still listed.
	require.Contains(t, byID, "kimi-for-coding")
	require.Contains(t, byID, "k3")

	// Canonical aliases are synthesized with metadata.
	require.Contains(t, byID, "kimi-k2.6")
	require.Equal(t, "model", byID["kimi-k2.6"].Object)
	require.NotNil(t, byID["kimi-k2.6"].Metadata)
	require.Equal(t, []string{"chat"}, byID["kimi-k2.6"].Metadata.Modes)
	require.NotNil(t, byID["kimi-k2.6"].Metadata.ContextWindow)
	require.Equal(t, 262144, *byID["kimi-k2.6"].Metadata.ContextWindow)

	require.Contains(t, byID, "bge_m3_embed")
	require.NotNil(t, byID["bge_m3_embed"].Metadata)
	require.Equal(t, []string{"embedding"}, byID["bge_m3_embed"].Metadata.Modes)
	require.Contains(t, byID["bge_m3_embed"].Metadata.Categories, core.CategoryEmbedding)
}

func TestNew_ListModelsWithoutNormalizerLeavesHandlerUnchanged(t *testing.T) {
	provider := &chatCaptureProvider{}
	srv := New(provider, &Config{})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	srv.echo.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp core.ModelsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Data, 2) // only the provider's two models, no aliases
}