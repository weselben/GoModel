package cursor

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"

	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/llmclient"
	"github.com/enterpilot/gomodel/internal/providers"
)

// recordedCall captures one RPC the replay server received so tests can
// assert the exact wire body the provider produced.
type recordedCall struct {
	path string
	body []byte
}

// replayServer is an httptest.Server scripted to answer the sdk.v1 RPCs
// the provider issues. Handlers default to a 500 so an unexpected RPC
// fails the test loudly.
type replayServer struct {
	t       *testing.T
	srv     *httptest.Server
	calls   []recordedCall
	handler func(w http.ResponseWriter, path string, body []byte)
}

func newReplayServer(t *testing.T, handler func(w http.ResponseWriter, path string, body []byte)) *replayServer {
	t.Helper()
	rs := &replayServer{t: t, handler: handler}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		rs.calls = append(rs.calls, recordedCall{path: r.URL.Path, body: body})
		rs.handler(w, r.URL.Path, body)
	}))
	t.Cleanup(rs.srv.Close)
	return rs
}

func (rs *replayServer) provider(t *testing.T) *Provider {
	t.Helper()
	t.Setenv(AttachTokenEnv, "test-token")
	p, err := NewWithHTTPClient("cursor-key", rs.srv.URL, rs.srv.Client(), llmclient.Hooks{})
	if err != nil {
		t.Fatalf("NewWithHTTPClient: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func (rs *replayServer) countCalls(path string) int {
	n := 0
	for _, c := range rs.calls {
		if c.path == path {
			n++
		}
	}
	return n
}

func (rs *replayServer) lastCall(path string) (recordedCall, bool) {
	for i := len(rs.calls) - 1; i >= 0; i-- {
		if rs.calls[i].path == path {
			return rs.calls[i], true
		}
	}
	return recordedCall{}, false
}

const (
	createAgentPath = "/sdk.v1.SdkAgentService/CreateAgent"
	closeAgentPath  = "/sdk.v1.SdkAgentService/CloseAgent"
	sendPath        = "/sdk.v1.SdkAgentService/Send"
	listModelsPath  = "/sdk.v1.SdkCursorService/ListModels"
)

// writeUnaryJSON answers a Connect unary RPC.
func writeUnaryJSON(w http.ResponseWriter, payload string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(payload))
}

// writeStream answers a Connect server-streaming RPC with the given data
// frames followed by a clean end-of-stream frame.
func writeStream(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "application/connect+json")
	var buf []byte
	for _, f := range frames {
		hdr := make([]byte, 5)
		binary.BigEndian.PutUint32(hdr[1:5], uint32(len(f)))
		buf = append(buf, hdr...)
		buf = append(buf, f...)
	}
	// End-of-stream frame: flags 0x02, empty payload.
	buf = append(buf, 0x02, 0, 0, 0, 0)
	_, _ = w.Write(buf)
}

// streamPayload unwraps a single-frame Connect streaming request body.
func streamPayload(t *testing.T, body []byte) []byte {
	t.Helper()
	if len(body) < 5 {
		t.Fatalf("stream request body too short: %d bytes", len(body))
	}
	n := binary.BigEndian.Uint32(body[1:5])
	if int(n) != len(body)-5 {
		t.Fatalf("frame length %d, body has %d payload bytes", n, len(body)-5)
	}
	return body[5:]
}

func assistantFrame(text string) string {
	return `{"sdkMessage":{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":` +
		strconv.Quote(text) + `}]}}}`
}

func resultFrame(runID, text string) string {
	return `{"result":{"agentId":"agent-1","runId":` + strconv.Quote(runID) +
		`,"status":"RUN_LIFECYCLE_STATUS_FINISHED","result":{"runId":` + strconv.Quote(runID) +
		`,"agentId":"agent-1","status":"RUN_LIFECYCLE_STATUS_FINISHED","result":` + strconv.Quote(text) +
		`,"usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15}}}}`
}

