package providers

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"gomodel/internal/core"
)

// HeaderOverridesConfig holds per-provider header configuration.
type HeaderOverridesConfig struct {
	// CustomUpstreamHeaders adds static headers to all provider requests.
	CustomUpstreamHeaders map[string]string

	// PassthroughUserHeaders forwards all user headers to upstream.
	PassthroughUserHeaders bool

	// SkipHeaders prevents forwarding specific headers to upstream.
	SkipHeaders []string

	// SkipMode determines how SkipHeaders works: "skip" or "allow".
	SkipMode string

	// DefaultHeaders are baseline headers applied after provider SetHeaders
	// and before static custom headers / passthrough overrides. Static
	// overrides default values; passthrough (when permitted) overrides static.
	// Blocked names are skipped.
	DefaultHeaders map[string]string
}

// passthroughCtxKey is the context key for storing passthrough headers.
type passthroughCtxKey struct{}

// WithPassthroughHeaders stores headers in context for passthrough.
func WithPassthroughHeaders(ctx context.Context, h http.Header) context.Context {
	return context.WithValue(ctx, passthroughCtxKey{}, h)
}

// PassthroughHeadersFromContext retrieves passthrough headers from context.
func PassthroughHeadersFromContext(ctx context.Context) http.Header {
	if h, ok := ctx.Value(passthroughCtxKey{}).(http.Header); ok {
		return h
	}
	return nil
}

// IsHeaderBlocked reports whether a header should be blocked from forwarding.
// Hard-coded floor: credential headers, internal x-gomodel-user-path, the
// configured user-path alias, and transport/hop-by-hop headers.
func IsHeaderBlocked(name string, userPathAlias string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))

	// Credential headers are always blocked.
	if core.IsCredentialHeader(name) {
		return true
	}

	// Internal header is always blocked.
	if lower == "x-gomodel-user-path" {
		return true
	}

	// Block configured alias.
	if userPathAlias != "" {
		aliasLower := strings.ToLower(strings.TrimSpace(userPathAlias))
		if lower == aliasLower {
			return true
		}
	}

	// Transport / hop-by-hop headers are always blocked (Santiago #3).
	switch lower {
	case "accept-encoding", "connection", "keep-alive", "te",
		"transfer-encoding", "trailer", "upgrade",
		"proxy-authorization", "content-length", "host":
		return true
	}

	return false
}

// FilterIncomingHeaders returns a filtered copy of headers, removing blocked ones.
func FilterIncomingHeaders(headers http.Header, userPathAlias string) http.Header {
	filtered := make(http.Header)
	for name, values := range headers {
		if !IsHeaderBlocked(name, userPathAlias) {
			filtered[name] = values
		}
	}
	return filtered
}

// ApplyHeaderOverrides applies header overrides to the request.
// Composes default headers first, then static headers, then passthrough;
// passthrough wins on conflict. Logs a warning when static and passthrough
// are both configured and overlap.
func ApplyHeaderOverrides(req *http.Request, cfg HeaderOverridesConfig, userPathAlias string) {
	if len(cfg.DefaultHeaders) > 0 {
		applyDefaultHeaders(req, cfg.DefaultHeaders, userPathAlias)
	}

	if len(cfg.CustomUpstreamHeaders) > 0 {
		applyStaticHeaders(req, cfg.CustomUpstreamHeaders, userPathAlias)
	}

	if cfg.PassthroughUserHeaders {
		if len(cfg.CustomUpstreamHeaders) > 0 {
			logStaticOverridden(req, cfg)
		}
		applyPassthroughHeaders(req, cfg, userPathAlias)
	}
}

