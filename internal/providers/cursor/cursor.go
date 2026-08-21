package cursor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"

	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/llmclient"
	"github.com/enterpilot/gomodel/internal/providers"
)

// DefaultBaseURL is the loopback endpoint the controlled bridge spawn
// listens on. The bridge prints the actual endpoint on its ready line
// (typically an ephemeral port); this constant is only used as a fallback
// when SetBaseURL is never called and the user runs an externally-managed
// bridge on the conventional port.
const DefaultBaseURL = "http://127.0.0.1:32123"

// AttachTokenEnv is the env var NewAttachedBridgeManager reads the bearer
// from on Start. Surfaced as a constant so contract tests can set the
// env var without restating the string.
const AttachTokenEnv = "CURSOR_BRIDGE_TOKEN"

// Service and method names must match the bridge's URL route table.
const (
	svcAgent  = "SdkAgentService"
	svcCursor = "SdkCursorService"

	methodCreateAgent = "CreateAgent"
	methodCloseAgent  = "CloseAgent"
	methodSend        = "Send"
	methodListModels  = "ListModels"
)

// Registration plugs the cursor provider into the factory. The DefaultBaseURL
// is the loopback address the embedded bridge listens on; it is consulted
// by NewWithHTTPClient (attach mode for tests). Production mode spawns the
// bridge on its own ephemeral port and ignores cfg.BaseURL.
var Registration = providers.Registration{
	Type: "cursor",
	New:  New,
	Discovery: providers.DiscoveryConfig{
		DefaultBaseURL: DefaultBaseURL,
	},
}

// Provider is a GoModel core.Provider that routes OpenAI-style chat
// completions through a local cursor-sdk-bridge connected to a user's
// Cursor subscription. Each request creates a fresh bridge agent, sends
// the flattened message history as a single UserMessage, drains the run
// stream for assistant text, and closes the agent before returning.
type Provider struct {
	// Whether to spawn the bridge subprocess on first RPC (production) or
	// attach to an externally-managed endpoint (contract tests).
	managed bool
	// BridgeManager start is deferred to the first RPC: spawning takes a
	// measurable amount of time, and provider construction must never
	// block on process startup.
	manager *BridgeManager
	// mu guards the lazy-start state and the cached transport below.
	mu        sync.Mutex
	startDone bool
	startErr  error
	// Cached transport built from the (endpoint, token) returned by Start.
	// Reset by SetBaseURL so a new endpoint is picked up on the next RPC.
	tr       *Transport
	curURL   string
	curToken string
	// Per-call API key forwarded on options.apiKey. The bridge fails
	// catalog calls closed when it is absent, so it travels on every
	// CreateAgent and ListModels request.
	apiKey string
	// Optional http.Client for the contract test seam. nil == http.DefaultClient.
	httpClient *http.Client
}

var _ core.Provider = (*Provider)(nil)

// New wires a production Provider: spawn-mode BridgeManager, default
// http.Client, default Transport. The bridge is not started until the
// first RPC.
func New(cfg providers.ProviderConfig, opts providers.ProviderOptions) core.Provider {
	_ = opts // cursor has no resilience/hooks wiring yet; kept for the factory signature.
	p := &Provider{
		managed: true,
		apiKey:  cfg.APIKey,
	}
	bm, err := NewManagedBridgeManager(cfg.APIKey)
	if err != nil {
		// Bridge binary resolution can fail at construction (binary
		// missing from PATH, CURSOR_SDK_BRIDGE_BIN unset). Defer the
		// failure to the first RPC so provider registration never panics
		// on boot.
		p.startErr = err
		p.startDone = true
		return p
	}
	p.manager = bm
	return p
}

