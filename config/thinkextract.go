package config

// ThinkExtractConfig controls the response-path translation of legacy
// `<think>...</think>` (and configured equivalents) into the native reasoning
// field. The translation only runs on responses — request-side bodies are
// never modified.
//
// The default is enabled because the translation is lossless on the wire
// (no model-visible character is dropped) and the dialect converters for
// OpenAI chat completions, OpenAI responses, and Anthropic messages all
// surface ExtraFields["reasoning_content"] as their native reasoning field.
// Operators who observe a regression on a model that already emits structured
// reasoning can disable the feature per deployment.
type ThinkExtractConfig struct {
	// Enabled toggles the translation globally. Default true.
	Enabled *bool `yaml:"enabled" env:"THINK_EXTRACT_ENABLED"`
	// ChatEnabled toggles the translation on the chat completions surface.
	// Nil falls back to Enabled. Env: THINK_EXTRACT_CHAT_ENABLED.
	ChatEnabled *bool `yaml:"chat_enabled" env:"THINK_EXTRACT_CHAT_ENABLED"`
	// ResponsesEnabled toggles the translation on the OpenAI responses
	// surface. Nil falls back to Enabled. Env: THINK_EXTRACT_RESPONSES_ENABLED.
	ResponsesEnabled *bool `yaml:"responses_enabled" env:"THINK_EXTRACT_RESPONSES_ENABLED"`
	// MessagesPolicy controls how synthesized reasoning is emitted on the
	// Anthropic messages surface. Values: off (default), unsigned, redacted.
	// "off" means no extraction runs for messages requests, so legacy tags
	// stay in the message content unchanged. Env: THINK_EXTRACT_MESSAGES_POLICY.
	MessagesPolicy string `yaml:"messages_policy" env:"THINK_EXTRACT_MESSAGES_POLICY"`
	// TagPairs overrides the recognition list. The default list covers the
	// union of vLLM/SGLang/Open WebUI standard tags. Format is a
	// comma-separated "<open>...</close>" list, e.g.
	// "<think>...</think>,<thinking>...</thinking>".
	TagPairs string `yaml:"tag_pairs" env:"THINK_EXTRACT_TAG_PAIRS"`
	// MaxBufferBytes caps the size of an unclosed block held in streaming
	// state before it is flushed as ordinary content. Default 65536.
	MaxBufferBytes int `yaml:"max_buffer_bytes" env:"THINK_EXTRACT_MAX_BUFFER_BYTES"`
}

// IsEnabled reports whether the translation is active at the global level.
// Nil receiver and nil Enabled pointer are both treated as default-true.
func (c ThinkExtractConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// IsEnabledForChat reports whether the translation runs on the chat
// completions surface. Falls back to the global Enabled value when the
// per-surface pointer is unset.
func (c ThinkExtractConfig) IsEnabledForChat() bool {
	if c.ChatEnabled != nil {
		return *c.ChatEnabled
	}
	return c.IsEnabled()
}

// IsEnabledForResponses reports whether the translation runs on the OpenAI
// responses surface. Falls back to the global Enabled value when unset.
func (c ThinkExtractConfig) IsEnabledForResponses() bool {
	if c.ResponsesEnabled != nil {
		return *c.ResponsesEnabled
	}
	return c.IsEnabled()
}

// IsEnabledForMessages reports whether the translation runs on the
// Anthropic messages surface. The messages policy defaults to off, so the
// translation only runs when an operator opts in explicitly. The global
// Enabled switch is also authoritative — a global off kills the feature
// everywhere regardless of the per-surface policy.
func (c ThinkExtractConfig) IsEnabledForMessages() bool {
	if !c.IsEnabled() {
		return false
	}
	switch c.MessagesPolicy {
	case "unsigned", "redacted":
		return true
	default:
		return false
	}
}

// MessagesPolicyOrDefault returns the configured messages policy, falling
// back to "off" when unset so the per-call site can rely on a non-empty
// value.
func (c ThinkExtractConfig) MessagesPolicyOrDefault() string {
	if c.MessagesPolicy == "" {
		return "off"
	}
	return c.MessagesPolicy
}