package providers

import "testing"

func TestSkipPassthroughHeader_ExactSkippedKeys(t *testing.T) {
	skipped := []string{
		"Authorization",
		"X-Api-Key",
		"Host",
		"Content-Length",
		"Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Te",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
		"Cookie",
		"Forwarded",
		"Set-Cookie",
	}

	for _, key := range skipped {
		key := key
		t.Run(key, func(t *testing.T) {
			if !SkipPassthroughHeader(key) {
				t.Fatalf("expected %q to be skipped", key)
			}
		})
	}
}

func TestSkipPassthroughHeader_XForwardedPrefix(t *testing.T) {
	cases := []string{
		"X-Forwarded-For",
		"X-Forwarded-Host",
		"X-Forwarded-Proto",
		"X-Forwarded-Client-IP",
	}

	for _, key := range cases {
		key := key
		t.Run(key, func(t *testing.T) {
			if !SkipPassthroughHeader(key) {
				t.Fatalf("expected %q (X-Forwarded-* prefix) to be skipped", key)
			}
		})
	}
}

func TestSkipPassthroughHeader_NotSkipped(t *testing.T) {
	notSkipped := []string{
		"X-Request-Id",
		"Content-Type",
		"Accept",
		"User-Agent",
		"X-Stainless-Arch",
		"X-Custom",
	}

	for _, key := range notSkipped {
		key := key
		t.Run(key, func(t *testing.T) {
			if SkipPassthroughHeader(key) {
				t.Fatalf("expected %q NOT to be skipped", key)
			}
		})
	}
}

func TestSkipPassthroughHeader_CaseInsensitive(t *testing.T) {
	cases := []struct {
		input    string
		expected bool
	}{
		{"authorization", true},
		{"AUTHORIZATION", true},
		{"x-api-key", true},
		{"x-api-KEY", true},
		{"x-forwarded-for", true},
		{"X-FORWARDED-FOR", true},
		{"x-forwarded-proto", true},
		{"x-request-id", false},
		{"CONTENT-TYPE", false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			got := SkipPassthroughHeader(tc.input)
			if got != tc.expected {
				t.Fatalf("SkipPassthroughHeader(%q) = %v, want %v", tc.input, got, tc.expected)
			}
		})
	}
}

func TestSkipPassthroughHeader_TrimsWhitespace(t *testing.T) {
	if !SkipPassthroughHeader("  Authorization  ") {
		t.Fatalf("expected whitespace-padded Authorization to be skipped")
	}

	if !SkipPassthroughHeader("\tX-Forwarded-For\n") {
		t.Fatalf("expected whitespace-padded X-Forwarded-For to be skipped")
	}

	if SkipPassthroughHeader("  Content-Type  ") {
		t.Fatalf("expected whitespace-padded Content-Type NOT to be skipped")
	}
}
