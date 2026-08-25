package kimicode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/llmclient"
	"github.com/enterpilot/gomodel/internal/providers"
)

// --- adaptChatRequest -------------------------------------------------------

func TestAdaptChatRequest_NilRequest(t *testing.T) {
	got, err := adaptChatRequest(nil)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestAdaptChatRequest_RewritesCanonicalToRaw(t *testing.T) {
	req := &core.ChatRequest{Model: "kimi-k2.7-code"}

	got, err := adaptChatRequest(req)
	require.NoError(t, err)
	require.NotSame(t, req, got, "canonical IDs should produce a shallow copy")
	require.Equal(t, "kimi-for-coding", got.Model, "canonical kimi-k2.7-code must rewrite to raw kimi-for-coding")
	require.Equal(t, "kimi-k2.7-code", req.Model, "caller's request must not be mutated")
}

func TestAdaptChatRequest_K2ForcedThinkingEnabled(t *testing.T) {
	req := &core.ChatRequest{Model: "kimi-k2.7-code"}
	req.Reasoning = &core.Reasoning{Effort: "high"}
	req.ExtraFields = core.UnknownJSONFieldsFromMap(map[string]json.RawMessage{
		"keep_me": json.RawMessage(`"yes"`),
	})

	got, err := adaptChatRequest(req)
	require.NoError(t, err)
	require.Equal(t, "kimi-for-coding", got.Model)
	require.Nil(t, got.Reasoning, "K2.7 forces thinking.type, not the typed Reasoning knob")

	thinking := got.ExtraFields.Lookup("thinking")
	require.JSONEq(t, `{"type":"enabled"}`, string(thinking), "K2.7 Code must force thinking.type: enabled")

	keep := got.ExtraFields.Lookup("keep_me")
	require.Equal(t, `"yes"`, string(keep), "untouched extra fields must survive")
}

func TestAdaptChatRequest_K2ForcedThinkingOverridesClientThinking(t *testing.T) {
	req := &core.ChatRequest{Model: "kimi-k2.7-code"}
	req.ExtraFields = core.UnknownJSONFieldsFromMap(map[string]json.RawMessage{
		"thinking": json.RawMessage(`{"type":"disabled","foo":"bar"}`),
	})

	got, err := adaptChatRequest(req)
	require.NoError(t, err)
	thinking := got.ExtraFields.Lookup("thinking")
	require.JSONEq(t, `{"type":"enabled"}`, string(thinking),
		"K2.7 Code's forced thinking.type must override any client thinking field")
}

func TestAdaptChatRequest_K26ForcedThinkingDisabled(t *testing.T) {
	req := &core.ChatRequest{Model: "kimi-k2.6"}

	got, err := adaptChatRequest(req)
	require.NoError(t, err)
	require.Equal(t, "kimi-for-coding", got.Model)
	thinking := got.ExtraFields.Lookup("thinking")
	require.JSONEq(t, `{"type":"disabled"}`, string(thinking),
		"kimi-k2.6 must force thinking.type: disabled, which routes to K2.6 upstream")
}

func TestAdaptChatRequest_K3FlattensReasoningEffort(t *testing.T) {
	req := &core.ChatRequest{Model: "kimi-k3"}
	req.Reasoning = &core.Reasoning{Effort: "high"}

	got, err := adaptChatRequest(req)
	require.NoError(t, err)
	require.Equal(t, "k3", got.Model)
	require.Nil(t, got.Reasoning, "typed Reasoning is replaced with the flat reasoning_effort field")

	effort := got.ExtraFields.Lookup("reasoning_effort")
	require.Equal(t, `"high"`, string(effort))
}

func TestAdaptChatRequest_K3PassesThroughWithoutReasoning(t *testing.T) {
	req := &core.ChatRequest{Model: "kimi-k3"}

	got, err := adaptChatRequest(req)
	require.NoError(t, err)
	require.Equal(t, "k3", got.Model, "canonical kimi-k3 must rewrite to raw k3 even without a reasoning knob")
	require.Nil(t, got.Reasoning, "K3 with no Reasoning must not be mutated")
	require.Empty(t, got.ExtraFields.Lookup("reasoning_effort"),
		"K3 without a Reasoning field should not gain a reasoning_effort on the wire")
}

func TestAdaptChatRequest_EmbeddingsModelIsIdentity(t *testing.T) {
	req := &core.ChatRequest{Model: "bge_m3_embed"}

	got, err := adaptChatRequest(req)
	require.NoError(t, err)
	require.Same(t, req, got, "bge_m3_embed's canonical name is its raw name — no rewrite, same pointer")
}

func TestAdaptChatRequest_RawIDsAreNeverRewritten(t *testing.T) {
	cases := []string{
		"kimi-for-coding",
		"kimi-for-coding-highspeed",
		"k3",
		"k3-256k",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			req := &core.ChatRequest{Model: raw}
			req.Reasoning = &core.Reasoning{Effort: "high"}

			got, err := adaptChatRequest(req)
			require.NoError(t, err)
			require.Equal(t, raw, got.Model, "raw upstream IDs must not be rewritten")
			require.Same(t, req, got, "raw IDs return the same request pointer (Postel's law: pass through)")
			require.NotNil(t, got.Reasoning, "raw IDs must not be modified — Reasoning field passes through untouched")
		})
	}
}

