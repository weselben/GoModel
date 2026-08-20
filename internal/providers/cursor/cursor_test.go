package cursor

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

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
		"StreamChatCompletion": func() error {
			_, err := p.StreamChatCompletion(context.Background(), &core.ChatRequest{})
			return err
		},
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
