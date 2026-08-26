package modelnormalizer

import (
	"testing"

	"github.com/goccy/go-json"

	"github.com/enterpilot/gomodel/internal/core"
)

func TestAdaptChatRequest_RewritesModel(t *testing.T) {
	n := New([]Rule{
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding", Thinking: ThinkingDisabled},
	})

	req := &core.ChatRequest{Model: "kimi-k2.6"}
	adapted, rewritten, err := n.AdaptChatRequest(req)
	if err != nil {
		t.Fatalf("AdaptChatRequest() error = %v", err)
	}
	if !rewritten {
		t.Fatal("AdaptChatRequest() rewritten = false, want true")
	}
	if adapted.Model != "kimicode/kimi-for-coding" {
		t.Fatalf("AdaptChatRequest() model = %q, want %q", adapted.Model, "kimicode/kimi-for-coding")
	}
	if adapted == req {
		t.Fatal("AdaptChatRequest() returned the same pointer, want a copy")
	}
}

func TestAdaptChatRequest_InjectsThinkingDisabled(t *testing.T) {
	n := New([]Rule{
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding", Thinking: ThinkingDisabled},
	})

	req := &core.ChatRequest{Model: "kimi-k2.6"}
	adapted, rewritten, err := n.AdaptChatRequest(req)
	if err != nil {
		t.Fatalf("AdaptChatRequest() error = %v", err)
	}
	if !rewritten {
		t.Fatal("AdaptChatRequest() rewritten = false, want true")
	}

	thinking := adapted.ExtraFields.Lookup("thinking")
	if thinking == nil {
		t.Fatal("AdaptChatRequest() thinking field missing from ExtraFields")
	}
	var parsed struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(thinking, &parsed); err != nil {
		t.Fatalf("unmarshal thinking: %v", err)
	}
	if parsed.Type != "disabled" {
		t.Fatalf("thinking.type = %q, want %q", parsed.Type, "disabled")
	}
}

func TestAdaptChatRequest_InjectsThinkingEnabled(t *testing.T) {
	n := New([]Rule{
		{Alias: "kimi-k2.7-code", Target: "kimicode/kimi-for-coding", Thinking: ThinkingEnabled},
	})

	req := &core.ChatRequest{Model: "kimi-k2.7-code"}
	adapted, rewritten, err := n.AdaptChatRequest(req)
	if err != nil {
		t.Fatalf("AdaptChatRequest() error = %v", err)
	}
	if !rewritten {
		t.Fatal("AdaptChatRequest() rewritten = false, want true")
	}

	thinking := adapted.ExtraFields.Lookup("thinking")
	if thinking == nil {
		t.Fatal("AdaptChatRequest() thinking field missing from ExtraFields")
	}
	var parsed struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(thinking, &parsed); err != nil {
		t.Fatalf("unmarshal thinking: %v", err)
	}
	if parsed.Type != "enabled" {
		t.Fatalf("thinking.type = %q, want %q", parsed.Type, "enabled")
	}
}

func TestAdaptChatRequest_PassthroughLeavesThinkingUntouched(t *testing.T) {
	n := New([]Rule{
		{Alias: "kimi-k3", Target: "kimicode/k3", Thinking: ThinkingPassthrough},
	})

	req := &core.ChatRequest{Model: "kimi-k3"}
	adapted, rewritten, err := n.AdaptChatRequest(req)
	if err != nil {
		t.Fatalf("AdaptChatRequest() error = %v", err)
	}
	if !rewritten {
		t.Fatal("AdaptChatRequest() rewritten = false, want true")
	}
	if adapted.Model != "kimicode/k3" {
		t.Fatalf("AdaptChatRequest() model = %q, want %q", adapted.Model, "kimicode/k3")
	}
	// No thinking field should be injected.
	if thinking := adapted.ExtraFields.Lookup("thinking"); thinking != nil {
		t.Fatalf("thinking = %s, want nil for passthrough", thinking)
	}
}

