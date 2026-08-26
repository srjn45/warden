package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeRemoteURL_DesignPairsCollapse(t *testing.T) {
	// Design §4.1: differing schemes/user/.git/trailing-slash/case for the same
	// repo must resolve to a single key. Each group must collapse to one key.
	groups := [][]string{
		{
			"git@github.com:org/repo.git",
			"https://github.com/org/repo",
			"https://github.com/org/repo.git",
			"https://github.com/org/repo/",
			"ssh://git@github.com/org/repo.git",
			"git://github.com/org/repo.git",
			"https://user:token@github.com/org/repo.git",
			"https://github.com:443/org/repo",
			"HTTPS://GitHub.com/Org/Repo.git",
			"  https://github.com/org/repo  ",
		},
		{
			"git@gitlab.com:group/sub/proj.git",
			"https://gitlab.com/group/sub/proj",
			"ssh://git@gitlab.com:22/group/sub/proj.git",
		},
	}
	wantKeys := []string{"github.com/org/repo", "gitlab.com/group/sub/proj"}

	for gi, variants := range groups {
		for _, in := range variants {
			got, ok := NormalizeRemoteURL(in)
			if !ok {
				t.Errorf("NormalizeRemoteURL(%q): ok=false, want a key", in)
				continue
			}
			if got != wantKeys[gi] {
				t.Errorf("NormalizeRemoteURL(%q) = %q, want %q", in, got, wantKeys[gi])
			}
		}
	}
}

func TestNormalizeRemoteURL_DistinctReposStayDistinct(t *testing.T) {
	a, _ := NormalizeRemoteURL("https://github.com/org/repo.git")
	b, _ := NormalizeRemoteURL("https://github.com/org/other.git")
	c, _ := NormalizeRemoteURL("https://gitlab.com/org/repo.git")
	if a == b || a == c || b == c {
		t.Fatalf("distinct repos collapsed: a=%q b=%q c=%q", a, b, c)
	}
}

func TestNormalizeRemoteURL_NoHostRejected(t *testing.T) {
	for _, in := range []string{"", "   ", "/home/me/repo", "./repo", "not a url"} {
		if key, ok := NormalizeRemoteURL(in); ok {
			t.Errorf("NormalizeRemoteURL(%q) = %q, ok=true; want ok=false", in, key)
		}
	}
}

func TestProjectKey_LocalFallbackTaggedNotRejected(t *testing.T) {
	// A remoteless repo must yield a `local:`-tagged key derived from its path,
	// never an empty/rejected result (design §4.1 decision 4).
	key := ProjectKey("", "/home/me/project")
	if !strings.HasPrefix(key, LocalKeyPrefix) {
		t.Fatalf("ProjectKey local fallback = %q, want %q prefix", key, LocalKeyPrefix)
	}
	if key == LocalKeyPrefix {
		t.Fatalf("ProjectKey local fallback = bare prefix %q, want a path-derived key", key)
	}
	if !strings.Contains(key, "project") {
		t.Errorf("ProjectKey local fallback = %q, want it derived from the path", key)
	}
}

func TestProjectKey_RemoteWinsOverLocal(t *testing.T) {
	key := ProjectKey("git@github.com:org/repo.git", "/home/me/project")
	if key != "github.com/org/repo" {
		t.Fatalf("ProjectKey = %q, want remote key to win", key)
	}
}

func TestProjectKey_LocalKeyPathNormalized(t *testing.T) {
	// Trailing slashes / uncleaned segments must not split one local repo.
	a := ProjectKey("", "/home/me/project")
	b := ProjectKey("", "/home/me/project/")
	c := ProjectKey("", "/home/me/foo/../project")
	if a != b || a != c {
		t.Fatalf("local keys not normalized: a=%q b=%q c=%q", a, b, c)
	}
}

func TestProjectKey_EmptyRootIsBarePrefix(t *testing.T) {
	if got := ProjectKey("", ""); got != LocalKeyPrefix {
		t.Fatalf("ProjectKey(\"\",\"\") = %q, want %q", got, LocalKeyPrefix)
	}
}

// --- ProjectKeyForDir integration (real git) --------------------------------

func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func TestProjectKeyForDir_ReadsRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	gitInit(t, dir)
	if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin",
		"git@github.com:org/repo.git").CombinedOutput(); err != nil {
		t.Fatalf("remote add: %v: %s", err, out)
	}
	if got := ProjectKeyForDir(context.Background(), dir); got != "github.com/org/repo" {
		t.Fatalf("ProjectKeyForDir = %q, want github.com/org/repo", got)
	}
}

func TestProjectKeyForDir_TwoWorktreesOneKey(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	gitInit(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-qm", "init"},
		{"remote", "add", "origin", "https://github.com/org/repo"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", dir, "worktree", "add", "-q", wt, "-b", "wt").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v: %s", err, out)
	}
	ctx := context.Background()
	if a, b := ProjectKeyForDir(ctx, dir), ProjectKeyForDir(ctx, wt); a != b || a != "github.com/org/repo" {
		t.Fatalf("worktrees keyed differently: main=%q worktree=%q", a, b)
	}
}

func TestProjectKeyForDir_NoRemoteLocalFallback(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	gitInit(t, dir)
	got := ProjectKeyForDir(context.Background(), dir)
	if !strings.HasPrefix(got, LocalKeyPrefix) {
		t.Fatalf("ProjectKeyForDir no-remote = %q, want %q prefix", got, LocalKeyPrefix)
	}
	// The key should root at the repo top level (symlinks resolved by git).
	if got == LocalKeyPrefix {
		t.Fatalf("ProjectKeyForDir no-remote = bare prefix, want a path")
	}
}
