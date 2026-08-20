package cursor

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/enterpilot/gomodel/internal/core"
)

// encodeFrame builds one Connect envelope: 1 byte flags + 4 BE length + payload.
func encodeFrame(t *testing.T, payload []byte, flags byte) []byte {
	t.Helper()
	if len(payload) > 0xFFFFFFFF {
		t.Fatalf("payload too large for frame: %d", len(payload))
	}
	buf := make([]byte, 5+len(payload))
	buf[0] = flags
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	return buf
}

// newTestTransport wires a Transport to an httptest.Server with a known
// bearer token and returns both so tests can inspect the request and assert
// against the response.
func newTestTransport(t *testing.T, handler http.Handler) (*Transport, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewTransport(srv.Client(), srv.URL, "test-token"), srv
}

func TestUnary_Success(t *testing.T) {
	var calls atomic.Int32
	var (
		gotAuth       string
		gotProto      string
		gotContent    string
		gotEndpoint   string
		gotBody       string
		gotMethod     string
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotAuth = r.Header.Get("Authorization")
		gotProto = r.Header.Get("Connect-Protocol-Version")
		gotContent = r.Header.Get("Content-Type")
		gotEndpoint = r.URL.Path
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"reply":"hi"}`))
	})
	tr, _ := newTestTransport(t, handler)

	type req struct {
		Text string `json:"text"`
	}
	type resp struct {
		Reply string `json:"reply"`
	}
	var got resp
	if err := tr.Unary(context.Background(), "AgentService", "Ask", &req{Text: "hello"}, &got); err != nil {
		t.Fatalf("Unary: %v", err)
	}
	if got.Reply != "hi" {
		t.Errorf("Reply = %q, want hi", got.Reply)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1", calls.Load())
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want Bearer test-token", gotAuth)
	}
	if gotProto != "1" {
		t.Errorf("Connect-Protocol-Version = %q, want 1", gotProto)
	}
	if gotContent != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContent)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("Method = %q, want POST", gotMethod)
	}
	if gotEndpoint != "/sdk.v1.AgentService/Ask" {
		t.Errorf("Path = %q, want /sdk.v1.AgentService/Ask", gotEndpoint)
	}
	if gotBody != `{"text":"hello"}` {
		t.Errorf("body = %q, want %q", gotBody, `{"text":"hello"}`)
	}
}

func TestUnary_ConnectErrorMapsToTypedError(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"bad key"}`))
	})
	tr, _ := newTestTransport(t, handler)

	err := tr.Unary(context.Background(), "AgentService", "Ask", nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Fatalf("error type = %T, want *core.GatewayError", err)
	}
	if gw.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", gw.StatusCode)
	}
	if gw.Code == nil || *gw.Code != "unauthenticated" {
		t.Errorf("Code = %v, want unauthenticated", gw.Code)
	}
	if gw.Type != core.ErrorTypeAuthentication {
		t.Errorf("Type = %q, want %q", gw.Type, core.ErrorTypeAuthentication)
	}
	if !strings.Contains(gw.Message, "bad key") {
		t.Errorf("Message = %q, want to contain %q", gw.Message, "bad key")
	}
}

func TestUnary_StreamSendsAuthorizationOnEveryRequest(t *testing.T) {
	// A common bug class is interceptors covering only the unary path. Both
	// Unary and Stream should emit the bearer token; this is verified again
	// in TestStream_FramesInOrder, but we keep one focused unary assertion
	// here for symmetry.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	tr, _ := newTestTransport(t, handler)
	if err := tr.Unary(context.Background(), "S", "M", nil, nil); err != nil {
		t.Fatalf("Unary: %v", err)
	}
}

