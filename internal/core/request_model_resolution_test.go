package core

import "testing"

func TestResolveRepetitionWithDefaults(t *testing.T) {
	tests := []struct {
		name       string
		resolution *RequestModelResolution
		defLimit   int
		defMax     int
		wantLimit  int
		wantMax    int
	}{
		{
			name:      "nil resolution inherits defaults",
			defLimit:  7,
			defMax:    8,
			wantLimit: 7,
			wantMax:   8,
		},
		{
			name:       "nil fields inherit defaults",
			resolution: &RequestModelResolution{},
			defLimit:   7,
			defMax:     8,
			wantLimit:  7,
			wantMax:    8,
		},
		{
			name:       "explicit zero limit disables guard",
			resolution: &RequestModelResolution{RepetitionLimit: ptr(0)},
			defLimit:   7,
			defMax:     8,
			wantLimit:  0,
			wantMax:    8,
		},
		{
			name:       "override fields win independently",
			resolution: &RequestModelResolution{RepetitionMaxPattern: ptr(3)},
			defLimit:   7,
			defMax:     8,
			wantLimit:  7,
			wantMax:    3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limit, maxPattern := ResolveRepetitionWithDefaults(tt.resolution, tt.defLimit, tt.defMax)
			if limit != tt.wantLimit || maxPattern != tt.wantMax {
				t.Fatalf("ResolveRepetitionWithDefaults() = (%d, %d), want (%d, %d)", limit, maxPattern, tt.wantLimit, tt.wantMax)
			}
		})
	}
}

func ptr(v int) *int { return &v }

func TestRequestModelResolutionRequestedQualifiedModel(t *testing.T) {
	tests := []struct {
		name string
		in   *RequestModelResolution
		want string
	}{
		{
			name: "raw alias with slash and no explicit provider stays raw",
			in: &RequestModelResolution{
				Requested: NewRequestedModelSelector("anthropic/claude-opus-4-6", ""),
			},
			want: "anthropic/claude-opus-4-6",
		},
		{
			name: "explicit provider with provider-prefixed model normalizes once",
			in: &RequestModelResolution{
				Requested: NewRequestedModelSelector("openai/gpt-4o", "openai"),
			},
			want: "openai/gpt-4o",
		},
		{
			name: "explicit provider without prefix becomes qualified model",
			in: &RequestModelResolution{
				Requested: NewRequestedModelSelector("gpt-4o", "openai"),
			},
			want: "openai/gpt-4o",
		},
		{
			name: "explicit provider preserves raw slash model",
			in: &RequestModelResolution{
				Requested: NewRequestedModelSelector("openai/gpt-oss-120b", "groq"),
			},
			want: "groq/openai/gpt-oss-120b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.RequestedQualifiedModel(); got != tt.want {
				t.Fatalf("RequestedQualifiedModel() = %q, want %q", got, tt.want)
			}
		})
	}
}