// NewWithHTTPClient is the contract-test seam. It is attach-mode: no
// subprocess is spawned; the bridge is whatever the test httptest server
// fronts. The configured base URL (or DefaultBaseURL when empty) is used
// until SetBaseURL overrides it. The bearer token is read from the
// AttachTokenEnv env var on Start.
func NewWithHTTPClient(apiKey string, baseURL string, httpClient *http.Client, hooks llmclient.Hooks) (*Provider, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	endpoint := baseURL
	if endpoint == "" {
		endpoint = DefaultBaseURL
	}
	bm, err := NewAttachedBridgeManager(endpoint, AttachTokenEnv)
	if err != nil {
		return nil, err
	}
	return &Provider{
		managed:    false,
		manager:    bm,
		apiKey:     apiKey,
		httpClient: httpClient,
		curURL:     endpoint,
		// hooks is reserved for future observability wiring; accepted on
		// the signature so callers can swap it in without breaking.
	}, nil
}

// SetBaseURL swaps the upstream endpoint and resets the lazy-start state
// so the next RPC re-runs the bridge handshake against the new URL. In
// attach mode the BridgeManager is rebuilt around the new endpoint; in
// managed mode the spawned process keeps its own endpoint and only the
// cached transport is dropped (startDone and the bearer are preserved).
func (p *Provider) SetBaseURL(url string) {
	if url == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.managed {
		bm, err := NewAttachedBridgeManager(url, AttachTokenEnv)
		if err == nil {
			p.manager = bm
			p.startDone = false
			p.startErr = nil
		}
		// Attach mode owns the URL; the bridge is whatever the operator
		// pointed at.
		p.curURL = url
		p.tr = nil
		p.curToken = ""
		return
	}
	// Managed mode: the spawned process keeps its own endpoint and its
	// own bearer; do NOT clobber the token and do NOT rewrite curURL to
	// the user-supplied value. Just drop the cached transport so the
	// next RPC rebuilds it from the live (manager.Start) endpoint.
	p.tr = nil
}

// Close shuts down the bridge if one was started. Idempotent and safe to
// defer.
func (p *Provider) Close() error {
	p.mu.Lock()
	m := p.manager
	p.mu.Unlock()
	if m == nil {
		return nil
	}
	return m.Close()
}

// transport lazily starts the bridge and returns a Transport bound to the
// endpoint+token pair. It is the single chokepoint for the bridge
// handshake on the RPC path.
func (p *Provider) transport(ctx context.Context) (*Transport, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.startDone {
		url, tok, err := p.manager.Start(ctx)
		if err != nil {
			p.startErr = err
		} else {
			p.curURL = url
			p.curToken = tok
		}
		p.startDone = true
	}
	if p.startErr != nil {
		return nil, p.startErr
	}
	if p.tr != nil {
		return p.tr, nil
	}
	hc := p.httpClient
	if hc == nil {
		hc = http.DefaultClient
	}
	p.tr = NewTransport(hc, p.curURL, p.curToken)
	return p.tr, nil
}

// ChatCompletion runs a single non-streaming turn:
//
//  1. Lazy-start the bridge.
//  2. CreateAgent with the requested model and the connection's API key.
//  3. Send a UserMessage carrying the flattened conversation history.
//  4. Drain the stream until the terminal result frame arrives, collecting
//     assistant text deltas.
//  5. CloseAgent (deferred) so the bridge releases local resources.
func (p *Provider) ChatCompletion(ctx context.Context, req *core.ChatRequest) (*core.ChatResponse, error) {
	if req == nil {
		return nil, core.NewInvalidRequestError("cursor: chat request is required", nil)
	}
	tr, err := p.transport(ctx)
	if err != nil {
		return nil, p.startFailure(err)
	}

	agentID, err := p.createAgent(ctx, tr, req.Model)
	if err != nil {
		return nil, err
	}
	// Background context for the cleanup RPC: the request ctx is often
	// already cancelled by the time defer runs (client disconnect, idle
	// timeout), and a CloseAgent cancelled by ctx leaves the bridge agent
	// leaked until the bridge itself shuts down. Mirror StreamChatCompletion's
	// agentCloser (lines 343-345).
	defer func() { _ = p.closeAgent(context.Background(), tr, agentID) }()

	resp, err := p.runSend(ctx, tr, agentID, req)
	if err != nil {
		return nil, err
	}
	resp.Model = req.Model
	return resp, nil
}