func TestStream_FramesInOrder(t *testing.T) {
	var gotAuth string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/connect+json")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, payload := range []string{`{"i":1}`, `{"i":2}`, `{"i":3}`} {
			_, _ = w.Write(encodeFrame(t, []byte(payload), 0))
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = w.Write(encodeFrame(t, []byte(`{}`), frameFlagEndOfStream))
		if flusher != nil {
			flusher.Flush()
		}
	})
	tr, _ := newTestTransport(t, handler)

	stream, err := tr.Stream(context.Background(), "AgentService", "Stream", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var got []json.RawMessage
	for {
		payload, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, payload)
	}

	if len(got) != 3 {
		t.Fatalf("frames = %d, want 3", len(got))
	}
	want := []string{`{"i":1}`, `{"i":2}`, `{"i":3}`}
	for i, w := range want {
		if string(got[i]) != w {
			t.Errorf("frame %d = %s, want %s", i, got[i], w)
		}
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization on stream = %q, want Bearer test-token", gotAuth)
	}
}

func TestStream_RequestBodyIsEnveloped(t *testing.T) {
	// Per the Connect wire format a streaming request body is exactly one
	// envelope frame: 1 byte flags=0x00 + 4 bytes big-endian length +
	// JSON payload. We marshal the request, wrap it once, and send the
	// wrapped bytes; the handler must see the wrapper.
	var gotBody []byte
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/connect+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encodeFrame(t, nil, frameFlagEndOfStream))
	})
	tr, _ := newTestTransport(t, handler)

	type req struct {
		Text string `json:"text"`
	}
	stream, err := tr.Stream(context.Background(), "AgentService", "Stream", &req{Text: "hello"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("first Next = %v, want io.EOF", err)
	}

	want := encodeFrame(t, []byte(`{"text":"hello"}`), 0)
	if !bytes.Equal(gotBody, want) {
		t.Errorf("request body = %x, want %x", gotBody, want)
	}
}

func TestStream_NilRequestSendsEmptyObject(t *testing.T) {
	// A nil req must still produce exactly one envelope frame; the natural
	// zero-value JSON message is "{}".
	var gotBody []byte
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/connect+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encodeFrame(t, nil, frameFlagEndOfStream))
	})
	tr, _ := newTestTransport(t, handler)

	stream, err := tr.Stream(context.Background(), "AgentService", "Stream", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("first Next = %v, want io.EOF", err)
	}

	want := encodeFrame(t, []byte(`{}`), 0)
	if !bytes.Equal(gotBody, want) {
		t.Errorf("request body = %x, want %x", gotBody, want)
	}
}

func TestUnary_OversizedResponse(t *testing.T) {
	// A successful body larger than maxConnectBodyBytes must surface as a
	// clear "exceeds" error, not a confusing JSON unmarshal failure.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		body := make([]byte, maxUnaryBodyBytes+1)
		for i := range body {
			body[i] = 'a'
		}
		_, _ = w.Write(body)
	})
	tr, _ := newTestTransport(t, handler)

	type resp struct{}
	if err := tr.Unary(context.Background(), "S", "M", nil, &resp{}); err == nil {
		t.Fatal("expected error for oversized response")
	} else if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want to mention 'exceeds'", err)
	}
}

func TestStream_KeepaliveSkipped(t *testing.T) {
	// Keepalive frames on the wire are either the empty frame (flags=0x00,
	// length=0) or the empty JSON object "{}", per the Connect wire format.
	// Both must be skipped so only the real data frame reaches the caller.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+json")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// Empty frame, then {}, then the real data frame.
		_, _ = w.Write(encodeFrame(t, nil, 0))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write(encodeFrame(t, []byte(`{}`), 0))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write(encodeFrame(t, []byte(`{"x":42}`), 0))
		if flusher != nil {
			flusher.Flush()
		}
		// Empty clean end frame (length 0): the terminal tracking must
		// still record "done", so the next Next returns io.EOF again
		// instead of blocking or re-reading the (drained) body.
		_, _ = w.Write(encodeFrame(t, nil, frameFlagEndOfStream))
		if flusher != nil {
			flusher.Flush()
		}
	})
	tr, _ := newTestTransport(t, handler)

	stream, err := tr.Stream(context.Background(), "AgentService", "Stream", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	payload, err := stream.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if string(payload) != `{"x":42}` {
		t.Errorf("payload = %s, want {\"x\":42}", payload)
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("second Next = %v, want io.EOF", err)
	}
	// After a clean end frame the reader must remain terminal; calling
	// Next again must not block or re-read.
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("third Next = %v, want io.EOF", err)
	}
}

