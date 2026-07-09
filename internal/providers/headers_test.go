package providers

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gomodel/internal/core"
)

func TestIsHeaderBlocked(t *testing.T) {
	tests := []struct {
		name          string
		header        string
		userPathAlias string
		want          bool
	}{
		{name: "authorization credential", header: "Authorization", want: true},
		{name: "lowercase authorization", header: "authorization", want: true},
		{name: "x-api-key credential", header: "X-Api-Key", want: true},
		{name: "cookie credential", header: "Cookie", want: true},
		{name: "set-cookie credential", header: "Set-Cookie", want: true},
		{name: "x-gomodel-key credential", header: "X-GoModel-Key", want: true},
		{name: "x-gomodel-user-path internal", header: "X-GoModel-User-Path", want: true},
		{name: "user path alias blocks header", header: "X-My-User-Path", userPathAlias: "X-My-User-Path", want: true},
		{name: "user path alias case-insensitive", header: "x-my-user-path", userPathAlias: "X-My-User-Path", want: true},
		{name: "user path alias whitespace trimmed", header: "  X-My-User-Path  ", userPathAlias: "X-My-User-Path", want: true},
		{name: "alias configured but header different", header: "X-Some-Other", userPathAlias: "X-My-User-Path", want: false},
		{name: "empty alias does not block extras", header: "X-Custom", userPathAlias: "", want: false},
		{name: "non-credential header allowed", header: "X-Custom-Header", want: false},
		{name: "content-type allowed", header: "Content-Type", want: false},
		{name: "accept allowed", header: "Accept", want: false},
		{name: "user-agent allowed", header: "User-Agent", want: false},
		{name: "empty header name", header: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsHeaderBlocked(tt.header, tt.userPathAlias)
			if got != tt.want {
				t.Errorf("IsHeaderBlocked(%q, %q) = %v, want %v", tt.header, tt.userPathAlias, got, tt.want)
			}
		})
	}
}

func TestIsHeaderBlocked_TransportHeadersBlocked(t *testing.T) {
	transport := []string{
		"Accept-Encoding",
		"Connection",
		"Keep-Alive",
		"Te",
		"Transfer-Encoding",
		"Trailer",
		"Upgrade",
		"Proxy-Authorization",
		"Content-Length",
		"Host",
	}
	for _, name := range transport {
		name := name
		t.Run(name, func(t *testing.T) {
			if !IsHeaderBlocked(name, "") {
				t.Errorf("IsHeaderBlocked(%q) = false, want true", name)
			}
		})
	}
}

func TestFilterIncomingHeaders_CopySemantics(t *testing.T) {
	userPathHeader := http.CanonicalHeaderKey("X-GoModel-User-Path")
	src := http.Header{
		"Authorization": {"Bearer secret"},
		"X-Api-Key":     {"k"},
		userPathHeader:  {"/v1/x"},
		"Content-Type":  {"application/json"},
		"X-Custom":      {"value"},
	}

	filtered := FilterIncomingHeaders(src, "")

	// Original must not be mutated.
	if got := src.Get("Authorization"); got != "Bearer secret" {
		t.Errorf("original Authorization mutated: %q", got)
	}
	if _, ok := src["X-Api-Key"]; !ok {
		t.Error("original X-Api-Key removed unexpectedly")
	}

	// Filtered must drop credentials and internal headers.
	if got := filtered.Get("Authorization"); got != "" {
		t.Errorf("filtered Authorization should be empty, got %q", got)
	}
	if _, ok := filtered["X-Api-Key"]; ok {
		t.Error("filtered X-Api-Key should be dropped")
	}
	if _, ok := filtered[userPathHeader]; ok {
		t.Error("filtered X-GoModel-User-Path should be dropped")
	}

	// Filtered must keep non-credential headers.
	if got := filtered.Get("Content-Type"); got != "application/json" {
		t.Errorf("filtered Content-Type = %q, want application/json", got)
	}
	if got := filtered.Get("X-Custom"); got != "value" {
		t.Errorf("filtered X-Custom = %q, want value", got)
	}

	// Filtered must be a new map (not the same underlying reference).
	if len(filtered) == 0 {
		t.Error("expected non-empty filtered result")
	}
	filtered.Set("Content-Type", "mutated")
	if src.Get("Content-Type") == "mutated" {
		t.Error("mutation of filtered leaked back to source")
	}
}

