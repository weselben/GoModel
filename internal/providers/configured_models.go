package providers

import (
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/enterpilot/gomodel/config"
	"github.com/enterpilot/gomodel/internal/core"
)

type configuredProviderModelsApplyReason string

const (
	configuredProviderModelsNotApplied    configuredProviderModelsApplyReason = ""
	configuredProviderModelsAllowlist     configuredProviderModelsApplyReason = "allowlist"
	configuredProviderModelsMerge         configuredProviderModelsApplyReason = "merge"
	configuredProviderModelsPattern       configuredProviderModelsApplyReason = "pattern"
	configuredProviderModelsUpstreamError configuredProviderModelsApplyReason = "upstream_error"
	// configuredProviderModelsUpstreamUnlisted means the provider has no model
	// listing endpoint (404/405 on /models). Servers that only expose a single
	// API, such as speech-to-text servers, are fully described by the
	// configured list, so it counts as authoritative rather than a fallback.
	configuredProviderModelsUpstreamUnlisted configuredProviderModelsApplyReason = "upstream_unlisted"
	configuredProviderModelsUpstreamNil      configuredProviderModelsApplyReason = "upstream_nil"
	configuredProviderModelsUpstreamEmpty    configuredProviderModelsApplyReason = "upstream_empty"
	// catalogProviderModelsUpstreamError means the provider's listing failed
	// and nothing else describes its inventory, so the model catalog's list
	// for its type stands in until a live listing succeeds.
	catalogProviderModelsUpstreamError configuredProviderModelsApplyReason = "catalog_upstream_error"
)

func normalizeConfiguredProviderModels(models []string) []string {
	if len(models) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(models))
	normalized := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		normalized = append(normalized, model)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

func applyConfiguredProviderModels(
	providerName string,
	providerType string,
	mode config.ConfiguredProviderModelsMode,
	configuredModels []string,
	upstream *core.ModelsResponse,
	upstreamErr error,
	fallbackCreated int64,
) (*core.ModelsResponse, configuredProviderModelsApplyReason) {
	if len(configuredModels) == 0 {
		return upstream, configuredProviderModelsNotApplied
	}

	mode = config.ResolveConfiguredProviderModelsMode(mode)

	// Pattern mode: serve the exact configured entries like an allowlist,
	// then expand the glob patterns (entries containing `*` or `?`)
	// against the upstream /models inventory. When the upstream cannot
	// supply one, only the exact entries survive — a pattern is never
	// published as a literal model ID, and the drop is logged.
	if mode == config.ConfiguredProviderModelsModePattern {
		return applyPatternConfiguredProviderModels(providerName, providerType, configuredModels, upstream, upstreamErr, fallbackCreated)
	}

	// Every case but a healthy listing in fallback or merge mode serves the
	// configured list; they differ only in the reason recorded for it.
	var reason configuredProviderModelsApplyReason
	mode = config.ResolveConfiguredProviderModelsMode(mode)
	switch {
	case mode == config.ConfiguredProviderModelsModeAllowlist:
		reason = configuredProviderModelsAllowlist
	case modelListingUnsupported(upstreamErr):
		reason = configuredProviderModelsUpstreamUnlisted
	case upstreamErr != nil:
		reason = configuredProviderModelsUpstreamError
	case upstream == nil:
		reason = configuredProviderModelsUpstreamNil
	case len(upstream.Data) == 0:
		reason = configuredProviderModelsUpstreamEmpty
	case mode == config.ConfiguredProviderModelsModeMerge:
		return mergeConfiguredProviderModelsResponse(providerName, providerType, configuredModels, upstream, fallbackCreated), configuredProviderModelsMerge
	default:
		return upstream, configuredProviderModelsNotApplied
	}
	return configuredProviderModelsResponse(providerName, providerType, configuredModels, upstream, fallbackCreated), reason
}

