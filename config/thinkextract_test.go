package config

import "testing"

func boolPtr(v bool) *bool { return &v }

func TestThinkExtractConfig_Defaults(t *testing.T) {
	cfg := ThinkExtractConfig{}
	if !cfg.IsEnabled() {
		t.Errorf("zero config: IsEnabled=false, want true")
	}
	if !cfg.IsEnabledForChat() {
		t.Errorf("zero config: IsEnabledForChat=false, want true")
	}
	if !cfg.IsEnabledForMessages() {
		t.Errorf("zero config: IsEnabledForMessages=false, want true")
	}
}

func TestThinkExtractConfig_GlobalOff(t *testing.T) {
	cfg := ThinkExtractConfig{Enabled: boolPtr(false)}
	if cfg.IsEnabled() {
		t.Errorf("IsEnabled=true, want false")
	}
	// Per-surface helpers fall back to the global switch.
	if cfg.IsEnabledForChat() {
		t.Errorf("IsEnabledForChat=true, want false (falls back to global)")
	}
	if cfg.IsEnabledForMessages() {
		t.Errorf("IsEnabledForMessages=true, want false (falls back to global)")
	}
}

func TestThinkExtractConfig_PerSurfaceOverride(t *testing.T) {
	cfg := ThinkExtractConfig{
		Enabled:         boolPtr(true),
		ChatEnabled:     boolPtr(false),
		MessagesEnabled: boolPtr(true),
	}
	if !cfg.IsEnabled() {
		t.Errorf("IsEnabled=false, want true")
	}
	if cfg.IsEnabledForChat() {
		t.Errorf("IsEnabledForChat=true, want false (per-surface override)")
	}
	if !cfg.IsEnabledForMessages() {
		t.Errorf("IsEnabledForMessages=false, want true (per-surface override)")
	}
}

func TestThinkExtractConfig_PerSurfaceTrueCannotResurrect(t *testing.T) {
	// The global switch is authoritative: per-surface true must not
	// resurrect the feature when the global switch is off. The app helper
	// enforces this by returning nil options when IsEnabled is false.
	cfg := ThinkExtractConfig{
		Enabled:     boolPtr(false),
		ChatEnabled: boolPtr(true),
	}
	if cfg.IsEnabled() {
		t.Errorf("IsEnabled=true, want false")
	}
	// IsEnabledForChat honours the explicit per-surface value; the
	// master-switch semantics live in thinkExtractOptionsFromConfig.
	if !cfg.IsEnabledForChat() {
		t.Errorf("IsEnabledForChat=false, want true (per-surface value)")
	}
}