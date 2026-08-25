// Kimi Code model map: canonical, client-facing model names mapped to the
// subscription endpoint's upstream model IDs, plus the thinking-control
// translation each endpoint family expects.
//
// The map is always on and purely additive (Postel's law): requests that
// already use a raw upstream ID — or any unknown ID — pass through byte-for-
// byte unchanged, so nothing a client can do today stops working. Canonical
// names only add options: they rewrite the model field to the upstream ID and
// apply the endpoint's thinking knob, so clients can pick "K2.7 with thinking
// on" (kimi-k2.7-code) or "the K2.6 behavior of the same endpoint"
// (kimi-k2.6) as distinct models.
package kimicode

import (
	"context"
	"strings"

	"github.com/goccy/go-json"

	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/providers"
)

// modelEntry describes one canonical model: the upstream ID it routes to,
// its capabilities for /v1/models metadata, and how the typed Reasoning
// field translates to Kimi's thinking controls.
type modelEntry struct {
	// Canonical is the client-facing model ID GoModel accepts.
	Canonical string
	// Raw is the upstream-facing model ID sent to Kimi Code.
	Raw string
	// Modes are the model's capabilities (e.g. "chat", "embeddings").
	Modes []string
	// ContextWindow is the upstream context length, 0 when unknown.
	ContextWindow int
	// Thinking, when non-nil, is forced onto every request for this model:
	// it is the whole point of aliases like kimi-k2.6 (thinking off) vs
	// kimi-k2.7-code (thinking on), which share one upstream ID. Nil leaves
	// the caller's reasoning fields alone.
	Thinking *thinking
}

// thinking is one of Kimi Code's two thinking-control knobs.
type thinking struct {
	// Type forces thinking.type (K2.7 Code family): "enabled" or "disabled".
	// Disabling routes the request to the K2.6 behavior upstream.
	Type string
}

var (
	k2ContextWindow  = 262144
	k3ContextWindow  = 262144
	k3FullContext    = 1048576
)

// staticModels is the Kimi Code subscription catalog. Order is the order the
// canonical entries appear in ListModels output.
var staticModels = []modelEntry{
	{Canonical: "kimi-k2.6", Raw: "kimi-for-coding", Modes: []string{"chat"},
		ContextWindow: k2ContextWindow, Thinking: &thinking{Type: "disabled"}},
	{Canonical: "kimi-k2.7-code", Raw: "kimi-for-coding", Modes: []string{"chat"},
		ContextWindow: k2ContextWindow, Thinking: &thinking{Type: "enabled"}},
	{Canonical: "kimi-k2.7-code-highspeed", Raw: "kimi-for-coding-highspeed", Modes: []string{"chat"},
		ContextWindow: k2ContextWindow, Thinking: &thinking{Type: "enabled"}},
	{Canonical: "kimi-k3", Raw: "k3", Modes: []string{"chat"},
		ContextWindow: k3FullContext},
	{Canonical: "kimi-k3-256k", Raw: "k3-256k", Modes: []string{"chat"},
		ContextWindow: k3ContextWindow},
	// bge_m3_embed is undocumented upstream; it is its own canonical name
	// (identity mapping) so the embedding model shows up in ListModels with
	// the right mode even when the upstream /models omits it.
	{Canonical: "bge_m3_embed", Raw: "bge_m3_embed", Modes: []string{"embedding"}},
}

// lookupCanonical returns the static entry for a canonical model ID, or nil
// when the ID is a raw upstream ID or unknown — those are never rewritten.
// The identity entry (bge_m3_embed) also returns nil: its canonical name is
// already the raw name, so no rewrite is needed.
func lookupCanonical(model string) *modelEntry {
	for i := range staticModels {
		if staticModels[i].Canonical == model && staticModels[i].Canonical != staticModels[i].Raw {
			return &staticModels[i]
		}
	}
	return nil
}

// adaptChatRequest rewrites a canonical model ID to its upstream ID and
// applies the model's thinking control. Raw and unknown IDs are returned
// unchanged (same pointer). The argument is never mutated; a shallow copy is
// returned when changes are needed.
func adaptChatRequest(req *core.ChatRequest) (*core.ChatRequest, error) {
	if req == nil {
		return req, nil
	}
	entry := lookupCanonical(req.Model)
	if entry == nil {
		return req, nil
	}

	adapted := *req
	adapted.Model = entry.Raw

	switch {
	case entry.Thinking != nil:
		// K2.7 family: force thinking.type, replacing any client-supplied
		// thinking field. The typed Reasoning knob is K3-style effort and
		// does not map to this family, so it is dropped.
		adapted.Reasoning = nil
		extra, err := core.MergeUnknownJSONFields(req.ExtraFields, map[string]json.RawMessage{
			"thinking": json.RawMessage(`{"type":"` + entry.Thinking.Type + `"}`),
		})
		if err != nil {
			return nil, core.NewInvalidRequestError("failed to adapt Kimi Code thinking field: "+err.Error(), err)
		}
		adapted.ExtraFields = extra
	case req.Reasoning != nil && strings.TrimSpace(req.Reasoning.Effort) != "":
		// K3 family: flatten the typed reasoning effort into the flat
		// reasoning_effort field Kimi expects (low/high/max are passed
		// through verbatim; upstream validates).
		flattened, err := providers.AdaptReasoningEffortRequest(&adapted, req.Reasoning.Effort)
		if err != nil {
			return nil, err
		}
		adapted = *flattened
	}

	return &adapted, nil
}

// ListModels returns the upstream model list plus synthesized entries for the
// canonical names that are not already listed, each carrying the static
// metadata (modes, categories, context window). Raw upstream entries are kept
// as-is so the response only grows.
func (p *Provider) ListModels(ctx context.Context) (*core.ModelsResponse, error) {
	resp, err := p.ChatCompatible.ListModels(ctx)
	if err != nil || resp == nil {
		return resp, err
	}

	seen := make(map[string]struct{}, len(resp.Data))
	for _, m := range resp.Data {
		seen[m.ID] = struct{}{}
	}
	for i := range staticModels {
		entry := &staticModels[i]
		if _, ok := seen[entry.Canonical]; ok {
			continue
		}
		resp.Data = append(resp.Data, core.Model{
			ID:      entry.Canonical,
			Object:  "model",
			OwnedBy: "moonshotai",
			Metadata: &core.ModelMetadata{
				Modes:         entry.Modes,
				Categories:    core.CategoriesForModes(entry.Modes),
				ContextWindow: &entry.ContextWindow,
			},
		})
	}
	return resp, nil
}