// applyPatternConfiguredProviderModels implements pattern mode: the exact
// entries are served like an allowlist, glob patterns are resolved against a
// healthy upstream inventory and merged in upstream order. The apply reason
// is configuredProviderModelsPattern on a healthy listing, and the standard
// upstream reasons when the listing failed — the exact entries then serve
// alone, as in allowlist mode.
func applyPatternConfiguredProviderModels(
	providerName string,
	providerType string,
	configuredModels []string,
	upstream *core.ModelsResponse,
	upstreamErr error,
	fallbackCreated int64,
) (*core.ModelsResponse, configuredProviderModelsApplyReason) {
	exact, patterns := splitConfiguredModels(configuredModels)

	// Resolve the reason exactly as the other modes do, so a failed
	// listing keeps the provider's runtime state honest.
	var reason configuredProviderModelsApplyReason
	switch {
	case modelListingUnsupported(upstreamErr):
		reason = configuredProviderModelsUpstreamUnlisted
	case upstreamErr != nil:
		reason = configuredProviderModelsUpstreamError
	case upstream == nil:
		reason = configuredProviderModelsUpstreamNil
	case len(upstream.Data) == 0:
		reason = configuredProviderModelsUpstreamEmpty
	default:
		reason = configuredProviderModelsPattern
	}

	if reason != configuredProviderModelsPattern {
		// Upstream cannot supply an inventory: patterns are unresolvable,
		// only the exact entries survive.
		if len(patterns) > 0 {
			slogPatternDrop(providerName, patterns, upstreamErr, upstream)
		}
		return configuredProviderModelsResponse(providerName, providerType, exact, upstream, fallbackCreated), reason
	}

	resp := patternConfiguredModelsResponse(providerName, providerType, exact, patterns, upstream, fallbackCreated)
	return resp, configuredProviderModelsPattern
}

// patternConfiguredModelsResponse resolves glob patterns against a healthy
// upstream inventory: upstream entries matching at least one pattern stay in
// upstream order with their metadata, then the exact configured entries follow
// in configured order — reusing the upstream entry when listed there, else
// synthesized. The registry only ever sees resolved model IDs, never patterns.
func patternConfiguredModelsResponse(providerName, providerType string, exact, patterns []string, upstream *core.ModelsResponse, fallbackCreated int64) *core.ModelsResponse {
	byID := make(map[string]core.Model, len(upstream.Data))
	data := make([]core.Model, 0, len(upstream.Data)+len(exact))
	appended := make(map[string]struct{}, len(upstream.Data)+len(exact))
	for _, model := range upstream.Data {
		modelID := strings.TrimSpace(model.ID)
		if modelID == "" {
			continue
		}
		if _, ok := byID[modelID]; ok {
			// Upstream listings can repeat an ID (also via whitespace
			// variants that normalize to one); the first entry wins.
			continue
		}
		// Registry lookups trim requested IDs, so the retained entry must be
		// indexed under the same normalized key it deduplicates by.
		model.ID = modelID
		byID[modelID] = model
		if matchesAnyGlob(patterns, modelID) {
			appended[modelID] = struct{}{}
			data = append(data, model)
		}
	}

	owner := configuredModelOwner(providerName, providerType)
	created := normalizeFallbackCreated(fallbackCreated)
	matchedCount := len(appended)
	if matchedCount == 0 && len(patterns) > 0 {
		slog.Warn("configured model patterns matched no upstream models",
			"provider", providerName,
			"patterns", len(patterns),
			"upstream_models", len(upstream.Data),
		)
	} else if matchedCount > 0 {
		slog.Debug("resolved configured provider model patterns against upstream inventory",
			"provider", providerName,
			"patterns", len(patterns),
			"matched", matchedCount,
		)
	}

	for _, modelID := range exact {
		if _, ok := appended[modelID]; ok {
			continue
		}
		appended[modelID] = struct{}{}
		model, ok := byID[modelID]
		if !ok {
			model = synthesizedConfiguredModel(modelID, owner, created)
		} else {
			if strings.TrimSpace(model.Object) == "" {
				model.Object = "model"
			}
			if strings.TrimSpace(model.OwnedBy) == "" {
				model.OwnedBy = owner
			}
			if model.Created == 0 {
				model.Created = created
			}
		}
		data = append(data, model)
	}

	return &core.ModelsResponse{
		Object: "list",
		Data:   data,
	}
}

