package projectkey

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeRemoteURL_DesignPairsCollapse(t *testing.T) {
	// §4.1: differing scheme/user/.git/trailing-slash/case for one repo → one key.
	variants := []string{
		"git@github.com:org/repo.git",
		"https://github.com/org/repo",
		"https://github.com/org/repo.git",
		"https://github.com/org/repo/",
		"ssh://git@github.com/org/repo.git",
		"https://user:token@github.com/org/repo.git",
		"https://github.com:443/org/repo",
		"HTTPS://GitHub.com/Org/Repo.git",
		"  https://github.com/org/repo  ",
	}
	for _, in := range variants {
		got, ok := NormalizeRemoteURL(in)
		if !ok || got != "github.com/org/repo" {
			t.Errorf("NormalizeRemoteURL(%q) = %q, ok=%v; want github.com/org/repo", in, got, ok)
		}
	}
}

func TestNormalizeRemoteURL_NoHostRejected(t *testing.T) {
	for _, in := range []string{"", "   ", "/home/me/repo", "./repo", "not a url"} {
		if key, ok := NormalizeRemoteURL(in); ok {
			t.Errorf("NormalizeRemoteURL(%q) = %q, ok=true; want false", in, key)
		}
	}
}

func TestKey_LocalFallbackTaggedAndNormalized(t *testing.T) {
	if got := Key("git@github.com:org/repo.git", "/home/me/p"); got != "github.com/org/repo" {
		t.Fatalf("Key remote = %q, want the remote key to win", got)
	}
	a := Key("", "/home/me/project")
	b := Key("", "/home/me/project/")
	c := Key("", "/home/me/foo/../project")
	if a != b || a != c {
		t.Fatalf("local keys not normalized: a=%q b=%q c=%q", a, b, c)
	}
	if !strings.HasPrefix(a, LocalKeyPrefix) || a == LocalKeyPrefix {
		t.Fatalf("local key = %q, want a %q-prefixed path", a, LocalKeyPrefix)
	}
}

func TestForDir_TwoWorktreesOneKey(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@example.com")
	run("config", "user.name", "t")
	run("commit", "--allow-empty", "-qm", "init")
	run("remote", "add", "origin", "https://github.com/org/repo")
	wt := filepath.Join(t.TempDir(), "wt")
	run("worktree", "add", "-q", wt, "-b", "wt")

	ctx := context.Background()
	if a, b := ForDir(ctx, dir), ForDir(ctx, wt); a != b || a != "github.com/org/repo" {
		t.Fatalf("worktrees keyed differently: main=%q worktree=%q", a, b)
	}
}
