package openai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"gomodel/internal/core"
	"gomodel/internal/llmclient"
)

func TestCompatibleProvider_ListModels_ReturnsUpstreamOnSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4o","object":"model","owned_by":"openai"}]}`))
	}))
	defer server.Close()

	provider := NewCompatibleProviderWithHTTPClient(
		"test-key",
		server.Client(),
		llmclient.Hooks{},
		CompatibleProviderConfig{
			ProviderName: "upstream-only",
			BaseURL:      server.URL,
		},
	)

	resp, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(resp.Data) != 1 || resp.Data[0].ID != "gpt-4o" {
		t.Fatalf("unexpected models: %+v", resp.Data)
	}
}

func TestCompatibleProvider_ListModels_DefaultsMissingObjectFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"openrouter/model","object":"","owned_by":"openrouter"}]}`))
	}))
	defer server.Close()

	provider := NewCompatibleProviderWithHTTPClient(
		"test-key",
		server.Client(),
		llmclient.Hooks{},
		CompatibleProviderConfig{
			ProviderName: "openrouter",
			BaseURL:      server.URL,
		},
	)

	resp, err := provider.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if resp.Object != "list" {
		t.Fatalf("response object = %q, want list", resp.Object)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("model count = %d, want 1", len(resp.Data))
	}
	if resp.Data[0].Object != "model" {
		t.Fatalf("model object = %q, want model", resp.Data[0].Object)
	}
}

func TestCompatibleProvider_ListModels_ReturnsUpstreamError(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	provider := NewCompatibleProviderWithHTTPClient(
		"test-key",
		server.Client(),
		llmclient.Hooks{},
		CompatibleProviderConfig{
			ProviderName: "test-provider",
			BaseURL:      server.URL,
		},
	)

	_, err := provider.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected error when upstream fails, got nil")
	}
	gatewayErr, ok := err.(*core.GatewayError)
	if !ok {
		t.Fatalf("error type = %T, want *core.GatewayError", err)
	}
	if gatewayErr.Type != core.ErrorTypeProvider && gatewayErr.Type != core.ErrorTypeNotFound {
		t.Errorf("gatewayErr.Type = %q, want provider_error or not_found_error", gatewayErr.Type)
	}
}

// headerCaptureServer wraps an httptest.Server whose handler records the
// headers of the most recent inbound request. Tests inspect captures via the
// returned getter to keep assertions on the same goroutine that served.
func headerCaptureServer(t *testing.T) (*httptest.Server, func() http.Header) {
	t.Helper()
	var last http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	return server, func() http.Header { return last }
}

// snapshotContext builds a context carrying a RequestSnapshot populated with
// the supplied inbound headers, so the header applier observes them as if
// they had arrived at ingress.
func snapshotContext(t *testing.T, headers map[string][]string) context.Context {
	t.Helper()
	snapshot := core.NewRequestSnapshot(
		http.MethodPost,
		"/v1/test",
		nil,
		nil,
		headers,
		"",
		nil,
		false,
		"",
		nil,
	)
	return core.WithRequestSnapshot(context.Background(), snapshot)
}

func TestCompatibleProvider_PassthroughForwardsInboundHeaders(t *testing.T) {
	server, captured := headerCaptureServer(t)

	provider := NewCompatibleProviderWithHTTPClient(
		"test-key",
		server.Client(),
		llmclient.Hooks{},
		CompatibleProviderConfig{
			ProviderName:           "passthrough",
			BaseURL:                server.URL,
			PassthroughUserHeaders: true,
		},
	)

	ctx := snapshotContext(t, map[string][]string{
		"X-Trace-Id":  {"trace-abc"},
		"X-Tenant-Id": {"acme"},
	})

	var out any
	if err := provider.Do(ctx, llmclient.Request{
		Method:   http.MethodPost,
		Endpoint: "/test",
		Body:     map[string]any{"hello": "world"},
	}, &out); err != nil {
		t.Fatalf("Do() error = %v", err)
	}

	got := captured()
	if v := got.Get("X-Trace-Id"); v != "trace-abc" {
		t.Errorf("X-Trace-Id = %q, want trace-abc", v)
	}
	if v := got.Get("X-Tenant-Id"); v != "acme" {
		t.Errorf("X-Tenant-Id = %q, want acme", v)
	}
}

func TestCompatibleProvider_PassthroughDisabledIgnoresInboundHeaders(t *testing.T) {
	server, captured := headerCaptureServer(t)

	provider := NewCompatibleProviderWithHTTPClient(
		"test-key",
		server.Client(),
		llmclient.Hooks{},
		CompatibleProviderConfig{
			ProviderName:           "no-passthrough",
			BaseURL:                server.URL,
			PassthroughUserHeaders: false,
		},
	)

	ctx := snapshotContext(t, map[string][]string{
		"X-Tenant-Id": {"acme"},
	})

	var out any
	if err := provider.Do(ctx, llmclient.Request{
		Method:   http.MethodPost,
		Endpoint: "/test",
		Body:     map[string]any{"hello": "world"},
	}, &out); err != nil {
		t.Fatalf("Do() error = %v", err)
	}

	if v := captured().Get("X-Tenant-Id"); v != "" {
		t.Errorf("X-Tenant-Id = %q, want empty when passthrough disabled", v)
	}
}

func TestCompatibleProvider_CustomHeadersApplied(t *testing.T) {
	server, captured := headerCaptureServer(t)

	provider := NewCompatibleProviderWithHTTPClient(
		"test-key",
		server.Client(),
		llmclient.Hooks{},
		CompatibleProviderConfig{
			ProviderName: "custom-headers",
			BaseURL:      server.URL,
			CustomHeaders: map[string]string{
				"X-Org":        "acme",
				"X-Request-Id": "req-1",
			},
		},
	)

	var out any
	if err := provider.Do(context.Background(), llmclient.Request{
		Method:   http.MethodPost,
		Endpoint: "/test",
		Body:     map[string]any{"hello": "world"},
	}, &out); err != nil {
		t.Fatalf("Do() error = %v", err)
	}

	got := captured()
	if v := got.Get("X-Org"); v != "acme" {
		t.Errorf("X-Org = %q, want acme", v)
	}
	if v := got.Get("X-Request-Id"); v != "req-1" {
		t.Errorf("X-Request-Id = %q, want req-1", v)
	}
}

func TestCompatibleProvider_SetHeadersAuthPreservedWhenNoConflict(t *testing.T) {
	server, captured := headerCaptureServer(t)

	provider := NewCompatibleProviderWithHTTPClient(
		"secret-key",
		server.Client(),
		llmclient.Hooks{},
		CompatibleProviderConfig{
			ProviderName: "auth-only",
			BaseURL:      server.URL,
			SetHeaders: func(req *http.Request, apiKey string) {
				req.Header.Set("Authorization", "Bearer "+apiKey)
				req.Header.Set("X-Provider", "test")
			},
		},
	)

	var out any
	if err := provider.Do(context.Background(), llmclient.Request{
		Method:   http.MethodPost,
		Endpoint: "/test",
		Body:     map[string]any{"hello": "world"},
	}, &out); err != nil {
		t.Fatalf("Do() error = %v", err)
	}

	got := captured()
	if v := got.Get("Authorization"); v != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want Bearer secret-key", v)
	}
	if v := got.Get("X-Provider"); v != "test" {
		t.Errorf("X-Provider = %q, want test", v)
	}
}

func TestCompatibleProvider_CustomHeaderOverridesSetHeaders(t *testing.T) {
	server, captured := headerCaptureServer(t)

	provider := NewCompatibleProviderWithHTTPClient(
		"secret-key",
		server.Client(),
		llmclient.Hooks{},
		CompatibleProviderConfig{
			ProviderName: "custom-overrides-auth",
			BaseURL:      server.URL,
			SetHeaders: func(req *http.Request, apiKey string) {
				req.Header.Set("Authorization", "Bearer "+apiKey)
				req.Header.Set("X-Provider", "factory")
			},
			CustomHeaders: map[string]string{
				"X-Provider": "custom",
			},
		},
	)

	var out any
	if err := provider.Do(context.Background(), llmclient.Request{
		Method:   http.MethodPost,
		Endpoint: "/test",
		Body:     map[string]any{"hello": "world"},
	}, &out); err != nil {
		t.Fatalf("Do() error = %v", err)
	}

	got := captured()
	if v := got.Get("X-Provider"); v != "custom" {
		t.Errorf("X-Provider = %q, want custom", v)
	}
	if v := got.Get("Authorization"); v != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want Bearer secret-key (custom header is non-auth)", v)
	}
}

func TestCompatibleProvider_PassthroughWinsOverCustomHeaders(t *testing.T) {
	server, captured := headerCaptureServer(t)

	provider := NewCompatibleProviderWithHTTPClient(
		"test-key",
		server.Client(),
		llmclient.Hooks{},
		CompatibleProviderConfig{
			ProviderName: "precedence",
			BaseURL:      server.URL,
			CustomHeaders: map[string]string{
				"X-Trace-Id":  "from-custom",
				"X-Only-Cust": "custom-only",
			},
			PassthroughUserHeaders: true,
		},
	)

	ctx := snapshotContext(t, map[string][]string{
		"X-Trace-Id": {"from-pass"},
	})

	var out any
	if err := provider.Do(ctx, llmclient.Request{
		Method:   http.MethodPost,
		Endpoint: "/test",
		Body:     map[string]any{"hello": "world"},
	}, &out); err != nil {
		t.Fatalf("Do() error = %v", err)
	}

	got := captured()
	if v := got.Get("X-Trace-Id"); v != "from-pass" {
		t.Errorf("X-Trace-Id = %q, want from-pass (passthrough wins)", v)
	}
	if v := got.Get("X-Only-Cust"); v != "custom-only" {
		t.Errorf("X-Only-Cust = %q, want custom-only (passthrough has no entry)", v)
	}
}

func TestCompatibleProvider_PassthroughSkipsHopByHopAndAuthHeaders(t *testing.T) {
	server, captured := headerCaptureServer(t)

	provider := NewCompatibleProviderWithHTTPClient(
		"secret-key",
		server.Client(),
		llmclient.Hooks{},
		CompatibleProviderConfig{
			ProviderName: "skip",
			BaseURL:      server.URL,
			SetHeaders: func(req *http.Request, apiKey string) {
				req.Header.Set("Authorization", "Bearer "+apiKey)
				req.Header.Set("X-Api-Key", apiKey)
			},
			PassthroughUserHeaders: true,
		},
	)

	ctx := snapshotContext(t, map[string][]string{
		"Authorization":   {"Bearer inbound-attacker"},
		"X-Api-Key":       {"inbound-key"},
		"Cookie":          {"session=abc"},
		"Connection":      {"close"},
		"Host":            {"inbound.example"},
		"Content-Length":  {"9999"},
		"X-Forwarded-For": {"1.2.3.4"},
		"X-Tenant-Id":     {"acme"},
	})

	var out any
	if err := provider.Do(ctx, llmclient.Request{
		Method:   http.MethodPost,
		Endpoint: "/test",
		Body:     map[string]any{"hello": "world"},
	}, &out); err != nil {
		t.Fatalf("Do() error = %v", err)
	}

	got := captured()
	if v := got.Get("Authorization"); v != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want Bearer secret-key (factory value, not passthrough)", v)
	}
	if v := got.Get("X-Api-Key"); v != "secret-key" {
		t.Errorf("X-Api-Key = %q, want secret-key (factory value, not passthrough)", v)
	}
	if v := got.Get("Cookie"); v != "" {
		t.Errorf("Cookie = %q, want empty (skipped passthrough)", v)
	}
	if v := got.Get("Connection"); v != "" {
		t.Errorf("Connection = %q, want empty (skipped passthrough)", v)
	}
	if v := got.Get("X-Forwarded-For"); v != "" {
		t.Errorf("X-Forwarded-For = %q, want empty (skipped passthrough)", v)
	}
	if v := got.Get("X-Tenant-Id"); v != "acme" {
		t.Errorf("X-Tenant-Id = %q, want acme (allowed passthrough)", v)
	}
}