func TestFilterIncomingHeaders_UserPathAlias(t *testing.T) {
	src := http.Header{
		"X-My-Alias": {"v"},
		"X-Custom":   {"keep"},
	}
	filtered := FilterIncomingHeaders(src, "X-My-Alias")
	if _, ok := filtered["X-My-Alias"]; ok {
		t.Error("alias header should be filtered out")
	}
	if filtered.Get("X-Custom") != "keep" {
		t.Error("non-alias header should be kept")
	}
}

func TestApplyHeaderOverrides_StaticMode(t *testing.T) {
	tests := []struct {
		name          string
		cfg           HeaderOverridesConfig
		userPathAlias string
		wantHeaders   map[string]string
		wantMissing   []string
	}{
		{
			name: "applies static custom headers",
			cfg: HeaderOverridesConfig{
				CustomUpstreamHeaders: map[string]string{
					"X-Provider-Region": "us-east-1",
					"X-Trace-Id":        "abc-123",
				},
			},
			wantHeaders: map[string]string{
				"X-Provider-Region": "us-east-1",
				"X-Trace-Id":        "abc-123",
			},
		},
		{
			name: "skips blocked credential header in static mode",
			cfg: HeaderOverridesConfig{
				CustomUpstreamHeaders: map[string]string{
					"Authorization": "Bearer leaked",
					"X-Api-Key":     "leaked",
					"X-Trace-Id":    "abc-123",
				},
			},
			wantHeaders: map[string]string{
				"X-Trace-Id": "abc-123",
			},
			wantMissing: []string{"Authorization", "X-Api-Key"},
		},
		{
			name: "skips blocked transport header in static mode",
			cfg: HeaderOverridesConfig{
				CustomUpstreamHeaders: map[string]string{
					"Content-Length": "4096",
					"X-Trace-Id":     "abc-123",
				},
			},
			wantHeaders: map[string]string{
				"X-Trace-Id": "abc-123",
			},
			wantMissing: []string{"Content-Length"},
		},
		{
			name: "skips blocked user path alias in static mode",
			cfg: HeaderOverridesConfig{
				CustomUpstreamHeaders: map[string]string{
					"X-My-User-Path": "/override",
					"X-Trace-Id":     "abc-123",
				},
			},
			userPathAlias: "X-My-User-Path",
			wantHeaders: map[string]string{
				"X-Trace-Id": "abc-123",
			},
			wantMissing: []string{"X-My-User-Path"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "http://example.com", nil)
			ApplyHeaderOverrides(req, tt.cfg, tt.userPathAlias)
			for key, want := range tt.wantHeaders {
				if got := req.Header.Get(key); got != want {
					t.Errorf("header %q = %q, want %q", key, got, want)
				}
			}
			for _, key := range tt.wantMissing {
				if got := req.Header.Get(key); got != "" {
					t.Errorf("header %q should be missing, got %q", key, got)
				}
			}
		})
	}
}

func TestApplyHeaderOverrides_PassthroughMode_ComposesStaticAndPassthrough(t *testing.T) {
	src := http.Header{
		"X-Tenant": {"acme"},
		"X-Team":   {"payments"},
	}

	req, _ := http.NewRequest(http.MethodPost, "http://example.com", nil)
	ctx := WithPassthroughHeaders(req.Context(), src)
	req = req.WithContext(ctx)

	cfg := HeaderOverridesConfig{
		PassthroughUserHeaders: true,
		SkipMode:               "allow",
		SkipHeaders:            []string{"X-Tenant", "X-Team"},
		CustomUpstreamHeaders: map[string]string{
			"X-Provider-Region": "us-east-1",
			"X-Trace-Id":        "static-123",
			"X-Tenant":          "static-tenant", // should be overridden by passthrough
		},
	}
	ApplyHeaderOverrides(req, cfg, "")

	if got := req.Header.Get("X-Provider-Region"); got != "us-east-1" {
		t.Errorf("X-Provider-Region = %q, want %q", got, "us-east-1")
	}
	if got := req.Header.Get("X-Trace-Id"); got != "static-123" {
		t.Errorf("X-Trace-Id = %q, want %q", got, "static-123")
	}
	if got := req.Header.Get("X-Tenant"); got != "acme" {
		t.Errorf("X-Tenant = %q, want %q", got, "acme")
	}
	if got := req.Header.Get("X-Team"); got != "payments" {
		t.Errorf("X-Team = %q, want %q", got, "payments")
	}
}

