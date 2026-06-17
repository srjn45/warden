package lifecycle

import (
	"os"
	"testing"
)

func TestResolveModel(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"opus alias", "opus", "claude-opus-4-8"},
		{"sonnet alias", "sonnet", "claude-sonnet-4-6"},
		{"haiku alias", "haiku", "claude-haiku-4-5"},
		{"fable alias", "fable", "claude-fable-5"},
		{"full model ID", "claude-custom-1", "claude-custom-1"},
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
	// Save and restore env var
	origEnv := os.Getenv("WARDEN_MODEL_DEFAULT")
	defer func() {
		if origEnv != "" {
			os.Setenv("WARDEN_MODEL_DEFAULT", origEnv)
		} else {
			os.Unsetenv("WARDEN_MODEL_DEFAULT")
		}
	}()

	t.Run("explicit model overrides default", func(t *testing.T) {
		os.Setenv("WARDEN_MODEL_DEFAULT", "haiku")
		got := modelOrDefault("opus")
		expected := "claude-opus-4-8"
		if got != expected {
			t.Errorf("modelOrDefault(%q) = %q, want %q", "opus", got, expected)
		}
	})

	t.Run("env var default with alias", func(t *testing.T) {
		os.Setenv("WARDEN_MODEL_DEFAULT", "haiku")
		got := modelOrDefault("")
		expected := "claude-haiku-4-5"
		if got != expected {
			t.Errorf("modelOrDefault(%q) = %q, want %q", "", got, expected)
		}
	})

	t.Run("env var default with full ID", func(t *testing.T) {
		os.Setenv("WARDEN_MODEL_DEFAULT", "claude-custom-1")
		got := modelOrDefault("")
		expected := "claude-custom-1"
		if got != expected {
			t.Errorf("modelOrDefault(%q) = %q, want %q", "", got, expected)
		}
	})

	t.Run("hardcoded default", func(t *testing.T) {
		os.Unsetenv("WARDEN_MODEL_DEFAULT")
		got := modelOrDefault("")
		expected := "claude-sonnet-4-5"
		if got != expected {
			t.Errorf("modelOrDefault(%q) = %q, want %q", "", got, expected)
		}
	})
}
