package config

import "testing"

func TestValidateStreamRepetitionConfigRejectsLimitOne(t *testing.T) {
	cfg := buildDefaultConfig()
	cfg.Resilience.StreamRepetitionLimit = 1
	err := validateStreamRepetitionConfig(&cfg.Resilience)
	if err == nil {
		t.Fatalf("expected error for stream_repetition_limit = 1, got nil")
	}

	cfg.Resilience.StreamRepetitionLimit = 0
	if err := validateStreamRepetitionConfig(&cfg.Resilience); err != nil {
		t.Fatalf("limit 0 (disabled) must be valid, got %v", err)
	}

	cfg.Resilience.StreamRepetitionLimit = 3
	if err := validateStreamRepetitionConfig(&cfg.Resilience); err != nil {
		t.Fatalf("limit 3 must be valid, got %v", err)
	}

	cfg.Resilience.StreamRepetitionLimit = -1
	if err := validateStreamRepetitionConfig(&cfg.Resilience); err == nil {
		t.Fatalf("expected error for stream_repetition_limit = -1, got nil")
	}
	cfg.Resilience.StreamRepetitionLimit = 0

	cfg.Resilience.StreamRepetitionMaxPattern = -1
	if err := validateStreamRepetitionConfig(&cfg.Resilience); err == nil {
		t.Fatalf("expected error for stream_repetition_max_pattern = -1, got nil")
	}
	cfg.Resilience.StreamRepetitionMaxPattern = 65
	if err := validateStreamRepetitionConfig(&cfg.Resilience); err == nil {
		t.Fatalf("expected error for stream_repetition_max_pattern = 65, got nil")
	}
	cfg.Resilience.StreamRepetitionMaxPattern = 0
	if err := validateStreamRepetitionConfig(&cfg.Resilience); err != nil {
		t.Fatalf("max_pattern 0 (default) must be valid, got %v", err)
	}
	cfg.Resilience.StreamRepetitionMaxPattern = 64
	if err := validateStreamRepetitionConfig(&cfg.Resilience); err != nil {
		t.Fatalf("max_pattern 64 must be valid, got %v", err)
	}
}

func TestLoadRejectsStreamRepetitionLimitOneViaEnv(t *testing.T) {
	clearAllConfigEnvVars(t)

	withTempDir(t, func(string) {
		t.Setenv("STREAM_REPETITION_LIMIT", "1")

		_, err := Load()
		if err == nil {
			t.Fatalf("Load() must reject STREAM_REPETITION_LIMIT=1, got nil error")
		}
	})
}
