package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/srjn45/warden/internal/projectkey"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

// ProjectRecents is the persistence seam for the Open Project panel's recent-
// projects list (Stage C5). *projectstore.Store satisfies it; tests inject a fake.
// The cockpit is the only holder — the daemon never touches this store — so a
// co-located second cockpit that cannot open it just runs with recents == nil.
type ProjectRecents interface {
	List() ([]projectstore.Recent, error)
	Touch(projectstore.Recent) error
}

// orchestratorRole is the built-in role every project's single orchestrator runs
// as; opening a project auto-spawns one (design §6.2), one per project key.
const orchestratorRole = "orchestrator"

// recentsMsg carries the loaded recent-projects list into the Open Project panel.
type recentsMsg struct {
	list []projectstore.Recent
	err  error
}

// projectOpenMsg is the resolved identity of a project the user chose to open —
// the local-navigated dir, a recent's stored path, or a fresh git clone — after
// its canonical project key (B2) has been resolved off the UI goroutine.
type projectOpenMsg struct {
	rec projectstore.Recent
	err error
}

// cloneDoneMsg reports a git-clone outcome; on success dir is the clone worktree
// under ~/.warden/workspace, which then flows through the normal open path.
type cloneDoneMsg struct {
	dir string
	err error
}

// recentsLoadCmd loads the persisted recent-projects list. A nil store (no
// persistence) yields an empty list rather than an error, so the panel still opens.
func recentsLoadCmd(r ProjectRecents) tea.Cmd {
	return func() tea.Msg {
		if r == nil {
			return recentsMsg{}
		}
		list, err := r.List()
		return recentsMsg{list: list, err: err}
	}
}

// resolveProjectCmd resolves a directory into a project identity (canonical key +
// remote + display name) off the UI goroutine — it runs git locally, exactly as
// the tick's projectKeysCmd does. The cockpit process is always co-located with
// the daemon, so local git resolution matches the daemon's B2 keying.
func resolveProjectCmd(dir string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bg()
		defer cancel()
		rec, err := resolveProject(ctx, dir)
		return projectOpenMsg{rec: rec, err: err}
	}
}

// cloneProjectCmd clones url into ~/.warden/workspace/<project> (disambiguating a
// name collision) and reports the clone dir, which the caller then opens like any
// local project. An existing clone of the same remote is reused (idempotent).
func cloneProjectCmd(url string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := bgLong()
		defer cancel()
		dir, err := cloneProject(ctx, url)
		return cloneDoneMsg{dir: dir, err: err}
	}
}

// handleOpenProjectKey drives the Open Project landing panel: the recent list plus
// the l/g entrances to open-local and open-via-git. Esc returns to the Projects
// view (design §6.2).
func (m controlPaneModel) handleOpenProjectKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		m.mode = modeNormal
		m.status = ""
		return m, nil
	case "down", "j":
		if m.recentCursor < len(m.recentList)-1 {
			m.recentCursor++
		}
		return m, nil
	case "up", "k":
		if m.recentCursor > 0 {
			m.recentCursor--
		}
		return m, nil
	case "l":
		// Open local — reuse the existing dir navigator (path input + tab-complete).
		m.mode = modeOpenProjectLocal
		m.tp.Reset()
		m.tp.Focus()
		m.dirCandidates = nil
		return m, nil
	case "g":
		// Open via git — prompt for a clone URL.
		m.mode = modeOpenProjectGit
		m.tp.Reset()
		m.tp.Focus()
		m.dirCandidates = nil
		return m, nil
	case "enter":
		if m.recentCursor < 0 || m.recentCursor >= len(m.recentList) {
			return m, nil
		}
		rec := m.recentList[m.recentCursor]
		m.mode = modeNormal
		m.status = "opening " + rec.Name + "…"
		return m, resolveProjectCmd(rec.Path)
	}
	return m, nil
}

