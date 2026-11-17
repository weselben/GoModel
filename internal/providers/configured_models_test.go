package providers

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/enterpilot/gomodel/config"
	"github.com/enterpilot/gomodel/internal/core"
	"github.com/stretchr/testify/require"
)

func TestApplyConfiguredProviderModels_BackfillsZeroCreatedForUpstreamMatch(t *testing.T) {
	resp, reason := applyConfiguredProviderModels(
		"test",
		"test-type",
		config.ConfiguredProviderModelsModeAllowlist,
		[]string{"configured-model"},
		&core.ModelsResponse{
			Object: "list",
			Data: []core.Model{
				{ID: "configured-model", Object: "model", OwnedBy: "upstream"},
			},
		},
		nil,
		123,
	)

	require.Equal(t, configuredProviderModelsAllowlist, reason)
	require.NotNil(t, resp)
	require.Len(t, resp.Data, 1)
	require.Equal(t, int64(123), resp.Data[0].Created)
	require.Equal(t, "upstream", resp.Data[0].OwnedBy)
}

func TestApplyConfiguredProviderModels_MergeAppendsMissingModels(t *testing.T) {
	upstream := &core.ModelsResponse{
		Object: "list",
		Data: []core.Model{
			// Padded ID: the retained entry must be normalized, not just deduped.
			{ID: " listed-model ", Object: "model", OwnedBy: "upstream", Created: 42},
			// A whitespace variant of the same ID must not create a duplicate.
			{ID: "listed-model", Object: "model", OwnedBy: "upstream-dup", Created: 43},
		},
	}
	resp, reason := applyConfiguredProviderModels(
		"test",
		"test-type",
		config.ConfiguredProviderModelsModeMerge,
		[]string{"listed-model", "unlisted-model"},
		upstream,
		nil,
		123,
	)

	require.Equal(t, configuredProviderModelsMerge, reason)
	require.NotNil(t, resp)
	require.Len(t, resp.Data, 2)
	require.Equal(t, "listed-model", resp.Data[0].ID)
	require.Equal(t, "upstream", resp.Data[0].OwnedBy)
	require.Equal(t, int64(42), resp.Data[0].Created, "Data[0] = %+v, want upstream entry kept authoritative", resp.Data[0])
	require.Equal(t, "unlisted-model", resp.Data[1].ID)
	require.Equal(t, "test-type", resp.Data[1].OwnedBy)
	require.Equal(t, int64(123), resp.Data[1].Created, "Data[1] = %+v, want synthesized configured entry", resp.Data[1])
}

func TestApplyConfiguredProviderModels_MergeFallsBackWhenUpstreamFails(t *testing.T) {
	tests := []struct {
		name       string
		upstream   *core.ModelsResponse
		err        error
		wantReason configuredProviderModelsApplyReason
	}{
		{name: "error", upstream: nil, err: errors.New("upstream down"), wantReason: configuredProviderModelsUpstreamError},
		{name: "nil", upstream: nil, wantReason: configuredProviderModelsUpstreamNil},
		{name: "empty", upstream: &core.ModelsResponse{Object: "list"}, wantReason: configuredProviderModelsUpstreamEmpty},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, reason := applyConfiguredProviderModels(
				"test",
				"test-type",
				config.ConfiguredProviderModelsModeMerge,
				[]string{"configured-model"},
				tt.upstream,
				tt.err,
				123,
			)
			require.Equal(t, tt.wantReason, reason)
			require.NotNil(t, resp)
			require.Len(t, resp.Data, 1)
			require.Equal(t, "configured-model", resp.Data[0].ID)
		})
	}
}

func TestApplyConfiguredProviderModels_MissingModelsEndpointIsAuthoritative(t *testing.T) {
	notFound := core.MarkModelListingUnsupported(core.ParseProviderError("openai", http.StatusNotFound, []byte("<html>404 Not Found</html>"), nil))
	tests := []struct {
		name       string
		mode       config.ConfiguredProviderModelsMode
		err        error
		wantReason configuredProviderModelsApplyReason
	}{
		{name: "fallback 404", mode: config.ConfiguredProviderModelsModeFallback, err: notFound, wantReason: configuredProviderModelsUpstreamUnlisted},
		{name: "merge 404", mode: config.ConfiguredProviderModelsModeMerge, err: notFound, wantReason: configuredProviderModelsUpstreamUnlisted},
		{name: "wrapped 404", mode: config.ConfiguredProviderModelsModeFallback, err: fmt.Errorf("list models: %w", notFound), wantReason: configuredProviderModelsUpstreamUnlisted},
		{name: "405", mode: config.ConfiguredProviderModelsModeFallback, err: core.MarkModelListingUnsupported(core.ParseProviderError("openai", http.StatusMethodNotAllowed, nil, nil)), wantReason: configuredProviderModelsUpstreamUnlisted},
		// A 404 from an API other than /models (e.g. the Bedrock control plane) is not marked.
		{name: "unmarked 404", mode: config.ConfiguredProviderModelsModeFallback, err: core.ParseProviderError("bedrock", http.StatusNotFound, nil, nil), wantReason: configuredProviderModelsUpstreamError},
		{name: "500", mode: config.ConfiguredProviderModelsModeFallback, err: core.ParseProviderError("openai", http.StatusInternalServerError, nil, nil), wantReason: configuredProviderModelsUpstreamError},
		{name: "401", mode: config.ConfiguredProviderModelsModeFallback, err: core.ParseProviderError("openai", http.StatusUnauthorized, nil, nil), wantReason: configuredProviderModelsUpstreamError},
		{name: "plain error", mode: config.ConfiguredProviderModelsModeFallback, err: errors.New("connection refused"), wantReason: configuredProviderModelsUpstreamError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, reason := applyConfiguredProviderModels("stt", "openai", tt.mode, []string{"whisper-1"}, nil, tt.err, 123)
			require.Equal(t, tt.wantReason, reason)
			require.NotNil(t, resp)
			require.Len(t, resp.Data, 1)
			require.Equal(t, "whisper-1", resp.Data[0].ID)
		})
	}
}

