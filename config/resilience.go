package config

import (
	"fmt"
	"time"
)

// RetryConfig holds resolved retry settings for an LLM client.
// This is the canonical type shared between config and llmclient.
type RetryConfig struct {
	MaxRetries     int           `yaml:"max_retries"     env:"RETRY_MAX_RETRIES"`
	InitialBackoff time.Duration `yaml:"initial_backoff" env:"RETRY_INITIAL_BACKOFF"`
	MaxBackoff     time.Duration `yaml:"max_backoff"     env:"RETRY_MAX_BACKOFF"`
	BackoffFactor  float64       `yaml:"backoff_factor"  env:"RETRY_BACKOFF_FACTOR"`
	JitterFactor   float64       `yaml:"jitter_factor"   env:"RETRY_JITTER_FACTOR"`
}

// DefaultRetryConfig returns the default retry settings.
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:     3,
		InitialBackoff: 1 * time.Second,
		MaxBackoff:     30 * time.Second,
		BackoffFactor:  2.0,
		JitterFactor:   0.1,
	}
}

// CircuitBreakerConfig holds resolved circuit breaker settings.
// This is the canonical type shared between config and llmclient.
type CircuitBreakerConfig struct {
	// Enabled switches the circuit breaker on or off. When false, requests are
	// never short-circuited regardless of the thresholds below.
	// Default: true
	Enabled          bool          `yaml:"enabled"           env:"CIRCUIT_BREAKER_ENABLED"`
	FailureThreshold int           `yaml:"failure_threshold" env:"CIRCUIT_BREAKER_FAILURE_THRESHOLD"`
	SuccessThreshold int           `yaml:"success_threshold" env:"CIRCUIT_BREAKER_SUCCESS_THRESHOLD"`
	Timeout          time.Duration `yaml:"timeout"           env:"CIRCUIT_BREAKER_TIMEOUT"`
}

// DefaultCircuitBreakerConfig returns the default circuit breaker settings.
func DefaultCircuitBreakerConfig() CircuitBreakerConfig {
	return CircuitBreakerConfig{
		Enabled:          true,
		FailureThreshold: 5,
		SuccessThreshold: 2,
		Timeout:          30 * time.Second,
	}
}

// ResilienceConfig holds resolved resilience settings (retry and circuit breaker).
type ResilienceConfig struct {
	Retry          RetryConfig          `yaml:"retry"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
	// StreamRepetitionLimit aborts a chat SSE stream when the same text unit
	// repeats this many times consecutively. 0 (default) disables the guard.
	StreamRepetitionLimit int `yaml:"stream_repetition_limit" env:"STREAM_REPETITION_LIMIT"`
	// StreamRepetitionMaxPattern is the maximum chain length in tokens that
	// the repetition guard considers as one repeating unit. 0 selects the
	// built-in default (8).
	StreamRepetitionMaxPattern int `yaml:"stream_repetition_max_pattern" env:"STREAM_REPETITION_MAX_PATTERN"`
}

// validateStreamRepetitionConfig rejects global repetition-guard settings that
// would silently clamp later. A negative or singular limit is meaningless —
// the guard clamps it to the minimum of 2 — so it is a config error here,
// matching the per-virtual-model rule in internal/virtualmodels/validation.go.
// StreamRepetitionMaxPattern must be 0 (built-in default of 8) or in 1..64;
// values outside that range would also be silently clamped.
func validateStreamRepetitionConfig(cfg *ResilienceConfig) error {
	if cfg.StreamRepetitionLimit < 0 || cfg.StreamRepetitionLimit == 1 {
		return fmt.Errorf("resilience.stream_repetition_limit must be 0 (disabled) or at least 2, got %d", cfg.StreamRepetitionLimit)
	}
	if cfg.StreamRepetitionMaxPattern < 0 || cfg.StreamRepetitionMaxPattern > 64 {
		return fmt.Errorf("resilience.stream_repetition_max_pattern must be 0 (default) or between 1 and 64, got %d", cfg.StreamRepetitionMaxPattern)
	}
	return nil
}

// RawResilienceConfig holds optional per-provider resilience overrides from YAML.
// Nil fields inherit from the global ResilienceConfig.
type RawResilienceConfig struct {
	Retry          *RawRetryConfig          `yaml:"retry"`
	CircuitBreaker *RawCircuitBreakerConfig `yaml:"circuit_breaker"`
}

// RawCircuitBreakerConfig holds optional per-provider circuit breaker overrides from YAML.
// Nil fields inherit from the global CircuitBreakerConfig.
type RawCircuitBreakerConfig struct {
	Enabled          *bool          `yaml:"enabled"`
	FailureThreshold *int           `yaml:"failure_threshold"`
	SuccessThreshold *int           `yaml:"success_threshold"`
	Timeout          *time.Duration `yaml:"timeout"`
}

// RawRetryConfig holds optional per-provider retry overrides from YAML.
// Nil fields inherit from the global RetryConfig.
type RawRetryConfig struct {
	MaxRetries     *int           `yaml:"max_retries"`
	InitialBackoff *time.Duration `yaml:"initial_backoff"`
	MaxBackoff     *time.Duration `yaml:"max_backoff"`
	BackoffFactor  *float64       `yaml:"backoff_factor"`
	JitterFactor   *float64       `yaml:"jitter_factor"`
}