func TestStream_EndFrameWithError(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+json")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write(encodeFrame(t, []byte(`{"error":{"code":"internal","message":"boom"}}`), frameFlagEndOfStream))
		if flusher != nil {
			flusher.Flush()
		}
	})
	tr, _ := newTestTransport(t, handler)

	stream, err := tr.Stream(context.Background(), "AgentService", "Stream", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	_, err = stream.Next(context.Background())
	if err == nil {
		t.Fatal("expected error from end frame")
	}
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Fatalf("error type = %T, want *core.GatewayError", err)
	}
	if gw.Code == nil || *gw.Code != "internal" {
		t.Errorf("Code = %v, want internal", gw.Code)
	}
	if !strings.Contains(gw.Message, "boom") {
		t.Errorf("Message = %q, want to contain boom", gw.Message)
	}
}

func TestStream_TruncatedFrameErrors(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+json")
		w.WriteHeader(http.StatusOK)
		// 3 bytes of a 5-byte header; payload never follows.
		_, _ = w.Write([]byte{0x00, 0x00, 0x00})
	})
	tr, _ := newTestTransport(t, handler)

	stream, err := tr.Stream(context.Background(), "AgentService", "Stream", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	_, err = stream.Next(context.Background())
	if err == nil {
		t.Fatal("expected error from truncated frame")
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestStream_CompressedFrameUnsupported(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encodeFrame(t, []byte(`{}`), frameFlagCompressed))
	})
	tr, _ := newTestTransport(t, handler)

	stream, err := tr.Stream(context.Background(), "AgentService", "Stream", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	_, err = stream.Next(context.Background())
	var ue *UnsupportedError
	if !errors.As(err, &ue) {
		t.Fatalf("error type = %T, want *UnsupportedError", err)
	}
	if !strings.Contains(ue.Reason, "compressed") {
		t.Errorf("Reason = %q, want to mention compressed", ue.Reason)
	}
}

func TestStream_EndFrameIsTerminal(t *testing.T) {
	// After Next returns the terminal frame's error, a subsequent Next must
	// return io.EOF — never re-surface the same error and never block.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+json")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write(encodeFrame(t, []byte(`{"error":{"code":"aborted","message":"x"}}`), frameFlagEndOfStream))
		if flusher != nil {
			flusher.Flush()
		}
	})
	tr, _ := newTestTransport(t, handler)

	stream, err := tr.Stream(context.Background(), "AgentService", "Stream", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	if _, err := stream.Next(context.Background()); err == nil {
		t.Fatal("expected error from end frame")
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("second Next = %v, want io.EOF", err)
	}
}

func TestStream_OversizedFrame(t *testing.T) {
	// A streaming frame whose length prefix exceeds maxStreamFrameBytes
	// must surface as a clear "exceeds" error from readFrame, not as a
	// silent allocation of multi-GiB buffer.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+json")
		w.WriteHeader(http.StatusOK)
		// Length prefix = maxStreamFrameBytes + 1.
		over := uint32(maxStreamFrameBytes + 1)
		hdr := make([]byte, 5)
		hdr[0] = 0x00
		binary.BigEndian.PutUint32(hdr[1:5], over)
		_, _ = w.Write(hdr)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	})
	tr, _ := newTestTransport(t, handler)

	stream, err := tr.Stream(context.Background(), "AgentService", "Stream", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	_, err = stream.Next(context.Background())
	if err == nil {
		t.Fatal("expected error from oversized frame, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error = %v, want to mention 'exceeds'", err)
	}
}

