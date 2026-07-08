package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRawProviderConfig_YAMLHeaderFields(t *testing.T) {
	yamlBytes := []byte(`
custom_upstream_headers:
  X-Custom-Header: value-a
  X-Another: value-b
passthrough_user_headers: true
passthrough_user_headers_skip:
  - authorization
  - cookie
passthrough_user_headers_skip_mode: allow
`)

	var got RawProviderConfig
	if err := yaml.Unmarshal(yamlBytes, &got); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}

	wantHeaders := map[string]string{
		"X-Custom-Header": "value-a",
		"X-Another":       "value-b",
	}
	if !reflect.DeepEqual(got.CustomUpstreamHeaders, wantHeaders) {
		t.Errorf("CustomUpstreamHeaders = %v, want %v", got.CustomUpstreamHeaders, wantHeaders)
	}
	if !got.PassthroughUserHeaders {
		t.Errorf("PassthroughUserHeaders = %v, want true", got.PassthroughUserHeaders)
	}
	wantSkip := []string{"authorization", "cookie"}
	if !reflect.DeepEqual(got.PassthroughUserHeadersSkip, wantSkip) {
		t.Errorf("PassthroughUserHeadersSkip = %v, want %v", got.PassthroughUserHeadersSkip, wantSkip)
	}
	if got.PassthroughUserHeadersSkipMode != "allow" {
		t.Errorf("PassthroughUserHeadersSkipMode = %q, want allow", got.PassthroughUserHeadersSkipMode)
	}
}

func TestRawProviderConfig_YAMLDefaults(t *testing.T) {
	yamlBytes := []byte(`
type: openai
api_key: sk-test
`)

	var got RawProviderConfig
	if err := yaml.Unmarshal(yamlBytes, &got); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}

	if got.CustomUpstreamHeaders != nil {
		t.Errorf("CustomUpstreamHeaders = %v, want nil", got.CustomUpstreamHeaders)
	}
	if got.PassthroughUserHeaders {
		t.Errorf("PassthroughUserHeaders = %v, want false", got.PassthroughUserHeaders)
	}
	if got.PassthroughUserHeadersSkip != nil {
		t.Errorf("PassthroughUserHeadersSkip = %v, want nil", got.PassthroughUserHeadersSkip)
	}
	if got.PassthroughUserHeadersSkipMode != "" {
		t.Errorf("PassthroughUserHeadersSkipMode = %q, want empty", got.PassthroughUserHeadersSkipMode)
	}
}

func TestRawProviderConfig_YAMLCustomHeadersEmpty(t *testing.T) {
	yamlBytes := []byte(`
custom_upstream_headers: {}
`)

	var got RawProviderConfig
	if err := yaml.Unmarshal(yamlBytes, &got); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}

	if got.CustomUpstreamHeaders == nil || len(got.CustomUpstreamHeaders) != 0 {
		t.Errorf("CustomUpstreamHeaders = %v, want empty map", got.CustomUpstreamHeaders)
	}
}

func TestLoad_ProviderHeaderFieldsFromYAML(t *testing.T) {
	clearAllConfigEnvVars(t)

	withTempDir(t, func(dir string) {
		yaml := `
providers:
  openai:
    type: openai
    api_key: sk-yaml-key
    custom_upstream_headers:
      X-Custom-Source: yaml
    passthrough_user_headers: true
    passthrough_user_headers_skip:
      - authorization
    passthrough_user_headers_skip_mode: skip
`
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0644); err != nil {
			t.Fatalf("Failed to write config.yaml: %v", err)
		}

		result, err := Load()
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}

		provider, exists := result.RawProviders["openai"]
		if !exists {
			t.Fatal("expected 'openai' raw provider to exist")
		}
		if got, want := provider.CustomUpstreamHeaders, map[string]string{"X-Custom-Source": "yaml"}; !reflect.DeepEqual(got, want) {
			t.Errorf("CustomUpstreamHeaders = %v, want %v", got, want)
		}
		if !provider.PassthroughUserHeaders {
			t.Errorf("PassthroughUserHeaders = %v, want true", provider.PassthroughUserHeaders)
		}
		if got, want := provider.PassthroughUserHeadersSkip, []string{"authorization"}; !reflect.DeepEqual(got, want) {
			t.Errorf("PassthroughUserHeadersSkip = %v, want %v", got, want)
		}
		if got, want := provider.PassthroughUserHeadersSkipMode, "skip"; got != want {
			t.Errorf("PassthroughUserHeadersSkipMode = %q, want %q", got, want)
		}
	})
}