func TestAdaptChatRequest_PreservesExistingExtraFields(t *testing.T) {
	n := New([]Rule{
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding", Thinking: ThinkingDisabled},
	})

	req := &core.ChatRequest{
		Model: "kimi-k2.6",
		ExtraFields: core.UnknownJSONFieldsFromMap(map[string]json.RawMessage{
			"custom": json.RawMessage(`"value"`),
		}),
	}
	adapted, rewritten, err := n.AdaptChatRequest(req)
	if err != nil {
		t.Fatalf("AdaptChatRequest() error = %v", err)
	}
	if !rewritten {
		t.Fatal("AdaptChatRequest() rewritten = false, want true")
	}
	// Both the original field and the injected thinking should be present.
	if custom := adapted.ExtraFields.Lookup("custom"); custom == nil {
		t.Fatal("custom field lost from ExtraFields")
	}
	if thinking := adapted.ExtraFields.Lookup("thinking"); thinking == nil {
		t.Fatal("thinking field missing from ExtraFields")
	}
}

func TestAdaptChatRequest_UnknownModelPassesThrough(t *testing.T) {
	n := New([]Rule{
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding"},
	})

	req := &core.ChatRequest{Model: "unknown-model"}
	adapted, rewritten, err := n.AdaptChatRequest(req)
	if err != nil {
		t.Fatalf("AdaptChatRequest() error = %v", err)
	}
	if rewritten {
		t.Fatal("AdaptChatRequest() rewritten = true, want false for unknown model")
	}
	if adapted != req {
		t.Fatal("AdaptChatRequest() returned a different pointer for unknown model, want same")
	}
	if adapted.Model != "unknown-model" {
		t.Fatalf("AdaptChatRequest() model = %q, want %q", adapted.Model, "unknown-model")
	}
}

func TestAdaptChatRequest_NilNormalizerPassesThrough(t *testing.T) {
	var n *Normalizer
	req := &core.ChatRequest{Model: "kimi-k2.6"}
	adapted, rewritten, err := n.AdaptChatRequest(req)
	if err != nil {
		t.Fatalf("AdaptChatRequest() error = %v", err)
	}
	if rewritten {
		t.Fatal("AdaptChatRequest() rewritten = true, want false for nil normalizer")
	}
	if adapted != req {
		t.Fatal("AdaptChatRequest() returned a different pointer, want same")
	}
}

func TestAdaptChatRequest_NilRequestPassesThrough(t *testing.T) {
	n := New([]Rule{{Alias: "a", Target: "b"}})
	adapted, rewritten, err := n.AdaptChatRequest(nil)
	if err != nil {
		t.Fatalf("AdaptChatRequest() error = %v", err)
	}
	if rewritten {
		t.Fatal("AdaptChatRequest() rewritten = true, want false for nil request")
	}
	if adapted != nil {
		t.Fatal("AdaptChatRequest() returned non-nil for nil request")
	}
}

func TestAdaptChatRequest_EmptyThinkingIsPassthrough(t *testing.T) {
	n := New([]Rule{
		{Alias: "kimi-k3", Target: "kimicode/k3"}, // no thinking field
	})

	req := &core.ChatRequest{Model: "kimi-k3"}
	adapted, rewritten, err := n.AdaptChatRequest(req)
	if err != nil {
		t.Fatalf("AdaptChatRequest() error = %v", err)
	}
	if !rewritten {
		t.Fatal("AdaptChatRequest() rewritten = false, want true")
	}
	if thinking := adapted.ExtraFields.Lookup("thinking"); thinking != nil {
		t.Fatalf("thinking = %s, want nil when no thinking policy set", thinking)
	}
}