// createAgent calls CreateAgent and returns the new agent_id.
func (p *Provider) createAgent(ctx context.Context, tr *Transport, model string) (string, error) {
	body := createAgentRequest{
		Options: agentOptions{
			Model:  modelSelection{ID: model},
			APIKey: p.apiKey,
			Local: &localAgentOptions{
				CWD: []string{p.workspaceOrDefault()},
			},
		},
	}
	var out createAgentResponse
	if err := tr.Unary(ctx, svcAgent, methodCreateAgent, &body, &out); err != nil {
		return "", err
	}
	if out.AgentID == "" {
		return "", core.NewProviderError("cursor", http.StatusBadGateway,
			"cursor: CreateAgent response missing agentId", nil)
	}
	return out.AgentID, nil
}

// closeAgent is best-effort: a failure to release the agent is logged via
// slog and returned as an error so callers can log it with context. Never
// propagated as a user-visible error — the user-visible response is
// already on the wire by the time defer Close runs.
func (p *Provider) closeAgent(ctx context.Context, tr *Transport, agentID string) error {
	body := closeAgentRequest{AgentID: agentID}
	var out closeAgentResponse
	if err := tr.Unary(ctx, svcAgent, methodCloseAgent, &body, &out); err != nil {
		slog.Warn("cursor: CloseAgent failed; bridge may leak the agent until shutdown",
			"agent_id", agentID, "err", err)
		return err
	}
	return nil
}

