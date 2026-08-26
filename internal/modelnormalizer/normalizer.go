// Package modelnormalizer provides a data-driven, gateway-edge model
// normalizer that rewrites chat requests before provider dispatch. It resolves
// canonical client-facing model aliases (e.g. "kimi-k2.6") to concrete
// provider/model targets and injects per-alias thinking policies, without
// touching any provider package.
//
// The normalizer runs as the first step of the chat prepare path (before
// model resolution), so the resolver and every downstream provider only see
// the already-rewritten request. Unknown model IDs pass through unchanged
// (Postel's law).
package modelnormalizer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/goccy/go-json"

	"github.com/enterpilot/gomodel/internal/core"
)

// ThinkingPolicy controls how the normalizer sets the `thinking` extension on
// the rewritten request.
type ThinkingPolicy string

const (
	// ThinkingPassthrough leaves any existing thinking field untouched and
	// injects nothing. The provider sees whatever the client sent (or nothing).
	ThinkingPassthrough ThinkingPolicy = "passthrough"
	// ThinkingEnabled injects {"thinking":{"type":"enabled"}} so the upstream
	// activates extended reasoning regardless of the client payload.
	ThinkingEnabled ThinkingPolicy = "enabled"
	// ThinkingDisabled injects {"thinking":{"type":"disabled"}} so the upstream
	// suppresses extended reasoning regardless of the client payload.
	ThinkingDisabled ThinkingPolicy = "disabled"
)

// Valid reports whether the policy is a known value. Empty string is treated
// as passthrough (the zero value).
func (p ThinkingPolicy) Valid() bool {
	switch p {
	case "", ThinkingPassthrough, ThinkingEnabled, ThinkingDisabled:
		return true
	default:
		return false
	}
}

// Rule declares one model normalization rule. It maps an alias to a
// provider-qualified target and optionally pins a thinking policy and/or
// advertises metadata for /v1/models.
type Rule struct {
	// Alias is the client-facing model ID that triggers this rule.
	Alias string `yaml:"alias" json:"alias"`
	// Target is the provider/model selector the request is rewritten to, e.g.
	// "kimicode/kimi-for-coding".
	Target string `yaml:"target" json:"target"`
	// Thinking is the thinking policy to inject. One of "enabled", "disabled",
	// or "passthrough" (default: passthrough).
	Thinking ThinkingPolicy `yaml:"thinking,omitempty" json:"thinking,omitempty"`
	// ContextWindow is the advertised context window for /v1/models metadata.
	ContextWindow *int `yaml:"context_window,omitempty" json:"context_window,omitempty"`
	// Modes declares the model's kinds for registry metadata, e.g. ["chat"].
	// Used to derive Categories via core.CategoriesForModes.
	Modes []string `yaml:"modes,omitempty" json:"modes,omitempty"`
}

// Normalizer applies rules to rewrite chat requests before provider dispatch.
// Rules are indexed by alias for O(1) lookup.
type Normalizer struct {
	byAlias map[string]Rule
}

// New creates a Normalizer from the given rules. Whitespace-only alias or
// target entries are skipped. Duplicate aliases are resolved last-wins so the
// env layer can override the YAML layer deterministically.
func New(rules []Rule) *Normalizer {
	if len(rules) == 0 {
		return nil
	}
	byAlias := make(map[string]Rule, len(rules))
	for _, r := range rules {
		alias := strings.TrimSpace(r.Alias)
		target := strings.TrimSpace(r.Target)
		if alias == "" || target == "" {
			continue
		}
		r.Alias = alias
		r.Target = target
		byAlias[alias] = r
	}
	if len(byAlias) == 0 {
		return nil
	}
	return &Normalizer{byAlias: byAlias}
}

// Lookup returns the rule registered under alias, if any.
func (n *Normalizer) Lookup(alias string) (Rule, bool) {
	if n == nil {
		return Rule{}, false
	}
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return Rule{}, false
	}
	r, ok := n.byAlias[alias]
	return r, ok
}

// Aliases returns the registered aliases in deterministic order.
func (n *Normalizer) Aliases() []string {
	if n == nil || len(n.byAlias) == 0 {
		return nil
	}
	aliases := make([]string, 0, len(n.byAlias))
	for a := range n.byAlias {
		aliases = append(aliases, a)
	}
	// sort.Strings is deterministic; no dedup needed since map keys are unique.
	sort.Strings(aliases)
	return aliases
}

