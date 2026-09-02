package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/usage"
)

// chatEvent is one SSE data frame carrying a single-token "a" delta, matching
// the locked OpenAI chat-completion streaming shape the guard inspects.
const chatEvent = `data: {"choices":[{"delta":{"content":"a"}}]}` + "\n\n"

// fakeSSEUpstream streams count copies of chatEvent with no trailing [DONE].
// It flushes each frame as it is written so the guard observes a live reader
// rather than a buffered string.
func fakeSSEUpstream(t *testing.T, count int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatalf("response writer does not implement http.Flusher")
		}
		for i := 0; i < count; i++ {
			if _, err := io.WriteString(w, chatEvent); err != nil {
				return
			}
			flusher.Flush()
		}
	}))
}

// newEchoContext builds an echo context with a recorder so proxyPassthroughResponse
// can write the relay directly to a captured buffer.
func newEchoContext(t *testing.T, method, target string) (*echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(method, target, strings.NewReader(""))
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func TestProxyPassthroughSSERepetitionGuard(t *testing.T) {
	cases := []struct {
		name       string
		limit      int
		maxPattern int
		upstream   int
	}{
		{name: "limit=3 upstream=20", limit: 3, maxPattern: 8, upstream: 20},
		{name: "limit=2 upstream=50", limit: 2, maxPattern: 8, upstream: 50},
		{name: "limit=0 byte-identical passthrough", limit: 0, maxPattern: 8, upstream: 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeSSEUpstream(t, tc.upstream)
			defer srv.Close()

			httpResp, err := http.Get(srv.URL)
			if err != nil {
				t.Fatalf("http.Get upstream: %v", err)
			}
			defer httpResp.Body.Close()

			resp := &core.PassthroughResponse{
				StatusCode: httpResp.StatusCode,
				Headers:    map[string][]string{"Content-Type": {"text/event-stream"}},
				Body:       httpResp.Body,
			}

			c, rec := newEchoContext(t, http.MethodPost, "/p/openai/chat/completions")
			usageLogger := &collectingUsageLogger{config: usage.Config{Enabled: true}}
			info := &core.PassthroughRouteInfo{
				Provider:    "openai",
				RawEndpoint: "chat/completions",
				Model:       "gpt-4o",
			}

			if err := proxyPassthroughResponse(c, nil, usageLogger, nil, "openai", "openai", "chat/completions", info, resp, tc.limit, tc.maxPattern); err != nil {
				t.Fatalf("proxyPassthroughResponse: %v", err)
			}

			out := rec.Body.String()

			if tc.limit <= 0 {
				// Disabled: relay must be byte-identical and no synthetic
				// [DONE] must be appended.
				want := strings.Repeat(chatEvent, tc.upstream)
				if out != want {
					t.Fatalf("limit=0: passthrough mismatch\nwant len=%d\ngot  len=%d", len(want), len(out))
				}
				if strings.Contains(out, "data: [DONE]") {
					t.Fatalf("limit=0: unexpected synthetic [DONE] in passthrough output")
				}
				return
			}

			// Enabled: the guard must end the stream with a synthetic [DONE]
			// and have fired before the full repetition played.
			if !strings.HasSuffix(out, "data: [DONE]\n\n") {
				t.Fatalf("enabled: output does not end with synthetic [DONE]: tail=%q", tail(out, 80))
			}
			// At most `limit` live 'a' deltas are emitted before the guard
			// fires; the guard observes each event before it inspects it, so
			// one accepted repeat leaks through.
			got := strings.Count(out, chatEvent)
			if got > tc.limit {
				t.Fatalf("enabled: emitted %d 'a' deltas, want <= limit=%d", got, tc.limit)
			}
		})
	}
}

// TestProxyPassthroughSSEUpstreamClosed confirms the guard closes the upstream
// body when a repetition is detected rather than draining it to the end.
func TestProxyPassthroughSSEUpstreamClosed(t *testing.T) {
	var closed atomic.Bool
	frame := []byte(chatEvent)
	// Stream many repeating frames so the guard is guaranteed to detect the
	// repetition regardless of off-by-one trigger semantics, then block
	// forever on a later read. The guard must close the body to unblock the
	// read loop rather than wait for the upstream to drain.
	const totalFrames = 50
	var calls atomic.Int32
	body := io.NopCloser(readerFunc(func(p []byte) (int, error) {
		n := calls.Add(1)
		if n <= totalFrames {
			return copy(p, frame), nil
		}
		<-make(chan struct{})
		return 0, io.EOF
	}))
	resp := &core.PassthroughResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string][]string{"Content-Type": {"text/event-stream"}},
		Body:       &closeTrackingReader{Reader: body, closed: &closed},
	}

	c, _ := newEchoContext(t, http.MethodPost, "/p/openai/chat/completions")
	info := &core.PassthroughRouteInfo{Provider: "openai", RawEndpoint: "chat/completions", Model: "gpt-4o"}

	if err := proxyPassthroughResponse(c, nil, nil, nil, "openai", "openai", "chat/completions", info, resp, 3, 8); err != nil {
		t.Fatalf("proxyPassthroughResponse: %v", err)
	}
	if !closed.Load() {
		t.Fatalf("expected upstream body to be closed on repetition trigger")
	}
}

// TestProxyPassthroughSSENoRepetition verifies that a non-repeating upstream
// streams through unchanged with no synthetic [DONE] injected by the guard.
func TestProxyPassthroughSSENoRepetition(t *testing.T) {
	upstream := "data: {\"choices\":[{" +
		"\"delta\":{\"content\":\"the quick brown fox\"}}]}\n\n" +
		"data: {\"choices\":[{" +
		"\"delta\":{\"content\":\" jumps over\"}}]}\n\n"
	resp := &core.PassthroughResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string][]string{"Content-Type": {"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(upstream)),
	}

	c, rec := newEchoContext(t, http.MethodPost, "/p/openai/chat/completions")
	info := &core.PassthroughRouteInfo{Provider: "openai", RawEndpoint: "chat/completions", Model: "gpt-4o"}

	if err := proxyPassthroughResponse(c, nil, nil, nil, "openai", "openai", "chat/completions", info, resp, 3, 8); err != nil {
		t.Fatalf("proxyPassthroughResponse: %v", err)
	}
	if rec.Body.String() != upstream {
		t.Fatalf("non-repeating passthrough mismatch\nwant: %q\ngot:  %q", upstream, rec.Body.String())
	}
}

// tail returns the last n bytes of s (or all of s if shorter), so failure
// messages print a bounded suffix of large SSE bodies.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// readerFunc adapts a function to io.Reader.
type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

// closeTrackingReader wraps a ReadCloser and records whether Close was called.
type closeTrackingReader struct {
	io.Reader
	closed *atomic.Bool
}

func (r *closeTrackingReader) Close() error {
	r.closed.Store(true)
	return nil
}