func TestNew_SkipsEmptyAliasOrTarget(t *testing.T) {
	n := New([]Rule{
		{Alias: "", Target: "kimicode/kimi-for-coding"},
		{Alias: "kimi-k2.6", Target: ""},
		{Alias: "  ", Target: "kimicode/kimi-for-coding"},
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding"},
	})
	if n == nil {
		t.Fatal("New() returned nil, want non-nil with one valid rule")
	}
	aliases := n.Aliases()
	if len(aliases) != 1 || aliases[0] != "kimi-k2.6" {
		t.Fatalf("Aliases() = %v, want [kimi-k2.6]", aliases)
	}
}

func TestNew_LastWinsForDuplicateAlias(t *testing.T) {
	n := New([]Rule{
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding", Thinking: ThinkingDisabled},
		{Alias: "kimi-k2.6", Target: "kimicode/k3", Thinking: ThinkingPassthrough},
	})
	if n == nil {
		t.Fatal("New() returned nil")
	}
	r, ok := n.Lookup("kimi-k2.6")
	if !ok {
		t.Fatal("Lookup() not found")
	}
	if r.Target != "kimicode/k3" {
		t.Fatalf("Target = %q, want %q (last wins)", r.Target, "kimicode/k3")
	}
	if r.Thinking != ThinkingPassthrough {
		t.Fatalf("Thinking = %q, want %q (last wins)", r.Thinking, ThinkingPassthrough)
	}
}

func TestNew_NilForNoValidRules(t *testing.T) {
	n := New(nil)
	if n != nil {
		t.Fatal("New(nil) returned non-nil, want nil")
	}
	n = New([]Rule{{Alias: "", Target: ""}})
	if n != nil {
		t.Fatal("New(empty rules) returned non-nil, want nil")
	}
}

func TestMergeRules_EnvOverridesConfig(t *testing.T) {
	base := []Rule{
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding", Thinking: ThinkingDisabled},
		{Alias: "kimi-k2.7-code", Target: "kimicode/kimi-for-coding"},
	}
	override := []Rule{
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding", Thinking: ThinkingPassthrough},
		{Alias: "kimi-k3", Target: "kimicode/k3"},
	}

	merged := MergeRules(base, override)
	if len(merged) != 3 {
		t.Fatalf("MergeRules() len = %d, want 3", len(merged))
	}

	// kimi-k2.6 is overridden in place (index 0).
	if merged[0].Alias != "kimi-k2.6" || merged[0].Thinking != ThinkingPassthrough {
		t.Fatalf("kimi-k2.6 not overridden: %+v", merged[0])
	}
	// kimi-k2.7-code is untouched.
	if merged[1].Alias != "kimi-k2.7-code" {
		t.Fatalf("kimi-k2.7-code moved or changed: %+v", merged[1])
	}
	// kimi-k3 is appended.
	if merged[2].Alias != "kimi-k3" {
		t.Fatalf("kimi-k3 not appended: %+v", merged[2])
	}
}

func TestMergeRules_NilOverrideReturnsBase(t *testing.T) {
	base := []Rule{{Alias: "a", Target: "b"}}
	merged := MergeRules(base, nil)
	if len(merged) != 1 || merged[0].Alias != "a" {
		t.Fatalf("MergeRules() = %v, want base unchanged", merged)
	}
}

