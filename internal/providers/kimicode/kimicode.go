// Package kimicode provides Kimi Code API integration for the LLM gateway.
//
// The "kimicode" provider routes to Kimi Code's OpenAI-compatible chat
// completions endpoint, so all transport goes through the shared chat-centric
// adapter. A static model map (models.go) adds canonical client-facing model
// names on top of the raw upstream IDs: raw IDs keep working unchanged, while
// canonical names are rewritten to their upstream ID and carry the endpoint's
// thinking control.
package kimicode

import (
	"net/http"
	"os"
	"strings"

	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/llmclient"
	"github.com/enterpilot/gomodel/internal/providers"
	"github.com/enterpilot/gomodel/internal/providers/openai"
)

const defaultBaseURL = "https://api.kimi.com/coding/v1"

// modelMapEnvVar gates the static model map (models.go). Setting it to "off"
// disables all map behavior: no canonical-name registration, no model
// rewriting, no thinking pinning. Default: enabled.
const modelMapEnvVar = "KIMICODE_MODEL_MAP"

// Registration provides factory registration for the Kimi Code provider.
var Registration = providers.Registration{
	Type: "kimicode",
	New:  New,
	Discovery: providers.DiscoveryConfig{
		DefaultBaseURL: defaultBaseURL,
	},
}

// Provider implements the core.Provider interface for Kimi Code. Kimi Code is
// OpenAI-compatible, so all transport goes through the shared chat-centric
// adapter: chat completions, model listing, embeddings, and passthrough are
// exposed via the embedded *openai.ChatCompatible.
type Provider struct {
	*openai.ChatCompatible
	// mapEnabled gates the static model map (models.go). False when
	// KIMICODE_MODEL_MAP=off.
	mapEnabled bool
}

var _ core.Provider = (*Provider)(nil)

// New creates a new Kimi Code provider.
func New(cfg providers.ProviderConfig, opts providers.ProviderOptions) core.Provider {
	p := &Provider{mapEnabled: modelMapEnabled()}
	p.ChatCompatible = openai.NewChatCompatible(cfg.APIKey, opts, openai.CompatibleProviderConfig{
		ProviderName:     "kimicode",
		BaseURL:          providers.ResolveBaseURL(cfg.BaseURL, defaultBaseURL),
		AdaptChatRequest: p.adaptChatRequest,
	})
	return p
}

// NewWithHTTPClient creates a new Kimi Code provider with a custom HTTP client.
// If httpClient is nil, http.DefaultClient is used.
//
// The signature is intentionally stable and matches every other chat-compatible
// provider on main: (apiKey, baseURL, httpClient, hooks).
func NewWithHTTPClient(apiKey string, baseURL string, httpClient *http.Client, hooks llmclient.Hooks) *Provider {
	p := &Provider{mapEnabled: modelMapEnabled()}
	p.ChatCompatible = openai.NewChatCompatibleWithHTTPClient(apiKey, httpClient, hooks, openai.CompatibleProviderConfig{
		ProviderName:     "kimicode",
		BaseURL:          providers.ResolveBaseURL(baseURL, defaultBaseURL),
		AdaptChatRequest: p.adaptChatRequest,
	})
	return p
}

// modelMapEnabled reports whether the static model map is active. It is on by
// default; KIMICODE_MODEL_MAP=off (any casing, surrounding whitespace ignored)
// turns it off.
func modelMapEnabled() bool {
	v := strings.TrimSpace(os.Getenv(modelMapEnvVar))
	return !strings.EqualFold(v, "off")
}
