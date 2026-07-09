package openrouter

import (
	"net/http"
	"os"
	"strings"

	"gomodel/internal/core"
	"gomodel/internal/llmclient"
	"gomodel/internal/providers"
	"gomodel/internal/providers/openai"
)

const (
	defaultBaseURL = "https://openrouter.ai/api/v1"
	defaultSiteURL = "https://gomodel.enterpilot.io"
	defaultAppName = "GoModel"
)

var Registration = providers.Registration{
	Type:                        "openrouter",
	New:                         New,
	PassthroughSemanticEnricher: openai.Registration.PassthroughSemanticEnricher,
	Discovery: providers.DiscoveryConfig{
		DefaultBaseURL: defaultBaseURL,
	},
}

type Provider struct {
	*openai.CompatibleProvider
}

func New(cfg providers.ProviderConfig, opts providers.ProviderOptions) core.Provider {
	baseURL := providers.ResolveBaseURL(cfg.BaseURL, defaultBaseURL)
	return &Provider{
		CompatibleProvider: openai.NewCompatibleProvider(cfg.APIKey, opts, openai.CompatibleProviderConfig{
			ProviderName:   "openrouter",
			BaseURL:        baseURL,
			SetHeaders:     setHeaders,
			DefaultHeaders: identityHeaders(),
		}),
	}
}

func NewWithHTTPClient(apiKey string, httpClient *http.Client, hooks llmclient.Hooks) *Provider {
	return &Provider{
		CompatibleProvider: openai.NewCompatibleProviderWithHTTPClient(apiKey, httpClient, hooks, openai.CompatibleProviderConfig{
			ProviderName:   "openrouter",
			BaseURL:        defaultBaseURL,
			SetHeaders:     setHeaders,
			DefaultHeaders: identityHeaders(),
		}),
	}
}

func setHeaders(req *http.Request, apiKey string) {
	providers.SetAuthHeaders(req, apiKey, providers.AuthHeaderConfig{
		AuthScheme:        "Bearer ",
		RequestIDHeader:   "X-Client-Request-Id",
		ValidateRequestID: providers.IsValidClientRequestID,
	})
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func identityHeaders() map[string]string {
	headers := make(map[string]string, 2)
	if siteURL := envOrDefault("OPENROUTER_SITE_URL", defaultSiteURL); siteURL != "" {
		headers["HTTP-Referer"] = siteURL
	}
	if appName := envOrDefault("OPENROUTER_APP_NAME", defaultAppName); appName != "" {
		headers["X-OpenRouter-Title"] = appName
	}
	return headers
}