// runSend issues Send and drains the stream. The terminal result frame is
// the source of the final assistant text and the run id.
func (p *Provider) runSend(ctx context.Context, tr *Transport, agentID string, req *core.ChatRequest) (*core.ChatResponse, error) {
	body := sendRequest{
		AgentID: agentID,
		Message: userMessage{Text: flattenHistory(req.Messages)},
	}
	stream, err := tr.Stream(ctx, svcAgent, methodSend, &body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()

	var text strings.Builder
	var terminal *runStreamResult
	for {
		frame, err := stream.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		env := runStreamEnvelope{}
		if err := json.Unmarshal(frame, &env); err != nil {
			return nil, core.NewProviderError("cursor", http.StatusBadGateway,
				"cursor: decode stream frame: "+err.Error(), err)
		}
		switch {
		case env.SDKMessage != nil && env.SDKMessage.Type == "assistant":
			extractAssistantText(env.SDKMessage.Message, &text)
		case env.Result != nil:
			terminal = env.Result
		}
	}

	if terminal == nil {
		return nil, core.NewProviderError("cursor", http.StatusBadGateway,
			"cursor: stream ended without a terminal result frame", nil)
	}
	if !terminalStatusOK(terminal.Status) {
		return nil, cursorRunError(terminal)
	}

	resp := &core.ChatResponse{
		ID:      terminal.Result.RunID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Choices: []core.Choice{{
			Index: 0,
			Message: core.ResponseMessage{
				Role:    "assistant",
				Content: pickFinalText(text.String(), terminal.Result.Result),
			},
			FinishReason: "stop",
		}},
	}
	if u := terminal.Result.Usage; u != nil {
		resp.Usage = core.Usage{
			PromptTokens:     int(u.InputTokens),
			CompletionTokens: int(u.OutputTokens),
			TotalTokens:      int(u.TotalTokens),
		}
	}
	return resp, nil
}

// StreamChatCompletion runs a single streaming turn:
//
//  1. Lazy-start the bridge.
//  2. CreateAgent with the requested model and the connection's API key.
//  3. Send a UserMessage carrying the flattened conversation history.
//  4. Wrap the resulting Connect frame stream in a streamConverter that
//     renders each frame as OpenAI chat.completion.chunk SSE, releasing
//     the agent on terminal frame, error, or explicit Close.
func (p *Provider) StreamChatCompletion(ctx context.Context, req *core.ChatRequest) (io.ReadCloser, error) {
	if req == nil {
		return nil, core.NewInvalidRequestError("cursor: chat request is required", nil)
	}
	tr, err := p.transport(ctx)
	if err != nil {
		return nil, p.startFailure(err)
	}
	agentID, err := p.createAgent(ctx, tr, req.Model)
	if err != nil {
		return nil, err
	}
	body := sendRequest{
		AgentID: agentID,
		Message: userMessage{Text: flattenHistory(req.Messages)},
	}
	stream, err := tr.Stream(ctx, svcAgent, methodSend, &body)
	if err != nil {
		// Best-effort release: the caller never received a body, so any
		// leaked agent would persist until the bridge shuts down.
		_ = p.closeAgent(context.Background(), tr, agentID)
		return nil, err
	}
	agentCloser := func() {
		_ = p.closeAgent(context.Background(), tr, agentID)
	}
	return newStreamConverter(ctx, stream, req.Model, agentCloser), nil
}

// ListModels calls SdkCursorService.ListModels with a per-call api_key.
// The bridge does not fall back to its env var for catalog calls (see
// docs/services.md), so the configured key is required even when the
// bridge was launched with CURSOR_API_KEY.
func (p *Provider) ListModels(ctx context.Context) (*core.ModelsResponse, error) {
	tr, err := p.transport(ctx)
	if err != nil {
		return nil, p.startFailure(err)
	}
	body := listModelsRequest{
		Options: cursorRequestOptions{APIKey: p.apiKey},
	}
	var out listModelsResponse
	if err := tr.Unary(ctx, svcCursor, methodListModels, &body, &out); err != nil {
		return nil, err
	}
	models := make([]core.Model, 0, len(out.Items))
	for _, m := range out.Items {
		entry := core.Model{
			ID:      m.ID,
			Object:  "model",
			OwnedBy: "cursor",
			Created: time.Now().Unix(),
		}
		if m.DisplayName != "" || m.Description != "" {
			entry.Metadata = &core.ModelMetadata{
				DisplayName: m.DisplayName,
				Description: m.Description,
			}
		}
		models = append(models, entry)
	}
	return &core.ModelsResponse{Object: "list", Data: models}, nil
}

// Responses is unsupported: the cursor backend speaks the agent SDK
// surface, not the OpenAI Responses API. Clients that need Responses
// semantics should translate their request to ChatCompletion.
func (p *Provider) Responses(_ context.Context, _ *core.ResponsesRequest) (*core.ResponsesResponse, error) {
	return nil, unsupported("responses")
}

// StreamResponses is unsupported for the same reason as Responses.
func (p *Provider) StreamResponses(_ context.Context, _ *core.ResponsesRequest) (io.ReadCloser, error) {
	return nil, unsupported("responses (stream)")
}

// Embeddings is unsupported: the cursor backend exposes no embeddings API.
func (p *Provider) Embeddings(_ context.Context, _ *core.EmbeddingRequest) (*core.EmbeddingResponse, error) {
	return nil, unsupported("embeddings")
}

// workspaceOrDefault returns the bridge's workspace dir, falling back to
// os.TempDir() (and finally "/") so local agents do not require write
// access to the filesystem root.
func (p *Provider) workspaceOrDefault() string {
	p.mu.Lock()
	m := p.manager
	p.mu.Unlock()
	if m != nil {
		if ws := m.Workspace(); ws != "" {
			return ws
		}
	}
	if tmp := os.TempDir(); tmp != "" {
		return tmp
	}
	return "/"
}

// startFailure turns a bridge-start failure into a provider error so the
// status code surfaces consistently. Two failure shapes map to two status
// codes:
//
//   - 503 Service Unavailable: the bridge binary is missing or otherwise
//     unreachable (resolveBridgeBinary / exec.LookPath failure). The
//     operator must install or point at the binary; the gateway did its
//     part.
//   - 502 Bad Gateway: the bridge was reachable (process spawned, stderr
//     pipe open, ready-line expected) but returned a malformed handshake,
//     crashed before the ready line, or timed out waiting for it. The
//     bridge exists; the wire is bad.
func (p *Provider) startFailure(err error) error {
	switch {
	case errors.Is(err, ErrBridgeUnreachable):
		return core.NewProviderError("cursor", http.StatusServiceUnavailable,
			"cursor: bridge unreachable: "+err.Error(), err)
	default:
		return core.NewProviderError("cursor", http.StatusBadGateway,
			"cursor: bridge unavailable: "+err.Error(), err)
	}
}

// unsupportedOperationCode mirrors the chatgpt provider's choice so the
// router sees the same marker for "this provider does not serve that".
const unsupportedOperationCode = "unsupported_provider_operation"

func unsupported(surface string) error {
	return core.NewInvalidRequestErrorWithStatus(http.StatusNotImplemented,
		"cursor provider does not implement "+surface, nil).WithCode(unsupportedOperationCode)
}

// flattenHistory collapses the chat message list into a single UserMessage
// text body, scoped by role. The cursor agent is the source of state, so
// the bridge only ever needs the latest user turn and a transcript of
// prior turns to put it in context.
func flattenHistory(messages []core.Message) string {
	if len(messages) == 0 {
		return ""
	}
	var b strings.Builder
	for i, m := range messages {
		if i > 0 {
			b.WriteString("\n\n")
		}
		switch strings.ToLower(m.Role) {
		case "system":
			b.WriteString("[SYSTEM]\n")
		case "user":
			b.WriteString("[USER]\n")
		case "assistant":
			b.WriteString("[ASSISTANT]\n")
		default:
			b.WriteString("[")
			b.WriteString(strings.ToUpper(m.Role))
			b.WriteString("]\n")
		}
		b.WriteString(core.ExtractTextContent(m.Content))
	}
	return b.String()
}

// extractAssistantText walks the public SDK assistant-message shape
// (role: assistant, content: [{type: text, text: ...}]) and appends every
// text block to out. Unknown block types are skipped silently so a future
// block addition cannot break the parser.
func extractAssistantText(payload json.RawMessage, out *strings.Builder) {
	if len(payload) == 0 {
		return
	}
	var msg assistantMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return
	}
	for _, block := range msg.Content {
		if block.Type == "text" {
			out.WriteString(block.Text)
		}
	}
}

