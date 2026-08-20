package cursor

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/enterpilot/gomodel/internal/core"
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