// openResolvedProject is the shared tail of every open path (recent / local / git):
// it records the project in the recent list and enforces the one-orchestrator-per-
// project invariant (the same invariant B3 enforces for group membership). If a
// live orchestrator already anchors this project key it is focused in the agent
// pane rather than duplicated; otherwise a fresh orchestrator is spawned in the
// project dir and the project node appears immediately via openedDirs.
func (m controlPaneModel) openResolvedProject(msg projectOpenMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.status = "cannot open project: " + msg.err.Error()
		return m, nil
	}
	rec := msg.rec
	// Persist into the recent list (best-effort; a store error never blocks the
	// open) and refresh the in-memory snapshot so a re-open shows it immediately.
	if m.recents != nil {
		if err := m.recents.Touch(rec); err != nil {
			m.status = "recent list not saved: " + err.Error()
		}
	}

	// One orchestrator per project: focus the incumbent instead of spawning a dup.
	if orch := m.orchestratorFor(rec.Key); orch != nil {
		m.openedAgent = orch.ID
		m.openedAgentDir = sourceDir(orch)
		m.pendingSelect = orch.ID
		m.status = "focused orchestrator for " + rec.Name
		m.repin("")
		if m.agentPane != "" && liveStatus(orch.Status) {
			return m, openInDetailCmd(m.agentPane, orch.TmuxSession, false)
		}
		return m, nil
	}

	// No incumbent — spawn the project's single orchestrator in its dir. Show the
	// project node right away (an opened dir) so the operator sees it before the
	// agent lands in the next list refresh.
	m.openedDirs[rec.Path] = time.Now()
	m.pendingSelect = dirKey(rec.Path)
	m.repin("")
	m.status = "spawning orchestrator for " + rec.Name + "…"
	return m, spawnCmd(m.api, "", "", rec.Path, orchestratorRole, "", false)
}

// orchestratorFor returns the live orchestrator anchoring project key, or nil. An
// orchestrator anchors a project when its role is orchestrator and its source
// dir's cached project key matches — the same keying the Projects frame groups by.
func (m controlPaneModel) orchestratorFor(key string) *store.Session {
	if key == "" {
		return nil
	}
	for _, s := range m.sessions {
		if s.IsTerminal() || !liveStatus(s.Status) {
			continue
		}
		if s.Role != orchestratorRole {
			continue
		}
		if m.projectKey(sourceDir(s)) == key {
			return s
		}
	}
	return nil
}

// openProjectBody renders the Open Project panel: the recent list (most-recent
// first) with the focused row marked, plus the two entrances. A sub-mode (local /
// git) keeps the list visible above its input, which renders in the footer.
func (m controlPaneModel) openProjectBody() string {
	var b strings.Builder
	b.WriteString(stPaneTitle.Render("Recent projects"))
	b.WriteString("\n")
	if len(m.recentList) == 0 {
		b.WriteString(stMuted.Render("  (none yet — open one below)"))
	}
	for i, r := range m.recentList {
		marker := "  "
		line := r.Name + stMuted.Render("  "+projectSubtitle(r))
		if i == m.recentCursor && m.mode == modeOpenProject {
			marker = stCursor.Render("› ")
			line = stCursor.Render(r.Name) + stMuted.Render("  "+projectSubtitle(r))
		}
		b.WriteString(marker + line + "\n")
	}
	b.WriteString("\n")
	b.WriteString(stMuted.Render("l  open local (directory navigator)") + "\n")
	b.WriteString(stMuted.Render("g  open via git (clone into ~/.warden/workspace)"))
	return b.String()
}

// projectSubtitle is the muted right-hand detail for a recent row: its git remote
// when it has one, else its local path — enough to disambiguate same-named repos.
func projectSubtitle(r projectstore.Recent) string {
	if r.Remote != "" {
		return r.Remote
	}
	return abbrevHome(r.Path)
}

// resolveProject reads a directory's canonical project identity by running git
// locally (origin remote + repo root), keyed via the B2 normalizer. A remoteless
// or non-git directory resolves to a `local:` key rooted at its path.
func resolveProject(ctx context.Context, dir string) (projectstore.Recent, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return projectstore.Recent{}, err
	}
	if !info.IsDir() {
		return projectstore.Recent{}, fmt.Errorf("%s is not a directory", dir)
	}
	remote := gitOutput(ctx, dir, "remote", "get-url", "origin")
	root := gitOutput(ctx, dir, "rev-parse", "--show-toplevel")
	if root == "" {
		root = dir
	}
	key := projectkey.Key(remote, root)
	return projectstore.Recent{
		Key:    key,
		Name:   projectDisplayName(key, root),
		Remote: remote,
		Path:   root,
	}, nil
}

