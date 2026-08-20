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
		body := make([]byte, maxConnectBodyBytes+1)
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
