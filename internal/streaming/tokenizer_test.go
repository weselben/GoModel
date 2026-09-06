package streaming

import (
	"reflect"
	"testing"
)

func TestNewTokenCounter(t *testing.T) {
	tests := []struct {
		name    string
		model   string
		wantNil bool
		wantErr bool
	}{
		{
			name:    "known model returns non-nil counter",
			model:   "gpt-4o",
			wantNil: false,
			wantErr: false,
		},
		{
			name:    "unknown model returns nil counter with nil error",
			model:   "no-such-model-xyz",
			wantNil: true,
			wantErr: false,
		},
		{
			name:    "empty model returns nil counter with nil error",
			model:   "",
			wantNil: true,
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			counter, err := NewTokenCounter(tt.model)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NewTokenCounter(%q) error = nil, want non-nil", tt.model)
				}
				if counter != nil {
					t.Fatalf("NewTokenCounter(%q) counter = %v, want nil when error", tt.model, counter)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewTokenCounter(%q) unexpected error: %v", tt.model, err)
			}
			if tt.wantNil {
				if counter != nil {
					t.Fatalf("NewTokenCounter(%q) counter = %v, want nil", tt.model, counter)
				}
				return
			}
			if counter == nil {
				t.Fatalf("NewTokenCounter(%q) counter = nil, want non-nil", tt.model)
			}
		})
	}
}

func TestTokenCounter_Tokens(t *testing.T) {
	counter, err := NewTokenCounter("gpt-4o")
	if err != nil {
		t.Fatalf("NewTokenCounter(gpt-4o) unexpected error: %v", err)
	}
	if counter == nil {
		t.Fatal("NewTokenCounter(gpt-4o) returned nil counter")
	}

	t.Run("non-empty text returns non-empty token slice", func(t *testing.T) {
		tokens := counter.Tokens("hello world")
		if len(tokens) == 0 {
			t.Fatalf("Tokens(%q) len = 0, want > 0", "hello world")
		}
		// "hello world" tokenizes to a small, known number of tokens under
		// o200k_base; asserting the bound keeps the test from passing if the
		// engine silently returns nothing.
		if len(tokens) > 8 {
			t.Fatalf("Tokens(%q) len = %d, want <= 8", "hello world", len(tokens))
		}
	})

	t.Run("empty text returns empty non-nil slice", func(t *testing.T) {
		tokens := counter.Tokens("")
		if tokens == nil {
			t.Fatal("Tokens(\"\") = nil, want empty non-nil slice")
		}
		if len(tokens) != 0 {
			t.Fatalf("Tokens(\"\") len = %d, want 0", len(tokens))
		}
	})

	t.Run("Tokens is deterministic across calls", func(t *testing.T) {
		first := counter.Tokens("hello world")
		second := counter.Tokens("hello world")
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("Tokens not deterministic: first = %v, second = %v", first, second)
		}
	})

	t.Run("Tokens is deterministic across counters built for the same model", func(t *testing.T) {
		other, err := NewTokenCounter("gpt-4o")
		if err != nil {
			t.Fatalf("NewTokenCounter(gpt-4o) unexpected error: %v", err)
		}
		if other == nil {
			t.Fatal("NewTokenCounter(gpt-4o) returned nil counter")
		}

		first := counter.Tokens("hello world")
		second := other.Tokens("hello world")
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("Tokens not deterministic across counters: first = %v, second = %v", first, second)
		}
	})
}