// pickFinalText prefers the terminal result's `result` string (the
// authoritative final text), and falls back to the concatenated stream
// deltas when the bridge omits the terminal field.
func pickFinalText(streamed, terminal string) string {
	if terminal != "" {
		return terminal
	}
	return streamed
}

// terminalStatusOK reports whether the run reached a usable terminal
// state. protojson encodes enums by their full proto name
// (RUN_LIFECYCLE_STATUS_FINISHED), while SDK message payloads shorten it
// to FINISHED; accept both so a bridge that normalizes either way keeps
// working.
func terminalStatusOK(status string) bool {
	return status == "FINISHED" || status == "RUN_LIFECYCLE_STATUS_FINISHED"
}

// cursorRunError builds a GatewayError that captures the run-level
// failure. The human-readable message from the status payload is the
// most useful clue for "ERROR" runs where the result is empty.
func cursorRunError(r *runStreamResult) error {
	msg := r.Result.Result
	if r.ErrorCode != "" {
		if msg != "" {
			msg = r.ErrorCode + ": " + msg
		} else {
			msg = r.ErrorCode
		}
	}
	if msg == "" {
		msg = "cursor: run failed with status " + r.Status
	}
	return core.NewProviderError("cursor", http.StatusBadGateway, msg, nil).
		WithCode(r.ErrorCode)
}
