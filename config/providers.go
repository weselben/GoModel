package config

// RawProviderConfig is the YAML-sourced provider configuration before env var
// overrides, credential filtering, or resilience merging. Exported so the
// providers package can resolve it into a fully-configured ProviderConfig.
type RawProviderConfig struct {
	Type   string `yaml:"type"`
	APIKey string `yaml:"api_key"`
	// APIKeys lists additional API keys for this provider. When more than one
	// key is resolved (counting APIKey), requests rotate across them round
	// robin. Set it via `api_keys:` or the `<PROVIDER>_API_KEY_<n>` env vars.
	APIKeys                  []string             `yaml:"api_keys"`
	BaseURL                  string               `yaml:"base_url"`
	APIVersion               string               `yaml:"api_version"`
	Backend                  string               `yaml:"backend"`
	AuthType                 string               `yaml:"auth_type"`
	APIMode                  string               `yaml:"api_mode"`
	VertexProject            string               `yaml:"vertex_project"`
	VertexLocation           string               `yaml:"vertex_location"`
	ServiceAccountFile       string               `yaml:"service_account_file"`
	ServiceAccountJSON       string               `yaml:"service_account_json"`
	ServiceAccountJSONBase64 string               `yaml:"service_account_json_base64"`
	GCPScope                 string               `yaml:"gcp_scope"`
	Models                   []RawProviderModel   `yaml:"models"`
	Resilience               *RawResilienceConfig `yaml:"resilience"`
	// CustomUpstreamHeaders are static key/value pairs injected on every upstream
	// request. They override provider default headers and are themselves overridden
	// by passthrough user headers when those are enabled and overlap.
	CustomUpstreamHeaders map[string]string `yaml:"custom_upstream_headers"`
	// PassthroughUserHeaders forwards caller headers to the upstream provider when true.
	// Hard-blocked headers (Authorization, Cookie, transport headers, and the user-path
	// header) are always stripped regardless of this setting.
	PassthroughUserHeaders bool `yaml:"passthrough_user_headers"`
	// PassthroughUserHeadersSkip is the list of header names that are excluded from
	// passthrough (skip mode) or exclusively allowed (allow mode).
	PassthroughUserHeadersSkip []string `yaml:"passthrough_user_headers_skip"`
	// PassthroughUserHeadersSkipMode controls how the skip list is applied:
	// "skip" drops listed headers (default), "allow" forwards only listed headers.
	PassthroughUserHeadersSkipMode string `yaml:"passthrough_user_headers_skip_mode"`
}
