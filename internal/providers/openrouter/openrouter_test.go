package openrouter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gomodel/internal/core"
	"gomodel/internal/llmclient"
	"gomodel/internal/providers"
)

func TestChatCompletion_AddsDefaultAttributionHeaders(t *testing.T) {
	var gotReferer string
	var gotTitle string
	var gotAuth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReferer = r.Header.Get("HTTP-Referer")
		gotTitle = r.Header.Get("X-OpenRouter-Title")
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-123",
			"object":"chat.completion",
			"created":1677652288,
			"model":"openai/gpt-4o-mini",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()

	provider := NewWithHTTPClient("test-api-key", server.Client(), llmclient.Hooks{})
	provider.SetBaseURL(server.URL)

	_, err := provider.ChatCompletion(context.Background(), &core.ChatRequest{
		Model: "openai/gpt-4o-mini",
		Messages: []core.Message{
			{Role: "user", Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotAuth != "Bearer test-api-key" {
		t.Fatalf("authorization = %q, want Bearer test-api-key", gotAuth)
	}
	if gotReferer != defaultSiteURL {
		t.Fatalf("HTTP-Referer = %q, want %q", gotReferer, defaultSiteURL)
	}
	if gotTitle != defaultAppName {
		t.Fatalf("X-OpenRouter-Title = %q, want %q", gotTitle, defaultAppName)
	}
}

func TestChatCompletion_UsesEnvOverridesForAttributionHeaders(t *testing.T) {
	t.Setenv("OPENROUTER_SITE_URL", "https://example.com")
	t.Setenv("OPENROUTER_APP_NAME", "Example App")

	var gotReferer string
	var gotTitle string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReferer = r.Header.Get("HTTP-Referer")
		gotTitle = r.Header.Get("X-OpenRouter-Title")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-123",
			"object":"chat.completion",
			"created":1677652288,
			"model":"openai/gpt-4o-mini",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()

	provider := NewWithHTTPClient("test-api-key", server.Client(), llmclient.Hooks{})
	provider.SetBaseURL(server.URL)

	_, err := provider.ChatCompletion(context.Background(), &core.ChatRequest{
		Model: "openai/gpt-4o-mini",
		Messages: []core.Message{
			{Role: "user", Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotReferer != "https://example.com" {
		t.Fatalf("HTTP-Referer = %q, want https://example.com", gotReferer)
	}
	if gotTitle != "Example App" {
		t.Fatalf("X-OpenRouter-Title = %q, want Example App", gotTitle)
	}
}

func TestPassthrough_AppliesIdentityDefaultHeaders(t *testing.T) {
	// DefaultHeaders are baseline identity headers seeded from env on every
	// request, including Passthrough. Caller headers supplied via
	// PassthroughRequest.Headers are forwarded alongside the defaults.
	// The X-Title alias is OpenRouter's own server-side concern; the gateway
	// does not suppress X-OpenRouter-Title based on caller-supplied X-Title.
	var gotReferer string
	var gotTitle string
	var gotLegacyTitle string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReferer = r.Header.Get("HTTP-Referer")
		gotTitle = r.Header.Get("X-OpenRouter-Title")
		gotLegacyTitle = r.Header.Get("X-Title")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer server.Close()

	provider := NewWithHTTPClient("test-api-key", server.Client(), llmclient.Hooks{})
	provider.SetBaseURL(server.URL)

	resp, err := provider.Passthrough(context.Background(), &core.PassthroughRequest{
		Method:   http.MethodPost,
		Endpoint: "responses",
		Body:     io.NopCloser(strings.NewReader(`{"model":"openai/gpt-4o-mini"}`)),
		Headers: http.Header{
			"Content-Type": {"application/json"},
			"X-Title":      {"Caller App"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if gotReferer != defaultSiteURL {
		t.Fatalf("HTTP-Referer = %q, want default %q", gotReferer, defaultSiteURL)
	}
	if gotLegacyTitle != "Caller App" {
		t.Fatalf("X-Title = %q, want Caller App", gotLegacyTitle)
	}
	if gotTitle != defaultAppName {
		t.Fatalf("X-OpenRouter-Title = %q, want default %q", gotTitle, defaultAppName)
	}
}

func TestChatCompletion_AppliesHeaderOverrides(t *testing.T) {
	var gotHeaders http.Header

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-123",
			"object":"chat.completion",
			"created":1677652288,
			"model":"openai/gpt-4o-mini",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()

	provider := New(providers.ProviderConfig{
		APIKey:  "test-api-key",
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
		Model: "openai/gpt-4o-mini",
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
		t.Fatalf("X-Tenant-Path = %q, want empty (user path header not forwarded)", got)
	}
	if got := gotHeaders.Get("X-User-Header"); got != "" {
		t.Fatalf("X-User-Header = %q, want empty (passthrough not enabled)", got)
	}
}

func TestChatCompletion_EnvIdentityHeadersApplied(t *testing.T) {
	t.Setenv("OPENROUTER_SITE_URL", "https://env-site.example")
	t.Setenv("OPENROUTER_APP_NAME", "Env App")

	var gotReferer string
	var gotTitle string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReferer = r.Header.Get("HTTP-Referer")
		gotTitle = r.Header.Get("X-OpenRouter-Title")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-123",
			"object":"chat.completion",
			"created":1677652288,
			"model":"openai/gpt-4o-mini",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()

	provider := New(providers.ProviderConfig{
		APIKey:  "test-api-key",
		BaseURL: server.URL,
	}, providers.ProviderOptions{})

	_, err := provider.ChatCompletion(context.Background(), &core.ChatRequest{
		Model: "openai/gpt-4o-mini",
		Messages: []core.Message{
			{Role: "user", Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("ChatCompletion() error = %v", err)
	}

	if gotReferer != "https://env-site.example" {
		t.Fatalf("HTTP-Referer = %q, want https://env-site.example", gotReferer)
	}
	if gotTitle != "Env App" {
		t.Fatalf("X-OpenRouter-Title = %q, want Env App", gotTitle)
	}
}

func TestChatCompletion_StaticHeaderOverridesEnvIdentity(t *testing.T) {
	t.Setenv("OPENROUTER_SITE_URL", "https://env-site.example")
	t.Setenv("OPENROUTER_APP_NAME", "Env App")

	var gotReferer string
	var gotTitle string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReferer = r.Header.Get("HTTP-Referer")
		gotTitle = r.Header.Get("X-OpenRouter-Title")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-123",
			"object":"chat.completion",
			"created":1677652288,
			"model":"openai/gpt-4o-mini",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()

	provider := New(providers.ProviderConfig{
		APIKey:  "test-api-key",
		BaseURL: server.URL,
	}, providers.ProviderOptions{
		HeaderOverrides: providers.HeaderOverridesConfig{
			CustomUpstreamHeaders: map[string]string{
				"HTTP-Referer":       "https://static-site.example",
				"X-OpenRouter-Title": "Static App",
			},
		},
	})

	_, err := provider.ChatCompletion(context.Background(), &core.ChatRequest{
		Model: "openai/gpt-4o-mini",
		Messages: []core.Message{
			{Role: "user", Content: "hi"},
		},
	})
	if err != nil {
		t.Fatalf("ChatCompletion() error = %v", err)
	}

	if gotReferer != "https://static-site.example" {
		t.Fatalf("HTTP-Referer = %q, want https://static-site.example (static overrides env)", gotReferer)
	}
	if gotTitle != "Static App" {
		t.Fatalf("X-OpenRouter-Title = %q, want Static App (static overrides env)", gotTitle)
	}
}

func TestPassthrough_CallerHeadersOverrideStatic(t *testing.T) {
	// Identity defaults from DefaultHeaders plus static overrides from
	// HeaderOverridesConfig, then caller passthrough headers. Caller wins
	// on overlap.
	var gotReferer string
	var gotTitle string
	var gotCustom string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReferer = r.Header.Get("HTTP-Referer")
		gotTitle = r.Header.Get("X-OpenRouter-Title")
		gotCustom = r.Header.Get("X-Custom-Header")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	provider := New(providers.ProviderConfig{
		APIKey:  "test-api-key",
		BaseURL: server.URL,
	}, providers.ProviderOptions{
		HeaderOverrides: providers.HeaderOverridesConfig{
			CustomUpstreamHeaders: map[string]string{
				"HTTP-Referer":       "https://static-site.example",
				"X-OpenRouter-Title": "Static App",
				"X-Custom-Header":    "static-value",
			},
			PassthroughUserHeaders: true,
			SkipMode:               "allow",
			SkipHeaders: []string{
				"HTTP-Referer",
				"X-OpenRouter-Title",
				"X-Custom-Header",
			},
		},
	})

	passthrough, ok := provider.(core.PassthroughProvider)
	if !ok {
		t.Fatal("openrouter provider does not implement core.PassthroughProvider")
	}

	callerHeaders := http.Header{
		"HTTP-Referer":       {"https://caller.example"},
		"X-OpenRouter-Title": {"Caller App"},
		"X-Custom-Header":    {"caller-value"},
	}
	ctx := providers.WithPassthroughHeaders(context.Background(), callerHeaders)

	resp, err := passthrough.Passthrough(ctx, &core.PassthroughRequest{
		Method:   http.MethodPost,
		Endpoint: "/chat/completions",
		Body:     io.NopCloser(strings.NewReader(`{"model":"openai/gpt-4o-mini"}`)),
		Headers: http.Header{
			"Content-Type":       {"application/json"},
			"HTTP-Referer":       {"https://caller.example"},
			"X-OpenRouter-Title": {"Caller App"},
			"X-Custom-Header":    {"caller-value"},
		},
	})
	if err != nil {
		t.Fatalf("Passthrough() error = %v", err)
	}
	defer resp.Body.Close()

	if gotReferer != "https://caller.example" {
		t.Fatalf("HTTP-Referer = %q, want https://caller.example (caller overrides static)", gotReferer)
	}
	if gotTitle != "Caller App" {
		t.Fatalf("X-OpenRouter-Title = %q, want Caller App (caller overrides static)", gotTitle)
	}
	if gotCustom != "caller-value" {
		t.Fatalf("X-Custom-Header = %q, want caller-value (caller overrides static)", gotCustom)
	}
}

func TestChatCompletion_PassthroughOverridesEnvIdentity(t *testing.T) {
	// When PassthroughUserHeaders is enabled with an allow list, caller
	// headers on the allow list win over env-seeded identity defaults
	// (HTTP-Referer / X-OpenRouter-Title). Headers absent from the allow
	// list must fall back to the env defaults.
	t.Setenv("OPENROUTER_SITE_URL", "https://env-site.example")
	t.Setenv("OPENROUTER_APP_NAME", "Env App")

	tests := []struct {
		name            string
		skipHeaders     []string
		callerHeaders   http.Header
		wantReferer     string
		wantTitle       string
		wantDescription string
	}{
		{
			name: "allowed caller headers override env defaults",
			skipHeaders: []string{
				"HTTP-Referer",
				"X-OpenRouter-Title",
			},
			callerHeaders: http.Header{
				"HTTP-Referer":       {"https://caller.example"},
				"X-OpenRouter-Title": {"Caller App"},
			},
			wantReferer:     "https://caller.example",
			wantTitle:       "Caller App",
			wantDescription: "allowed headers should win over env identity defaults",
		},
		{
			name: "header outside allow list falls back to env default",
			skipHeaders: []string{
				"X-OpenRouter-Title",
			},
			callerHeaders: http.Header{
				"HTTP-Referer":       {"https://caller.example"},
				"X-OpenRouter-Title": {"Caller App"},
			},
			wantReferer:     "https://env-site.example",
			wantTitle:       "Caller App",
			wantDescription: "non-allowed header must fall back to env default; allowed header still wins",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotReferer string
			var gotTitle string

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotReferer = r.Header.Get("HTTP-Referer")
				gotTitle = r.Header.Get("X-OpenRouter-Title")
				w.Header().Set("Content-Type", "application/json")
				body := `{"id":"chatcmpl-123","object":"chat.completion","created":1677652288,"model":"openai/gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()

			provider := New(providers.ProviderConfig{
				APIKey:  "test-api-key",
				BaseURL: server.URL,
			}, providers.ProviderOptions{
				HeaderOverrides: providers.HeaderOverridesConfig{
					PassthroughUserHeaders: true,
					SkipMode:               "allow",
					SkipHeaders:            tt.skipHeaders,
				},
			})

			ctx := providers.WithPassthroughHeaders(context.Background(), tt.callerHeaders)

			_, err := provider.ChatCompletion(ctx, &core.ChatRequest{
				Model: "openai/gpt-4o-mini",
				Messages: []core.Message{
					{Role: "user", Content: "hi"},
				},
			})
			if err != nil {
				t.Fatalf("ChatCompletion() error = %v", err)
			}

			if gotReferer != tt.wantReferer {
				t.Fatalf("HTTP-Referer = %q, want %q (%s)", gotReferer, tt.wantReferer, tt.wantDescription)
			}
			if gotTitle != tt.wantTitle {
				t.Fatalf("X-OpenRouter-Title = %q, want %q (%s)", gotTitle, tt.wantTitle, tt.wantDescription)
			}
		})
	}
}
