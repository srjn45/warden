package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// lookFunc builds a LookPath stub: every name in present resolves, the rest fail.
func lookFunc(present ...string) func(string) (string, error) {
	set := map[string]bool{}
	for _, p := range present {
		set[p] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
}

// recordRun captures the install commands setup would run.
type recordRun struct {
	cmds []string
	err  error
}

func (r *recordRun) run(cmd string) error {
	r.cmds = append(r.cmds, cmd)
	return r.err
}

func TestSetupNothingMissing(t *testing.T) {
	// Everything (required + optional) resolves → no-op.
	rr := &recordRun{}
	var out bytes.Buffer
	sio := setupIO{
		look:    lookFunc("tmux", "git", "claude", "gh", "ollama"),
		run:     rr.run,
		confirm: func(string) bool { return true },
		goos:    "linux",
	}
	if err := runSetup(&out, sio, false); err != nil {
		t.Fatal(err)
	}
	if len(rr.cmds) != 0 {
		t.Fatalf("nothing should be installed, ran: %v", rr.cmds)
	}
	if !strings.Contains(out.String(), "nothing to do") {
		t.Fatalf("expected no-op message:\n%s", out.String())
	}
}

func TestSetupMissingRequiredConfirmYes(t *testing.T) {
	// claude missing on linux+apt; user confirms → native installer runs.
	rr := &recordRun{}
	var out bytes.Buffer
	sio := setupIO{
		look:    lookFunc("tmux", "git", "gh", "ollama", "apt"),
		run:     rr.run,
		confirm: func(string) bool { return true },
		goos:    "linux",
	}
	if err := runSetup(&out, sio, false); err != nil {
		t.Fatal(err)
	}
	if len(rr.cmds) != 1 || rr.cmds[0] != "curl -fsSL https://claude.ai/install.sh | bash" {
		t.Fatalf("expected claude native installer, ran: %v", rr.cmds)
	}
}

func TestSetupConfirmNoSkips(t *testing.T) {
	// User declines → nothing runs, output says skipped.
	rr := &recordRun{}
	var out bytes.Buffer
	sio := setupIO{
		look:    lookFunc("tmux", "git", "gh", "ollama", "apt"),
		run:     rr.run,
		confirm: func(string) bool { return false },
		goos:    "linux",
	}
	if err := runSetup(&out, sio, false); err != nil {
		t.Fatal(err)
	}
	if len(rr.cmds) != 0 {
		t.Fatalf("declining should run nothing, ran: %v", rr.cmds)
	}
	if !strings.Contains(out.String(), "skipped claude") {
		t.Fatalf("expected skip message:\n%s", out.String())
	}
}

func TestSetupYesSkipsPrompts(t *testing.T) {
	// --yes installs every missing dep without consulting confirm.
	rr := &recordRun{}
	var out bytes.Buffer
	confirmCalled := false
	sio := setupIO{
		look: lookFunc("apt"), // nothing installed
		run:  rr.run,
		confirm: func(string) bool {
			confirmCalled = true
			return false
		},
		goos: "linux",
	}
	if err := runSetup(&out, sio, true); err != nil {
		t.Fatal(err)
	}
	if confirmCalled {
		t.Fatal("--yes must not consult confirm")
	}
	// tmux, git, claude, gh, ollama → 5 install commands.
	if len(rr.cmds) != 5 {
		t.Fatalf("expected 5 installs, ran: %v", rr.cmds)
	}
	joined := strings.Join(rr.cmds, "\n")
	for _, want := range []string{
		"sudo apt install -y tmux",
		"sudo apt install -y git",
		"curl -fsSL https://claude.ai/install.sh | bash",
		"sudo apt install -y gh",
		"curl -fsSL https://ollama.com/install.sh | sh",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
}

func TestSetupInstallFailureIsReported(t *testing.T) {
	rr := &recordRun{err: errors.New("boom")}
	var out bytes.Buffer
	sio := setupIO{
		look:    lookFunc("tmux", "git", "claude", "gh", "apt"), // only ollama missing
		run:     rr.run,
		confirm: func(string) bool { return true },
		goos:    "linux",
	}
	if err := runSetup(&out, sio, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "failed to install ollama") {
		t.Fatalf("expected failure report:\n%s", out.String())
	}
}

func TestDetectPkgManager(t *testing.T) {
	// macOS with brew.
	if pm, ok := detectPkgManager("darwin", lookFunc("brew")); pm != pmBrew || !ok {
		t.Fatalf("darwin+brew: got %v %v", pm, ok)
	}
	// macOS without brew: still brew, but not present.
	if pm, ok := detectPkgManager("darwin", lookFunc()); pm != pmBrew || ok {
		t.Fatalf("darwin no-brew: got %v %v", pm, ok)
	}
	// apt preferred over dnf/pacman when several exist.
	if pm, ok := detectPkgManager("linux", lookFunc("apt", "dnf", "pacman")); pm != pmApt || !ok {
		t.Fatalf("linux apt-preferred: got %v %v", pm, ok)
	}
	if pm, _ := detectPkgManager("linux", lookFunc("dnf", "pacman")); pm != pmDnf {
		t.Fatalf("linux dnf: got %v", pm)
	}
	if pm, _ := detectPkgManager("linux", lookFunc("pacman")); pm != pmPacman {
		t.Fatalf("linux pacman: got %v", pm)
	}
	// none found.
	if pm, ok := detectPkgManager("linux", lookFunc()); pm != pmNone || ok {
		t.Fatalf("linux none: got %v %v", pm, ok)
	}
}

func TestResolveActionPackageManagers(t *testing.T) {
	tmux := setupDep{name: "tmux", required: true}
	cases := []struct {
		name        string
		goos        string
		pm          pkgManager
		brewPresent bool
		want        string
	}{
		{"apt", "linux", pmApt, false, "sudo apt install -y tmux"},
		{"dnf", "linux", pmDnf, false, "sudo dnf install -y tmux"},
		{"pacman", "linux", pmPacman, false, "sudo pacman -S --noconfirm tmux"},
		{"brew", "darwin", pmBrew, true, "brew install tmux"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveAction(tmux, c.goos, c.pm, c.brewPresent)
			if got.cmd != c.want {
				t.Fatalf("got %q want %q", got.cmd, c.want)
			}
		})
	}
}

func TestResolveActionNoPackageManager(t *testing.T) {
	got := resolveAction(setupDep{name: "git"}, "linux", pmNone, false)
	if got.cmd != "" {
		t.Fatalf("no pkg manager should be print-only, got cmd %q", got.cmd)
	}
	if !strings.Contains(got.note, "manually") {
		t.Fatalf("expected manual note, got %q", got.note)
	}
}

func TestResolveActionBrewAbsentOnMac(t *testing.T) {
	got := resolveAction(setupDep{name: "tmux"}, "darwin", pmBrew, false)
	if got.cmd != "" {
		t.Fatalf("brew absent should be print-only, got cmd %q", got.cmd)
	}
	if !strings.Contains(got.note, "brew.sh") {
		t.Fatalf("expected brew bootstrap instruction, got %q", got.note)
	}
}

func TestResolveActionGhPacmanPackageName(t *testing.T) {
	got := resolveAction(setupDep{name: "gh"}, "linux", pmPacman, false)
	if got.cmd != "sudo pacman -S --noconfirm github-cli" {
		t.Fatalf("gh on pacman should map to github-cli, got %q", got.cmd)
	}
	// apt keeps the gh name.
	if got := resolveAction(setupDep{name: "gh"}, "linux", pmApt, false); got.cmd != "sudo apt install -y gh" {
		t.Fatalf("gh on apt should stay gh, got %q", got.cmd)
	}
}

func TestResolveActionClaudeSpecialInstaller(t *testing.T) {
	// claude is never a package — always the native installer, every platform.
	for _, goos := range []string{"linux", "darwin"} {
		got := resolveAction(setupDep{name: "claude", required: true}, goos, pmApt, true)
		if got.cmd != "curl -fsSL https://claude.ai/install.sh | bash" {
			t.Fatalf("%s: claude installer wrong: %q", goos, got.cmd)
		}
	}
}

func TestResolveActionOllamaSpecialInstaller(t *testing.T) {
	// Linux → official script.
	if got := resolveAction(setupDep{name: "ollama"}, "linux", pmApt, false); got.cmd != "curl -fsSL https://ollama.com/install.sh | sh" {
		t.Fatalf("linux ollama installer wrong: %q", got.cmd)
	}
	// macOS with brew → brew install.
	if got := resolveAction(setupDep{name: "ollama"}, "darwin", pmBrew, true); got.cmd != "brew install ollama" {
		t.Fatalf("darwin+brew ollama wrong: %q", got.cmd)
	}
	// macOS without brew → print-only with official download URL.
	got := resolveAction(setupDep{name: "ollama"}, "darwin", pmBrew, false)
	if got.cmd != "" || !strings.Contains(got.note, "ollama.com/download") {
		t.Fatalf("darwin no-brew ollama should be print-only: cmd=%q note=%q", got.cmd, got.note)
	}
}

func TestSetupDepsMirrorsDoctorLists(t *testing.T) {
	deps := setupDeps()
	if len(deps) != len(requiredBinaries)+len(optionalBinaries) {
		t.Fatalf("setupDeps should mirror doctor lists, got %d", len(deps))
	}
	byName := map[string]bool{}
	for _, d := range deps {
		byName[d.name] = d.required
	}
	for _, r := range requiredBinaries {
		if req, ok := byName[r]; !ok || !req {
			t.Fatalf("%q should be a required dep", r)
		}
	}
	for _, o := range optionalBinaries {
		if req, ok := byName[o]; !ok || req {
			t.Fatalf("%q should be an optional dep", o)
		}
	}
}

func TestSetupPrintsFinalReport(t *testing.T) {
	rr := &recordRun{}
	var out bytes.Buffer
	sio := setupIO{
		look:    lookFunc("tmux", "git", "gh", "ollama", "apt"),
		run:     rr.run,
		confirm: func(string) bool { return true },
		goos:    "linux",
	}
	if err := runSetup(&out, sio, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Final dependency check:") {
		t.Fatalf("expected final report header:\n%s", out.String())
	}
}
