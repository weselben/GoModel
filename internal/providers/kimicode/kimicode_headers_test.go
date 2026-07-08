package kimicode

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"gomodel/internal/core"
	"gomodel/internal/providers"
)

func TestNew_AppliesHeaderOverrides(t *testing.T) {
	var receivedHeaders http.Header

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	provider := New(
		providers.ProviderConfig{APIKey: "test-key", BaseURL: server.URL},
		providers.ProviderOptions{
			HeaderOverrides: providers.HeaderOverridesConfig{
				CustomUpstreamHeaders: map[string]string{
					"X-Custom-Header": "custom-value",
					"X-Tenant-ID":     "tenant-123",
				},
			},
		},
	)

	_, err := provider.ChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "kimi-code-model",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("ChatCompletion() error = %v", err)
	}

	if receivedHeaders == nil {
		t.Fatal("receivedHeaders is nil; server did not receive a request")
	}

	if got := receivedHeaders.Get("X-Custom-Header"); got != "custom-value" {
		t.Errorf("X-Custom-Header = %q, want %q", got, "custom-value")
	}
	if got := receivedHeaders.Get("X-Tenant-ID"); got != "tenant-123" {
		t.Errorf("X-Tenant-ID = %q, want %q", got, "tenant-123")
	}
}

func TestNew_AppliesUserPathHeader(t *testing.T) {
	var receivedHeaders http.Header

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	provider := New(
		providers.ProviderConfig{APIKey: "test-key", BaseURL: server.URL},
		providers.ProviderOptions{
			UserPathHeader: "X-Tenant-Path",
		},
	)

	ctx := providers.WithPassthroughHeaders(context.Background(), http.Header{
		"X-Tenant-Path": {"tenant/42"},
		"X-User-Header": {"user-value"},
	})

	_, err := provider.ChatCompletion(ctx, &core.ChatRequest{
		Model:    "kimi-code-model",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("ChatCompletion() error = %v", err)
	}

	if receivedHeaders == nil {
		t.Fatal("receivedHeaders is nil; server did not receive a request")
	}
	if got := receivedHeaders.Get("X-Tenant-Path"); got != "" {
		t.Errorf("X-Tenant-Path = %q, want empty (blocked by user-path alias)", got)
	}
	if got := receivedHeaders.Get("X-User-Header"); got != "" {
		t.Errorf("X-User-Header = %q, want empty (passthrough disabled by default)", got)
	}
}