// wildcardPatternUpstream returns a healthy upstream inventory for the pattern
// mode tests. Metadata (OwnedBy/Created) marks entries that must survive
// pattern resolution untouched.
func wildcardPatternUpstream() *core.ModelsResponse {
	return &core.ModelsResponse{
		Object: "list",
		Data: []core.Model{
			{ID: "openai/gpt-4o:free", Object: "model", OwnedBy: "upstream", Created: 42},
			{ID: "deepseek/deepseek-r1:free", Object: "model", OwnedBy: "upstream", Created: 43},
			{ID: "openai/gpt-4o", Object: "model", OwnedBy: "upstream", Created: 44},
			{ID: "meta/llama-3-free", Object: "model", OwnedBy: "upstream", Created: 45},
		},
	}
}

func patternModeModelIDs(resp *core.ModelsResponse) []string {
	ids := make([]string, 0, len(resp.Data))
	for _, model := range resp.Data {
		ids = append(ids, model.ID)
	}
	return ids
}

func TestApplyConfiguredProviderModels_PatternExpandsPatterns(t *testing.T) {
	tests := []struct {
		name       string
		configured []string
		wantIDs    []string
	}{
		{
			name:       "suffix glob",
			configured: []string{"*:free"},
			wantIDs:    []string{"openai/gpt-4o:free", "deepseek/deepseek-r1:free"},
		},
		{
			name:       "suffix glob with exact extra",
			configured: []string{"*:free", "extra-model"},
			wantIDs:    []string{"openai/gpt-4o:free", "deepseek/deepseek-r1:free", "extra-model"},
		},
		{
			name:       "prefix glob",
			configured: []string{"*-free"},
			wantIDs:    []string{"meta/llama-3-free"},
		},
		{
			name:       "substring glob",
			configured: []string{"*free*"},
			wantIDs:    []string{"openai/gpt-4o:free", "deepseek/deepseek-r1:free", "meta/llama-3-free"},
		},
		{
			name:       "glob is case-insensitive",
			configured: []string{"*:FREE"},
			wantIDs:    []string{"openai/gpt-4o:free", "deepseek/deepseek-r1:free"},
		},
		{
			name:       "question mark glob",
			configured: []string{"openai/gpt-4?"},
			wantIDs:    []string{"openai/gpt-4o"},
		},
		{
			name:       "star alone unions upstream with exact extras",
			configured: []string{"*", "extra-model"},
			wantIDs: []string{
				"openai/gpt-4o:free",
				"deepseek/deepseek-r1:free",
				"openai/gpt-4o",
				"meta/llama-3-free",
				"extra-model",
			},
		},
		{
			name:       "exact entry already matched by pattern stays in upstream order",
			configured: []string{"*:free", "deepseek/deepseek-r1:free"},
			wantIDs:    []string{"openai/gpt-4o:free", "deepseek/deepseek-r1:free"},
		},
		{
			name:       "exact entry listed upstream but not matched is appended with upstream metadata",
			configured: []string{"*:free", "openai/gpt-4o"},
			wantIDs:    []string{"openai/gpt-4o:free", "deepseek/deepseek-r1:free", "openai/gpt-4o"},
		},
		{
			name:       "pattern matching nothing still keeps exact entries",
			configured: []string{"anthropic/*", "extra-model"},
			wantIDs:    []string{"extra-model"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, reason := applyConfiguredProviderModels(
				"test",
				"test-type",
				config.ConfiguredProviderModelsModePattern,
				tt.configured,
				wildcardPatternUpstream(),
				nil,
				123,
			)
			require.Equal(t, configuredProviderModelsPattern, reason)
			require.NotNil(t, resp)
			require.Equal(t, tt.wantIDs, patternModeModelIDs(resp))

			for _, model := range resp.Data {
				require.NotContains(t, model.ID, "*", "a pattern must never be published as a literal model ID")
				require.NotContains(t, model.ID, "?", "a pattern must never be published as a literal model ID")
			}
		})
	}
}