// AdaptChatRequest rewrites req.Model to the rule's target and injects the
// per-alias thinking policy into ExtraFields. It returns (rewrittenReq, true,
// nil) when a rule matched, or (req, false, nil) when no rule matched — the
// original request pointer is returned unchanged so the caller can use it
// directly without copying.
func (n *Normalizer) AdaptChatRequest(req *core.ChatRequest) (*core.ChatRequest, bool, error) {
	if n == nil || req == nil {
		return req, false, nil
	}
	r, ok := n.Lookup(req.Model)
	if !ok {
		return req, false, nil
	}

	// Shallow-copy the request so the caller's original is never mutated.
	adapted := *req
	adapted.Model = r.Target

	// Inject the thinking policy when the rule pins one.
	if r.Thinking != "" && r.Thinking != ThinkingPassthrough {
		thinking, err := thinkingJSON(r.Thinking)
		if err != nil {
			return req, false, fmt.Errorf("modelnormalizer: rule %q: %w", r.Alias, err)
		}
		extra, err := core.MergeUnknownJSONFields(req.ExtraFields, map[string]json.RawMessage{
			"thinking": thinking,
		})
		if err != nil {
			return req, false, fmt.Errorf("modelnormalizer: rule %q: merge thinking: %w", r.Alias, err)
		}
		adapted.ExtraFields = extra
	}

	return &adapted, true, nil
}

// thinkingJSON serializes the thinking extension for the given policy.
// The shape matches what providers like Kimi Code and Xiaomi expect:
// {"type":"enabled"} or {"type":"disabled"}.
func thinkingJSON(policy ThinkingPolicy) (json.RawMessage, error) {
	switch policy {
	case ThinkingEnabled:
		return json.RawMessage(`{"type":"enabled"}`), nil
	case ThinkingDisabled:
		return json.RawMessage(`{"type":"disabled"}`), nil
	default:
		return nil, fmt.Errorf("unknown thinking policy %q", policy)
	}
}

// MergeRules layers override rules over base rules, keyed by alias. Later
// entries in override replace matching base entries in place and append new
// ones. This mirrors the VIRTUAL_MODELS env layering pattern.
func MergeRules(base, override []Rule) []Rule {
	if len(override) == 0 {
		return base
	}
	merged := make([]Rule, len(base))
	copy(merged, base)
	index := make(map[string]int, len(merged))
	for i, r := range merged {
		index[strings.ToLower(strings.TrimSpace(r.Alias))] = i
	}
	for _, r := range override {
		key := strings.ToLower(strings.TrimSpace(r.Alias))
		if pos, ok := index[key]; ok {
			merged[pos] = r
			continue
		}
		index[key] = len(merged)
		merged = append(merged, r)
	}
	return merged
}

// MergeMetadata synthesizes core.Model entries for each rule so /v1/models
// can advertise the canonical aliases alongside provider models. Rules with
// no modes or context_window are still listed (metadata is optional).
func MergeMetadata(rules []Rule) []core.Model {
	if len(rules) == 0 {
		return nil
	}
	models := make([]core.Model, 0, len(rules))
	seen := make(map[string]struct{}, len(rules))
	for _, r := range rules {
		alias := strings.TrimSpace(r.Alias)
		if alias == "" {
			continue
		}
		if _, dup := seen[alias]; dup {
			continue
		}
		seen[alias] = struct{}{}

		entry := core.Model{
			ID:     alias,
			Object: "model",
		}
		entry.Metadata = ruleMetadata(r)
		models = append(models, entry)
	}
	return models
}

func ruleMetadata(r Rule) *core.ModelMetadata {
	var meta *core.ModelMetadata
	if len(r.Modes) > 0 {
		modes := make([]string, len(r.Modes))
		copy(modes, r.Modes)
		if meta == nil {
			meta = &core.ModelMetadata{}
		}
		meta.Modes = modes
		meta.Categories = core.CategoriesForModes(modes)
	}
	if r.ContextWindow != nil {
		if meta == nil {
			meta = &core.ModelMetadata{}
		}
		meta.ContextWindow = r.ContextWindow
	}
	return meta
}

// ExposedModels returns one core.Model entry per registered alias so the
// /v1/models handler can advertise canonical names alongside provider models.
// The normalizer satisfies the ExposedModelLister interface consumed by the
// handler's ListModels merge. Rules are deduplicated by alias (last-wins).
func (n *Normalizer) ExposedModels() []core.Model {
	if n == nil {
		return nil
	}
	rules := make([]Rule, 0, len(n.byAlias))
	for _, r := range n.byAlias {
		rules = append(rules, r)
	}
	return MergeMetadata(rules)
}