func TestAdaptChatRequest_UnknownIDsPassThrough(t *testing.T) {
	req := &core.ChatRequest{Model: "some-future-model"}

	got, err := adaptChatRequest(req)
	require.NoError(t, err)
	require.Same(t, req, got, "unknown IDs return the same request pointer")
}

func TestAdaptChatRequest_DoesNotMutateCaller(t *testing.T) {
	req := &core.ChatRequest{Model: "kimi-k2.6"}
	req.Reasoning = &core.Reasoning{Effort: "high"}
	req.ExtraFields = core.UnknownJSONFieldsFromMap(map[string]json.RawMessage{
		"client_only": json.RawMessage(`42`),
	})

	_, err := adaptChatRequest(req)
	require.NoError(t, err)

	require.Equal(t, "kimi-k2.6", req.Model, "caller's Model must remain canonical")
	require.NotNil(t, req.Reasoning, "caller's Reasoning must not be cleared")
	require.Equal(t, "high", req.Reasoning.Effort)
	clientOnly := req.ExtraFields.Lookup("client_only")
	require.Equal(t, `42`, string(clientOnly), "caller's ExtraFields must not be merged into")
}

// --- ListModels -------------------------------------------------------------

func TestListModels_MergesStaticAndUpstream(t *testing.T) {
	upstream := `{
		"object":"list",
		"data":[
			{"id":"kimi-for-coding","object":"model","owned_by":"moonshotai","created":1},
			{"id":"kimi-for-coding-highspeed","object":"model","owned_by":"moonshotai","created":2},
			{"id":"k3","object":"model","owned_by":"moonshotai","created":3},
			{"id":"k3-256k","object":"model","owned_by":"moonshotai","created":4}
		]
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstream))
	}))
	defer server.Close()

	provider := NewWithHTTPClient("k", server.URL, server.Client(), llmclient.Hooks{})

	resp, err := provider.ListModels(context.Background())
	require.NoError(t, err)
	require.NotNil(t, resp)

	ids := make(map[string]core.Model, len(resp.Data))
	for _, m := range resp.Data {
		ids[m.ID] = m
	}

	// Raw upstream IDs are preserved (Postel's law: nothing taken away).
	for _, id := range []string{"kimi-for-coding", "kimi-for-coding-highspeed", "k3", "k3-256k"} {
		require.Contains(t, ids, id, "raw upstream model %q must remain in the listing", id)
	}
	// Canonical aliases are added on top.
	for _, id := range []string{"kimi-k2.6", "kimi-k2.7-code", "kimi-k2.7-code-highspeed", "kimi-k3", "kimi-k3-256k", "bge_m3_embed"} {
		require.Contains(t, ids, id, "canonical alias %q must be synthesized into the listing", id)
	}

	// Synthesized chat aliases carry the expected metadata.
	k27 := ids["kimi-k2.7-code"]
	require.NotNil(t, k27.Metadata)
	require.Equal(t, []string{"chat"}, k27.Metadata.Modes)
	require.Equal(t, []core.ModelCategory{core.CategoryTextGeneration}, k27.Metadata.Categories)
	require.NotNil(t, k27.Metadata.ContextWindow)
	require.Equal(t, 262144, *k27.Metadata.ContextWindow)

	// The embedding model is marked as an embedding, not chat.
	embed := ids["bge_m3_embed"]
	require.NotNil(t, embed.Metadata)
	// Mode strings use the registry's canonical singular form ("embedding",
	// matching internal/core modeToCategory) so CategoriesForModes derives
	// CategoryEmbedding; the user-facing name "embeddings" stays the plural.
	require.Equal(t, []string{"embedding"}, embed.Metadata.Modes)
	require.Equal(t, []core.ModelCategory{core.CategoryEmbedding}, embed.Metadata.Categories)
}

func TestListModels_PreservesUpstreamOnlyUnknownModels(t *testing.T) {
	upstream := `{
		"object":"list",
		"data":[
			{"id":"kimi-for-coding","object":"model","owned_by":"moonshotai","created":1},
			{"id":"future-model","object":"model","owned_by":"moonshotai","created":99}
		]
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(upstream))
	}))
	defer server.Close()

	provider := NewWithHTTPClient("k", server.URL, server.Client(), llmclient.Hooks{})

	resp, err := provider.ListModels(context.Background())
	require.NoError(t, err)
	require.NotNil(t, resp)

	ids := make(map[string]core.Model, len(resp.Data))
	for _, m := range resp.Data {
		ids[m.ID] = m
	}
	require.Contains(t, ids, "future-model", "unknown upstream models must pass through (no rewriting)")
	require.Nil(t, ids["future-model"].Metadata, "unknown upstream models get no synthetic metadata")
}

