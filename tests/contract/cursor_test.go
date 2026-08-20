//go:build contract

// Contract tests in this file are intended to run with: -tags=contract -timeout=5m.
package contract

import (
	"context"
	"encoding/binary"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/llmclient"
	"github.com/enterpilot/gomodel/internal/providers/cursor"
)

// Connect route paths the cursor transport POSTs to; they must match
// connectEndpoint in internal/providers/cursor/connect_transport.go.
const (
	cursorCreateAgentPath = "/sdk.v1.SdkAgentService/CreateAgent"
	cursorSendPath        = "/sdk.v1.SdkAgentService/Send"
	cursorCloseAgentPath  = "/sdk.v1.SdkAgentService/CloseAgent"
	cursorListModelsPath  = "/sdk.v1.SdkCursorService/ListModels"
)

// newCursorReplayProvider builds an attach-mode cursor provider: no bridge
// subprocess is spawned, and the replay client intercepts every Connect
// call at the RoundTripper, so the base URL host is irrelevant.
func newCursorReplayProvider(t *testing.T, routes map[string]replayRoute) *cursor.Provider {
	t.Helper()

	provider, err := cursor.NewWithHTTPClient("cursor-test", "http://127.0.0.1:1", newReplayHTTPClient(t, routes), llmclient.Hooks{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = provider.Close() })
	return provider
}

// connectFixtureRoute mirrors sseFixtureRoute for Connect server-streaming
// RPCs: the fixture file holds one JSON payload per line, and each line is
// framed into a Connect envelope (1 byte flags + 4 byte big-endian length +
// payload). A clean end-of-stream frame terminates the replayed stream.
func connectFixtureRoute(t *testing.T, path string) replayRoute {
	t.Helper()

	var body []byte
	for _, line := range strings.Split(string(loadGoldenFileRaw(t, path)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		body = appendConnectFrame(body, 0x00, []byte(line))
	}
	body = appendConnectFrame(body, 0x02, []byte("{}"))
	return replayRoute{
		statusCode:  http.StatusOK,
		contentType: "application/connect+json",
		body:        body,
	}
}

func appendConnectFrame(dst []byte, flags byte, payload []byte) []byte {
	var hdr [5]byte
	hdr[0] = flags
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(payload)))
	dst = append(dst, hdr[:]...)
	return append(dst, payload...)
}

// cursorChatRoutes wires the full agent lifecycle every chat turn drives:
// CreateAgent, the Send stream, and the deferred CloseAgent release.
func cursorChatRoutes(t *testing.T) map[string]replayRoute {
	t.Helper()
	return map[string]replayRoute{
		replayKey(http.MethodPost, cursorCreateAgentPath): jsonFixtureRoute(t, "cursor/create_agent.json"),
		replayKey(http.MethodPost, cursorSendPath):        connectFixtureRoute(t, "cursor/chat_completion.stream"),
		replayKey(http.MethodPost, cursorCloseAgentPath):  jsonFixtureRoute(t, "cursor/close_agent.json"),
	}
}

func TestCursorReplayChatCompletion(t *testing.T) {
	provider := newCursorReplayProvider(t, cursorChatRoutes(t))

	resp, err := provider.ChatCompletion(context.Background(), &core.ChatRequest{
		Model: "gpt-5",
		Messages: []core.Message{{
			Role:    "user",
			Content: "hello",
		}},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Equal(t, "hello world", resp.Choices[0].Message.Content)
	require.Equal(t, 10, resp.Usage.PromptTokens)
	require.Equal(t, 5, resp.Usage.CompletionTokens)
	require.Equal(t, 15, resp.Usage.TotalTokens)

	compareGoldenJSON(t, goldenPathForFixture("cursor/chat_completion.stream"), resp)
}

func TestCursorReplayStreamChatCompletion(t *testing.T) {
	provider := newCursorReplayProvider(t, cursorChatRoutes(t))

	stream, err := provider.StreamChatCompletion(context.Background(), &core.ChatRequest{
		Model: "gpt-5",
		Messages: []core.Message{{
			Role:    "user",
			Content: "stream",
		}},
	})
	require.NoError(t, err)

	raw := readAllStream(t, stream)
	chunks, done := parseChatStream(t, raw)
	require.True(t, done)
	require.Equal(t, "hello world", extractChatStreamText(chunks))

	// The Send fixture is shared with the unary case; this golden records
	// its normalized OpenAI SSE rendering.
	compareGoldenJSON(t, "cursor/chat_completion_stream.golden.json", map[string]any{
		"done":   done,
		"chunks": chunks,
		"text":   extractChatStreamText(chunks),
	})
}

func TestCursorReplayListModels(t *testing.T) {
	provider := newCursorReplayProvider(t, map[string]replayRoute{
		replayKey(http.MethodPost, cursorListModelsPath): jsonFixtureRoute(t, "cursor/list_models.json"),
	})

	resp, err := provider.ListModels(context.Background())
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.Len(t, resp.Data, 3)

	compareGoldenJSON(t, goldenPathForFixture("cursor/list_models.json"), resp)
}

func TestCursorReplayChatCompletionError(t *testing.T) {
	provider := newCursorReplayProvider(t, map[string]replayRoute{
		replayKey(http.MethodPost, cursorCreateAgentPath): jsonFixtureRoute(t, "cursor/create_agent.json"),
		replayKey(http.MethodPost, cursorSendPath): {
			statusCode:  http.StatusUnauthorized,
			contentType: "application/json",
			body:        []byte(`{"code":"unauthenticated","message":"bad key"}`),
		},
		replayKey(http.MethodPost, cursorCloseAgentPath): jsonFixtureRoute(t, "cursor/close_agent.json"),
	})

	_, err := provider.ChatCompletion(context.Background(), &core.ChatRequest{
		Model: "gpt-5",
		Messages: []core.Message{{
			Role:    "user",
			Content: "hello",
		}},
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "bad key")
	var gwErr *core.GatewayError
	require.ErrorAs(t, err, &gwErr)
	require.Equal(t, http.StatusUnauthorized, gwErr.StatusCode)
}