func TestMergeMetadata_SynthesizesModels(t *testing.T) {
	cw := 262144
	rules := []Rule{
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding", Modes: []string{"chat"}, ContextWindow: &cw},
		{Alias: "kimi-k3", Target: "kimicode/k3", Modes: []string{"chat"}},
		{Alias: "bge_m3_embed", Target: "kimicode/bge_m3_embed", Modes: []string{"embedding"}},
	}

	models := MergeMetadata(rules)
	if len(models) != 3 {
		t.Fatalf("MergeMetadata() len = %d, want 3", len(models))
	}

	byID := make(map[string]core.Model, len(models))
	for _, m := range models {
		byID[m.ID] = m
	}

	k26 := byID["kimi-k2.6"]
	if k26.ID != "kimi-k2.6" || k26.Object != "model" {
		t.Fatalf("kimi-k2.6 entry wrong: %+v", k26)
	}
	if k26.Metadata == nil {
		t.Fatal("kimi-k2.6 metadata is nil")
	}
	if len(k26.Metadata.Modes) != 1 || k26.Metadata.Modes[0] != "chat" {
		t.Fatalf("kimi-k2.6 modes = %v, want [chat]", k26.Metadata.Modes)
	}
	if k26.Metadata.ContextWindow == nil || *k26.Metadata.ContextWindow != 262144 {
		t.Fatalf("kimi-k2.6 context_window = %v, want 262144", k26.Metadata.ContextWindow)
	}
	if len(k26.Metadata.Categories) != 1 || k26.Metadata.Categories[0] != core.CategoryTextGeneration {
		t.Fatalf("kimi-k2.6 categories = %v, want [text_generation]", k26.Metadata.Categories)
	}

	embed := byID["bge_m3_embed"]
	if embed.Metadata == nil {
		t.Fatal("bge_m3_embed metadata is nil")
	}
	if len(embed.Metadata.Modes) != 1 || embed.Metadata.Modes[0] != "embedding" {
		t.Fatalf("bge_m3_embed modes = %v, want [embedding]", embed.Metadata.Modes)
	}
	if len(embed.Metadata.Categories) != 1 || embed.Metadata.Categories[0] != core.CategoryEmbedding {
		t.Fatalf("bge_m3_embed categories = %v, want [embedding]", embed.Metadata.Categories)
	}
}

func TestMergeMetadata_NoMetadataFields(t *testing.T) {
	rules := []Rule{
		{Alias: "kimi-k3", Target: "kimicode/k3"}, // no modes, no context_window
	}

	models := MergeMetadata(rules)
	if len(models) != 1 {
		t.Fatalf("MergeMetadata() len = %d, want 1", len(models))
	}
	if models[0].Metadata != nil {
		t.Fatalf("Metadata = %+v, want nil when no metadata fields set", models[0].Metadata)
	}
}

func TestMergeMetadata_DedupesAliases(t *testing.T) {
	rules := []Rule{
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding"},
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding"},
	}
	models := MergeMetadata(rules)
	if len(models) != 1 {
		t.Fatalf("MergeMetadata() len = %d, want 1 (deduped)", len(models))
	}
}

func TestMergeMetadata_NilForEmpty(t *testing.T) {
	if models := MergeMetadata(nil); len(models) != 0 {
		t.Fatalf("MergeMetadata(nil) = %v, want empty", models)
	}
	// Rules with blank aliases are silently skipped, so the result is empty
	// (but not nil — a defensive caller's length check still works).
	models := MergeMetadata([]Rule{{Alias: "", Target: "x"}})
	if len(models) != 0 {
		t.Fatalf("MergeMetadata(blank alias) = %v, want empty", models)
	}
}

func TestThinkingPolicyValid(t *testing.T) {
	valid := []ThinkingPolicy{"", ThinkingPassthrough, ThinkingEnabled, ThinkingDisabled}
	for _, p := range valid {
		if !p.Valid() {
			t.Fatalf("ThinkingPolicy(%q).Valid() = false, want true", p)
		}
	}
	invalid := []ThinkingPolicy{"bogus", "ENABLED", "off"}
	for _, p := range invalid {
		if p.Valid() {
			t.Fatalf("ThinkingPolicy(%q).Valid() = true, want false", p)
		}
	}
}

func TestLookup_TrimsWhitespace(t *testing.T) {
	n := New([]Rule{{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding"}})
	r, ok := n.Lookup("  kimi-k2.6  ")
	if !ok {
		t.Fatal("Lookup() not found with whitespace")
	}
	if r.Alias != "kimi-k2.6" {
		t.Fatalf("Alias = %q, want %q", r.Alias, "kimi-k2.6")
	}
}
