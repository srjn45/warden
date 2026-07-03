// Package memory is warden's backend-neutral project memory store (#53). It owns
// a committed, repo-keyed .warden/memory.md — the canonical source of durable,
// cross-agent facts ("where X lives", "run Y via `wd check`", project invariants)
// that survives an agent's teardown so the next agent (any backend) doesn't re-pay
// the rediscovery tax.
//
// PR-0 scope (this package): locate the file (implicit repo-root keying,
// auto-create on first use, no `wd init` gate), parse it into a typed model that
// TOLERATES the trust/timestamp/provenance metadata the curation pass (PR-2) will
// later write, and render a budgeted projection string. The render fn is the seam
// PR-1 reuses for launch-time injection — so it is exported and takes the parsed
// model + a byte budget.
//
// The package is deliberately neutral: no agentbackend, lifecycle, or daemon
// coupling, and the repo-root resolver + file IO are injectable so tests need no
// real git repo. warden READS but never rewrites a human's CLAUDE.md / AGENTS.md /
// CONVENTIONS.md — only its own .warden/memory.md.
package memory

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// MemoryFile is the canonical per-repo memory path, relative to the repo root and
// living beside the existing committed .warden/check.yml.
const MemoryFile = ".warden/memory.md"

// headerTemplate seeds a freshly auto-created .warden/memory.md. It is a single
// HTML comment so the whole template projects as preamble (never as a phantom
// entry) and documents the entry shape for a human hand-authoring memory in PR-0.
const headerTemplate = `<!-- warden project memory — committed, repo-keyed (.warden/memory.md, beside check.yml).

Durable, cross-agent facts the NEXT agent should not have to rediscover: where
things live, how to run X, project invariants. Keep it compact and navigational
("X lives in Y", "run Z via ` + "`wd check`" + `"), not prose.

warden READS but never rewrites your CLAUDE.md / AGENTS.md / CONVENTIONS.md — this
file is warden's own.

Entry form (the [...] metadata prefix is optional — a plain "- bullet" is fine):
  - [trusted · 2026-06-30 · agent a1b2 · sha 04e2aed] The daemon API is spec-first:
    edit openapi.yaml then ` + "`make generate`" + `; never hand-write handlers.
  - [unverified · 2026-06-30 · agent c3d4] Tests run behind ` + "`wd check`" + `.
  - A plain human-authored bullet with no metadata.

trust is one of: unverified, trusted. Timestamps are absolute (YYYY-MM-DD).
-->
`

// RepoRootFunc resolves the git repo root that contains dir. It is the injectable
// keying seam: production uses GitRepoRoot (shells `git rev-parse`), tests pass a
// stub so they need no real repo.
type RepoRootFunc func(ctx context.Context, dir string) (string, error)

// GitRepoRoot resolves the repo root for dir by shelling
// `git -C dir rev-parse --show-toplevel` — the same pattern lifecycle already uses
// for rev-parse (lifecycle GitBranch), reproduced here rather than imported so this
// package stays neutral (the lifecycle helper is a method on a Runner-bearing
// Lifecycle and not cleanly reusable from a dependency-light package).
func GitRepoRoot(ctx context.Context, dir string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("resolve repo root for %q: %w", dir, err)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("resolve repo root for %q: empty toplevel", dir)
	}
	return root, nil
}

// Store locates and reads .warden/memory.md for a repo, keyed implicitly by repo
// root. The zero value works (RepoRoot defaults to GitRepoRoot); inject RepoRoot in
// tests to avoid a real git repo.
type Store struct {
	RepoRoot RepoRootFunc
}

func (s *Store) repoRoot(ctx context.Context, dir string) (string, error) {
	if s.RepoRoot != nil {
		return s.RepoRoot(ctx, dir)
	}
	return GitRepoRoot(ctx, dir)
}

// Locate resolves the absolute .warden/memory.md path for the repo containing dir.
// It does NOT touch the filesystem (no auto-create) — use Resolve for that.
func (s *Store) Locate(ctx context.Context, dir string) (string, error) {
	root, err := s.repoRoot(ctx, dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, MemoryFile), nil
}

// Resolve returns the .warden/memory.md path for the repo containing dir,
// auto-creating it (parent dir + a commented header template) on first use. created
// reports whether this call wrote the file — so the CLI can tell the human a fresh
// memory was just seeded. No `wd init` gate: the file springs into being the first
// time anyone asks for it.
func (s *Store) Resolve(ctx context.Context, dir string) (path string, created bool, err error) {
	path, err = s.Locate(ctx, dir)
	if err != nil {
		return "", false, err
	}
	if _, statErr := os.Stat(path); statErr == nil {
		return path, false, nil
	} else if !os.IsNotExist(statErr) {
		return "", false, fmt.Errorf("stat %q: %w", path, statErr)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", false, fmt.Errorf("create .warden dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(headerTemplate), 0o644); err != nil {
		return "", false, fmt.Errorf("seed %q: %w", path, err)
	}
	return path, true, nil
}

// Load resolves (auto-creating on first use) and parses the repo's memory into a
// typed Memory. created mirrors Resolve. The returned Memory is empty-but-valid for
// a freshly seeded file (header-only preamble, no entries).
func (s *Store) Load(ctx context.Context, dir string) (m *Memory, path string, created bool, err error) {
	path, created, err = s.Resolve(ctx, dir)
	if err != nil {
		return nil, "", false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", false, fmt.Errorf("read %q: %w", path, err)
	}
	return Parse(string(raw)), path, created, nil
}