// hasModelPattern reports whether any configured entry is a glob pattern.
// Entries containing `*` or `?` are patterns; anything else is an exact model
// ID (substring matching is written `*free*`, not `free`).
func hasModelPattern(models []string) bool {
	for _, model := range models {
		if strings.ContainsAny(model, "*?") {
			return true
		}
	}
	return false
}

// splitConfiguredModels separates a configured model list into exact model IDs
// and glob patterns, preserving the configured order within each group.
func splitConfiguredModels(models []string) (exact []string, patterns []string) {
	for _, model := range models {
		if strings.ContainsAny(model, "*?") {
			patterns = append(patterns, model)
			continue
		}
		exact = append(exact, model)
	}
	return exact, patterns
}

// slogPatternDrop logs why configured patterns were dropped.
func slogPatternDrop(providerName string, patterns []string, upstreamErr error, upstream *core.ModelsResponse) {
	if len(patterns) == 0 {
		return
	}
	switch {
	case modelListingUnsupported(upstreamErr):
		slog.Debug("provider has no /models endpoint, dropping configured patterns",
			"provider", providerName,
			"patterns", len(patterns),
		)
	case upstreamErr != nil:
		slog.Warn("upstream /models failed, dropping configured patterns",
			"provider", providerName,
			"patterns", len(patterns),
			"error", upstreamErr,
		)
	case upstream == nil:
		slog.Warn("upstream returned nil model list, dropping configured patterns",
			"provider", providerName,
			"patterns", len(patterns),
		)
	default:
		slog.Warn("upstream returned empty model list, dropping configured patterns",
			"provider", providerName,
			"patterns", len(patterns),
		)
	}
}

// modelListingUnsupported reports whether err means the upstream has no model
// listing endpoint at all, as opposed to a listing that failed. Providers mark
// such errors at the /models call, so a 404 from another API (e.g. the Bedrock
// control plane) still counts as a failure.
func modelListingUnsupported(err error) bool {
	return errors.Is(err, core.ErrModelListingUnsupported)
}

// configuredModelOwner picks the owned_by value for synthesized entries.
func configuredModelOwner(providerName, providerType string) string {
	owner := strings.TrimSpace(providerType)
	if owner == "" {
		owner = strings.TrimSpace(providerName)
	}
	return owner
}

func normalizeFallbackCreated(fallbackCreated int64) int64 {
	if fallbackCreated <= 0 {
		return time.Now().Unix()
	}
	return fallbackCreated
}

func synthesizedConfiguredModel(modelID, owner string, created int64) core.Model {
	return core.Model{
		ID:      modelID,
		Object:  "model",
		OwnedBy: owner,
		Created: created,
	}
}

// mergeConfiguredProviderModelsResponse unions a healthy upstream inventory
// with the configured list: upstream entries stay authoritative and configured
// models the upstream does not list are appended as synthesized entries, so
// models a provider serves without listing them remain routable.
func mergeConfiguredProviderModelsResponse(providerName, providerType string, configuredModels []string, upstream *core.ModelsResponse, fallbackCreated int64) *core.ModelsResponse {
	data := uniqueUpstreamModels(upstream)
	seen := make(map[string]struct{}, len(data)+len(configuredModels))
	for _, model := range data {
		seen[model.ID] = struct{}{}
	}

	owner := configuredModelOwner(providerName, providerType)
	created := normalizeFallbackCreated(fallbackCreated)
	for _, modelID := range configuredModels {
		if _, ok := seen[modelID]; ok {
			continue
		}
		seen[modelID] = struct{}{}
		data = append(data, synthesizedConfiguredModel(modelID, owner, created))
	}

	return &core.ModelsResponse{
		Object: "list",
		Data:   data,
	}
}

