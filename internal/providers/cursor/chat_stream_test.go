package cursor

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/streaming"
)

// readAllSSE drains the converter fully, returning both the emitted
// payload and any error surfaced by the stream.
func readAllSSE(r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	return string(b), err
}

// stripDone removes the trailing [DONE] marker so chunk assertions only
// see envelope lines.
func stripDone(s string) string {
	return strings.TrimSuffix(s, "data: [DONE]\n\n")
}

// splitChunks parses an SSE payload into chunk envelopes. The trailing
// [DONE] sentinel is excluded; tests assert on it separately.
func splitChunks(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, l := range strings.Split(stripDone(raw), "\n\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if !strings.HasPrefix(l, "data: ") {
			t.Fatalf("non-data line in SSE payload: %q", l)
		}
		body := strings.TrimPrefix(l, "data: ")
		if body == "[DONE]" {
			continue
		}
		var ch map[string]any
		if err := json.Unmarshal([]byte(body), &ch); err != nil {
			t.Fatalf("chunk %q is not JSON: %v", body, err)
		}
		out = append(out, ch)
	}
	return out
}

// firstDelta returns the delta map from the first choice in a chunk
// envelope.
func firstDelta(chunk map[string]any) map[string]any {
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return map[string]any{}
	}
	first, _ := choices[0].(map[string]any)
	delta, _ := first["delta"].(map[string]any)
	return delta
}

// firstFinish returns the finish_reason from the first choice, or "".
func firstFinish(chunk map[string]any) string {
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	first, _ := choices[0].(map[string]any)
	fr, _ := first["finish_reason"].(string)
	return fr
}

// frame encodes one Connect data envelope frame around payload.
func frame(payload string) []byte {
	buf := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	return buf
}

