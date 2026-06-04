package cli

import (
	"strings"
	"testing"
)

func TestResolveCtxValueFromArg(t *testing.T) {
	v, err := resolveCtxValue("", false, nil, []string{"key", "the-value"})
	if err != nil || v != "the-value" {
		t.Fatalf("got %q err=%v", v, err)
	}
}

func TestResolveCtxValueFromStdin(t *testing.T) {
	v, err := resolveCtxValue("", true, strings.NewReader("piped"), []string{"key"})
	if err != nil || v != "piped" {
		t.Fatalf("got %q err=%v", v, err)
	}
}

func TestResolveCtxValueMissingErrors(t *testing.T) {
	if _, err := resolveCtxValue("", false, nil, []string{"key"}); err == nil {
		t.Fatalf("expected error when no value/file/stdin")
	}
}
