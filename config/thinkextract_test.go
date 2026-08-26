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
	if cfg.IsEnabledForMessages() {
		t.Errorf("zero config: IsEnabledForMessages=true, want false (messages policy defaults to off)")
	}
	if got := cfg.MessagesPolicyOrDefault(); got != "off" {
		t.Errorf("MessagesPolicyOrDefault=%q, want %q", got, "off")
	}
}

func TestThinkExtractConfig_GlobalOff(t *testing.T) {
	cfg := ThinkExtractConfig{Enabled: boolPtr(false)}
	if cfg.IsEnabled() {
		t.Errorf("IsEnabled=true, want false")
	}
	if cfg.IsEnabledForChat() {
		t.Errorf("IsEnabledForChat=true, want false (falls back to global)")
	}
	if cfg.IsEnabledForMessages() {
		t.Errorf("IsEnabledForMessages=true, want false (falls back to global)")
	}
}

func TestThinkExtractConfig_PerSurfaceOverride(t *testing.T) {
	cfg := ThinkExtractConfig{
		Enabled:        boolPtr(true),
		ChatEnabled:    boolPtr(false),
		MessagesPolicy: "unsigned",
	}
	if !cfg.IsEnabled() {
		t.Errorf("IsEnabled=false, want true")
	}
	if cfg.IsEnabledForChat() {
		t.Errorf("IsEnabledForChat=true, want false (per-surface override)")
	}
	if !cfg.IsEnabledForMessages() {
		t.Errorf("IsEnabledForMessages=false, want true (unsigned policy)")
	}
}

func TestThinkExtractConfig_PerSurfaceTrueCannotResurrect(t *testing.T) {
	cfg := ThinkExtractConfig{
		Enabled:     boolPtr(false),
		ChatEnabled: boolPtr(true),
	}
	if cfg.IsEnabled() {
		t.Errorf("IsEnabled=true, want false")
	}
	if !cfg.IsEnabledForChat() {
		t.Errorf("IsEnabledForChat=false, want true (per-surface value)")
	}
}

func TestThinkExtractConfig_MessagesPolicyParsing(t *testing.T) {
	tests := []struct {
		name string
		cfg  ThinkExtractConfig
		want bool
	}{
		{name: "empty means off", cfg: ThinkExtractConfig{}, want: false},
		{name: "off explicit", cfg: ThinkExtractConfig{MessagesPolicy: "off"}, want: false},
		{name: "unsigned enables", cfg: ThinkExtractConfig{MessagesPolicy: "unsigned"}, want: true},
		{name: "redacted enables", cfg: ThinkExtractConfig{MessagesPolicy: "redacted"}, want: true},
		{name: "unknown falls back to off", cfg: ThinkExtractConfig{MessagesPolicy: "nonsense"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.IsEnabledForMessages(); got != tt.want {
				t.Errorf("IsEnabledForMessages()=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestThinkExtractConfig_MessagesPolicyOrDefault(t *testing.T) {
	if got := (ThinkExtractConfig{}).MessagesPolicyOrDefault(); got != "off" {
		t.Errorf("empty cfg default=%q, want off", got)
	}
	if got := (ThinkExtractConfig{MessagesPolicy: "redacted"}).MessagesPolicyOrDefault(); got != "redacted" {
		t.Errorf("explicit cfg default=%q, want redacted", got)
	}
}