// TestApplyHeaderOverrides_PassthroughMode_DefaultOpenSkipList verifies that
// when PassthroughUserHeaders is true, skip_mode is "skip", and skip_headers is
// empty, all non-blocked headers are forwarded (default-open behavior).
func TestApplyHeaderOverrides_PassthroughMode_DefaultOpenSkipList(t *testing.T) {
	src := http.Header{
		"X-Tenant":   {"acme"},
		"X-Team":     {"payments"},
		"X-Custom":   {"value"},
		"User-Agent": {"tester"},
	}

	req, _ := http.NewRequest(http.MethodPost, "http://example.com", nil)
	ctx := WithPassthroughHeaders(req.Context(), src)
	req = req.WithContext(ctx)

	cfg := HeaderOverridesConfig{
		PassthroughUserHeaders: true,
		SkipMode:               "skip",
		SkipHeaders:            []string{},
	}
	ApplyHeaderOverrides(req, cfg, "")

	// Skip mode with empty skip list forwards all non-blocked headers (default-open).
	for _, name := range []string{"X-Tenant", "X-Team", "X-Custom", "User-Agent"} {
		if got := req.Header.Get(name); got == "" {
			t.Errorf("header %q should be forwarded in default-open skip mode, got empty", name)
		}
	}
}

func TestApplyHeaderOverrides_PassthroughMode_SkipList(t *testing.T) {
	src := http.Header{
		"X-Tenant":   {"acme"},
		"X-Team":     {"payments"},
		"X-Custom":   {"value"},
		"User-Agent": {"tester"},
	}

	req, _ := http.NewRequest(http.MethodPost, "http://example.com", nil)
	ctx := WithPassthroughHeaders(req.Context(), src)
	req = req.WithContext(ctx)

	cfg := HeaderOverridesConfig{
		PassthroughUserHeaders: true,
		SkipMode:               "skip",
		SkipHeaders:            []string{"X-Team"},
	}
	ApplyHeaderOverrides(req, cfg, "")

	if got := req.Header.Get("X-Tenant"); got != "acme" {
		t.Errorf("X-Tenant = %q, want %q", got, "acme")
	}
	if got := req.Header.Get("X-Custom"); got != "value" {
		t.Errorf("X-Custom = %q, want %q", got, "value")
	}
	if got := req.Header.Get("User-Agent"); got != "tester" {
		t.Errorf("User-Agent = %q, want %q", got, "tester")
	}
	if got := req.Header.Get("X-Team"); got != "" {
		t.Errorf("X-Team should be skipped, got %q", got)
	}
}

func TestApplyHeaderOverrides_PassthroughMode_AllowList(t *testing.T) {
	src := http.Header{
		"X-Tenant":   {"acme"},
		"X-Team":     {"payments"},
		"X-Custom":   {"value"},
		"User-Agent": {"tester"},
	}

	req, _ := http.NewRequest(http.MethodPost, "http://example.com", nil)
	ctx := WithPassthroughHeaders(req.Context(), src)
	req = req.WithContext(ctx)

	cfg := HeaderOverridesConfig{
		PassthroughUserHeaders: true,
		SkipMode:               "allow",
		SkipHeaders:            []string{"X-Tenant", "X-Allowed"},
	}
	ApplyHeaderOverrides(req, cfg, "")

	if got := req.Header.Get("X-Tenant"); got != "acme" {
		t.Errorf("X-Tenant = %q, want %q", got, "acme")
	}
	for _, missing := range []string{"X-Team", "X-Custom", "User-Agent"} {
		if got := req.Header.Get(missing); got != "" {
			t.Errorf("%s must be blocked in allow mode, got %q", missing, got)
		}
	}
}