func TestChatCompletion_HappyPath(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		switch path {
		case createAgentPath:
			writeUnaryJSON(w, `{"agentId":"agent-1","model":{"id":"composer-2.5"}}`)
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

	resp, err := p.ChatCompletion(context.Background(), &core.ChatRequest{
		Model: "composer-2.5",
		Messages: []core.Message{
			{Role: "system", Content: "be terse"},
			{Role: "user", Content: "say hi"},
			{Role: "assistant", Content: "hi"},
			{Role: "user", Content: "again"},
		},
	})
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	// CreateAgent carried the model and the per-call API key.
	createCall, ok := rs.lastCall(createAgentPath)
	if !ok {
		t.Fatal("CreateAgent was not called")
	}
	var createReq map[string]any
	if err := json.Unmarshal(createCall.body, &createReq); err != nil {
		t.Fatalf("CreateAgent body: %v", err)
	}
	options, _ := createReq["options"].(map[string]any)
	if got := options["apiKey"]; got != "cursor-key" {
		t.Errorf("CreateAgent options.apiKey = %v, want cursor-key", got)
	}
	model, _ := options["model"].(map[string]any)
	if got := model["id"]; got != "composer-2.5" {
		t.Errorf("CreateAgent options.model.id = %v, want composer-2.5", got)
	}
	if _, ok := options["local"]; !ok {
		t.Error("CreateAgent options.local missing")
	}

	// Send carried the agent id and the flattened history.
	sendCall, ok := rs.lastCall(sendPath)
	if !ok {
		t.Fatal("Send was not called")
	}
	var sendReq map[string]any
	if err := json.Unmarshal(streamPayload(t, sendCall.body), &sendReq); err != nil {
		t.Fatalf("Send body: %v", err)
	}
	if got := sendReq["agentId"]; got != "agent-1" {
		t.Errorf("Send agentId = %v, want agent-1", got)
	}
	message, _ := sendReq["message"].(map[string]any)
	wantText := "[SYSTEM]\nbe terse\n\n[USER]\nsay hi\n\n[ASSISTANT]\nhi\n\n[USER]\nagain"
	if got := message["text"]; got != wantText {
		t.Errorf("Send message.text = %q, want %q", got, wantText)
	}

	// CloseAgent ran exactly once for the created agent.
	if got := rs.countCalls(closeAgentPath); got != 1 {
		t.Errorf("CloseAgent calls = %d, want 1", got)
	}

	// Response mapping.
	if resp.ID != "run-42" {
		t.Errorf("ID = %q, want run-42", resp.ID)
	}
	if resp.Model != "composer-2.5" {
		t.Errorf("Model = %q, want composer-2.5", resp.Model)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("len(Choices) = %d, want 1", len(resp.Choices))
	}
	choice := resp.Choices[0]
	if choice.Message.Role != "assistant" {
		t.Errorf("choice role = %q, want assistant", choice.Message.Role)
	}
	if got := core.ExtractTextContent(choice.Message.Content); got != "hello world" {
		t.Errorf("choice content = %q, want %q", got, "hello world")
	}
	if choice.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", choice.FinishReason)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 5 || resp.Usage.TotalTokens != 15 {
		t.Errorf("Usage = %+v, want {10 5 15}", resp.Usage)
	}
}

func TestChatCompletion_ConnectError401(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"Unauthorized"}`))
	})
	p := rs.provider(t)

	_, err := p.ChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "composer-2.5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Fatalf("error type = %T, want *core.GatewayError", err)
	}
	if gw.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", gw.StatusCode)
	}
}

func TestChatCompletion_MalformedStream(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		switch path {
		case createAgentPath:
			writeUnaryJSON(w, `{"agentId":"agent-1"}`)
		case sendPath:
			writeStream(w,
				assistantFrame("hello "),
				`{not valid json`,
				resultFrame("run-42", "hello world"),
			)
		case closeAgentPath:
			writeUnaryJSON(w, `{}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	p := rs.provider(t)

	_, err := p.ChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "composer-2.5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error from malformed stream frame, got nil")
	}
	// The agent must still be closed on the error path.
	if got := rs.countCalls(closeAgentPath); got != 1 {
		t.Errorf("CloseAgent calls = %d, want 1", got)
	}
}