// uniqueUpstreamModels returns the upstream entries with trimmed IDs, in
// listing order, dropping blank IDs and repeats. Upstream listings can repeat
// an ID (also via whitespace variants that normalize to one); the first entry
// wins, and registry lookups trim requested IDs, so each entry is keyed by
// its trimmed ID.
func uniqueUpstreamModels(upstream *core.ModelsResponse) []core.Model {
	if upstream == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(upstream.Data))
	out := make([]core.Model, 0, len(upstream.Data))
	for _, model := range upstream.Data {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" {
			continue
		}
		if _, ok := seen[model.ID]; ok {
			continue
		}
		seen[model.ID] = struct{}{}
		out = append(out, model)
	}
	return out
}

func configuredProviderModelsResponse(providerName, providerType string, configuredModels []string, upstream *core.ModelsResponse, fallbackCreated int64) *core.ModelsResponse {
	listed := uniqueUpstreamModels(upstream)
	byID := make(map[string]core.Model, len(listed))
	for _, model := range listed {
		byID[model.ID] = model
	}

	owner := configuredModelOwner(providerName, providerType)
	fallbackCreated = normalizeFallbackCreated(fallbackCreated)

	data := make([]core.Model, 0, len(configuredModels))
	for _, modelID := range configuredModels {
		model, ok := byID[modelID]
		if !ok {
			data = append(data, synthesizedConfiguredModel(modelID, owner, fallbackCreated))
			continue
		}
		if strings.TrimSpace(model.Object) == "" {
			model.Object = "model"
		}
		if strings.TrimSpace(model.OwnedBy) == "" {
			model.OwnedBy = owner
		}
		if model.Created == 0 {
			model.Created = fallbackCreated
		}
		data = append(data, model)
	}

	return &core.ModelsResponse{
		Object: "list",
		Data:   data,
	}
}

func modelsResponseFromProviderMap(providerModels map[string]*ModelInfo) *core.ModelsResponse {
	if len(providerModels) == 0 {
		return &core.ModelsResponse{Object: "list"}
	}
	modelIDs := make([]string, 0, len(providerModels))
	for modelID := range providerModels {
		modelIDs = append(modelIDs, modelID)
	}
	sort.Strings(modelIDs)

	data := make([]core.Model, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		if info := providerModels[modelID]; info != nil {
			data = append(data, info.Model)
		}
	}
	return &core.ModelsResponse{
		Object: "list",
		Data:   data,
	}
}

func modelInfoMapFromResponse(resp *core.ModelsResponse, provider core.Provider, providerName, providerType string) map[string]*ModelInfo {
	out := make(map[string]*ModelInfo)
	if resp == nil {
		return out
	}
	for _, model := range resp.Data {
		modelID := strings.TrimSpace(model.ID)
		if modelID == "" {
			continue
		}
		model.ID = modelID
		out[modelID] = newModelInfo(model, provider, providerName, providerType)
	}
	return out
}

func rebuildGlobalModelMap(modelsByProvider map[string]map[string]*ModelInfo, providerOrderNames []string) map[string]*ModelInfo {
	global := make(map[string]*ModelInfo)
	seenProvider := make(map[string]struct{}, len(providerOrderNames))
	for _, providerName := range providerOrderNames {
		seenProvider[providerName] = struct{}{}
		addProviderModels(global, modelsByProvider[providerName])
	}

	remaining := make([]string, 0, len(modelsByProvider))
	for providerName := range modelsByProvider {
		if _, seen := seenProvider[providerName]; seen {
			continue
		}
		remaining = append(remaining, providerName)
	}
	sort.Strings(remaining)
	for _, providerName := range remaining {
		addProviderModels(global, modelsByProvider[providerName])
	}
	return global
}

func addProviderModels(global map[string]*ModelInfo, providerModels map[string]*ModelInfo) {
	if len(providerModels) == 0 {
		return
	}
	modelIDs := make([]string, 0, len(providerModels))
	for modelID := range providerModels {
		modelIDs = append(modelIDs, modelID)
	}
	sort.Strings(modelIDs)
	for _, modelID := range modelIDs {
		if _, exists := global[modelID]; exists {
			continue
		}
		if info := providerModels[modelID]; info != nil {
			global[modelID] = info
		}
	}
}
