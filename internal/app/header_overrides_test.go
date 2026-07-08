package app

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"gomodel/config"
	"gomodel/internal/core"
	"gomodel/internal/providers"
)

// minimalAppProvider is a routable provider that does nothing.
type minimalAppProvider struct{}

func (minimalAppProvider) ChatCompletion(_ context.Context, _ *core.ChatRequest) (*core.ChatResponse, error) {
	return nil, nil
}
func (minimalAppProvider) StreamChatCompletion(_ context.Context, _ *core.ChatRequest) (io.ReadCloser, error) {
	return nil, nil
}
func (minimalAppProvider) ListModels(_ context.Context) (*core.ModelsResponse, error) {
	return &core.ModelsResponse{Object: "list"}, nil
}
func (minimalAppProvider) Responses(_ context.Context, _ *core.ResponsesRequest) (*core.ResponsesResponse, error) {
	return nil, nil
}
func (minimalAppProvider) StreamResponses(_ context.Context, _ *core.ResponsesRequest) (io.ReadCloser, error) {
	return nil, nil
}
func (minimalAppProvider) Embeddings(_ context.Context, _ *core.EmbeddingRequest) (*core.EmbeddingResponse, error) {
	return nil, nil
}
func (minimalAppProvider) Route(_ context.Context, _ *core.ChatRequest) (string, error) {
	return "", nil
}

func TestNew_AppliesHeaderOverrideWiring(t *testing.T) {
	ctx := t.Context()

	cacheDir := t.TempDir()
	dbFile := filepath.Join(t.TempDir(), "test.db")

	cfg := &config.Config{
		Server: config.ServerConfig{
			Port:                        "0",
			BasePath:                    "/",
			UserPathHeader:              "X-Tenant",
			EnablePassthroughRoutes:     true,
			AllowPassthroughV1Alias:     true,
			EnabledPassthroughProviders: []string{"test"},
		},
		Models: config.ModelsConfig{
			EnabledByDefault:             true,
			ConfiguredProviderModelsMode: config.ConfiguredProviderModelsModeFallback,
		},
		Cache: config.CacheConfig{
			Model: config.ModelCacheConfig{
				RefreshInterval: 3600,
				RecheckInterval: 60,
				Local:           &config.LocalCacheConfig{CacheDir: cacheDir},
			},
		},
		Storage: config.StorageConfig{
			Type: "sqlite",
			SQLite: config.SQLiteStorageConfig{
				Path: dbFile,
			},
		},
		Logging:    config.LogConfig{Enabled: false},
		Usage:      config.UsageConfig{Enabled: false},
		Budgets:    config.BudgetsConfig{Enabled: false},
		RateLimits: config.RateLimitsConfig{Enabled: false},
		HTTP: config.HTTPConfig{
			Timeout:               5,
			ResponseHeaderTimeout: 5,
		},
		Admin: config.AdminConfig{
			EndpointsEnabled: false,
			UIEnabled:        false,
			LiveLogsEnabled:  false,
		},
		Workflows: config.WorkflowsConfig{
			RefreshInterval: time.Minute,
		},
	}

	factory := providers.NewProviderFactory()
	factory.Add(providers.Registration{
		Type: "test",
		New: func(_ providers.ProviderConfig, _ providers.ProviderOptions) core.Provider {
			return minimalAppProvider{}
		},
	})

	loadResult := &config.LoadResult{
		Config: cfg,
		RawProviders: map[string]config.RawProviderConfig{
			"test": {
				Type:                   "test",
				APIKey:                 "sk-test",
				PassthroughUserHeaders: true,
			},
		},
	}

	app, err := New(ctx, Config{
		AppConfig: loadResult,
		Factory:   factory,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() {
		if err := app.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown() error = %v", err)
		}
	}()

	if app.config.Server.UserPathHeader != "X-Tenant" {
		t.Errorf("UserPathHeader = %q, want X-Tenant", app.config.Server.UserPathHeader)
	}
	if !app.providers.AnyPassthroughUserHeaders {
		t.Error("AnyPassthroughUserHeaders = false, want true")
	}
}
