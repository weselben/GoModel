package virtualmodels

import (
	"context"
	"testing"

	"github.com/enterpilot/gomodel/internal/core"
)

// TestResolveRepetitionLimit covers the precedence rules mirrored from
// ResolveSlowdown: alias wins over its concrete target; nil fields inherit; an
// alias that pins at least one field short-circuits; otherwise the policy
// precedence (exact > providerWide > modelWide > global) applies.
func TestResolveRepetitionLimit(t *testing.T) {
	tests := []struct {
		name           string
		rows           []VirtualModel
		requested      core.RequestedModelSelector
		resolved       core.ModelSelector
		ctx            context.Context
		wantLimit      *int
		wantMaxPattern *int
	}{
		{
			name: "alias overrides concrete model",
			rows: []VirtualModel{
				{
					Source:               "guard-alias",
					Targets:              []Target{{Provider: "openai", Model: "gpt-4o"}},
					RepetitionLimit:      new(4),
					RepetitionMaxPattern: new(12),
					Enabled:              true,
				},
				{
					Source:               "openai/gpt-4o",
					ProviderName:         "openai",
					Model:                "gpt-4o",
					RepetitionLimit:      new(2),
					RepetitionMaxPattern: new(6),
					Enabled:              true,
				},
			},
			requested:      core.NewRequestedModelSelector("guard-alias", ""),
			resolved:       core.ModelSelector{Provider: "openai", Model: "gpt-4o"},
			wantLimit:      new(4),
			wantMaxPattern: new(12),
		},
		{
			name: "alias inherits concrete model when both fields are nil",
			rows: []VirtualModel{
				{
					Source:  "plain-alias",
					Targets: []Target{{Provider: "openai", Model: "gpt-4o"}},
					Enabled: true,
				},
				{
					Source:               "openai/gpt-4o",
					ProviderName:         "openai",
					Model:                "gpt-4o",
					RepetitionLimit:      new(2),
					RepetitionMaxPattern: new(6),
					Enabled:              true,
				},
			},
			requested:      core.NewRequestedModelSelector("plain-alias", ""),
			resolved:       core.ModelSelector{Provider: "openai", Model: "gpt-4o"},
			wantLimit:      new(2),
			wantMaxPattern: new(6),
		},
		{
			name: "explicit alias limit zero is an override, not inherit",
			rows: []VirtualModel{
				{
					Source:          "off-alias",
					Targets:         []Target{{Provider: "openai", Model: "gpt-4o"}},
					RepetitionLimit: new(0),
					Enabled:         true,
				},
				{
					Source:          "openai/gpt-4o",
					ProviderName:    "openai",
					Model:           "gpt-4o",
					RepetitionLimit: new(2),
					Enabled:         true,
				},
			},
			requested:      core.NewRequestedModelSelector("off-alias", ""),
			resolved:       core.ModelSelector{Provider: "openai", Model: "gpt-4o"},
			wantLimit:      new(0),
			wantMaxPattern: nil,
		},
		{
			name: "policy precedence: exact beats model-wide",
			rows: []VirtualModel{
				{
					Source:               "openai/gpt-4o",
					ProviderName:         "openai",
					Model:                "gpt-4o",
					RepetitionLimit:      new(5),
					RepetitionMaxPattern: new(7),
					Enabled:              true,
				},
				{
					Source:               "gpt-4o",
					ProviderName:         "",
					Model:                "gpt-4o",
					RepetitionLimit:      new(2),
					RepetitionMaxPattern: new(4),
					Enabled:              true,
				},
			},
			requested:      core.NewRequestedModelSelector("openai/gpt-4o", "openai"),
			resolved:       core.ModelSelector{Provider: "openai", Model: "gpt-4o"},
			wantLimit:      new(5),
			wantMaxPattern: new(7),
		},
		{
			name: "non-matching user path stops the policy",
			rows: []VirtualModel{{
				Source:               "openai/gpt-4o",
				ProviderName:         "openai",
				Model:                "gpt-4o",
				UserPaths:            []string{"/team/alpha"},
				RepetitionLimit:      new(3),
				RepetitionMaxPattern: new(8),
				Enabled:              true,
			}},
			requested:      core.NewRequestedModelSelector("openai/gpt-4o", "openai"),
			resolved:       core.ModelSelector{Provider: "openai", Model: "gpt-4o"},
			ctx:            core.WithEffectiveUserPath(context.Background(), "/team/beta"),
			wantLimit:      nil,
			wantMaxPattern: nil,
		},
		{
			name: "explicit provider request skips alias lookup",
			rows: []VirtualModel{
				{
					Source:          "openai/gpt-4o",
					Targets:         []Target{{Provider: "openai", Model: "gpt-4o"}},
					RepetitionLimit: new(7),
					Enabled:         true,
				},
			},
			requested: core.NewRequestedModelSelector("openai/gpt-4o", "openai"),
			resolved:  core.ModelSelector{Provider: "openai", Model: "gpt-4o"},
			wantLimit: nil, wantMaxPattern: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := newSlowdownService(t, tt.rows...)
			ctx := tt.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			gotLimit, gotMax := service.ResolveRepetitionLimit(ctx, tt.requested, tt.resolved)
			if (gotLimit == nil) != (tt.wantLimit == nil) ||
				(gotLimit != nil && *gotLimit != *tt.wantLimit) {
				t.Fatalf("RepetitionLimit = %v, want %v", gotLimit, tt.wantLimit)
			}
			if (gotMax == nil) != (tt.wantMaxPattern == nil) ||
				(gotMax != nil && *gotMax != *tt.wantMaxPattern) {
				t.Fatalf("MaxPattern = %v, want %v", gotMax, tt.wantMaxPattern)
			}
		})
	}
}

// TestVirtualModelCloneDoesNotShareRepetition mirrors the Slowdown clone test:
// the pointer fields must deep-copy so a snapshot consumer cannot mutate the
// cached source row.
func TestVirtualModelCloneDoesNotShareRepetition(t *testing.T) {
	original := VirtualModel{RepetitionLimit: new(5), RepetitionMaxPattern: new(8)}
	cloned := original.clone()

	*cloned.RepetitionLimit = 1
	*cloned.RepetitionMaxPattern = 2
	if *original.RepetitionLimit != 5 || *original.RepetitionMaxPattern != 8 {
		t.Fatalf("mutating clone changed original: limit=%d max=%d", *original.RepetitionLimit, *original.RepetitionMaxPattern)
	}
}
