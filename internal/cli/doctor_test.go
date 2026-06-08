package cli

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckBinary(t *testing.T) {
	okLook := func(string) (string, error) { return "/usr/bin/foo", nil }
	missLook := func(string) (string, error) { return "", errors.New("not found") }

	if r := checkBinary("foo", true, okLook); !r.ok || !r.required || r.detail != "/usr/bin/foo" {
		t.Fatalf("found: %+v", r)
	}
	if r := checkBinary("foo", false, missLook); r.ok || r.required {
		t.Fatalf("missing optional should be not-ok, not-required: %+v", r)
	}
}

func TestCheckBinaries(t *testing.T) {
	// Only "git" resolves; the rest are missing.
	look := func(name string) (string, error) {
		if name == "git" {
			return "/usr/bin/git", nil
		}
		return "", errors.New("not found")
	}
	results := checkBinaries(look)

	byName := map[string]checkResult{}
	for _, r := range results {
		byName[r.name] = r
	}
	for _, req := range requiredBinaries {
		r, present := byName[req]
		if !present {
			t.Fatalf("required binary %q not checked", req)
		}
		if !r.required {
			t.Fatalf("%q should be required", req)
		}
	}
	for _, opt := range optionalBinaries {
		r := byName[opt]
		if r.required {
			t.Fatalf("%q should be optional (warn-only)", opt)
		}
	}
	if !byName["git"].ok {
		t.Fatalf("git should pass: %+v", byName["git"])
	}
}

func TestCheckDataDir(t *testing.T) {
	dir := t.TempDir()
	if r := checkDataDir(dir); !r.ok || !r.required {
		t.Fatalf("writable dir should pass: %+v", r)
	}

	missing := filepath.Join(dir, "nope")
	if r := checkDataDir(missing); r.ok {
		t.Fatalf("missing dir should fail: %+v", r)
	}

	file := filepath.Join(dir, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := checkDataDir(file); r.ok {
		t.Fatalf("file (not dir) should fail: %+v", r)
	}
}

func TestCheckDaemon(t *testing.T) {
	body := func() *http.Response {
		return &http.Response{StatusCode: 200, Body: http.NoBody}
	}

	okGet := func(string) (*http.Response, error) { return body(), nil }
	if r := checkDaemon("http://x", okGet); !r.ok || !r.required {
		t.Fatalf("200 should pass: %+v", r)
	}

	badGet := func(string) (*http.Response, error) {
		return &http.Response{StatusCode: 500, Body: http.NoBody}, nil
	}
	if r := checkDaemon("http://x", badGet); r.ok {
		t.Fatalf("500 should fail: %+v", r)
	}

	errGet := func(string) (*http.Response, error) { return nil, errors.New("connection refused") }
	if r := checkDaemon("http://x", errGet); r.ok || !strings.Contains(r.detail, "unreachable") {
		t.Fatalf("error should fail with unreachable: %+v", r)
	}
}

func TestAllRequiredPass(t *testing.T) {
	pass := []checkResult{
		{name: "a", ok: true, required: true},
		{name: "b", ok: false, required: false}, // optional fail is tolerated
	}
	if !allRequiredPass(pass) {
		t.Fatal("optional failure should not fail the suite")
	}

	fail := []checkResult{
		{name: "a", ok: false, required: true},
	}
	if allRequiredPass(fail) {
		t.Fatal("required failure should fail the suite")
	}
}

func TestFormatReport(t *testing.T) {
	results := []checkResult{
		{name: "tmux", ok: true, required: true, detail: "/usr/bin/tmux"},
		{name: "gh", ok: false, required: false, detail: "not found on PATH"},
		{name: "daemon", ok: false, required: true, detail: "unreachable"},
	}
	out := formatReport("1.2.3", results)

	for _, want := range []string{"1.2.3", "tmux", "gh", "daemon", "FAIL", "warn"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "FAILED") {
		t.Fatalf("required failure should produce a failing summary:\n%s", out)
	}

	allOK := formatReport("1.2.3", []checkResult{{name: "git", ok: true, required: true, detail: "/usr/bin/git"}})
	if !strings.Contains(allOK, "passed") {
		t.Fatalf("all-pass summary missing:\n%s", allOK)
	}
}