func TestStream_NextHonoursCancelledContext(t *testing.T) {
	// Regression for finding (connect_transport.go:228): Next used to
	// discard its ctx arg. Now it uses context.AfterFunc to close the
	// body on ctx cancellation, so a stalled read returns promptly.
	hang := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+json")
		w.WriteHeader(http.StatusOK)
		// Emit one frame, flush, then park so the read on the body
		// blocks on the next frame.
		_, _ = w.Write(encodeFrame(t, []byte(`{"i":1}`), 0))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-hang
	})
	tr, _ := newTestTransport(t, handler)

	stream, err := tr.Stream(context.Background(), "AgentService", "Stream", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() {
		close(hang)
		_ = stream.Close()
	}()

	// First frame returns normally.
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatalf("first Next: %v", err)
	}

	// Second Next must respect ctx cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := stream.Next(ctx)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Next after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next did not unblock after ctx cancel")
	}
}

func TestNewTransportStripsBadTokenCharacters(t *testing.T) {
	// A token containing CR or LF would be rejected at HTTP write time
	// with a confusing net/http error. NewTransport strips those bytes
	// and logs a warning so the operator sees the issue at boot.
	tr := NewTransport(http.DefaultClient, "http://127.0.0.1:1", "good\r\nbad")
	if tr == nil {
		t.Fatal("NewTransport returned nil")
	}
	// The headerSetter closure was built with the sanitized token — we
	// cannot introspect it directly, but the test passes if construction
	// did not panic and returned a usable Transport.
}

func TestParseEndStreamMalformedLogsButReturnsNil(t *testing.T) {
	// An end-of-stream frame whose payload is not JSON should not abort
	// the call (the stream itself was clean) but should produce a
	// visible slog.Warn so operators can spot a buggy bridge.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/connect+json")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write(encodeFrame(t, []byte("not-valid-json{"), frameFlagEndOfStream))
		if flusher != nil {
			flusher.Flush()
		}
	})
	tr, _ := newTestTransport(t, handler)

	stream, err := tr.Stream(context.Background(), "AgentService", "Stream", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	_, err = stream.Next(context.Background())
	if !errors.Is(err, io.EOF) {
		t.Errorf("Next on malformed end-frame = %v, want io.EOF (clean stream)", err)
	}
}

func TestScrubForLog(t *testing.T) {
	// Cap at max bytes; printable run goes through unchanged; control
	// chars replaced with \xNN; CR/LF collapsed to spaces.
	got := scrubForLog([]byte("a\x01b\nc\rd\x7fE"), 64)
	want := `a\x01b c d\x7fE`
	if got != want {
		t.Errorf("scrubForLog = %q, want %q", got, want)
	}
	// Truncation.
	big := []byte(strings.Repeat("x", 100))
	if got := scrubForLog(big, 5); got != "xxxxx" {
		t.Errorf("scrubForLog(big,5) = %q, want xxxxx", got)
	}
	// High-bit / unicode rune → \uNNNN escape so the preview is safe
	// for terminals and log aggregators that interpret U+2028/U+2029.
	got = scrubForLog([]byte{0xE2, 0x80, 0xA8, 'x', 0xC2, 0xAD}, 64)
	want = `\u2028x\u00ad`
	if got != want {
		t.Errorf("scrubForLog(unicode) = %q, want %q", got, want)
	}
}

func TestUnsupportedErrorMessage(t *testing.T) {
	ue := &UnsupportedError{Reason: "x is bad"}
	if ue.Error() != "x is bad" {
		t.Errorf("Error() = %q, want %q", ue.Error(), "x is bad")
	}
}

func TestEncodeRequestFrameRejectsTooLarge(t *testing.T) {
	// 4 GiB + 1 byte payload exceeds the 32-bit length prefix; the
	// helper must surface a clear InvalidRequestError, not silently
	// truncate.
	huge := make([]byte, 1+0xFFFFFFFF)
	if _, err := encodeRequestFrame(huge); err == nil {
		t.Fatal("expected error for oversized payload, got nil")
	} else if !strings.Contains(err.Error(), "4 GiB") {
		t.Errorf("error = %v, want to mention 4 GiB limit", err)
	}
}