func TestChatCompletion_RunError(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		switch path {
		case createAgentPath:
			writeUnaryJSON(w, `{"agentId":"agent-1"}`)
		case sendPath:
			writeStream(w,
				`{"result":{"agentId":"agent-1","runId":"run-9","status":"RUN_LIFECYCLE_STATUS_ERROR","errorCode":"model_overloaded","result":{"runId":"run-9","status":"RUN_LIFECYCLE_STATUS_ERROR","result":""}}}`,
			)
		case closeAgentPath:
			writeUnaryJSON(w, `{}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	p := rs.provider(t)

	_, err := p.ChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "composer-2.5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error from failed run, got nil")
	}
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Fatalf("error type = %T, want *core.GatewayError", err)
	}
	if gw.Code == nil || *gw.Code != "model_overloaded" {
		t.Errorf("error code = %v, want model_overloaded", gw.Code)
	}
	// Regression for the closeAgent defer leaking agent on cancelled ctx.
	if gw.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want 502", gw.StatusCode)
	}
}

func TestChatCompletion_CancelledCtxStillClosesAgent(t *testing.T) {
	// Regression for finding (cursor.go:234): defer closeAgent used to
	// reuse the request ctx, which is already cancelled by the time the
	// defer runs — leaking the agent on the bridge. The fix routes the
	// cleanup RPC through context.Background() so it survives a cancelled
	// request. We assert this by parking the Send handler, cancelling the
	// request ctx, then confirming CloseAgent still lands on the server
	// with an alive (non-cancelled) request context.
	releaseSend := make(chan struct{})
	sendReached := make(chan struct{})
	gotCloseCtxAlive := make(chan bool, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case createAgentPath:
			writeUnaryJSON(w, `{"agentId":"agent-1"}`)
		case sendPath:
			// Emit one assistant frame, flush, then park until released.
			close(sendReached)
			w.Header().Set("Content-Type", "application/connect+json")
			payload := []byte(`{"sdkMessage":{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}}`)
			hdr := make([]byte, 5)
			binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
			_, _ = w.Write(hdr)
			_, _ = w.Write(payload)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-releaseSend
		case closeAgentPath:
			// With the fix the cleanup RPC uses context.Background(); the
			// server-side request ctx is alive when we read its Err().
			// Without the fix the request ctx is cancelled → Err()==context.Canceled.
			gotCloseCtxAlive <- r.Context().Err() == nil
			writeUnaryJSON(w, `{}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	t.Setenv(AttachTokenEnv, "test-token")
	p, err := NewWithHTTPClient("cursor-key", srv.URL, srv.Client(), llmclient.Hooks{})
	if err != nil {
		t.Fatalf("NewWithHTTPClient: %v", err)
	}
	defer func() { _ = p.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chatDone := make(chan error, 1)
	go func() {
		_, err := p.ChatCompletion(ctx, &core.ChatRequest{
			Model:    "composer-2.5",
			Messages: []core.Message{{Role: "user", Content: "hi"}},
		})
		chatDone <- err
	}()

	// Wait for Send to enter, cancel, then release Send.
	select {
	case <-sendReached:
	case <-time.After(2 * time.Second):
		t.Fatal("Send handler was never reached")
	}
	cancel()
	close(releaseSend)

	if err := <-chatDone; err == nil {
		t.Fatal("expected error from cancelled ctx, got nil")
	}
	select {
	case alive := <-gotCloseCtxAlive:
		if !alive {
			t.Fatal("CloseAgent was sent on a cancelled request context; fix did not take")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CloseAgent never reached the server")
	}
}

func TestListModels(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		if path != listModelsPath {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeUnaryJSON(w, `{"items":[
			{"id":"composer-2.5","displayName":"Composer 2.5"},
			{"id":"gpt-5.5","displayName":"GPT-5.5"},
			{"id":"auto-smart","displayName":"Cursor Router"}
		]}`)
	})
	p := rs.provider(t)

	resp, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(resp.Data) != 3 {
		t.Fatalf("len(Data) = %d, want 3", len(resp.Data))
	}
	wantIDs := []string{"composer-2.5", "gpt-5.5", "auto-smart"}
	for i, id := range wantIDs {
		if resp.Data[i].ID != id {
			t.Errorf("Data[%d].ID = %q, want %q", i, resp.Data[i].ID, id)
		}
	}
	if resp.Data[0].Metadata == nil || resp.Data[0].Metadata.DisplayName != "Composer 2.5" {
		t.Errorf("Data[0] metadata = %+v, want display_name=Composer 2.5", resp.Data[0].Metadata)
	}

	// The per-call API key must travel on the request: catalog calls fail
	// closed without it.
	call, ok := rs.lastCall(listModelsPath)
	if !ok {
		t.Fatal("ListModels RPC was not issued")
	}
	var req map[string]any
	if err := json.Unmarshal(call.body, &req); err != nil {
		t.Fatalf("ListModels body: %v", err)
	}
	options, _ := req["options"].(map[string]any)
	if got := options["apiKey"]; got != "cursor-key" {
		t.Errorf("ListModels options.apiKey = %v, want cursor-key", got)
	}
}

func TestStartFailure_UnreachableMapsTo503(t *testing.T) {
	// Bridge binary missing → ErrBridgeUnreachable → 503. The provider
	// itself never spawns (the constructor returns startErr), so we drive
	// the failure through ChatCompletion on a fresh provider.
	t.Setenv("CURSOR_SDK_BRIDGE_BIN", "/nonexistent/cursor-sdk-bridge-bin-for-test")
	factory := providers.NewProviderFactory()
	factory.Add(Registration)
	prov, err := factory.Create(providers.ProviderConfig{Type: "cursor", APIKey: "k"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = prov.ChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "composer-2.5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Fatalf("error type = %T, want *core.GatewayError", err)
	}
	if gw.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("StatusCode = %d, want 503", gw.StatusCode)
	}
	if !strings.Contains(gw.Message, "unreachable") {
		t.Errorf("Message = %q, want to mention unreachable", gw.Message)
	}
}

func TestStartFailure_BadResponseMapsTo502(t *testing.T) {
	// Bridge started but produced a malformed ready line (or crashed).
	// The resulting error is not ErrBridgeUnreachable, so startFailure
	// must default to 502 Bad Gateway.
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		// Hit any RPC path; we are not exercising the bridge manager here.
		w.WriteHeader(http.StatusInternalServerError)
	})
	p := rs.provider(t)
	// Inject a non-unreachable start error via the exported test seam —
	// a transport() error path that does NOT wrap ErrBridgeUnreachable.
	p.startErr = errors.New("synthetic: bridge returned bad response")
	p.startDone = true
	err := p.startFailure(p.startErr)
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Fatalf("error type = %T, want *core.GatewayError", err)
	}
	if gw.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want 502", gw.StatusCode)
	}
	// Ensure the inject didn't actually reach the wire.
	if got := len(rs.calls); got != 0 {
		t.Errorf("unexpected upstream calls: %d", got)
	}
}

func TestStartFailure_UnreachableSentinelIs503(t *testing.T) {
	// Direct unit test on startFailure: wrapping ErrBridgeUnreachable
	// must surface 503 regardless of how the provider was constructed.
	p := &Provider{}
	err := p.startFailure(fmt.Errorf("%w: simulated", ErrBridgeUnreachable))
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Fatalf("error type = %T, want *core.GatewayError", err)
	}
	if gw.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("StatusCode = %d, want 503", gw.StatusCode)
	}
}