func TestListModels_PropagatesUpstreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"message":"boom"}}`, http.StatusInternalServerError)
	}))
	defer server.Close()

	provider := NewWithHTTPClient("k", server.URL, server.Client(), llmclient.Hooks{})
	_, err := provider.ListModels(context.Background())
	require.Error(t, err, "ListModels must not swallow upstream errors")
}

// --- end-to-end through ChatCompletion --------------------------------------

// TestChatCompletion_AppliesAdaptChatRequestThroughStandardConstructor is the
// production-wiring companion to TestEmbeddings_RoundTrip: it ensures
// AdaptChatRequest is plumbed into New() (the factory path), not only
// NewWithHTTPClient (the test-only constructor).
func TestChatCompletion_AppliesAdaptChatRequestThroughStandardConstructor(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-kimi",
			"created":1,
			"model":"kimi-for-coding",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()

	provider, ok := New(providers.ProviderConfig{BaseURL: server.URL}, providers.ProviderOptions{}).(*Provider)
	require.True(t, ok, "New() must return *Provider")

	_, err := provider.ChatCompletion(context.Background(), &core.ChatRequest{
		Model: "kimi-k2.7-code",
		Messages: []core.Message{
			{Role: "user", Content: "hi"},
		},
	})
	require.NoError(t, err)

	require.Equal(t, "kimi-for-coding", gotBody["model"], "canonical ID must be rewritten to the raw upstream ID")
	thinking, ok := gotBody["thinking"].(map[string]any)
	require.True(t, ok, "K2.7 Code requests must include a thinking field on the wire")
	require.Equal(t, "enabled", thinking["type"])
}