func TestApplyHeaderOverrides_PassthroughMode_BlockedHeadersNeverForwarded(t *testing.T) {
	src := http.Header{
		"Authorization":       {"Bearer user"},
		"X-Api-Key":           {"k"},
		"X-GoModel-Key":       {"mgmt"},
		"Cookie":              {"session=secret"},
		"X-GoModel-User-Path": {"/v1/x"},
		"X-Tenant":            {"acme"},
	}

	req, _ := http.NewRequest(http.MethodPost, "http://example.com", nil)
	ctx := WithPassthroughHeaders(req.Context(), src)
	req = req.WithContext(ctx)

	cfg := HeaderOverridesConfig{
		PassthroughUserHeaders: true,
		SkipHeaders: []string{
			"Authorization",
			"X-Api-Key",
			"X-GoModel-Key",
			"Cookie",
			"X-GoModel-User-Path",
			"X-Tenant",
		},
		SkipMode: "allow",
	}
	ApplyHeaderOverrides(req, cfg, "")

	if got := req.Header.Get("X-Tenant"); got != "acme" {
		t.Errorf("X-Tenant = %q, want %q", got, "acme")
	}
	for _, blocked := range []string{"Authorization", "X-Api-Key", "X-GoModel-Key", "Cookie", "X-GoModel-User-Path"} {
		if got := req.Header.Get(blocked); got != "" {
			t.Errorf("blocked header %q leaked through passthrough allow list: %q", blocked, got)
		}
	}
}

func TestApplyHeaderOverrides_PassthroughMode_TaggingStripHeaders(t *testing.T) {
	strip := map[string]struct{}{
		"x-tenant-id": {},
		"x-label":     {},
	}

	src := http.Header{
		"X-Tenant-Id":  {"acme"},
		"X-Label":      {"priority"},
		"X-Request-Id": {"r-1"},
	}

	req, _ := http.NewRequest(http.MethodPost, "http://example.com", nil)
	ctx := req.Context()
	ctx = WithPassthroughHeaders(ctx, src)
	ctx = core.WithTaggingStripHeaders(ctx, strip)
	req = req.WithContext(ctx)

	cfg := HeaderOverridesConfig{
		PassthroughUserHeaders: true,
		SkipMode:               "allow",
		SkipHeaders:            []string{"X-Request-Id"},
	}
	ApplyHeaderOverrides(req, cfg, "")

	for _, blocked := range []string{"X-Tenant-Id", "X-Label"} {
		if got := req.Header.Get(blocked); got != "" {
			t.Errorf("tagging strip header %q leaked through: %q", blocked, got)
		}
	}
	if got := req.Header.Get("X-Request-Id"); got != "r-1" {
		t.Errorf("X-Request-Id = %q, want %q", got, "r-1")
	}
}

// capturingSlogHandler is a minimal slog.Handler used by the passthrough
// logging test to assert on emitted log records without coupling to the
// global slog default.
type capturingSlogHandler struct {
	records []slogRecord
}

type slogRecord struct {
	level slog.Level
	msg   string
	attrs map[string]any
}

func (h *capturingSlogHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *capturingSlogHandler) Handle(_ context.Context, r slog.Record) error {
	rec := slogRecord{level: r.Level, msg: r.Message, attrs: make(map[string]any)}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.Any()
		return true
	})
	h.records = append(h.records, rec)
	return nil
}
func (h *capturingSlogHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *capturingSlogHandler) WithGroup(_ string) slog.Handler      { return h }