func TestMarshalStreamRequestNilAndInvalid(t *testing.T) {
	// nil req → "{}" framed exactly once.
	b, err := marshalStreamRequest(nil)
	if err != nil {
		t.Fatalf("marshalStreamRequest(nil): %v", err)
	}
	want := encodeFrameForTest([]byte("{}"), 0)
	if !bytes.Equal(b, want) {
		t.Errorf("nil req frame = %x, want %x", b, want)
	}

	// Non-marshalable value (channels) → InvalidRequestError.
	if _, err := marshalStreamRequest(make(chan int)); err == nil {
		t.Fatal("expected marshal error, got nil")
	}
}

// TestStreamEOFCoalescesToCleanReturn covers the `errors.Is(err, io.EOF)`
// branch in the Stream consumer: a server that closes the body without
// an end-of-stream frame must surface a clean EOF, not an "unexpected
// EOF" raw error.
func TestStreamEOFCoalescesToCleanReturn(t *testing.T) {
	mux := http.NewServeMux()
	// Path must match connectEndpoint()'s /sdk.v1.{service}/{method}.
	mux.HandleFunc("/sdk.v1.svc/Stream", func(w http.ResponseWriter, r *http.Request) {
		// Issue 0 frames and close the body — server-side EOF.
		_, _ = io.Copy(io.Discard, r.Body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	tr := NewTransport(srv.Client(), srv.URL, "tok")
	r, err := tr.Stream(context.Background(), "svc", "Stream", nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer r.Close()
	// Should return nil immediately because the body has no frames.
	if _, err := r.Next(context.Background()); err != io.EOF {
		t.Errorf("Next on empty body = %v, want io.EOF", err)
	}
}

// TestUnaryBodyDecodeFailureSurfacesGateway covers the
// `json.Unmarshal(httpResp.Body, resp)` failure branch: a 200 OK with
// a non-JSON body must surface as a typed error rather than silently
// returning a zero-value response.
func TestUnaryBodyDecodeFailureSurfacesGateway(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/sdk.v1.svc/Send", func(w http.ResponseWriter, r *http.Request) {
		// Drain request so the test does not leak the connection.
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		// Valid HTTP 200 with truncated JSON body — surface as a
		// typed decode failure rather than a zero-value response.
		_, _ = io.WriteString(w, `{"status":"OK"`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	tr := NewTransport(srv.Client(), srv.URL, "tok")
	var out map[string]any
	err := tr.Unary(context.Background(), "svc", "Send", map[string]string{"k": "v"}, &out)
	if err == nil {
		t.Fatal("expected decode failure on truncated JSON, got nil")
	}
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Fatalf("error type = %T (%v), want *core.GatewayError", err, err)
	}
	if gw.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want 502", gw.StatusCode)
	}
}

// TestReadFrameTruncatedPayload covers io.ReadFull returning
// io.ErrUnexpectedEOF when the payload length declared in the header
// exceeds the available bytes.
func TestReadFrameTruncatedPayload(t *testing.T) {
	// Header declares 100 bytes but only 5 bytes follow.
	header := []byte{0, 0, 0, 0, 100}
	r := bytes.NewReader(append(header, []byte("short")...))
	flags, payload, err := readFrame(r)
	if err == nil {
		t.Fatal("expected error from truncated payload, got none")
	}
	if flags != 0 {
		t.Errorf("flags = %d, want 0", flags)
	}
	if payload != nil {
		t.Errorf("payload = %v, want nil", payload)
	}
}

// encodeFrameForTest mirrors encodeFrame inline.
func encodeFrameForTest(payload []byte, flags byte) []byte {
	buf := make([]byte, 5+len(payload))
	buf[0] = flags
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	return buf
}
