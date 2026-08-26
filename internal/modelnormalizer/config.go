package modelnormalizer

import (
	"github.com/enterpilot/gomodel/config"

	"github.com/enterpilot/gomodel/internal/core"
)

// BuildFromConfig constructs a Normalizer from a config.Config slice. Empty or
// blank rules are silently dropped. The result is nil when no valid rules are
// declared, so callers can pass it directly to server/gateway hooks without
// nil-checking downstream.
func BuildFromConfig(rules []config.ModelNormalizerRule) *Normalizer {
	if len(rules) == 0 {
		return nil
	}
	converted := make([]Rule, 0, len(rules))
	for _, r := range rules {
		converted = append(converted, Rule{
			Alias:         r.Alias,
			Target:        r.Target,
			Thinking:      ThinkingPolicy(r.Thinking),
			ContextWindow: r.ContextWindow,
			Modes:         r.Modes,
		})
	}
	return New(converted)
}

// ChainedExposedModelLister wraps two ExposedModels functions so the
// secondary's output is appended to the primary. The server layer adapts this
// onto its ExposedModelLister interface. Used when a normalizer is configured
// alongside another lister (e.g. virtual models) so canonical aliases still
// appear in /v1/models.
type ChainedExposedModelLister struct {
	Primary   func() []core.Model
	Secondary func() []core.Model
}

// ExposedModels concatenates both sources. A nil primary collapses to the
// secondary; a nil secondary collapses to the primary; both nil returns nil.
func (c ChainedExposedModelLister) ExposedModels() []core.Model {
	if c.Primary == nil {
		if c.Secondary == nil {
			return nil
		}
		return c.Secondary()
	}
	if c.Secondary == nil {
		return c.Primary()
	}
	return append(c.Primary(), c.Secondary()...)
}