// TestApplyHeaderOverrides_PassthroughMode_LogsOverlappingStaticHeaders verifies
// the warning log emitted when both PassthroughUserHeaders and CustomUpstreamHeaders
// are configured and at least one name overlaps. The test installs a dedicated
// capturing slog.Handler and asserts the message lands at warn level with the
// expected overlapping names (CodeRabbit 3525742919).
func TestApplyHeaderOverrides_PassthroughMode_LogsOverlappingStaticHeaders(t *testing.T) {
	handler := &capturingSlogHandler{}
	logger := slog.New(handler)
	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })

	req, _ := http.NewRequest(http.MethodPost, "http://example.com", nil)
	ctx := WithPassthroughHeaders(req.Context(), http.Header{
		"X-Tenant": {"acme"},
		"X-Team":   {"payments"},
	})
	req = req.WithContext(ctx)

	cfg := HeaderOverridesConfig{
		PassthroughUserHeaders: true,
		SkipMode:               "allow",
		SkipHeaders:            []string{"X-Tenant", "X-Team"},
		CustomUpstreamHeaders: map[string]string{
			"X-Provider-Region": "us-east-1",
			"X-Trace-Id":        "static-123",
			"X-Tenant":          "static-tenant", // overlaps with passthrough
		},
	}
	ApplyHeaderOverrides(req, cfg, "")

	var found *slogRecord
	for i := range handler.records {
		if handler.records[i].level == slog.LevelWarn &&
			strings.Contains(handler.records[i].msg, "overlap") {
			found = &handler.records[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected warn log about overlapping custom/passthrough headers, got records: %+v", handler.records)
	}
	overlap, ok := found.attrs["overlapping_names"].([]string)
	if !ok {
		t.Fatalf("expected overlapping_names attribute, got %T: %+v", found.attrs["overlapping_names"], found.attrs)
	}
	if len(overlap) != 1 || overlap[0] != "X-Tenant" {
		t.Errorf("expected overlapping_names = [X-Tenant], got %v", overlap)
	}

	// Sanity: passthrough still applied, overlapping value won.
	if got := req.Header.Get("X-Tenant"); got != "acme" {
		t.Errorf("passthrough header X-Tenant not applied: %q", got)
	}
	if got := req.Header.Get("X-Provider-Region"); got != "us-east-1" {
		t.Errorf("static header X-Provider-Region not applied: %q", got)
	}
}

// TestApplyHeaderOverrides_PassthroughDisabled_SkipsUserHeaders verifies that when
// PassthroughUserHeaders is false, skip_headers and skip_mode are ignored and no
// user headers are forwarded regardless of whether the skip/allow list is populated.
func TestApplyHeaderOverrides_PassthroughDisabled_SkipsUserHeaders(t *testing.T) {
	src := http.Header{
		"X-Tenant":   {"acme"},
		"X-Team":     {"payments"},
		"X-Custom":   {"value"},
		"User-Agent": {"tester"},
	}

	req, _ := http.NewRequest(http.MethodPost, "http://example.com", nil)
	ctx := WithPassthroughHeaders(req.Context(), src)
	req = req.WithContext(ctx)

	// PassthroughUserHeaders is false but skip list is populated with "allow" mode
	// that would forward everything if passthrough were active.
	cfg := HeaderOverridesConfig{
		PassthroughUserHeaders: false,
		SkipMode:               "allow",
		SkipHeaders:            []string{"X-Tenant", "X-Team"},
	}
	ApplyHeaderOverrides(req, cfg, "")

	// No user headers should be forwarded since PassthroughUserHeaders is false.
	for _, name := range []string{"X-Tenant", "X-Team", "X-Custom", "User-Agent"} {
		if got := req.Header.Get(name); got != "" {
			t.Errorf("header %q should not be forwarded when PassthroughUserHeaders=false, got %q", name, got)
		}
	}
}

// TestApplyHeaderOverrides_PassthroughDisabled_SkipsUserHeaders_EmptyList verifies
// that skip_headers and skip_mode are ignored even when the skip list is empty.
func TestApplyHeaderOverrides_PassthroughDisabled_SkipsUserHeaders_EmptyList(t *testing.T) {
	src := http.Header{
		"X-Tenant": {"acme"},
		"X-Team":   {"payments"},
	}

	req, _ := http.NewRequest(http.MethodPost, "http://example.com", nil)
	ctx := WithPassthroughHeaders(req.Context(), src)
	req = req.WithContext(ctx)

	cfg := HeaderOverridesConfig{
		PassthroughUserHeaders: false,
		SkipMode:               "skip",
		SkipHeaders:            []string{},
	}
	ApplyHeaderOverrides(req, cfg, "")

	for _, name := range []string{"X-Tenant", "X-Team"} {
		if got := req.Header.Get(name); got != "" {
			t.Errorf("header %q should not be forwarded when PassthroughUserHeaders=false, got %q", name, got)
		}
	}
}

// TestApplyHeaderOverrides_CustomHeadersAppliedWhenPassthroughDisabled verifies
// that custom_upstream_headers are applied even when PassthroughUserHeaders is false.
func TestApplyHeaderOverrides_CustomHeadersAppliedWhenPassthroughDisabled(t *testing.T) {
	src := http.Header{
		"X-Tenant": {"acme"},
		"X-Team":   {"payments"},
	}

	req, _ := http.NewRequest(http.MethodPost, "http://example.com", nil)
	ctx := WithPassthroughHeaders(req.Context(), src)
	req = req.WithContext(ctx)

	cfg := HeaderOverridesConfig{
		PassthroughUserHeaders: false,
		CustomUpstreamHeaders: map[string]string{
			"X-Provider-Region": "us-east-1",
			"X-Trace-Id":        "static-123",
		},
	}
	ApplyHeaderOverrides(req, cfg, "")

	// Custom headers must be applied even though passthrough is disabled.
	if got := req.Header.Get("X-Provider-Region"); got != "us-east-1" {
		t.Errorf("X-Provider-Region = %q, want %q", got, "us-east-1")
	}
	if got := req.Header.Get("X-Trace-Id"); got != "static-123" {
		t.Errorf("X-Trace-Id = %q, want %q", got, "static-123")
	}

	// User headers must not be forwarded.
	for _, name := range []string{"X-Tenant", "X-Team"} {
		if got := req.Header.Get(name); got != "" {
			t.Errorf("header %q should not be forwarded when PassthroughUserHeaders=false, got %q", name, got)
		}
	}
}

// TestApplyHeaderOverrides_PassthroughEnabled_AllowModeEmptyList verifies that
// when PassthroughUserHeaders is true, skip_mode is "allow", and skip_headers is
// empty, no headers are forwarded because the allow list contains nothing.
func TestApplyHeaderOverrides_PassthroughEnabled_AllowModeEmptyList(t *testing.T) {
	src := http.Header{
		"X-Tenant":   {"acme"},
		"X-Team":     {"payments"},
		"X-Custom":   {"value"},
		"User-Agent": {"tester"},
	}

	req, _ := http.NewRequest(http.MethodPost, "http://example.com", nil)
	ctx := WithPassthroughHeaders(req.Context(), src)
	req = req.WithContext(ctx)

	cfg := HeaderOverridesConfig{
		PassthroughUserHeaders: true,
		SkipMode:               "allow",
		SkipHeaders:            []string{},
	}
	ApplyHeaderOverrides(req, cfg, "")

	// Allow mode with empty list forwards nothing.
	for _, name := range []string{"X-Tenant", "X-Team", "X-Custom", "User-Agent"} {
		if got := req.Header.Get(name); got != "" {
			t.Errorf("header %q should not be forwarded in allow mode with empty list, got %q", name, got)
		}
	}
}

func TestPassthroughHeadersFromContext_EdgeCases(t *testing.T) {
	ctx := context.Background()

	if got := PassthroughHeadersFromContext(ctx); got != nil {
		t.Errorf("PassthroughHeadersFromContext(empty ctx) = %v, want nil", got)
	}

	ctx = WithPassthroughHeaders(ctx, http.Header{"X-Test": []string{"value"}})
	got := PassthroughHeadersFromContext(ctx)
	if got == nil || got.Get("X-Test") == "" {
		t.Errorf("PassthroughHeadersFromContext(with headers) missing X-Test")
	}

	ctx2 := WithPassthroughHeaders(context.Background(), http.Header{})
	if got2 := PassthroughHeadersFromContext(ctx2); got2 == nil {
		t.Error("PassthroughHeadersFromContext(empty headers) should return non-nil header")
	}
}

func TestSourceHasHeader_Variants(t *testing.T) {
	tests := []struct {
		name   string
		source http.Header
		header string
		want   bool
	}{
		{name: "header present", source: http.Header{"X-Test": []string{"val"}}, header: "X-Test", want: true},
		{name: "header missing", source: http.Header{"X-Other": []string{"val"}}, header: "X-Test", want: false},
		{name: "empty source", source: http.Header{}, header: "X-Test", want: false},
		{name: "nil source", source: nil, header: "X-Test", want: false},
		{name: "case insensitive present", source: http.Header{"x-test": []string{"val"}}, header: "X-Test", want: true},
		{name: "case insensitive missing", source: http.Header{"X-Other": []string{"val"}}, header: "x-test", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sourceHasHeader(tt.source, tt.header); got != tt.want {
				t.Errorf("sourceHasHeader(%q, %q) = %v, want %v", tt.source, tt.header, got, tt.want)
			}
		})
	}
}