// logStaticOverridden warns that passthrough will override static headers for
// overlapping names. Still sends both; passthrough wins on conflict.
func logStaticOverridden(req *http.Request, cfg HeaderOverridesConfig) {
	source := PassthroughHeadersFromContext(req.Context())
	skipSet := normalizeHeaderSet(cfg.SkipHeaders)
	stripSet := core.TaggingStripHeadersFromContext(req.Context())
	overlap := make([]string, 0)
	for name := range cfg.CustomUpstreamHeaders {
		if sourceHasHeader(source, name) && shouldForward(name, skipSet, cfg.SkipMode, "", stripSet) {
			overlap = append(overlap, name)
		}
	}
	if len(overlap) == 0 {
		return
	}
	slog.Warn(
		"custom upstream headers overlap with passthrough user headers; passthrough will override static values for overlapping names",
		"overlapping_names", overlap,
	)
}

// sourceHasHeader reports whether the passthrough header source contains a
// header with the given canonical name (case-insensitive).
func sourceHasHeader(source http.Header, name string) bool {
	if source == nil {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(name))
	for srcName := range source {
		if strings.ToLower(strings.TrimSpace(srcName)) == lower {
			return true
		}
	}
	return false
}

// applyPassthroughHeaders reads headers from request context, applies skip/allow
// list, and sets headers. Also treats tagging "do not pass" headers as blocked.
func applyPassthroughHeaders(req *http.Request, cfg HeaderOverridesConfig, userPathAlias string) {
	source := PassthroughHeadersFromContext(req.Context())
	if source == nil {
		return
	}

	skipSet := normalizeHeaderSet(cfg.SkipHeaders)
	stripSet := core.TaggingStripHeadersFromContext(req.Context())

	for name, values := range source {
		if shouldForward(name, skipSet, cfg.SkipMode, userPathAlias, stripSet) {
			req.Header.Del(name)
			for _, v := range values {
				req.Header.Add(name, v)
			}
		}
	}
}

// applyStaticHeaders adds static custom headers to request, skipping blocked names.
func applyStaticHeaders(req *http.Request, headers map[string]string, userPathAlias string) {
	for name, value := range headers {
		if !IsHeaderBlocked(name, userPathAlias) {
			req.Header.Set(name, value)
		}
	}
}

// applyDefaultHeaders seeds baseline headers on the request after provider
// SetHeaders have run and before static custom headers apply. Blocked names
// (credentials, transport, configured user-path alias) are skipped so defaults
// cannot leak protected values upstream.
func applyDefaultHeaders(req *http.Request, headers map[string]string, userPathAlias string) {
	for name, value := range headers {
		if !IsHeaderBlocked(name, userPathAlias) {
			req.Header.Set(name, value)
		}
	}
}

// shouldForward reports whether header should be forwarded based on skip/allow
// configuration. Checks floor (hard-coded blocks and tagging strip set) first,
// then applies skip/allow list. Default-open: when the mode is not "allow" and
// the skip set is empty, all non-blocked headers are forwarded.
func shouldForward(name string, skipSet map[string]bool, mode string, userPathAlias string, stripSet map[string]struct{}) bool {
	lower := strings.ToLower(strings.TrimSpace(name))

	// Hard-coded floor: check blocked headers first.
	if IsHeaderBlocked(name, userPathAlias) {
		return false
	}

	// Tagging "do not pass" headers are treated as blocked (Santiago #2).
	// Strip keys are stored canonically (see TaggingStripHeadersFromContext),
	// so look up the canonical form of the header name.
	if stripSet != nil {
		if _, blocked := stripSet[http.CanonicalHeaderKey(name)]; blocked {
			return false
		}
	}

	// Allow-mode is a closed list; every other mode is default-open with the
	// configured list acting as a skip list.
	if mode == "allow" {
		return skipSet[lower]
	}
	if len(skipSet) == 0 {
		return true
	}
	return !skipSet[lower]
}

// normalizeHeaderSet converts header list to case-insensitive lookup set.
func normalizeHeaderSet(headers []string) map[string]bool {
	result := make(map[string]bool)
	for _, h := range headers {
		lower := strings.ToLower(strings.TrimSpace(h))
		result[lower] = true
	}
	return result
}