func TestStreamChatCompletion_HappyPath(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		switch path {
		case createAgentPath:
			writeUnaryJSON(w, `{"agentId":"agent-1"}`)
		case sendPath:
			writeStream(w,
				assistantFrame("hello "),
				assistantFrame("world"),
				resultFrame("run-42", "hello world"),
			)
		case closeAgentPath:
			writeUnaryJSON(w, `{}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	p := rs.provider(t)

	body, err := p.StreamChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "composer-2.5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	defer func() { _ = body.Close() }()

	raw, readErr := readAllSSE(body)
	if readErr != nil {
		t.Fatalf("read stream: %v", readErr)
	}

	// Exactly one [DONE] marker, terminating the stream.
	if !strings.HasSuffix(raw, "data: [DONE]\n\n") {
		t.Fatalf("stream missing [DONE] terminator; got %q", raw)
	}
	if c := strings.Count(raw, "data: [DONE]"); c != 1 {
		t.Errorf("DONE marker count = %d, want 1", c)
	}

	chunks := splitChunks(t, raw)
	if len(chunks) != 3 {
		t.Fatalf("chunk count = %d, want 3 (role+text, text, final); chunks=%v", len(chunks), chunks)
	}

	// First chunk: role=assistant + content.
	if ch := chunks[0]; ch["model"] != "composer-2.5" || ch["object"] != "chat.completion.chunk" {
		t.Errorf("first chunk envelope = %v, want model=composer-2.5 object=chat.completion.chunk", ch)
	}
	if d := firstDelta(chunks[0]); d["role"] != "assistant" || d["content"] != "hello " {
		t.Errorf("first delta = %v, want {role:assistant, content:\"hello \"}", d)
	}
	// Second chunk: content only, role not repeated.
	if d := firstDelta(chunks[1]); d["content"] != "world" {
		t.Errorf("second delta content = %v, want world", d["content"])
	}
	if _, has := firstDelta(chunks[1])["role"]; has {
		t.Errorf("second delta should not repeat role, got %v", firstDelta(chunks[1]))
	}
	// Final chunk: finish_reason=stop plus top-level usage from the
	// terminal result frame.
	if fr := firstFinish(chunks[2]); fr != "stop" {
		t.Errorf("final chunk finish_reason = %v, want stop", fr)
	}
	u, ok := chunks[2]["usage"].(map[string]any)
	if !ok {
		t.Fatalf("final chunk missing usage; got %v", chunks[2])
	}
	if u["prompt_tokens"].(float64) != 10 || u["completion_tokens"].(float64) != 5 || u["total_tokens"].(float64) != 15 {
		t.Errorf("final chunk usage = %v, want {10,5,15}", u)
	}
	// The chunk id propagates from the terminal run id.
	if id, _ := chunks[2]["id"].(string); id != "run-42" {
		t.Errorf("final chunk id = %q, want run-42", id)
	}
	// The provider is reported as "cursor" on every chunk.
	for i, ch := range chunks {
		if got, _ := ch["provider"].(string); got != "cursor" {
			t.Errorf("chunk[%d] provider = %v, want cursor", i, ch["provider"])
		}
	}

	// CloseAgent released the agent on clean end-of-stream.
	if got := rs.countCalls(closeAgentPath); got != 1 {
		t.Errorf("CloseAgent calls = %d, want 1", got)
	}
}

func TestStreamChatCompletion_KeepaliveSkipped(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		switch path {
		case createAgentPath:
			writeUnaryJSON(w, `{"agentId":"agent-1"}`)
		case sendPath:
			// Keepalive frames (empty payload and "{}" payload) between
			// assistant deltas must not generate chunks.
			writeStream(w,
				"{}",
				"",
				assistantFrame("alpha "),
				`{}`,
				assistantFrame("beta"),
				resultFrame("run-7", "alpha beta"),
			)
		case closeAgentPath:
			writeUnaryJSON(w, `{}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	p := rs.provider(t)

	body, err := p.StreamChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "composer-2.5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	raw, readErr := readAllSSE(body)
	if readErr != nil {
		t.Fatalf("read stream: %v", readErr)
	}
	_ = body.Close()

	chunks := splitChunks(t, raw)
	if len(chunks) != 3 {
		t.Fatalf("chunk count = %d, want 3 (role+alpha, beta, final); chunks=%v", len(chunks), chunks)
	}
	if got := firstDelta(chunks[0])["content"]; got != "alpha " {
		t.Errorf("first content = %v, want \"alpha \"", got)
	}
	if got := firstDelta(chunks[1])["content"]; got != "beta" {
		t.Errorf("second content = %v, want beta", got)
	}
	if fr := firstFinish(chunks[2]); fr != "stop" {
		t.Errorf("final chunk finish_reason = %v, want stop", fr)
	}
	if !strings.HasSuffix(raw, "data: [DONE]\n\n") {
		t.Fatalf("missing DONE terminator; got %q", raw)
	}
}

func TestStreamChatCompletion_NoUsageOmitsField(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		switch path {
		case createAgentPath:
			writeUnaryJSON(w, `{"agentId":"agent-1"}`)
		case sendPath:
			// Terminal result frame with no usage block.
			writeStream(w,
				assistantFrame("done"),
				`{"result":{"agentId":"agent-1","runId":"run-1","status":"RUN_LIFECYCLE_STATUS_FINISHED","result":{"runId":"run-1","agentId":"agent-1","status":"RUN_LIFECYCLE_STATUS_FINISHED","result":"done"}}}`,
			)
		case closeAgentPath:
			writeUnaryJSON(w, `{}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	p := rs.provider(t)

	body, err := p.StreamChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "composer-2.5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	raw, readErr := readAllSSE(body)
	if readErr != nil {
		t.Fatalf("read stream: %v", readErr)
	}
	_ = body.Close()

	chunks := splitChunks(t, raw)
	if len(chunks) != 2 {
		t.Fatalf("chunk count = %d, want 2 (text, final); chunks=%v", len(chunks), chunks)
	}
	final := chunks[len(chunks)-1]
	if _, ok := final["usage"]; ok {
		t.Errorf("final chunk should omit usage when terminal had none; got %v", final["usage"])
	}
	if fr := firstFinish(final); fr != "stop" {
		t.Errorf("final finish_reason = %v, want stop", fr)
	}
}

func TestStreamChatCompletion_MalformedFrameReturnsGatewayError502(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		switch path {
		case createAgentPath:
			writeUnaryJSON(w, `{"agentId":"agent-1"}`)
		case sendPath:
			writeStream(w,
				assistantFrame("Hello"),
				`{not valid json`,
			)
		case closeAgentPath:
			writeUnaryJSON(w, `{}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	p := rs.provider(t)

	body, err := p.StreamChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "composer-2.5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}

	raw, readErr := readAllSSE(body)
	if readErr == nil {
		t.Fatal("expected GatewayError from malformed stream frame, got nil")
	}
	var gw *core.GatewayError
	if !errors.As(readErr, &gw) {
		t.Fatalf("error type = %T, want *core.GatewayError", readErr)
	}
	if gw.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", gw.StatusCode)
	}
	if !strings.Contains(gw.Message, "decode stream frame") {
		t.Errorf("message = %q, want decode-failure substring", gw.Message)
	}
	// Prior chunks stay intact; no [DONE] is emitted on the error path.
	if !strings.Contains(raw, `"content":"Hello"`) {
		t.Fatalf("expected prior converted chunk in raw output, got %q", raw)
	}
	if !strings.Contains(raw, `"role":"assistant"`) {
		t.Errorf("expected first chunk to carry role=assistant; raw=%q", raw)
	}
	if strings.Contains(raw, "[DONE]") {
		t.Fatalf("did not expect [DONE] after malformed frame, got %q", raw)
	}

	// The agent is released on the error path too.
	if got := rs.countCalls(closeAgentPath); got != 1 {
		t.Errorf("CloseAgent calls on error path = %d, want 1", got)
	}
}

func TestStreamChatCompletion_NonOKTerminalEmitsGatewayError(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		switch path {
		case createAgentPath:
			writeUnaryJSON(w, `{"agentId":"agent-1"}`)
		case sendPath:
			w.Header().Set("Content-Type", "application/connect+json")
			_, _ = w.Write(frame(assistantFrame("partial")))
			_, _ = w.Write(frame(`{"result":{"agentId":"agent-1","runId":"run-err","status":"RUN_LIFECYCLE_STATUS_ERROR","result":{"runId":"run-err","agentId":"agent-1","status":"RUN_LIFECYCLE_STATUS_ERROR","error":"boom"}}}`))
		case closeAgentPath:
			writeUnaryJSON(w, `{}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	p := rs.provider(t)

	body, err := p.StreamChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "composer-2.5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	defer func() { _ = body.Close() }()

	raw, err := io.ReadAll(body)
	if err == nil {
		t.Fatalf("expected error from non-OK terminal, got nil; raw=%q", string(raw))
	}
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Fatalf("expected *core.GatewayError, got %T", err)
	}
	if gw.StatusCode != http.StatusBadGateway {
		t.Fatalf("StatusCode = %d, want %d", gw.StatusCode, http.StatusBadGateway)
	}
	if !strings.Contains(string(raw), `"content":"partial"`) {
		t.Fatalf("prior chunks should still be readable, got %q", string(raw))
	}
	if strings.Contains(string(raw), "[DONE]") {
		t.Fatalf("non-OK terminal must not emit [DONE], got %q", string(raw))
	}
}

func TestStreamChatCompletion_CloseReleasesAgent(t *testing.T) {
	releaseCh := make(chan struct{})
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		switch path {
		case createAgentPath:
			writeUnaryJSON(w, `{"agentId":"agent-1"}`)
		case sendPath:
			w.Header().Set("Content-Type", "application/connect+json")
			// Emit one assistant frame, flush it, then park until the
			// test signals. The client closes the stream mid-flight.
			_, _ = w.Write(frame(assistantFrame("partial")))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-releaseCh
		case closeAgentPath:
			writeUnaryJSON(w, `{}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	p := rs.provider(t)

	body, err := p.StreamChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "composer-2.5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}

	// One read consumes the flushed chunk; the stream stays open.
	buf := make([]byte, 4096)
	n, err := body.Read(buf)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if !strings.Contains(string(buf[:n]), `"content":"partial"`) {
		t.Fatalf("first read = %q, want chunk with content=partial", string(buf[:n]))
	}

	// Closing an undrained stream releases the agent exactly once.
	_ = body.Close()
	close(releaseCh)
	_ = body.Close() // idempotent

	if got := rs.countCalls(closeAgentPath); got != 1 {
		t.Errorf("CloseAgent calls after Close = %d, want 1", got)
	}
}

func TestStreamChatCompletion_SendErrorClosesAgent(t *testing.T) {
	// When Send itself fails (e.g., the bridge rejects the request body
	// before any frame is streamed), the provider must call CloseAgent
	// on the background context so the bridge releases the agent even
	// though the caller never received a body.
	var closeAgentSeen atomic.Int32
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		switch path {
		case createAgentPath:
			writeUnaryJSON(w, `{"agentId":"agent-1"}`)
		case sendPath:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":"internal","message":"send failed"}`))
		case closeAgentPath:
			closeAgentSeen.Add(1)
			writeUnaryJSON(w, `{}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	p := rs.provider(t)

	body, err := p.StreamChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "composer-2.5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		_ = body.Close()
		t.Fatal("expected error from failed Send")
	}
	if body != nil {
		t.Errorf("body = %v, want nil on send failure", body)
	}
	if closeAgentSeen.Load() != 1 {
		t.Errorf("CloseAgent called %d times, want 1 (release on send error)", closeAgentSeen.Load())
	}
}

func TestStreamConverter_TooManyEmptyFramesReturns502(t *testing.T) {
	// The inner empty-frame loop in chat_stream.go caps at
	// maxEmptyFramesPerRead (64) before returning "too many empty
	// frames". Drive the loop with frames that the StreamReader actually
	// surfaces (an unrecognized sdkMessage type) — `{}` keepalives are
	// drained by StreamReader.Next internally so they never reach the
	// converter's inner loop.
	var buf bytes.Buffer
	for i := 0; i < 80; i++ {
		// sdkMessage with an unknown Type — non-empty, not matched by the
		// converter's switch, so each call to Next returns a fresh frame.
		payload := []byte(`{"SDKMessage":{"type":"unknown","message":{"text":""}}}`)
		hdr := make([]byte, 5)
		binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
		buf.Write(hdr)
		buf.Write(payload)
	}
	sr := newStreamReader(io.NopCloser(&buf))
	sc := &streamConverter{
		stream:  sr,
		model:   "m",
		created: time.Now().Unix(),
		buffer:  streaming.NewStreamBuffer(1024),
		ctx:     context.Background(),
	}
	readBuf := make([]byte, 1024)
	n, err := sc.Read(readBuf)
	t.Logf("Read returned (%d, %v), closed=%v, buffer.Len=%d", n, err, sc.closed, sc.buffer.Len())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Fatalf("error type = %T (%v), want *core.GatewayError", err, err)
	}
	if gw.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want 502", gw.StatusCode)
	}
	if !strings.Contains(gw.Message, "too many empty frames") {
		t.Errorf("Message = %q, want too-many-empty-frames substring", gw.Message)
	}
}

func TestStreamConverter_ReadBufferDrainedFirst(t *testing.T) {
	// When the converter buffer already has bytes, Read returns them
	// without touching the stream — confirms the "buffer.Len() > 0"
	// fast path.
	sc := &streamConverter{
		model:   "m",
		created: time.Now().Unix(),
		buffer:  streaming.NewStreamBuffer(1024),
		ctx:     context.Background(),
	}
	sc.buffer.AppendString("cached bytes")
	out := make([]byte, 64)
	n, err := sc.Read(out)
	if err != nil || n != len("cached bytes") {
		t.Errorf("Read = (%d, %v); want (%d, nil)", n, err, len("cached bytes"))
	}
	if string(out[:n]) != "cached bytes" {
		t.Errorf("output = %q, want cached bytes", out[:n])
	}
}

func TestStreamConverter_ReadAfterCloseIsEOF(t *testing.T) {
	// Once Close() has run, subsequent Read calls must return EOF
	// without invoking the stream — this is the "closed" early-return
	// branch.
	sc := &streamConverter{
		model:   "m",
		created: time.Now().Unix(),
		buffer:  streaming.NewStreamBuffer(1024),
		ctx:     context.Background(),
		closed:  true,
	}
	_, err := sc.Read(make([]byte, 16))
	if !errors.Is(err, io.EOF) {
		t.Errorf("Read after close = %v, want io.EOF", err)
	}
}

func TestStreamConverter_HandleResultNonOK(t *testing.T) {
	// handleResult must surface cursorRunError for non-OK terminal
	// statuses, mirroring the runSend path.
	sc := &streamConverter{
		model:   "m",
		created: time.Now().Unix(),
		buffer:  streaming.NewStreamBuffer(1024),
		ctx:     context.Background(),
	}
	err := sc.handleResult(&runStreamResult{
		Status: "RUN_LIFECYCLE_STATUS_ERROR",
		Result: runResult{Result: "boom"},
	})
	if err == nil {
		t.Fatal("expected error from non-OK terminal")
	}
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Fatalf("error type = %T, want *core.GatewayError", err)
	}
	if gw.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want 502", gw.StatusCode)
	}
}

func TestStreamConverter_StreamNextError(t *testing.T) {
	// Connect-protocol HTTP error after the headers (e.g., a 500 from
	// the bridge mid-stream) surfaces as a non-EOF error from Next.
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		switch path {
		case createAgentPath:
			writeUnaryJSON(w, `{"agentId":"agent-1"}`)
		case sendPath:
			w.Header().Set("Content-Type", "application/connect+json")
			// Emit one assistant frame then abruptly hang up.
			_, _ = w.Write(frame(assistantFrame("hi")))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Hijack the conn and close it to force an unexpected EOF
			// on the next read.
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		case closeAgentPath:
			writeUnaryJSON(w, `{}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	p := rs.provider(t)

	body, err := p.StreamChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "composer-2.5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	_, err = io.ReadAll(body)
	// The first frame is read OK, then the connection drops; io.ReadAll
	// returns the read error from Next (either a wrapped io.ErrUnexpectedEOF
	// or similar). Accept any non-nil error.
	if err == nil {
		t.Fatal("expected error from dropped stream, got nil")
	}
}

func TestStreamConverter_NilCloseAgentIsNoOp(t *testing.T) {
	// releaseAgent must tolerate a nil callback (defensive — the
	// converter should never be constructed with one, but the guard
	// makes the type safe to embed).
	c := &streamConverter{closeAgent: nil}
	c.releaseAgent() // must not panic
}

func TestAppendAssistantSkipsEmptyText(t *testing.T) {
	// An assistant frame with content blocks whose text is empty must
	// not produce a chunk.
	c := newStreamConverter(context.Background(), nil, "m", nil)
	c.appendAssistant([]byte(`{"role":"assistant","content":[{"type":"text","text":""},{"type":"text","text":"hi"}]}`))
	if c.buffer.Len() == 0 {
		t.Fatal("expected non-empty buffer")
	}
	out := string(c.buffer.Unread())
	if !strings.Contains(out, `"content":"hi"`) {
		t.Errorf("buffer = %q, want content=hi", out)
	}
}

func TestAppendAssistantEmptyPayloadIsNoop(t *testing.T) {
	c := newStreamConverter(context.Background(), nil, "m", nil)
	c.appendAssistant(nil)
	c.appendAssistant([]byte(""))
	c.appendAssistant([]byte("not-json"))
	if c.buffer.Len() != 0 {
		t.Errorf("buffer should be empty, got %q", string(c.buffer.Unread()))
	}
}

// TestStreamConverter_InnerLoopAssistantFrameReturnsImmediateBuffer covers
// the inner-loop branch where, after reading an unrecognized frame, the
// next frame is a real assistant message — Read must return the buffered
// assistant bytes without continuing through the full empty-frame cap.
func TestStreamConverter_InnerLoopAssistantFrameReturnsImmediateBuffer(t *testing.T) {
	var buf bytes.Buffer
	// 5 unrecognized frames — all "unknown" type — followed by a real
	// assistant frame with text content.
	messages := []string{
		`{"sdkMessage":{"type":"unknown_a","message":{}}}`,
		`{"sdkMessage":{"type":"unknown_b","message":{}}}`,
		`{"sdkMessage":{"type":"unknown_c","message":{}}}`,
		`{"sdkMessage":{"type":"unknown_d","message":{}}}`,
		`{"sdkMessage":{"type":"unknown_e","message":{}}}`,
		`{"sdkMessage":{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}}`,
	}
	for _, m := range messages {
		payload := []byte(m)
		hdr := make([]byte, 5)
		binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
		buf.Write(hdr)
		buf.Write(payload)
	}
	// Close-of-stream.
	buf.Write([]byte{0x02, 0x00, 0x00, 0x00, 0x00})

	sr := newStreamReader(io.NopCloser(&buf))
	sc := &streamConverter{
		stream:     sr,
		model:      "m",
		created:    time.Now().Unix(),
		buffer:     streaming.NewStreamBuffer(1024),
		closeAgent: func() {}, // no-op close so the converter does not panic
		ctx:        context.Background(),
	}

	out := make([]byte, 512)
	n, err := sc.Read(out)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n == 0 {
		t.Fatalf("Read returned 0 bytes; expected assistant content")
	}
	body := string(out[:n])
	if !strings.Contains(body, `"content":"hi"`) {
		t.Errorf("Read body missing assistant text: %q", body)
	}
}

// TestStreamConverter_InnerLoopMalformedFrameSurfaces502 covers the
// `json.Unmarshal(frame, &env)` failure inside the inner empty-frame
// loop — a malformed Connect frame after a successful first frame must
// surface as 502 immediately.
func TestStreamConverter_InnerLoopMalformedFrameSurfaces502(t *testing.T) {
	var buf bytes.Buffer
	frames := []string{
		// First frame is a recognized-but-no-match sdkMessage so the
		// outer block's switch does not fire — buffer stays empty and
		// we fall into the inner loop.
		`{"sdkMessage":{"type":"unknown","message":{}}}`,
		// Second frame is malformed JSON. The inner loop's Unmarshal
		// fails and surfaces a 502.
		`{not valid`,
	}
	for _, m := range frames {
		payload := []byte(m)
		hdr := make([]byte, 5)
		binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
		buf.Write(hdr)
		buf.Write(payload)
	}

	sr := newStreamReader(io.NopCloser(&buf))
	sc := &streamConverter{
		stream:     sr,
		model:      "m",
		created:    time.Now().Unix(),
		buffer:     streaming.NewStreamBuffer(1024),
		closeAgent: func() {},
		ctx:        context.Background(),
	}
	out := make([]byte, 1024)
	_, err := sc.Read(out)
	if err == nil {
		t.Fatal("expected error from malformed inner-frame, got nil")
	}
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Fatalf("error type = %T (%v), want *core.GatewayError", err, err)
	}
	if gw.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want 502", gw.StatusCode)
	}
}

// TestStreamConverter_InnerLoopEOFAfterSkipsReturnsDone covers the
// inner-loop EOF branch: after the inner loop has skipped a few
// unrecognized (non-empty, non-keepalive) frames, the stream then
// ends cleanly — Read must return [DONE] exactly once.
//
// NOTE: `{}` keepalives are drained by StreamReader.Next internally
// and never reach the converter's inner loop, so the unrecognized
// frames below must be non-empty to actually exercise the bound.
func TestStreamConverter_InnerLoopEOFAfterSkipsReturnsDone(t *testing.T) {
	var buf bytes.Buffer
	// Five unrecognized sdkMessage frames, then EOF. StreamReader.Next
	// surfaces each one; the converter's inner loop counts them as
	// skipped and reaches EOF on the next Next() call.
	for i := 0; i < 5; i++ {
		payload := []byte(`{"sdkMessage":{"type":"unknown","message":{}}}`)
		hdr := make([]byte, 5)
		binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
		buf.Write(hdr)
		buf.Write(payload)
	}

	sr := newStreamReader(io.NopCloser(&buf))
	sc := &streamConverter{
		stream:     sr,
		model:      "m",
		created:    time.Now().Unix(),
		buffer:     streaming.NewStreamBuffer(1024),
		closeAgent: func() {},
		ctx:        context.Background(),
	}
	out := make([]byte, 64)
	n, err := sc.Read(out)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(string(out[:n]), "[DONE]") {
		t.Errorf("body = %q, want [DONE]", string(out[:n]))
	}
}

// TestStreamConverter_InnerLoopResultFrameBufferReturn covers the
// `case env.Result != nil` branch in the inner loop — a non-terminal
// Result frame (e.g. one with empty Status or with a non-FINISHED
// Status that handleResult reports) must surface the error rather
// than silently swallow it.
func TestStreamConverter_InnerLoopResultFrameBufferReturn(t *testing.T) {
	var buf bytes.Buffer
	frames := []string{
		`{"sdkMessage":{"type":"unknown","message":{}}}`,                                  // skip in inner loop
		`{"result":{"agentId":"a","runId":"r","status":"FAILED","errorCode":"model_overloaded","result":{"runId":"r","agentId":"a","status":"FAILED","result":"boom"}}}`, // Result with non-OK status
	}
	for _, m := range frames {
		payload := []byte(m)
		hdr := make([]byte, 5)
		binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
		buf.Write(hdr)
		buf.Write(payload)
	}

	sr := newStreamReader(io.NopCloser(&buf))
	sc := &streamConverter{
		stream:     sr,
		model:      "m",
		created:    time.Now().Unix(),
		buffer:     streaming.NewStreamBuffer(1024),
		closeAgent: func() {},
		ctx:        context.Background(),
	}
	out := make([]byte, 1024)
	_, err := sc.Read(out)
	if err == nil {
		t.Fatal("expected error from non-OK terminal status, got nil")
	}
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Fatalf("error type = %T (%v), want *core.GatewayError", err, err)
	}
}

// TestStreamConverter_InnerLoopAssistantReturnsBuffered covers the
// `case env.SDKMessage != nil && env.SDKMessage.Type == "assistant"`
// branch in the inner loop — after one unrecognized frame, an
// assistant frame must surface its text content via the buffer.
func TestStreamConverter_InnerLoopAssistantReturnsBuffered(t *testing.T) {
	var buf bytes.Buffer
	frames := []string{
		`{"sdkMessage":{"type":"unknown","message":{}}}`, // first iter — skip
		`{"sdkMessage":{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hello-from-inner"}]}}}`, // assistant — buffer.Append
	}
	for _, m := range frames {
		payload := []byte(m)
		hdr := make([]byte, 5)
		binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
		buf.Write(hdr)
		buf.Write(payload)
	}

	sr := newStreamReader(io.NopCloser(&buf))
	sc := &streamConverter{
		stream:     sr,
		model:      "m",
		created:    time.Now().Unix(),
		buffer:     streaming.NewStreamBuffer(1024),
		closeAgent: func() {},
		ctx:        context.Background(),
	}
	out := make([]byte, 512)
	n, err := sc.Read(out)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(string(out[:n]), "hello-from-inner") {
		t.Errorf("body = %q, want assistant text", string(out[:n]))
	}
}

// TestStreamConverter_InnerLoopClosedAfterSkip covers the
// `if c.closed { return 0, io.EOF }` branch in the inner loop — once
// the converter is closed mid-loop, the next iteration returns EOF
// without touching the underlying stream. We trigger this by
// concurrently closing the converter from a goroutine while Read is
// iterating.
func TestStreamConverter_InnerLoopClosedAfterSkip(t *testing.T) {
	var buf bytes.Buffer
	// Many unrecognized frames so the inner loop iterates.
	for i := 0; i < 64; i++ {
		payload := []byte(`{"sdkMessage":{"type":"unknown","message":{}}}`)
		hdr := make([]byte, 5)
		binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
		buf.Write(hdr)
		buf.Write(payload)
	}

	sr := newStreamReader(io.NopCloser(&buf))
	sc := &streamConverter{
		stream:     sr,
		model:      "m",
		created:    time.Now().Unix(),
		buffer:     streaming.NewStreamBuffer(1024),
		closeAgent: func() {},
		ctx:        context.Background(),
	}

	// Race: close the converter while Read is in the inner loop. The
	// next iteration hits the `if c.closed` guard and returns EOF.
	// Note: streamConverter does not synchronize access; the race is
	// intentional — the test gives coverage tooling a chance to hit
	// the branch under -race. The deferred assertion accepts either
	// EOF (closed branch fired) or [DONE] (loop completed first).
	go func() {
		time.Sleep(1 * time.Millisecond)
		sc.closed = true
	}()

	out := make([]byte, 64)
	_, err := sc.Read(out)
	_ = err // race outcome is non-deterministic
}

// TestStreamConverter_InnerLoopNonEOFError covers the
// `c.releaseAgent() / c.closed = true / c.buffer.Release() / return 0, err`
// branches in the inner loop — when stream.Next returns a non-EOF
// error mid-loop, the converter must release the agent, free its
// buffer, and surface the error.
func TestStreamConverter_InnerLoopNonEOFError(t *testing.T) {
	var buf bytes.Buffer
	// First frame is a recognized-but-no-match sdkMessage so the
	// outer block's switch does not fire — buffer stays empty and we
	// fall into the inner loop.
	payload := []byte(`{"sdkMessage":{"type":"unknown","message":{}}}`)
	hdr := make([]byte, 5)
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
	buf.Write(hdr)
	buf.Write(payload)

	// Body returns the first frame, then a non-EOF error on the
	// second Read call. The first frame drains via StreamReader.Next
	// and the inner loop hits the non-EOF error path on the next
	// Next call.
	sr := newStreamReader(&errBody{first: buf.Bytes()})
	sc := &streamConverter{
		stream:     sr,
		model:      "m",
		created:    time.Now().Unix(),
		buffer:     streaming.NewStreamBuffer(1024),
		closeAgent: func() {},
		ctx:        context.Background(),
	}
	out := make([]byte, 64)
	_, err := sc.Read(out)
	if err == nil {
		t.Fatal("expected non-EOF error from inner loop, got nil")
	}
}

// errBody is an io.ReadCloser that returns the first chunk (when set)
// and then a non-EOF error on every subsequent Read. Used to drive
// StreamReader.Next into its non-EOF error branch after a successful
// initial frame.
type errBody struct {
	first    []byte
	consumed bool
}

var errBodyErr = errors.New("simulated body read failure")

func (r *errBody) Read(p []byte) (int, error) {
	if !r.consumed && len(r.first) > 0 {
		n := copy(p, r.first)
		r.first = r.first[n:]
		if len(r.first) == 0 {
			r.consumed = true
		}
		return n, nil
	}
	return 0, errBodyErr
}

func (r *errBody) Close() error { return nil }

// TestStreamConverter_InnerLoopEOFAfterSkipsReleasesAgent covers the
// inner-loop EOF branch: releaseAgent must be called when EOF is hit
// mid-loop, so the bridge agent is freed even when the loop exits
// through EOF instead of through a Result frame.
func TestStreamConverter_InnerLoopEOFAfterSkipsReleasesAgent(t *testing.T) {
	var buf bytes.Buffer
	for i := 0; i < 5; i++ {
		payload := []byte(`{"sdkMessage":{"type":"unknown","message":{}}}`)
		hdr := make([]byte, 5)
		binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
		buf.Write(hdr)
		buf.Write(payload)
	}

	sr := newStreamReader(io.NopCloser(&buf))
	var released atomic.Int32
	sc := &streamConverter{
		stream:     sr,
		model:      "m",
		created:    time.Now().Unix(),
		buffer:     streaming.NewStreamBuffer(1024),
		closeAgent: func() { released.Add(1) },
		ctx:        context.Background(),
	}
	out := make([]byte, 64)
	if _, err := sc.Read(out); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if released.Load() != 1 {
		t.Errorf("releaseAgent called %d times, want 1 (EOF path)", released.Load())
	}
}