func TestApplyConfiguredProviderModels_PatternPreservesUpstreamMetadata(t *testing.T) {
	resp, reason := applyConfiguredProviderModels(
		"test",
		"test-type",
		config.ConfiguredProviderModelsModePattern,
		[]string{"*:free", "openai/gpt-4o", "extra-model"},
		wildcardPatternUpstream(),
		nil,
		123,
	)
	require.Equal(t, configuredProviderModelsPattern, reason)
	require.NotNil(t, resp)
	require.Len(t, resp.Data, 4)

	// Matched upstream entries keep their metadata.
	require.Equal(t, int64(42), resp.Data[0].Created)
	require.Equal(t, "upstream", resp.Data[0].OwnedBy)
	require.Equal(t, int64(43), resp.Data[1].Created)

	// An exact entry listed upstream reuses the upstream entry, trimmed and
	// with the existing fixups — not a synthesized one.
	require.Equal(t, "openai/gpt-4o", resp.Data[2].ID)
	require.Equal(t, int64(44), resp.Data[2].Created)
	require.Equal(t, "upstream", resp.Data[2].OwnedBy)

	// An exact entry the upstream does not list is synthesized.
	require.Equal(t, "extra-model", resp.Data[3].ID)
	require.Equal(t, "model", resp.Data[3].Object)
	require.Equal(t, "test-type", resp.Data[3].OwnedBy)
	require.Equal(t, int64(123), resp.Data[3].Created)
}

// A pattern list in another mode is not expanded: only pattern mode resolves
// globs, so an allowlist treats the pattern as a literal (and therefore
// unroutable) entry while fallback and merge keep their usual behavior.
func TestApplyConfiguredProviderModels_PatternOnlyInPatternMode(t *testing.T) {
	t.Run("allowlist keeps the pattern as a literal entry", func(t *testing.T) {
		resp, reason := applyConfiguredProviderModels(
			"test",
			"test-type",
			config.ConfiguredProviderModelsModeAllowlist,
			[]string{"*:free"},
			wildcardPatternUpstream(),
			nil,
			123,
		)
		require.Equal(t, configuredProviderModelsAllowlist, reason)
		require.NotNil(t, resp)
		require.Equal(t, []string{"*:free"}, patternModeModelIDs(resp))
	})

	t.Run("merge unions upstream with the pattern as a literal entry", func(t *testing.T) {
		resp, reason := applyConfiguredProviderModels(
			"test",
			"test-type",
			config.ConfiguredProviderModelsModeMerge,
			[]string{"*:free"},
			wildcardPatternUpstream(),
			nil,
			123,
		)
		require.Equal(t, configuredProviderModelsMerge, reason)
		require.NotNil(t, resp)
		require.Equal(t, []string{"openai/gpt-4o:free", "deepseek/deepseek-r1:free", "openai/gpt-4o", "meta/llama-3-free", "*:free"}, patternModeModelIDs(resp))
	})

	t.Run("fallback serves upstream unchanged", func(t *testing.T) {
		upstream := wildcardPatternUpstream()
		resp, reason := applyConfiguredProviderModels(
			"test",
			"test-type",
			config.ConfiguredProviderModelsModeFallback,
			[]string{"*:free"},
			upstream,
			nil,
			123,
		)
		require.Equal(t, configuredProviderModelsNotApplied, reason)
		require.Equal(t, patternModeModelIDs(upstream), patternModeModelIDs(resp))
	})
}

// When the upstream cannot supply an inventory, patterns are unresolvable and
// dropped with the existing fallback reasons — only the exact entries survive,
// and no pattern is ever synthesized as a literal model ID.
func TestApplyConfiguredProviderModels_PatternFallsBackToExactEntries(t *testing.T) {
	notFound := core.MarkModelListingUnsupported(core.ParseProviderError("openai", http.StatusNotFound, nil, nil))
	tests := []struct {
		name       string
		upstream   *core.ModelsResponse
		err        error
		wantReason configuredProviderModelsApplyReason
	}{
		{name: "error", err: errors.New("upstream down"), wantReason: configuredProviderModelsUpstreamError},
		{name: "unlisted", err: notFound, wantReason: configuredProviderModelsUpstreamUnlisted},
		{name: "nil", wantReason: configuredProviderModelsUpstreamNil},
		{name: "empty", upstream: &core.ModelsResponse{Object: "list"}, wantReason: configuredProviderModelsUpstreamEmpty},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, reason := applyConfiguredProviderModels(
				"test",
				"test-type",
				config.ConfiguredProviderModelsModePattern,
				[]string{"*:free", "exact-model"},
				tt.upstream,
				tt.err,
				123,
			)
			require.Equal(t, tt.wantReason, reason)
			require.NotNil(t, resp)
			require.Equal(t, []string{"exact-model"}, patternModeModelIDs(resp))
		})
	}
}

func TestHasModelPattern(t *testing.T) {
	require.False(t, hasModelPattern(nil))
	require.False(t, hasModelPattern([]string{"gpt-4o", "free"}))
	require.True(t, hasModelPattern([]string{"gpt-4o", "*:free"}))
	require.True(t, hasModelPattern([]string{"gpt-4?"}))
}