func TestLogStaticOverridden_Overlap(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	slog.SetDefault(logger)

	req := httptest.NewRequest("POST", "/", nil)
	cfg := HeaderOverridesConfig{
		CustomUpstreamHeaders:  map[string]string{"X-Custom": "static"},
		PassthroughUserHeaders: true,
	}
	req = req.WithContext(WithPassthroughHeaders(req.Context(), http.Header{"X-Custom": []string{"passthrough"}}))

	logStaticOverridden(req, cfg)
	if buf.String() == "" {
		t.Error("logStaticOverridden should produce log output on overlap")
	}
}

func TestLogStaticOverridden_Overlap_EmptyCase(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	slog.SetDefault(logger)

	req := httptest.NewRequest("POST", "/", nil)
	cfg := HeaderOverridesConfig{
		CustomUpstreamHeaders:  map[string]string{"X-Static": "val"},
		PassthroughUserHeaders: true,
	}
	req = req.WithContext(WithPassthroughHeaders(req.Context(), http.Header{"X-Pass": []string{"val"}}))

	logStaticOverridden(req, cfg)
	if buf.String() != "" {
		t.Errorf("logStaticOverridden without overlap should not log, got %q", buf.String())
	}
}

func TestApplyPassthroughHeaders_Variants(t *testing.T) {
	tests := []struct {
		name            string
		ctx             context.Context
		config          HeaderOverridesConfig
		userPathAlias   string
		wantPassthrough bool
	}{
		{name: "no context", ctx: context.Background(), config: HeaderOverridesConfig{PassthroughUserHeaders: true}, wantPassthrough: false},
		{name: "disabled by config", ctx: context.Background(), config: HeaderOverridesConfig{PassthroughUserHeaders: false}, wantPassthrough: false},
		{name: "enabled with headers", ctx: WithPassthroughHeaders(context.Background(), http.Header{"X-Test": []string{"val"}}), config: HeaderOverridesConfig{PassthroughUserHeaders: true}, wantPassthrough: true},
		{name: "enabled but headers empty", ctx: WithPassthroughHeaders(context.Background(), http.Header{}), config: HeaderOverridesConfig{PassthroughUserHeaders: true}, wantPassthrough: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/", nil)
			req = req.WithContext(tt.ctx)

			initialLen := len(req.Header)
			applyPassthroughHeaders(req, tt.config, tt.userPathAlias)

			hasPassthrough := len(req.Header) > initialLen && req.Header.Get("X-Test") != ""
			if hasPassthrough != tt.wantPassthrough {
				t.Errorf("applyPassthroughHeaders passthrough presence = %v, want %v", hasPassthrough, tt.wantPassthrough)
			}
		})
	}
}