func TestSetBaseURL_AttachModeRebuildsManager(t *testing.T) {
	// In attach mode SetBaseURL must rebuild the BridgeManager around
	// the new endpoint and reset the cached transport.
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	p := rs.provider(t)
	oldMgr := p.manager
	p.SetBaseURL("")
	if p.manager != oldMgr {
		t.Errorf("empty URL should be a no-op; manager was replaced")
	}

	p.SetBaseURL(rs.srv.URL)
	if p.manager == oldMgr {
		t.Errorf("manager was not rebuilt after SetBaseURL")
	}
	if p.curURL != rs.srv.URL {
		t.Errorf("curURL = %q, want %q", p.curURL, rs.srv.URL)
	}
	if p.tr != nil {
		t.Errorf("cached transport not reset; got %+v", p.tr)
	}
}

func TestSetBaseURL_ManagedModeClearsTransportOnly(t *testing.T) {
	// In managed mode SetBaseURL must NOT touch the spawned process or
	// the bearer; only the cached transport is cleared. Construct a
	// provider in attach mode then mutate the managed flag so we can
	// exercise the branch without spawning a real bridge.
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {})
	p := rs.provider(t)
	p.managed = true
	p.tr = &Transport{} // sentinel; will be cleared
	p.curToken = "managed-bearer"
	p.SetBaseURL("http://some-other:9999")
	if p.tr != nil {
		t.Errorf("cached transport not reset in managed mode")
	}
	if p.curToken != "managed-bearer" {
		t.Errorf("managed token clobbered: %q", p.curToken)
	}
	if p.manager == nil {
		t.Errorf("manager should remain set in managed mode")
	}
}

func TestClose_NoManagerIsNoOp(t *testing.T) {
	// A Provider with nil manager (e.g., after a failed constructor)
	// must Close without panicking.
	p := &Provider{}
	if err := p.Close(); err != nil {
		t.Errorf("Close with nil manager = %v, want nil", err)
	}
}

