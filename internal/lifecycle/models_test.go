package lifecycle

import (
	"testing"
)

// lifecycleWithModel builds a Lifecycle whose config reports modelDefault as the
// configured default model (empty → the hardcoded fallback applies).
func lifecycleWithModel(modelDefault string) *Lifecycle {
	return New(&FakeRunner{}, &FakeConfig{ModelDefault: modelDefault})
}

func TestResolveModel(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"short name preserved", "opus", "opus"},
		{"sonnet preserved", "sonnet", "sonnet"},
		{"haiku preserved", "haiku", "haiku"},
		{"fable preserved", "fable", "fable"},
		{"full model ID preserved", "claude-custom-1", "claude-custom-1"},
		{"empty string", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveModel(tt.input)
			if got != tt.expected {
				t.Errorf("ResolveModel(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestModelOrDefault(t *testing.T) {
	t.Run("explicit model overrides configured default", func(t *testing.T) {
		l := lifecycleWithModel("haiku")
		got := l.modelOrDefault("opus")
		expected := "opus"
		if got != expected {
			t.Errorf("modelOrDefault(%q) = %q, want %q", "opus", got, expected)
		}
	})

	t.Run("configured default preserved verbatim", func(t *testing.T) {
		l := lifecycleWithModel("haiku")
		got := l.modelOrDefault("")
		expected := "haiku"
		if got != expected {
			t.Errorf("modelOrDefault(%q) = %q, want %q", "", got, expected)
		}
	})

	t.Run("configured default with full ID", func(t *testing.T) {
		l := lifecycleWithModel("claude-custom-1")
		got := l.modelOrDefault("")
		expected := "claude-custom-1"
		if got != expected {
			t.Errorf("modelOrDefault(%q) = %q, want %q", "", got, expected)
		}
	})

	t.Run("hardcoded fallback when config empty", func(t *testing.T) {
		l := lifecycleWithModel("")
		got := l.modelOrDefault("")
		expected := DefaultModel
		if got != expected {
			t.Errorf("modelOrDefault(%q) = %q, want %q", "", got, expected)
		}
	})
}