// TestApplyHeaderOverrides_DefaultStaticPassthroughPrecedence is the canonical,
// provider-agnostic table-driven test that documents the four-layer precedence
// of ApplyHeaderOverrides once for the entire codebase:
//
//  1. defaults:        DefaultHeaders seed baseline values.
//  2. static:          CustomUpstreamHeaders override default values.
//  3. passthrough:     when permitted, passthrough user headers override static.
//  4. blocked/skip:    IsHeaderBlocked skips defaults and statics; shouldForward
//     additionally filters passthrough via SkipHeaders/SkipMode.
//
// Each case asserts the final header value (or absence) for every named header
// so the precedence rules are visible in one place.
func TestApplyHeaderOverrides_DefaultStaticPassthroughPrecedence(t *testing.T) {
	const (
		defaultVal  = "default-val"
		staticVal   = "static-val"
		passVal     = "pass-val"
		otherStatic = "other-static"
		otherPass   = "other-pass"
	)

	tests := []struct {
		name          string
		cfg           HeaderOverridesConfig
		userPathAlias string
		passthrough   http.Header // nil disables passthrough source
		want          map[string]string
		wantMissing   []string
	}{
		{
			name: "defaults only",
			cfg: HeaderOverridesConfig{
				DefaultHeaders: map[string]string{
					"X-Default": defaultVal,
				},
			},
			want: map[string]string{
				"X-Default": defaultVal,
			},
		},
		{
			name: "static overrides defaults",
			cfg: HeaderOverridesConfig{
				DefaultHeaders: map[string]string{
					"X-Shared":  defaultVal,
					"X-Default": defaultVal,
				},
				CustomUpstreamHeaders: map[string]string{
					"X-Shared": staticVal,
					"X-Static": staticVal,
					// X-Default is only in DefaultHeaders to prove defaults are preserved where static does not override them.
				},
			},
			want: map[string]string{
				"X-Shared":  staticVal,
				"X-Static":  staticVal,
				"X-Default": defaultVal,
			},
		},
		{
			name: "passthrough overrides static and defaults",
			cfg: HeaderOverridesConfig{
				DefaultHeaders: map[string]string{
					"X-Shared": defaultVal,
				},
				CustomUpstreamHeaders: map[string]string{
					"X-Shared": staticVal,
				},
				PassthroughUserHeaders: true,
				// default-open (skip mode with empty skip list) — all non-blocked
				// passthrough headers are forwarded.
			},
			passthrough: http.Header{
				"X-Shared": {passVal},
			},
			want: map[string]string{
				"X-Shared": passVal,
			},
		},
		{
			name: "blocked default skipped, static still applied",
			cfg: HeaderOverridesConfig{
				DefaultHeaders: map[string]string{
					"Authorization":  "Bearer leaked", // blocked credential
					"Content-Length": "4096",          // blocked transport header
					"X-Default":      defaultVal,
				},
				CustomUpstreamHeaders: map[string]string{
					"Authorization": "Bearer leaked", // blocked credential
					"X-Static":      staticVal,
				},
			},
			want: map[string]string{
				"X-Default": defaultVal,
				"X-Static":  staticVal,
			},
			wantMissing: []string{"Authorization", "Content-Length"},
		},
		{
			name: "skipped default preserved when passthrough not allowed",
			cfg: HeaderOverridesConfig{
				DefaultHeaders: map[string]string{
					"X-Shared":  defaultVal,
					"X-Default": defaultVal,
				},
				CustomUpstreamHeaders: map[string]string{
					"X-Shared": staticVal,
				},
				PassthroughUserHeaders: true,
				// Allow mode: only the listed names are forwarded.
				SkipMode:    "allow",
				SkipHeaders: []string{"X-Allowed"},
			},
			passthrough: http.Header{
				"X-Shared":  {passVal},   // not in allow list → blocked from passthrough
				"X-Allowed": {passVal},   // allowed, but no static/default conflict
				"X-Other":   {otherPass}, // not allowed
			},
			want: map[string]string{
				"X-Shared":  staticVal, // static preserved (passthrough blocked)
				"X-Default": defaultVal,
				"X-Allowed": passVal,
			},
			wantMissing: []string{"X-Other"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "http://example.com", nil)
			if tt.passthrough != nil {
				ctx := WithPassthroughHeaders(req.Context(), tt.passthrough)
				req = req.WithContext(ctx)
			}
			ApplyHeaderOverrides(req, tt.cfg, tt.userPathAlias)

			for key, want := range tt.want {
				if got := req.Header.Get(key); got != want {
					t.Errorf("header %q = %q, want %q", key, got, want)
				}
			}
			for _, key := range tt.wantMissing {
				if got := req.Header.Get(key); got != "" {
					t.Errorf("header %q should be missing, got %q", key, got)
				}
			}
		})
	}
}
