package gateway

import (
	"testing"

	"github.com/enterpilot/gomodel/internal/core"
)

// TestResolveEffectiveRepetition mirrors the StreamResult override semantics:
// workflow override 0 => guard off; override N => N; nil => global.
// Per-field overrides inherit independently: an unset field falls back to the
// orchestrator global even when its sibling override is set.
func TestResolveEffectiveRepetition(t *testing.T) {
	zero, five, twelve := 0, 5, 12
	tests := []struct {
		name           string
		globalLimit    int
		globalMax      int
		workflow       *core.Workflow
		wantLimit      int
		wantMaxPattern int
	}{
		{
			name:           "nil overrides inherit global",
			globalLimit:    10,
			globalMax:      8,
			workflow:       &core.Workflow{Resolution: &core.RequestModelResolution{}},
			wantLimit:      10,
			wantMaxPattern: 8,
		},
		{
			name:        "override N wins over global",
			globalLimit: 10,
			globalMax:   8,
			workflow: &core.Workflow{Resolution: &core.RequestModelResolution{
				RepetitionLimit:      &five,
				RepetitionMaxPattern: &twelve,
			}},
			wantLimit:      5,
			wantMaxPattern: 12,
		},
		{
			name:        "override limit zero disables guard for request",
			globalLimit: 10,
			globalMax:   8,
			workflow: &core.Workflow{Resolution: &core.RequestModelResolution{
				RepetitionLimit: &zero,
			}},
			wantLimit:      0,
			wantMaxPattern: 8,
		},
		{
			name:        "maxPattern-only override keeps global limit active",
			globalLimit: 10,
			globalMax:   8,
			workflow: &core.Workflow{Resolution: &core.RequestModelResolution{
				RepetitionMaxPattern: &twelve,
			}},
			wantLimit:      10,
			wantMaxPattern: 12,
		},
		{
			name:           "nil workflow falls back to global",
			globalLimit:    7,
			globalMax:      6,
			workflow:       nil,
			wantLimit:      7,
			wantMaxPattern: 6,
		},
		{
			name:           "nil resolution falls back to global",
			globalLimit:    4,
			globalMax:      9,
			workflow:       &core.Workflow{},
			wantLimit:      4,
			wantMaxPattern: 9,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := &InferenceOrchestrator{
				streamRepetitionLimit:      tt.globalLimit,
				streamRepetitionMaxPattern: tt.globalMax,
			}
			gotLimit, gotMax := o.resolveEffectiveRepetition(tt.workflow)
			if gotLimit != tt.wantLimit {
				t.Fatalf("limit = %d, want %d", gotLimit, tt.wantLimit)
			}
			if gotMax != tt.wantMaxPattern {
				t.Fatalf("maxPattern = %d, want %d", gotMax, tt.wantMaxPattern)
			}
		})
	}
}
