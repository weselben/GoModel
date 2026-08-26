package config

import (
	"fmt"
	"os"
	"strings"
)

// ModelNormalizerRule declares one model normalization rule in config.yaml or
// the MODEL_NORMALIZER env var. It maps a canonical client-facing alias to a
// provider/model target and optionally pins a thinking policy, mirroring the
// behavior of the internal/modelnormalizer package. Rules are read-only at
// runtime: they live alongside virtual_models as infrastructure-as-code.
type ModelNormalizerRule struct {
	// Alias is the client-facing model ID that triggers this rule.
	Alias string `yaml:"alias" json:"alias"`

	// Target is the provider/model selector the request is rewritten to, e.g.
	// "kimicode/kimi-for-coding".
	Target string `yaml:"target" json:"target"`

	// Thinking pins the thinking policy: "enabled", "disabled", or "passthrough".
	// Empty is treated as passthrough (no field is injected).
	Thinking string `yaml:"thinking,omitempty" json:"thinking,omitempty"`

	// ContextWindow is the advertised context window for /v1/models metadata.
	ContextWindow *int `yaml:"context_window,omitempty" json:"context_window,omitempty"`

	// Modes declares the model's kinds for registry metadata, e.g. ["chat"].
	Modes []string `yaml:"modes,omitempty" json:"modes,omitempty"`
}

const envModelNormalizer = "MODEL_NORMALIZER"

// validateModelNormalizerRules checks that every rule has a non-empty alias
// and target and a known thinking policy. A rule with an invalid policy would
// silently rewrite requests to an undefined state, so fail fast at load time.
func validateModelNormalizerRules(rules []ModelNormalizerRule) error {
	for i, r := range rules {
		if strings.TrimSpace(r.Alias) == "" {
			return fmt.Errorf("model_normalizer[%d]: alias is required", i)
		}
		if strings.TrimSpace(r.Target) == "" {
			return fmt.Errorf("model_normalizer[%d] (%s): target is required", i, r.Alias)
		}
		switch strings.TrimSpace(r.Thinking) {
		case "", "passthrough", "enabled", "disabled":
		default:
			return fmt.Errorf("model_normalizer[%d] (%s): thinking must be one of: enabled, disabled, passthrough; got %q", i, r.Alias, r.Thinking)
		}
	}
	return nil
}

// applyModelNormalizerEnv parses the MODEL_NORMALIZER env var — a JSON array
// of model normalization rules — and merges it over the YAML-declared list.
// Env entries override YAML entries with the same alias (case-insensitive),
// consistent with the rest of the config pipeline where env always wins.
func applyModelNormalizerEnv(cfg *Config, strict bool) error {
	raw := strings.TrimSpace(os.Getenv(envModelNormalizer))
	if raw == "" {
		return nil
	}
	var fromEnv []ModelNormalizerRule
	if err := decodeIaCJSON(envModelNormalizer, raw, &fromEnv, strict); err != nil {
		return fmt.Errorf("invalid %s: %w", envModelNormalizer, err)
	}
	cfg.ModelNormalizer = mergeByKey(cfg.ModelNormalizer, fromEnv, func(rule ModelNormalizerRule) string {
		return canonicalTextKey(rule.Alias)
	})
	return nil
}