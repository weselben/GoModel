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
	// Enabled toggles the translation. Default true.
	Enabled *bool `yaml:"enabled" env:"THINK_EXTRACT_ENABLED"`
	// TagOpen overrides the opening tag. Default "<think>".
	TagOpen string `yaml:"tag_open" env:"THINK_EXTRACT_TAG_OPEN"`
	// TagClose overrides the closing tag. Default "</think>".
	TagClose string `yaml:"tag_close" env:"THINK_EXTRACT_TAG_CLOSE"`
	// MaxBufferBytes caps the size of an unclosed block held in streaming
	// state before it is flushed as ordinary content. Default 65536.
	MaxBufferBytes int `yaml:"max_buffer_bytes" env:"THINK_EXTRACT_MAX_BUFFER_BYTES"`
}

// IsEnabled reports whether the translation is active. Nil receiver and
// nil Enabled pointer are both treated as the default-true behaviour.
func (c ThinkExtractConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}