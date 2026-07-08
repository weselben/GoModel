package deepseek

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"gomodel/internal/core"
	"gomodel/internal/providers"
)

func TestChatCompletion_AppliesHeaderOverrides(t *testing.T) {
	var gotHeaders http.Header

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-deepseek",
			"created":1677652288,
			"model":"deepseek-v4-pro",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()

	provider := New(providers.ProviderConfig{
		APIKey:  "deepseek-key",
		BaseURL: server.URL,
	}, providers.ProviderOptions{
		HeaderOverrides: providers.HeaderOverridesConfig{
			CustomUpstreamHeaders: map[string]string{
				"X-Custom-Header": "custom-value",
			},
		},
		UserPathHeader: "X-Tenant-Path",
	})

	ctx := providers.WithPassthroughHeaders(context.Background(), http.Header{
		"X-Tenant-Path": {"tenant/42"},
		"X-User-Header": {"user-value"},
	})

	_, err := provider.ChatCompletion(ctx, &core.ChatRequest{
		Model: "deepseek-v4-pro",
		Messages: []core.Message{
			{Role: "user", Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("ChatCompletion() error = %v", err)
	}

	if got := gotHeaders.Get("X-Custom-Header"); got != "custom-value" {
		t.Fatalf("X-Custom-Header = %q, want custom-value", got)
	}
	if got := gotHeaders.Get("X-Tenant-Path"); got != "" {
		t.Fatalf("X-Tenant-Path = %q, want empty", got)
	}
	if got := gotHeaders.Get("X-User-Header"); got != "" {
		t.Fatalf("X-User-Header = %q, want empty", got)
	}
}

func TestPassthrough_AppliesHeaderOverrides(t *testing.T) {
	var gotHeaders http.Header

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	provider := New(providers.ProviderConfig{
		APIKey:  "deepseek-key",
		BaseURL: server.URL,
	}, providers.ProviderOptions{
		HeaderOverrides: providers.HeaderOverridesConfig{
			CustomUpstreamHeaders: map[string]string{
				"X-Custom-Header": "custom-value",
			},
			PassthroughUserHeaders: true,
			SkipMode:               "allow",
			SkipHeaders:            []string{"X-User-Header"},
		},
		UserPathHeader: "X-Tenant-Path",
	})

	ctx := providers.WithPassthroughHeaders(context.Background(), http.Header{
		"X-Tenant-Path": {"tenant/42"},
		"X-User-Header": {"user-value"},
	})

	passthrough, ok := provider.(core.PassthroughProvider)
	if !ok {
		t.Fatal("deepseek provider does not implement core.PassthroughProvider")
	}
	resp, err := passthrough.Passthrough(ctx, &core.PassthroughRequest{
		Method:   http.MethodPost,
		Endpoint: "/beta/completions",
		Body:     http.NoBody,
	})
	if err != nil {
		t.Fatalf("Passthrough() error = %v", err)
	}
	defer resp.Body.Close()

	if got := gotHeaders.Get("X-Custom-Header"); got != "custom-value" {
		t.Fatalf("X-Custom-Header = %q, want custom-value", got)
	}
	if got := gotHeaders.Get("X-Tenant-Path"); got != "" {
		t.Fatalf("X-Tenant-Path = %q, want empty", got)
	}
	if got := gotHeaders.Get("X-User-Header"); got != "user-value" {
		t.Fatalf("X-User-Header = %q, want user-value", got)
	}
}
