// Package kimi provides Kimi API integration for the LLM gateway.
package kimi

import (
	"context"
	"io"
	"net/http"
	"time"

	"gomodel/internal/core"
	"gomodel/internal/llmclient"
	"gomodel/internal/providers"
	"gomodel/internal/providers/openai"
)

// Registration provides factory registration for the Kimi provider.
var Registration = providers.Registration{
	Type: "kimi",
	New:  New,
	Discovery: providers.DiscoveryConfig{
		DefaultBaseURL: defaultBaseURL,
	},
}

const (
	defaultBaseURL = "https://api.kimi.com/coding/v1"
)

// Provider implements the core.Provider interface for Kimi. Kimi's API is
// OpenAI-compatible, so all transport goes through the shared compatible
// provider.
type Provider struct {
	compat *openai.CompatibleProvider
}

// New creates a new Kimi provider.
func New(providerCfg providers.ProviderConfig, opts providers.ProviderOptions) core.Provider {
	return &Provider{
		compat: openai.NewCompatibleProvider(providerCfg.APIKey, opts, openai.CompatibleProviderConfig{
			ProviderName: "kimi",
			BaseURL:      providers.ResolveBaseURL(providerCfg.BaseURL, defaultBaseURL),
			SetHeaders:   setHeaders,
		}),
	}
}

// NewWithHTTPClient creates a new Kimi provider with a custom HTTP client.
// If httpClient is nil, http.DefaultClient is used.
func NewWithHTTPClient(apiKey string, httpClient *http.Client, hooks llmclient.Hooks) *Provider {
	return &Provider{
		compat: openai.NewCompatibleProviderWithHTTPClient(apiKey, httpClient, hooks, openai.CompatibleProviderConfig{
			ProviderName: "kimi",
			BaseURL:      defaultBaseURL,
			SetHeaders:   setHeaders,
		}),
	}
}

// setHeaders sets the required headers for Kimi API requests.
// Only ZooCode-identifying headers are spoofed; standard browser/SDK
// metadata (Accept-*, Connection, Sec-Fetch-Mode, X-Stainless-*) is
// omitted because an API server does not validate them for access control.
func setHeaders(req *http.Request, apiKey string) {
	providers.SetAuthHeaders(req, apiKey, providers.AuthHeaderConfig{
		AuthScheme: "Bearer ",
	})

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Http-Referer", "https://github.com/Zoo-Code-Org/Zoo-Code")
	req.Header.Set("User-Agent", "ZooCode/3.62.0")
	req.Header.Set("X-Title", "Zoo Code")
}

// SetBaseURL allows configuring a custom base URL for the provider.
func (p *Provider) SetBaseURL(url string) {
	p.compat.SetBaseURL(url)
}

// ChatCompletion sends a chat completion request to Kimi.
func (p *Provider) ChatCompletion(ctx context.Context, req *core.ChatRequest) (*core.ChatResponse, error) {
	return p.compat.ChatCompletion(ctx, req)
}

// StreamChatCompletion returns a raw response body for streaming (caller must close).
func (p *Provider) StreamChatCompletion(ctx context.Context, req *core.ChatRequest) (io.ReadCloser, error) {
	return p.compat.StreamChatCompletion(ctx, req)
}

// ListModels returns a synthetic model list for Kimi.
func (p *Provider) ListModels(ctx context.Context) (*core.ModelsResponse, error) {
	return &core.ModelsResponse{
		Object: "list",
		Data: []core.Model{
			{
				ID:      "kimi-for-coding",
				Object:  "model",
				OwnedBy: "kimi",
				Created: time.Now().Unix(),
			},
		},
	}, nil
}

// Responses sends a Responses API request to Kimi.
func (p *Provider) Responses(ctx context.Context, req *core.ResponsesRequest) (*core.ResponsesResponse, error) {
	return p.compat.Responses(ctx, req)
}

// StreamResponses returns a raw response body for streaming Responses API (caller must close).
func (p *Provider) StreamResponses(ctx context.Context, req *core.ResponsesRequest) (io.ReadCloser, error) {
	return p.compat.StreamResponses(ctx, req)
}

// Embeddings sends an embeddings request to Kimi.
func (p *Provider) Embeddings(ctx context.Context, req *core.EmbeddingRequest) (*core.EmbeddingResponse, error) {
	return p.compat.Embeddings(ctx, req)
}

// CreateSpeech synthesizes speech through Kimi's OpenAI-compatible /audio/speech API.
func (p *Provider) CreateSpeech(ctx context.Context, req *core.AudioSpeechRequest) (*core.AudioResponse, error) {
	return p.compat.CreateSpeech(ctx, req)
}

// CreateTranscription transcribes audio through Kimi's OpenAI-compatible /audio/transcriptions API.
func (p *Provider) CreateTranscription(ctx context.Context, req *core.AudioTranscriptionRequest) (*core.AudioResponse, error) {
	return p.compat.CreateTranscription(ctx, req)
}

// CreateBatch creates a native Kimi batch job.
func (p *Provider) CreateBatch(ctx context.Context, req *core.BatchRequest) (*core.BatchResponse, error) {
	return p.compat.CreateBatch(ctx, req)
}

// GetBatch retrieves a native Kimi batch job.
func (p *Provider) GetBatch(ctx context.Context, id string) (*core.BatchResponse, error) {
	return p.compat.GetBatch(ctx, id)
}

// ListBatches lists native Kimi batch jobs.
func (p *Provider) ListBatches(ctx context.Context, limit int, after string) (*core.BatchListResponse, error) {
	return p.compat.ListBatches(ctx, limit, after)
}

// CancelBatch cancels a native Kimi batch job.
func (p *Provider) CancelBatch(ctx context.Context, id string) (*core.BatchResponse, error) {
	return p.compat.CancelBatch(ctx, id)
}

// GetBatchResults fetches Kimi batch results via the output file API.
func (p *Provider) GetBatchResults(ctx context.Context, id string) (*core.BatchResultsResponse, error) {
	return p.compat.GetBatchResults(ctx, id)
}

// CreateFile uploads a file through Kimi's OpenAI-compatible /files API.
func (p *Provider) CreateFile(ctx context.Context, req *core.FileCreateRequest) (*core.FileObject, error) {
	return p.compat.CreateFile(ctx, req)
}

// ListFiles lists files through Kimi's OpenAI-compatible /files API.
func (p *Provider) ListFiles(ctx context.Context, purpose string, limit int, after string) (*core.FileListResponse, error) {
	return p.compat.ListFiles(ctx, purpose, limit, after)
}

// GetFile retrieves one file object through Kimi's OpenAI-compatible /files API.
func (p *Provider) GetFile(ctx context.Context, id string) (*core.FileObject, error) {
	return p.compat.GetFile(ctx, id)
}

// DeleteFile deletes a file object through Kimi's OpenAI-compatible /files API.
func (p *Provider) DeleteFile(ctx context.Context, id string) (*core.FileDeleteResponse, error) {
	return p.compat.DeleteFile(ctx, id)
}

// GetFileContent fetches raw file bytes through Kimi's /files/{id}/content API.
func (p *Provider) GetFileContent(ctx context.Context, id string) (*core.FileContentResponse, error) {
	return p.compat.GetFileContent(ctx, id)
}

// Passthrough forwards a raw request to Kimi without any transformation.
func (p *Provider) Passthrough(ctx context.Context, req *core.PassthroughRequest) (*core.PassthroughResponse, error) {
	return p.compat.Passthrough(ctx, req)
}