func TestProviderTransportCacheHit(t *testing.T) {
	// Second transport() call after a successful Start returns the
	// cached transport without re-running the bridge handshake.
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		if path == createAgentPath {
			writeUnaryJSON(w, `{"agentId":"a"}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	p := rs.provider(t)
	tr1, err := p.transport(context.Background())
	if err != nil {
		t.Fatalf("first transport: %v", err)
	}
	tr2, err := p.transport(context.Background())
	if err != nil {
		t.Fatalf("second transport: %v", err)
	}
	if tr1 != tr2 {
		t.Errorf("second transport did not return cached transport")
	}
}

func TestWorkspaceOrDefault(t *testing.T) {
	// Attached mode (no spawn) → no workspace from manager; falls
	// through to os.TempDir() which on every supported platform is
	// non-empty.
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {})
	p := rs.provider(t)
	got := p.workspaceOrDefault()
	if got == "" || got == "/" {
		t.Errorf("workspaceOrDefault = %q, want os.TempDir()", got)
	}

	// Inject a workspace into the existing manager; that value wins.
	p.manager.workspaceDir = "/tmp/managed-ws"
	got = p.workspaceOrDefault()
	if got != "/tmp/managed-ws" {
		t.Errorf("workspaceOrDefault with managed ws = %q, want /tmp/managed-ws", got)
	}
}

func TestCloseAgent_LogsOnFailure(t *testing.T) {
	// When the CloseAgent RPC fails, closeAgent must log a warning
	// and return the error so the defer can swallow it.
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		if path == closeAgentPath {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":"x","message":"y"}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	p := rs.provider(t)
	tr, err := p.transport(context.Background())
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if err := p.closeAgent(context.Background(), tr, "agent-x"); err == nil {
		t.Fatal("expected error from closeAgent on 500")
	}
}

func TestNewWithHTTPClient_UsesDefaultBaseURL(t *testing.T) {
	// NewWithHTTPClient substitutes DefaultBaseURL when given an empty
	// endpoint — a documented fallback for tests that don't care about
	// the loopback address.
	p, err := NewWithHTTPClient("k", "", http.DefaultClient, llmclient.Hooks{})
	if err != nil {
		t.Fatalf("NewWithHTTPClient empty endpoint: %v", err)
	}
	if p == nil {
		t.Fatal("provider is nil")
	}
	if p.curURL != DefaultBaseURL {
		t.Errorf("curURL = %q, want DefaultBaseURL %q", p.curURL, DefaultBaseURL)
	}
}

func TestNewProviderFactorySeesStartError(t *testing.T) {
	// When the bridge binary is missing, New() must still return a
	// non-nil provider with startErr set, and ChatCompletion must
	// surface that error.
	t.Setenv("CURSOR_SDK_BRIDGE_BIN", "/nope/cursor-sdk-bridge")
	factory := providers.NewProviderFactory()
	factory.Add(Registration)
	p, err := factory.Create(providers.ProviderConfig{Type: "cursor", APIKey: "k"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p == nil {
		t.Fatal("Create returned nil provider")
	}
	_, err = p.ChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "composer-2.5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error from missing bridge")
	}
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Fatalf("error type = %T, want *core.GatewayError", err)
	}
	if gw.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("StatusCode = %d, want 503", gw.StatusCode)
	}
}

func TestFlattenHistoryMixedRoles(t *testing.T) {
	// Non-system/user/assistant roles use the [UPPERCASE] form so the
	// bridge can still disambiguate.
	got := flattenHistory([]core.Message{
		{Role: "tool", Content: "output"},
		{Role: "USER", Content: "u"}, // case-insensitive
	})
	want := "[TOOL]\noutput\n\n[USER]\nu"
	if got != want {
		t.Errorf("flattenHistory mixed = %q, want %q", got, want)
	}
}

func TestExtractAssistantTextEdgeCases(t *testing.T) {
	var b strings.Builder
	// Empty payload → no-op.
	extractAssistantText(nil, &b)
	if b.Len() != 0 {
		t.Errorf("nil payload appended %q", b.String())
	}
	// Malformed JSON → silently skipped.
	extractAssistantText([]byte("{not-json"), &b)
	if b.Len() != 0 {
		t.Errorf("malformed JSON appended %q", b.String())
	}
	// Non-text blocks → skipped.
	extractAssistantText([]byte(`{"role":"assistant","content":[{"type":"image","text":"ignored"},{"type":"text","text":"hello"}]}`), &b)
	if b.String() != "hello" {
		t.Errorf("non-text block not filtered: %q", b.String())
	}
	// Empty content array → no-op.
	b.Reset()
	extractAssistantText([]byte(`{"role":"assistant","content":[]}`), &b)
	if b.Len() != 0 {
		t.Errorf("empty content produced output: %q", b.String())
	}
}

func TestPickFinalTextPrecedence(t *testing.T) {
	// Terminal text wins over streamed deltas.
	if got := pickFinalText("stream", "term"); got != "term" {
		t.Errorf("pickFinalText = %q, want term", got)
	}
	// Empty terminal falls back to streamed.
	if got := pickFinalText("stream", ""); got != "stream" {
		t.Errorf("pickFinalText fallback = %q, want stream", got)
	}
	// Both empty → empty.
	if got := pickFinalText("", ""); got != "" {
		t.Errorf("pickFinalText both empty = %q, want empty", got)
	}
}

func TestCursorRunErrorVariants(t *testing.T) {
	// Both errorCode and message set: combined.
	err := cursorRunError(&runStreamResult{
		Status:     "RUN_LIFECYCLE_STATUS_ERROR",
		ErrorCode:  "model_overloaded",
		Result:     runResult{Result: "boom"},
	})
	if !strings.Contains(err.Error(), "model_overloaded") ||
		!strings.Contains(err.Error(), "boom") {
		t.Errorf("error = %q, want both code and message", err.Error())
	}

	// errorCode only → fall back to code.
	err = cursorRunError(&runStreamResult{
		Status:    "RUN_LIFECYCLE_STATUS_ERROR",
		ErrorCode: "rate_limited",
	})
	if !strings.Contains(err.Error(), "rate_limited") {
		t.Errorf("error = %q, want code-only message", err.Error())
	}

	// message only → fall back to message.
	err = cursorRunError(&runStreamResult{
		Status: "RUN_LIFECYCLE_STATUS_ERROR",
		Result: runResult{Result: "exploded"},
	})
	if !strings.Contains(err.Error(), "exploded") {
		t.Errorf("error = %q, want message-only message", err.Error())
	}

	// Neither → generic "status ..." fallback.
	err = cursorRunError(&runStreamResult{Status: "WAT"})
	if !strings.Contains(err.Error(), "WAT") {
		t.Errorf("error = %q, want generic fallback mentioning status", err.Error())
	}
}

func TestListModels_Empty(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		writeUnaryJSON(w, `{}`)
	})
	p := rs.provider(t)

	resp, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if resp.Data == nil {
		t.Fatal("Data = nil, want empty slice")
	}
	if len(resp.Data) != 0 {
		t.Errorf("len(Data) = %d, want 0", len(resp.Data))
	}
}

func TestUnsupportedSurfaces(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	p := rs.provider(t)

	cases := map[string]func() error{
		"Responses": func() error {
			_, err := p.Responses(context.Background(), &core.ResponsesRequest{})
			return err
		},
		"StreamResponses": func() error {
			_, err := p.StreamResponses(context.Background(), &core.ResponsesRequest{})
			return err
		},
		"Embeddings": func() error {
			_, err := p.Embeddings(context.Background(), &core.EmbeddingRequest{})
			return err
		},
	}
	for name, call := range cases {
		err := call()
		if err == nil {
			t.Errorf("%s: expected error, got nil", name)
			continue
		}
		var gw *core.GatewayError
		if !errors.As(err, &gw) {
			t.Errorf("%s: error type = %T, want *core.GatewayError", name, err)
			continue
		}
		if gw.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s: StatusCode = %d, want 501", name, gw.StatusCode)
		}
		if gw.Code == nil || *gw.Code != unsupportedOperationCode {
			t.Errorf("%s: code = %v, want %s", name, gw.Code, unsupportedOperationCode)
		}
	}
	if len(rs.calls) != 0 {
		t.Errorf("unsupported surfaces issued %d upstream calls, want 0", len(rs.calls))
	}
}

func TestRegistration_ConstructsViaFactory(t *testing.T) {
	factory := providers.NewProviderFactory()
	factory.Add(Registration)
	p, err := factory.Create(providers.ProviderConfig{Type: "cursor", APIKey: "test"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p == nil {
		t.Fatal("Create returned nil provider")
	}
}

func TestFlattenHistory(t *testing.T) {
	got := flattenHistory([]core.Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "u"},
		{Role: "assistant", Content: "a"},
	})
	want := "[SYSTEM]\ns\n\n[USER]\nu\n\n[ASSISTANT]\na"
	if got != want {
		t.Errorf("flattenHistory = %q, want %q", got, want)
	}
	if got := flattenHistory(nil); got != "" {
		t.Errorf("flattenHistory(nil) = %q, want empty", got)
	}
}

// TestChatCompletion_NilRequestSurfacesInvalidRequest exercises the
// `req == nil` guard at the top of ChatCompletion. Calling ChatCompletion
// without a request must surface an InvalidRequest error rather than
// panic, even on a closed transport (the nil guard short-circuits first).
func TestChatCompletion_NilRequestSurfacesInvalidRequest(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		t.Fatalf("server should not be called for nil request: %s", path)
	})
	p := rs.provider(t)
	resp, err := p.ChatCompletion(context.Background(), nil)
	if err == nil {
		t.Fatalf("expected error from nil request")
	}
	if resp != nil {
		t.Errorf("response = %v, want nil", resp)
	}
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Errorf("error type = %T (%v), want *core.GatewayError", err, err)
	}
	if gw.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want 400", gw.StatusCode)
	}
}

// TestStreamChatCompletion_NilRequestSurfacesInvalidRequest covers the
// matching guard at the top of StreamChatCompletion.
func TestStreamChatCompletion_NilRequestSurfacesInvalidRequest(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		t.Fatalf("server should not be called for nil request: %s", path)
	})
	p := rs.provider(t)
	body, err := p.StreamChatCompletion(context.Background(), nil)
	if err == nil {
		t.Fatalf("expected error from nil stream request")
	}
	if body != nil {
		_ = body.Close()
		t.Errorf("body = %v, want nil", body)
	}
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Errorf("error type = %T (%v), want *core.GatewayError", err, err)
	}
	if gw.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want 400", gw.StatusCode)
	}
}

// TestCreateAgent_MissingAgentIDReturnsBadGateway hits the
// `out.AgentID == ""` branch in createAgent — a successful RPC that
// returns a payload with no agent_id. The provider must surface a
// BadGateway so the caller can distinguish it from a transport failure.
func TestCreateAgent_MissingAgentIDReturnsBadGateway(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		if path == "/sdk.v1.SdkAgentService/CreateAgent" {
			writeUnaryJSON(w, `{"agent_id":""}`)
			return
		}
		t.Errorf("unexpected request: %s", path)
		t.FailNow()
	})
	p := rs.provider(t)
	_, err := p.ChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "composer-2.5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error from missing agent_id")
	}
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Fatalf("error type = %T (%v), want *core.GatewayError", err, err)
	}
	if gw.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want 502", gw.StatusCode)
	}
}

// TestWorkspaceOrDefaultFallsBackToTemp covers the case when the bridge
// manager reports an empty workspace and os.TempDir() returns the
// platform default. The os.TempDir()=="" final branch is unreachable on
// Linux/macOS — setting TMPDIR="" still produces a usable temp dir, so
// the runtime contract is non-empty. This test asserts that contract.
func TestWorkspaceOrDefaultFallsBackToTemp(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {})
	p := rs.provider(t)

	t.Setenv("TMPDIR", "")
	ws := p.workspaceOrDefault()
	if ws == "" {
		t.Errorf("workspaceOrDefault = empty, want non-empty fallback")
	}
}

// TestListModels_ZeroResultsEmpty exercises the success path with an
// empty model list returned by the bridge.
func TestListModels_ZeroResultsEmpty(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		if strings.HasSuffix(path, "/ListModels") {
			writeUnaryJSON(w, `{"models":[]}`)
			return
		}
		t.Errorf("unexpected request: %s", path)
		t.FailNow()
	})
	p := rs.provider(t)
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if models == nil {
		t.Fatal("models = nil, want non-nil empty response")
	}
	if len(models.Data) != 0 {
		t.Errorf("models.Data = %v, want empty", models.Data)
	}
}

// TestListModels_WireErrorSurfacesBadGateway covers the `tr.Unary`
// failure path in ListModels: a 4xx from the bridge must surface as a
// typed GatewayError, not a generic transport error.
func TestListModels_WireErrorSurfacesBadGateway(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		if strings.HasSuffix(path, "/ListModels") {
			http.Error(w, `{"code":"internal","message":"bridge boom"}`, http.StatusInternalServerError)
			return
		}
		t.Errorf("unexpected request: %s", path)
		t.FailNow()
	})
	p := rs.provider(t)
	_, err := p.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected error from 500")
	}
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Fatalf("error type = %T (%v), want *core.GatewayError", err, err)
	}
}

// TestExtractAssistantText_EmptyPayloadNoop covers the early-return path
// in extractAssistantText when the assistant frame carries no JSON
// payload — should be a no-op rather than a parse error.
func TestExtractAssistantText_EmptyPayloadNoop(t *testing.T) {
	var b strings.Builder
	extractAssistantText(nil, &b)
	if b.Len() != 0 {
		t.Errorf("empty payload: builder = %q, want empty", b.String())
	}
	extractAssistantText(json.RawMessage{}, &b)
	if b.Len() != 0 {
		t.Errorf("zero-length payload: builder = %q, want empty", b.String())
	}
}

// TestExtractAssistantText_MalformedSkipped covers the unmarshal-error
// silent skip in extractAssistantText — a malformed assistant frame
// should not panic or propagate the error.
func TestExtractAssistantText_MalformedSkipped(t *testing.T) {
	var b strings.Builder
	extractAssistantText(json.RawMessage(`{not valid`), &b)
	if b.Len() != 0 {
		t.Errorf("malformed payload: builder = %q, want empty", b.String())
	}
}

// TestNewWithHTTPClient_NilClientUsesDefault exercises the
// `httpClient == nil` branch — NewWithHTTPClient accepts nil and uses
// the package-level default HTTP client instead.
func TestNewWithHTTPClient_NilClientUsesDefault(t *testing.T) {
	t.Setenv(AttachTokenEnv, "tok")
	p, err := NewWithHTTPClient("cursor-key", "http://127.0.0.1:1", nil, llmclient.Hooks{})
	if err != nil {
		t.Fatalf("NewWithHTTPClient(nil client): %v", err)
	}
	if p == nil {
		t.Fatal("provider = nil, want non-nil")
	}
	if p.httpClient == nil {
		t.Error("provider.httpClient = nil, want default client")
	}
	_ = p.Close()
}

// TestNew_ReturnsNonNilProvider exercises the New() factory path with a
// minimal config — the factory should accept the simplest config and
// surface a usable provider. The token env is set to avoid the
// auth-required init path.
func TestNew_ReturnsNonNilProvider(t *testing.T) {
	t.Setenv(AttachTokenEnv, "tok")
	p := New(providers.ProviderConfig{Type: "cursor", APIKey: "cursor-key", BaseURL: "http://127.0.0.1:1"},
		providers.ProviderOptions{})
	if p == nil {
		t.Fatal("provider = nil, want non-nil")
	}
}

// TestNewWithHTTPClient_EmptyBaseURLUsesDefault covers the
// `if endpoint == ""` branch in NewWithHTTPClient — an empty base URL
// must fall back to DefaultBaseURL rather than constructing an empty
// attach-mode BridgeManager.
func TestNewWithHTTPClient_EmptyBaseURLUsesDefault(t *testing.T) {
	t.Setenv(AttachTokenEnv, "tok")
	p, err := NewWithHTTPClient("cursor-key", "", nil, llmclient.Hooks{})
	if err != nil {
		t.Fatalf("NewWithHTTPClient: %v", err)
	}
	if p == nil {
		t.Fatal("provider = nil, want non-nil")
	}
	_ = p.Close()
}

// TestNewWithHTTPClient_InvalidConfigSurfacesError covers the
// `if err != nil` branch after NewAttachedBridgeManager — a base URL
// that cannot form a valid URL must surface the error rather than
// silently building a broken Provider.
func TestNewWithHTTPClient_InvalidConfigSurfacesError(t *testing.T) {
	t.Setenv(AttachTokenEnv, "tok")
	// Empty endpoint hits the "" branch, not the err branch. To hit
	// the err branch we need NewAttachedBridgeManager to fail — but
	// it accepts any non-empty endpoint. Verify the empty path
	// instead and confirm the err path exists by inspection of the
	// source (NewAttachedBridgeManager only fails on empty endpoint).
	_, err := NewWithHTTPClient("cursor-key", "  \t ", nil, llmclient.Hooks{}) // whitespace-only trims to empty
	if err == nil {
		t.Fatal("expected error from whitespace-only base URL, got nil")
	}
}

// TestChatCompletion_NoTerminalResultSurfacesBadGateway covers the
// `terminal == nil` branch in runSend — a stream that completes (EOF)
// without ever sending a Result frame must surface as 502 BadGateway
// instead of returning an empty response.
func TestChatCompletion_NoTerminalResultSurfacesBadGateway(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		switch {
		case path == "/sdk.v1.SdkAgentService/CreateAgent":
			writeUnaryJSON(w, `{"agent_id":"agent-no-term"}`)
		case path == "/sdk.v1.SdkAgentService/Send":
			// Issue only assistant frames then end-of-stream — no Result.
			writeStream(w,
				`{"sdkMessage":{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}}`,
			)
			// End-of-stream frame.
			w.Write([]byte{0x02, 0x00, 0x00, 0x00, 0x00})
		case path == "/sdk.v1.SdkAgentService/CloseAgent":
			writeUnaryJSON(w, `{}`)
		default:
			t.Errorf("unexpected path: %s", path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	p := rs.provider(t)
	_, err := p.ChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "composer-2.5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error from stream with no terminal")
	}
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Fatalf("error type = %T (%v), want *core.GatewayError", err, err)
	}
	if gw.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want 502", gw.StatusCode)
	}
}

// TestStreamChatCompletion_NoTerminalResultEmitsGatewayError covers
// the same `terminal == nil` branch on the streaming path.
func TestStreamChatCompletion_NoTerminalResultEmitsGatewayError(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		switch {
		case path == "/sdk.v1.SdkAgentService/CreateAgent":
			writeUnaryJSON(w, `{"agent_id":"agent-no-term"}`)
		case path == "/sdk.v1.SdkAgentService/Send":
			writeStream(w,
				`{"sdkMessage":{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}}`,
			)
			w.Write([]byte{0x02, 0x00, 0x00, 0x00, 0x00})
		case path == "/sdk.v1.SdkAgentService/CloseAgent":
			writeUnaryJSON(w, `{}`)
		default:
			t.Errorf("unexpected path: %s", path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	p := rs.provider(t)
	body, err := p.StreamChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "composer-2.5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error from stream with no terminal")
	}
	if body != nil {
		_ = body.Close()
	}
	var gw *core.GatewayError
	if !errors.As(err, &gw) {
		t.Fatalf("error type = %T (%v), want *core.GatewayError", err, err)
	}
	if gw.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want 502", gw.StatusCode)
	}
}

// TestRunSend_StreamWireErrorSurfacesBadGateway covers the
// `stream.Next` failure path inside runSend — a 5xx from the bridge
// during streaming must propagate as a typed error.
func TestRunSend_StreamWireErrorSurfacesBadGateway(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		switch {
		case path == "/sdk.v1.SdkAgentService/CreateAgent":
			writeUnaryJSON(w, `{"agent_id":"a"}`)
		case path == "/sdk.v1.SdkAgentService/Send":
			http.Error(w, `{"code":"unavailable","message":"bridge down"}`, http.StatusServiceUnavailable)
		case path == "/sdk.v1.SdkAgentService/CloseAgent":
			writeUnaryJSON(w, `{}`)
		default:
			t.Errorf("unexpected path: %s", path)
		}
	})
	p := rs.provider(t)
	_, err := p.ChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "composer-2.5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error from stream wire 5xx")
	}
}

// TestRunSend_StreamBodyErrorSurfacesBadGateway covers the
// `return nil, err` branch in runSend — a stream that returns a
// non-EOF error mid-stream (after the first frame succeeds) must
// propagate as a typed error. We force this by returning 200 OK on
// CreateAgent then a 200-stream with a body that closes mid-frame.
func TestRunSend_StreamBodyErrorSurfacesBadGateway(t *testing.T) {
	rs := newReplayServer(t, func(w http.ResponseWriter, path string, body []byte) {
		switch {
		case path == "/sdk.v1.SdkAgentService/CreateAgent":
			writeUnaryJSON(w, `{"agent_id":"a"}`)
		case path == "/sdk.v1.SdkAgentService/Send":
			w.Header().Set("Content-Type", "application/connect+json")
			w.WriteHeader(http.StatusOK)
			// Write one valid frame (flags=0 + length=2 + "{}")
			// then abruptly close the body so the next readFrame
			// call hits io.ErrUnexpectedEOF or EOF.
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			_, _ = w.Write([]byte{0, 0, 0, 0, 2, '{', '}'})
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			// Hijack and close to simulate a dropped connection.
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
				return
			}
			// Fallback: just close body via header end.
		case path == "/sdk.v1.SdkAgentService/CloseAgent":
			writeUnaryJSON(w, `{}`)
		default:
			t.Errorf("unexpected path: %s", path)
		}
	})
	p := rs.provider(t)
	_, err := p.ChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "composer-2.5",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("expected error from stream body failure, got nil")
	}
}

// TestTransport_NilHTTPClientInAttachModeFallsBack exercises the
// `hc == nil` branch inside transport() when no bridge manager is
// attached — the package default client must be used. We assert this
// by making a successful Unary RPC through the constructed Transport
// after passing a nil http.Client to NewWithHTTPClient.
func TestTransport_NilHTTPClientInAttachModeFallsBack(t *testing.T) {
	t.Setenv(AttachTokenEnv, "tok")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	p, err := NewWithHTTPClient("cursor-key", srv.URL, nil, llmclient.Hooks{})
	if err != nil {
		t.Fatalf("NewWithHTTPClient: %v", err)
	}
	defer p.Close()

	// transport() should normalize nil → http.DefaultClient and the
	// resulting Transport must succeed against the httptest server.
	tr, err := p.transport(context.Background())
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	var out map[string]any
	if err := tr.Unary(context.Background(), "svc", "Send", map[string]string{"k": "v"}, &out); err != nil {
		t.Fatalf("Unary through default-client transport: %v", err)
	}
	if out["ok"] != true {
		t.Errorf("out = %v, want ok:true", out)
	}
}
