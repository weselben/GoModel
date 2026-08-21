package cursor

import (
	"github.com/goccy/go-json"
)

// Wire-format structs for the cursor-sdk-bridge sdk.v1 Connect/JSON protocol.
//
// The bridge encodes every proto message with Connect's protojson rules
// (lowerCamelCase fields, enum names as strings). These structs mirror
// the wire payload shape exactly. When a future bridge version renames a
// field, this is the single file to update — keep changes here, not in
// the provider core.
//
// Sources for the field names:
//   - proto/sdk/v1/sdk_messages.proto
//   - proto/sdk/v1/sdk_agent_service.proto
//   - proto/sdk/v1/sdk_cursor_service.proto
//   - docs/smoke-test.md (canonical JSON examples)
//
// The smoke test confirms the field names are camelCase (apiKey, agentId,
// sdkMessage, runId, inputTokens, ...) despite the proto definitions using
// snake_case. Protocol buffers are encoded with their json_name (or its
// lowerCamelCase default) on the wire.

// CursorRequestOptions carries the per-call API key. The bridge refuses
// catalog calls that omit it.
type cursorRequestOptions struct {
	APIKey string `json:"apiKey"`
}

// ModelSelection is the {id, params[]} shape used everywhere a model is
// referenced.
type modelSelection struct {
	ID     string                `json:"id"`
	Params []modelParameterValue `json:"params,omitempty"`
}

type modelParameterValue struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// LocalAgentOptions chooses the local runtime and supplies the workspace.
type localAgentOptions struct {
	CWD []string `json:"cwd"`
}

// AgentOptions is the body of CreateAgentRequest. Both Local and Cloud
// are pointers so one of them can be omitted from the JSON.
type agentOptions struct {
	Model  modelSelection     `json:"model"`
	APIKey string             `json:"apiKey"`
	Local  *localAgentOptions `json:"local,omitempty"`
}

type createAgentRequest struct {
	Options agentOptions `json:"options"`
}

type createAgentResponse struct {
	AgentID string         `json:"agentId"`
	Model   modelSelection `json:"model,omitempty"`
}

// UserMessage is the per-turn payload. The text field carries the
// flattened conversation history; images are not supported by the
// gateway yet (would need a separate SdkImage envelope).
type userMessage struct {
	Text string `json:"text"`
}

// SendRequest is the streaming-RPC body. The message is the user turn;
// options is kept reserved for future per-send overrides (model, mode).
type sendRequest struct {
	AgentID string      `json:"agentId"`
	Message userMessage `json:"message"`
}

type closeAgentRequest struct {
	AgentID string `json:"agentId"`
}

// closeAgentResponse is intentionally empty: the proto defines CloseAgentResponse
// as {} and we keep the JSON object explicit so the body decoder accepts it.
type closeAgentResponse struct{}

// ListModelsRequest mirrors the proto exactly: a single Options field.
type listModelsRequest struct {
	Options cursorRequestOptions `json:"options"`
}

// SdkModel is the per-item shape on ListModelsResponse.
type sdkModel struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName,omitempty"`
	Description string `json:"description,omitempty"`
}

type listModelsResponse struct {
	Items []sdkModel `json:"items"`
}

// TokenUsage mirrors the proto total-token accounting. Fields are
// optional so partial payloads (e.g. a usage report missing cache reads)
// unmarshal cleanly.
type tokenUsage struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
	TotalTokens      int64 `json:"totalTokens"`
}

// RunResult is the terminal-state snapshot. The `result` field carries
// the final assistant text; usage is optional because the backend may
// omit it on a run that never reached a token-reporting turn.
type runResult struct {
	RunID      string      `json:"runId"`
	AgentID    string      `json:"agentId"`
	Status     string      `json:"status"`
	Result     string      `json:"result"`
	DurationMs int64       `json:"durationMs"`
	Usage      *tokenUsage `json:"usage,omitempty"`
}

// runStreamResult is the terminal frame's envelope payload.
type runStreamResult struct {
	AgentID   string    `json:"agentId"`
	RunID     string    `json:"runId"`
	Status    string    `json:"status"`
	ErrorCode string    `json:"errorCode,omitempty"`
	Result    runResult `json:"result"`
}

// runStreamEnvelope is the on-wire shape of one RunStreamMessage. Each
// field is a different `oneof` case in the proto; only one is set per
// frame. The frame's offset field is ignored.
type runStreamEnvelope struct {
	SDKMessage *sdkMessage      `json:"sdkMessage,omitempty"`
	Result     *runStreamResult `json:"result,omitempty"`
	Done       *struct{}        `json:"done,omitempty"`
}

// sdkMessage is the on-wire shape of the SdkMessage proto: a string
// discriminator plus a JSON payload (the google.protobuf.Struct). The
// payload shape is the public SDK's message type for the discriminator,
// so we accept arbitrary JSON and only decode the shapes we care about.
type sdkMessage struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message"`
}

// assistantMessage is the assistant payload shape from the public SDK:
// {role: "assistant", content: [{type: "text", text: "..."}, ...]}.
type assistantMessage struct {
	Role    string             `json:"role"`
	Content []assistantContent `json:"content"`
}

type assistantContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
