package virtualmodels

import (
	"context"
	"testing"

	"github.com/goccy/go-json"

	"github.com/enterpilot/gomodel/config"
	"github.com/enterpilot/gomodel/internal/core"
)

func TestRewriteChatRequest_DisableReasoningStripsControls(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newTestService(t)
	if err := svc.Upsert(ctx, VirtualModel{
		Source:           "kimi-k2.6",
		Targets:          []Target{{Provider: "openai", Model: "gpt-4o"}},
		DisableReasoning: true,
		Enabled:          true,
	}); err != nil {
		t.Fatalf("Upsert(redirect) error = %v", err)
	}
	checker := testCatalog()

	req := &core.ChatRequest{
		Model:     "kimi-k2.6",
		Reasoning: &core.Reasoning{Effort: "high"},
		ExtraFields: core.UnknownJSONFieldsFromMap(map[string]json.RawMessage{
			"reasoning_effort": json.RawMessage(`"high"`),
			"keep_me":          json.RawMessage(`"yes"`),
		}),
	}
	chat, err := rewriteChatRequest(ctx, svc, checker, req)
	if err != nil {
		t.Fatalf("rewriteChatRequest() error = %v", err)
	}
	if chat.Model != "gpt-4o" || chat.Provider != "openai" {
		t.Fatalf("rewriteChatRequest() selector = %q/%q, want openai/gpt-4o", chat.Provider, chat.Model)
	}
	if chat.Reasoning != nil {
		t.Fatalf("Reasoning = %+v, want nil when disable_reasoning is set", chat.Reasoning)
	}
	if got := chat.ExtraFields.Lookup("reasoning_effort"); got != nil {
		t.Fatalf("reasoning_effort = %s, want stripped", got)
	}
	thinking := chat.ExtraFields.Lookup("thinking")
	if thinking == nil {
		t.Fatal("thinking field missing, want thinking.type=disabled")
	}
	var thinkingMap map[string]string
	if err := json.Unmarshal(thinking, &thinkingMap); err != nil || thinkingMap["type"] != "disabled" {
		t.Fatalf("thinking = %s, want {\"type\":\"disabled\"}", thinking)
	}
	if got := chat.ExtraFields.Lookup("keep_me"); string(got) != `"yes"` {
		t.Fatalf("keep_me = %s, want preserved", got)
	}

	// The caller's request must not be mutated.
	if req.Reasoning == nil || req.Reasoning.Effort != "high" {
		t.Fatalf("caller Reasoning mutated: %+v", req.Reasoning)
	}
	if got := req.ExtraFields.Lookup("reasoning_effort"); string(got) != `"high"` {
		t.Fatalf("caller reasoning_effort mutated: %s", got)
	}
}

func TestRewriteChatRequest_DisableReasoningUnsetKeepsControls(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	svc := newRedirectService(t)
	checker := testCatalog()

	req := &core.ChatRequest{
		Model:     "fast",
		Reasoning: &core.Reasoning{Effort: "high"},
		ExtraFields: core.UnknownJSONFieldsFromMap(map[string]json.RawMessage{
			"reasoning_effort": json.RawMessage(`"high"`),
		}),
	}
	chat, err := rewriteChatRequest(ctx, svc, checker, req)
	if err != nil {
		t.Fatalf("rewriteChatRequest() error = %v", err)
	}
	if chat.Reasoning == nil || chat.Reasoning.Effort != "high" {
		t.Fatalf("Reasoning = %+v, want preserved when disable_reasoning is unset", chat.Reasoning)
	}
	if got := chat.ExtraFields.Lookup("reasoning_effort"); string(got) != `"high"` {
		t.Fatalf("reasoning_effort = %s, want preserved", got)
	}
	if got := chat.ExtraFields.Lookup("thinking"); got != nil {
		t.Fatalf("thinking = %s, want absent when disable_reasoning is unset", got)
	}
}

func TestService_DisableReasoningForSource(t *testing.T) {
	t.Parallel()
	svc := newTestService(t)
	ctx := context.Background()
	if err := svc.Upsert(ctx, VirtualModel{
		Source:           "k26",
		Targets:          []Target{{Provider: "openai", Model: "gpt-4o"}},
		DisableReasoning: true,
		Enabled:          true,
	}); err != nil {
		t.Fatalf("Upsert(k26) error = %v", err)
	}
	if err := svc.Upsert(ctx, VirtualModel{
		Source:  "k27",
		Targets: []Target{{Provider: "openai", Model: "gpt-4o"}},
		Enabled: true,
	}); err != nil {
		t.Fatalf("Upsert(k27) error = %v", err)
	}

	if !svc.DisableReasoningForSource("k26") {
		t.Error("DisableReasoningForSource(k26) = false, want true")
	}
	if svc.DisableReasoningForSource("k27") {
		t.Error("DisableReasoningForSource(k27) = true, want false")
	}
	if svc.DisableReasoningForSource("missing") {
		t.Error("DisableReasoningForSource(missing) = true, want false")
	}
	if svc.DisableReasoningForSource("") {
		t.Error("DisableReasoningForSource(\"\") = true, want false")
	}
}

func TestConfigModel_MapsDisableReasoning(t *testing.T) {
	t.Parallel()
	models := ConfigModels([]config.VirtualModelConfig{{Source: "k26", Target: "openai/gpt-4o", DisableReasoning: true}})
	if len(models) != 1 {
		t.Fatalf("len(models) = %d, want 1", len(models))
	}
	if !models[0].DisableReasoning {
		t.Error("DisableReasoning = false, want true after config mapping")
	}
	if !models[0].Managed {
		t.Error("Managed = false, want true for config-declared models")
	}
}