// projectDisplayName is the human label for a project: the repo leaf of a remote
// key ("github.com/org/repo" → "repo"), or the directory base of a local key.
func projectDisplayName(key, path string) string {
	if key == "" || strings.HasPrefix(key, projectkey.LocalKeyPrefix) {
		return filepath.Base(path)
	}
	if i := strings.LastIndex(key, "/"); i >= 0 && i < len(key)-1 {
		return key[i+1:]
	}
	return key
}

// cloneProject clones url into ~/.warden/workspace/<project>, deriving <project>
// from the repo name and disambiguating a collision with an unrelated repo (design
// §6.2). An existing clone of the same remote is reused rather than re-cloned, so
// re-opening a git project is idempotent.
func cloneProject(ctx context.Context, url string) (string, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return "", fmt.Errorf("empty git URL")
	}
	workspace, err := workspaceDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return "", err
	}
	target, reuse, err := cloneTarget(ctx, workspace, url)
	if err != nil {
		return "", err
	}
	if reuse {
		return target, nil
	}
	out, err := exec.CommandContext(ctx, "git", "clone", url, target).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git clone: %s", strings.TrimSpace(string(out)))
	}
	return target, nil
}

// cloneTarget picks the clone destination under workspace for url. It prefers the
// repo's own name; if that dir is already a clone of the same remote it is reused
// (reuse=true), and if it is a DIFFERENT repo the name is disambiguated to the
// full host-org-repo key (then -2, -3… as a last resort) so two different repos
// with the same leaf name never collide.
func cloneTarget(ctx context.Context, workspace, url string) (dir string, reuse bool, err error) {
	want, _ := projectkey.NormalizeRemoteURL(url)
	candidates := []string{repoNameFromURL(url)}
	if want != "" {
		candidates = append(candidates, strings.ReplaceAll(want, "/", "-"))
	}
	for _, name := range candidates {
		name = sanitizeName(name)
		if name == "" {
			continue
		}
		path := filepath.Join(workspace, name)
		switch existing := existingRemoteKey(ctx, path); {
		case existing == "" && !pathExists(path):
			return path, false, nil // free slot
		case want != "" && existing == want:
			return path, true, nil // already cloned here → reuse
		}
	}
	// Last resort: suffix the leaf name until a free slot is found.
	base := sanitizeName(repoNameFromURL(url))
	if base == "" {
		base = "project"
	}
	for i := 2; i < 1000; i++ {
		path := filepath.Join(workspace, fmt.Sprintf("%s-%d", base, i))
		if !pathExists(path) {
			return path, false, nil
		}
		if want != "" && existingRemoteKey(ctx, path) == want {
			return path, true, nil
		}
	}
	return "", false, fmt.Errorf("could not find a free workspace slot for %s", url)
}

// existingRemoteKey returns the normalized project key of the git repo at path, or
// "" if path is not an existing git repo (or has no origin remote).
func existingRemoteKey(ctx context.Context, path string) string {
	if !pathExists(path) {
		return ""
	}
	key, _ := projectkey.NormalizeRemoteURL(gitOutput(ctx, path, "remote", "get-url", "origin"))
	return key
}

// repoNameFromURL extracts the repository leaf name from a git URL, handling both
// scp-like (git@host:org/repo.git) and URL (https://host/org/repo.git) forms.
func repoNameFromURL(url string) string {
	u := strings.TrimSpace(url)
	u = strings.TrimRight(u, "/")
	u = strings.TrimSuffix(u, ".git")
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		u = u[i+1:]
	}
	return u
}

// sanitizeName keeps a workspace directory name to a safe leaf: no path
// separators, no leading dots, whitespace trimmed.
func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, string(os.PathSeparator), "-")
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.TrimLeft(name, ".")
	return strings.TrimSpace(name)
}

// workspaceDir is ~/.warden/workspace, the home for git-cloned projects.
func workspaceDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".warden", "workspace"), nil
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// gitOutput runs `git -C dir <args...>` and returns trimmed stdout, or "" on any
// error — the same helper shape projectkey uses, kept local so the TUI depends
// only on the leaf normalizer.
func gitOutput(ctx context.Context, dir string, args ...string) string {
	full := append([]string{"-C", dir}, args...)
	out, err := exec.CommandContext(ctx, "git", full...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
