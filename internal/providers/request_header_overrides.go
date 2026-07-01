package providers

import (
	"context"
	"net/http"
	"strings"

	"gomodel/internal/core"
)

// ApplyRequestHeaderOverrides merges provider request header overrides
// (custom headers) and inbound passthrough headers into the supplied
// http.Header.
//
// Order of operations:
//
//  1. Apply customHeaders first via http.Header.Set on the canonical key —
//     these overwrite any pre-existing provider defaults (including auth
//     headers set up by the factory) for the same key. Skipped passthrough
//     headers are still writable here, since the caller explicitly opted in.
//
//  2. If passthrough is true and an inbound RequestSnapshot is present on
//     ctx, walk its HeadersView. For every key that does not satisfy
//     SkipPassthroughHeader, delete any values currently in h (including the
//     custom headers just applied) and Add every value from the snapshot.
//     Passthrough wins over customHeaders for any non-skipped key.
//
//  3. If customHeaders is empty and passthrough is false, the function is a
//     no-op for the supplied header and side-effect free.
//
// The function never panics and is safe for nil/empty header maps and nil
// custom header values.
func ApplyRequestHeaderOverrides(ctx context.Context, h http.Header, customHeaders map[string]string, passthrough bool) {
	if h == nil {
		return
	}

	hasCustom := len(customHeaders) > 0
	if hasCustom {
		for key, value := range customHeaders {
			canonicalKey := http.CanonicalHeaderKey(strings.TrimSpace(key))
			if canonicalKey == "" {
				continue
			}
			h.Set(canonicalKey, value)
		}
	}

	if !passthrough {
		return
	}

	snapshot := core.GetRequestSnapshot(ctx)
	if snapshot == nil {
		return
	}
	headers := snapshot.HeadersView()
	if len(headers) == 0 {
		return
	}

	for key, values := range headers {
		if SkipPassthroughHeader(key) {
			continue
		}
		canonicalKey := http.CanonicalHeaderKey(key)
		if canonicalKey == "" {
			continue
		}
		h.Del(canonicalKey)
		if len(values) == 0 {
			continue
		}
		for _, value := range values {
			h.Add(canonicalKey, value)
		}
	}
}
