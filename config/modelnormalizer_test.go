package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestApplyModelNormalizerEnv_ParsesAndMerges(t *testing.T) {
	cfg := &Config{ModelNormalizer: []ModelNormalizerRule{
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding", Thinking: "disabled"},
		{Alias: "kimi-k2.7-code", Target: "kimicode/kimi-for-coding"},
	}}
	t.Setenv(envModelNormalizer, `[
		{"alias":"kimi-k2.6","target":"kimicode/kimi-for-coding","thinking":"passthrough"},
		{"alias":"kimi-k3","target":"kimicode/k3","context_window":1048576,"modes":["chat"]}
	]`)

	if err := applyModelNormalizerEnv(cfg, true); err != nil {
		t.Fatalf("applyModelNormalizerEnv() error = %v", err)
	}
	if len(cfg.ModelNormalizer) != 3 {
		t.Fatalf("merged len = %d, want 3", len(cfg.ModelNormalizer))
	}
	// "kimi-k2.6" is overridden in place (env wins) and keeps its position.
	k26 := cfg.ModelNormalizer[0]
	if k26.Alias != "kimi-k2.6" || k26.Thinking != "passthrough" {
		t.Fatalf("env did not override kimi-k2.6: %#v", k26)
	}
	// "kimi-k2.7-code" is untouched; "kimi-k3" is appended.
	if cfg.ModelNormalizer[1].Alias != "kimi-k2.7-code" || cfg.ModelNormalizer[2].Alias != "kimi-k3" {
		t.Fatalf("merge order wrong: %#v", cfg.ModelNormalizer)
	}
	// Verify the new entry carries metadata.
	k3 := cfg.ModelNormalizer[2]
	if k3.ContextWindow == nil || *k3.ContextWindow != 1048576 {
		t.Fatalf("kimi-k3 context_window = %v, want 1048576", k3.ContextWindow)
	}
	if len(k3.Modes) != 1 || k3.Modes[0] != "chat" {
		t.Fatalf("kimi-k3 modes = %v, want [chat]", k3.Modes)
	}
}

func TestApplyModelNormalizerEnv_Invalid(t *testing.T) {
	cfg := &Config{}
	t.Setenv(envModelNormalizer, `{not valid json`)
	if err := applyModelNormalizerEnv(cfg, true); err == nil {
		t.Fatalf("applyModelNormalizerEnv() error = nil, want parse error")
	}
}

// The env layer overrides YAML entry by entry, so a typo must fail loudly rather
// than let a malformed env entry silently win over a correct YAML one.
func TestApplyModelNormalizerEnv_RejectsUnknownField(t *testing.T) {
	cfg := &Config{}
	t.Setenv(envModelNormalizer, `[{"alias":"kimi-k2.6","targets":"kimicode/kimi-for-coding"}]`)

	err := applyModelNormalizerEnv(cfg, true)
	if err == nil {
		t.Fatal("applyModelNormalizerEnv() error = nil, want unknown-field error")
	}
	if !strings.Contains(err.Error(), "targets") {
		t.Fatalf("applyModelNormalizerEnv() error = %q, want it to name the unknown field", err)
	}
}

// json.Decoder stops after the first value and leaves the rest unread, so trailing
// data must be rejected explicitly — silently applying half an env var is the failure
// this path exists to prevent. Structural, therefore fatal in both modes.
func TestApplyModelNormalizerEnv_RejectsTrailingData(t *testing.T) {
	trailing := map[string]string{
		"garbage suffix":        `[{"alias":"a","target":"b"}] and then some junk`,
		"second JSON value":     `[{"alias":"a","target":"b"}] {"alias":"c","target":"d"}`,
		"second JSON on a line": "[{\"alias\":\"a\",\"target\":\"b\"}]\n{\"alias\":\"c\",\"target\":\"d\"}",
	}
	for name, raw := range trailing {
		for _, strict := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/strict=%v", name, strict), func(t *testing.T) {
				cfg := &Config{}
				t.Setenv(envModelNormalizer, raw)

				err := applyModelNormalizerEnv(cfg, strict)
				if err == nil {
					t.Fatal("applyModelNormalizerEnv() error = nil, want trailing-data error")
				}
				if !strings.Contains(err.Error(), "unexpected data after the JSON value") {
					t.Fatalf("applyModelNormalizerEnv() error = %q, want a trailing-data error", err)
				}
			})
		}
	}
}

func TestValidateModelNormalizerRules(t *testing.T) {
	tests := []struct {
		name    string
		rules   []ModelNormalizerRule
		wantErr string
	}{
		{name: "nil is valid"},
		{name: "empty slice is valid", rules: []ModelNormalizerRule{}},
		{name: "valid rule", rules: []ModelNormalizerRule{{Alias: "a", Target: "b"}}},
		{name: "valid with thinking", rules: []ModelNormalizerRule{{Alias: "a", Target: "b", Thinking: "disabled"}}},
		{name: "valid with passthrough", rules: []ModelNormalizerRule{{Alias: "a", Target: "b", Thinking: "passthrough"}}},
		{name: "missing alias", rules: []ModelNormalizerRule{{Target: "b"}}, wantErr: "alias is required"},
		{name: "missing target", rules: []ModelNormalizerRule{{Alias: "a"}}, wantErr: "target is required"},
		{name: "invalid thinking", rules: []ModelNormalizerRule{{Alias: "a", Target: "b", Thinking: "bogus"}}, wantErr: "thinking must be one of"},
		{name: "whitespace alias", rules: []ModelNormalizerRule{{Alias: "  ", Target: "b"}}, wantErr: "alias is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateModelNormalizerRules(tt.rules)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateModelNormalizerRules() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateModelNormalizerRules() error = nil, want %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateModelNormalizerRules() error = %q, want to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestApplyModelNormalizerEnv_NoOpWhenUnset(t *testing.T) {
	cfg := &Config{ModelNormalizer: []ModelNormalizerRule{
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding"},
	}}
	if err := applyModelNormalizerEnv(cfg, true); err != nil {
		t.Fatalf("applyModelNormalizerEnv() error = %v", err)
	}
	if len(cfg.ModelNormalizer) != 1 {
		t.Fatalf("len = %d, want 1 (unchanged)", len(cfg.ModelNormalizer))
	}
}

func TestApplyModelNormalizerEnv_CaseInsensitiveAliasKey(t *testing.T) {
	cfg := &Config{ModelNormalizer: []ModelNormalizerRule{
		{Alias: "Kimi-K2.6", Target: "kimicode/kimi-for-coding", Thinking: "disabled"},
	}}
	t.Setenv(envModelNormalizer, `[{"alias":"kimi-k2.6","target":"kimicode/kimi-for-coding","thinking":"passthrough"}]`)

	if err := applyModelNormalizerEnv(cfg, true); err != nil {
		t.Fatalf("applyModelNormalizerEnv() error = %v", err)
	}
	if len(cfg.ModelNormalizer) != 1 {
		t.Fatalf("len = %d, want 1 (case-insensitive alias key)", len(cfg.ModelNormalizer))
	}
	if cfg.ModelNormalizer[0].Thinking != "passthrough" {
		t.Fatalf("thinking = %q, want passthrough", cfg.ModelNormalizer[0].Thinking)
	}
}
