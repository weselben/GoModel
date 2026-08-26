package modelnormalizer

import (
	"testing"

	"github.com/enterpilot/gomodel/config"

	"github.com/enterpilot/gomodel/internal/core"
)

func TestBuildFromConfig(t *testing.T) {
	cw := 262144
	cfgRules := []config.ModelNormalizerRule{
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding", Thinking: "disabled", ContextWindow: &cw, Modes: []string{"chat"}},
		{Alias: "kimi-k3", Target: "kimicode/k3"},
	}

	n := BuildFromConfig(cfgRules)
	if n == nil {
		t.Fatal("BuildFromConfig() returned nil for valid rules")
	}

	r, ok := n.Lookup("kimi-k2.6")
	if !ok {
		t.Fatal("Lookup kimi-k2.6 not found")
	}
	if r.Target != "kimicode/kimi-for-coding" {
		t.Fatalf("Target = %q, want %q", r.Target, "kimicode/kimi-for-coding")
	}
	if r.Thinking != ThinkingDisabled {
		t.Fatalf("Thinking = %q, want %q", r.Thinking, ThinkingDisabled)
	}
	if r.ContextWindow == nil || *r.ContextWindow != 262144 {
		t.Fatalf("ContextWindow = %v, want 262144", r.ContextWindow)
	}
	if len(r.Modes) != 1 || r.Modes[0] != "chat" {
		t.Fatalf("Modes = %v, want [chat]", r.Modes)
	}
}

func TestBuildFromConfig_NilForEmpty(t *testing.T) {
	if n := BuildFromConfig(nil); n != nil {
		t.Fatalf("BuildFromConfig(nil) = %v, want nil", n)
	}
	if n := BuildFromConfig([]config.ModelNormalizerRule{}); n != nil {
		t.Fatalf("BuildFromConfig(empty) = %v, want nil", n)
	}
}

func TestBuildFromConfig_SkipsBlankRules(t *testing.T) {
	cfgRules := []config.ModelNormalizerRule{
		{Alias: "", Target: "x"},
		{Alias: "a", Target: ""},
		{Alias: "valid", Target: "b"},
	}
	n := BuildFromConfig(cfgRules)
	if n == nil {
		t.Fatal("BuildFromConfig() returned nil, want non-nil with one valid rule")
	}
	aliases := n.Aliases()
	if len(aliases) != 1 || aliases[0] != "valid" {
		t.Fatalf("Aliases() = %v, want [valid]", aliases)
	}
}

func TestExposedModels_SatisfiesListModelsHook(t *testing.T) {
	cw := 262144
	n := New([]Rule{
		{Alias: "kimi-k2.6", Target: "kimicode/kimi-for-coding", Modes: []string{"chat"}, ContextWindow: &cw},
		{Alias: "bge_m3_embed", Target: "kimicode/bge_m3_embed", Modes: []string{"embedding"}},
	})

	models := n.ExposedModels()
	if len(models) != 2 {
		t.Fatalf("ExposedModels() len = %d, want 2", len(models))
	}

	byID := make(map[string]core.Model, len(models))
	for _, m := range models {
		byID[m.ID] = m
	}

	embed := byID["bge_m3_embed"]
	if embed.Metadata == nil {
		t.Fatal("bge_m3_embed metadata is nil")
	}
	if len(embed.Metadata.Categories) != 1 || embed.Metadata.Categories[0] != core.CategoryEmbedding {
		t.Fatalf("bge_m3_embed categories = %v, want [embedding]", embed.Metadata.Categories)
	}
}

func TestExposedModels_NilNormalizer(t *testing.T) {
	var n *Normalizer
	if models := n.ExposedModels(); models != nil {
		t.Fatalf("nil Normalizer ExposedModels() = %v, want nil", models)
	}
}

func TestChainedExposedModelLister_Concatenates(t *testing.T) {
	primary := func() []core.Model { return []core.Model{{ID: "a"}, {ID: "b"}} }
	secondary := func() []core.Model { return []core.Model{{ID: "c"}} }

	chain := ChainedExposedModelLister{Primary: primary, Secondary: secondary}
	got := chain.ExposedModels()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "b" || got[2].ID != "c" {
		t.Fatalf("order = [%s, %s, %s], want [a, b, c]", got[0].ID, got[1].ID, got[2].ID)
	}
}

func TestChainedExposedModelLister_NilPrimaries(t *testing.T) {
	secondary := func() []core.Model { return []core.Model{{ID: "c"}} }

	if got := (ChainedExposedModelLister{Secondary: secondary}).ExposedModels(); len(got) != 1 {
		t.Fatalf("nil primary: len = %d, want 1", len(got))
	}
	if got := (ChainedExposedModelLister{Primary: secondary}).ExposedModels(); len(got) != 1 {
		t.Fatalf("nil secondary: len = %d, want 1", len(got))
	}
	if got := (ChainedExposedModelLister{}).ExposedModels(); got != nil {
		t.Fatalf("both nil: got %v, want nil", got)
	}
}