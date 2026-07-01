package providers

import (
	"net/http"
	"strings"
)

// SkipPassthroughHeader reports whether a request header key must be excluded
// from provider passthrough logic.
//
// The skip list intentionally mirrors the private helper used in
// internal/server/passthrough_support.go: hop-by-hop and transport-managed
// headers, credential headers, cookies, and any X-Forwarded-* prefix.
//
// The input key is trimmed and canonicalized with http.CanonicalHeaderKey
// before matching, so callers do not need to normalize keys themselves.
func SkipPassthroughHeader(key string) bool {
	canonicalKey := http.CanonicalHeaderKey(strings.TrimSpace(key))
	switch canonicalKey {
	case "Authorization", "X-Api-Key", "Host", "Content-Length", "Connection", "Keep-Alive",
		"Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
		"Cookie", "Forwarded", "Set-Cookie":
		return true
	default:
		return strings.HasPrefix(canonicalKey, "X-Forwarded-")
	}
}
