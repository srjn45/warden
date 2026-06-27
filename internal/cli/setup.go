package cli

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// setup is intentionally CLI-only and is NEVER routed through the daemon/MCP.
// It installs host packages — often via `sudo` and piped install scripts — which
// is a host-administration action that is meaningless on a remote/MCP surface and
// would be a security smell to expose there. This is a deliberate exception to
// warden's usual MCP/CLI-parity rule, in the same spirit as doctor/repl/tutorial.
// Do NOT add an MCP tool for setup.

// setupDep is one dependency `setup` can install.
type setupDep struct {
	name     string
	required bool
}

// setupDeps is the ordered list `setup` offers: required first, then optional.
// It mirrors doctor's required/optional binary lists exactly (so setup and
// doctor never drift), and like doctor's optional set it includes gh and ollama.
func setupDeps() []setupDep {
	deps := make([]setupDep, 0, len(requiredBinaries)+len(optionalBinaries))
	for _, b := range requiredBinaries {
		deps = append(deps, setupDep{name: b, required: true})
	}
	for _, b := range optionalBinaries {
		deps = append(deps, setupDep{name: b, required: false})
	}
	return deps
}

// pkgManager identifies the host package manager setup drives.
type pkgManager string

const (
	pmBrew   pkgManager = "brew"
	pmApt    pkgManager = "apt"
	pmDnf    pkgManager = "dnf"
	pmPacman pkgManager = "pacman"
	pmNone   pkgManager = ""
)

// detectPkgManager picks the package manager for goos. macOS is always
// Homebrew; the bool reports whether brew itself is installed (we never
// auto-bootstrap brew). Linux probes apt, dnf, then pacman via look in that
// preference order; the bool reports whether any was found.
func detectPkgManager(goos string, look func(string) (string, error)) (pkgManager, bool) {
	if goos == "darwin" {
		_, err := look("brew")
		return pmBrew, err == nil
	}
	for _, pm := range []pkgManager{pmApt, pmDnf, pmPacman} {
		if _, err := look(string(pm)); err == nil {
			return pm, true
		}
	}
	return pmNone, false
}

// pkgName maps a dep to its package name for a manager. Most names are identical
// across managers; gh ships as github-cli on Arch.
func pkgName(dep string, pm pkgManager) string {
	if dep == "gh" && pm == pmPacman {
		return "github-cli"
	}
	return dep
}

// setupAction is the resolved plan for one dep: a shell command to run, or
// (when cmd == "") a print-only fallback whose reason/instruction is in note.
type setupAction struct {
	dep  setupDep
	cmd  string // shell command to run; "" means print-only (manual)
	note string // shown to the user; the manual instruction when cmd == ""
}

// resolveAction computes how to install dep on goos given the detected package
// manager. brewPresent is only meaningful on macOS.
func resolveAction(dep setupDep, goos string, pm pkgManager, brewPresent bool) setupAction {
	switch dep.name {
	case "claude":
		// Claude Code is not a brew/apt/dnf/pacman package; use the official
		// native installer (works on macOS, Linux, and WSL). npm fallback if the
		// native installer fails: `npm install -g @anthropic-ai/claude-code`.
		return setupAction{dep: dep, cmd: "curl -fsSL https://claude.ai/install.sh | bash"}
	case "ollama":
		if goos == "darwin" {
			if brewPresent {
				return setupAction{dep: dep, cmd: "brew install ollama"}
			}
			return setupAction{dep: dep, note: "Homebrew not found; install Ollama from https://ollama.com/download"}
		}
		// Linux (and other non-darwin): official install script.
		return setupAction{dep: dep, cmd: "curl -fsSL https://ollama.com/install.sh | sh"}
	}

	// tmux, git, gh: ordinary package-manager packages.
	if goos == "darwin" {
		if !brewPresent {
			return setupAction{dep: dep, note: "Homebrew not found; install it from https://brew.sh then re-run, or install " + dep.name + " manually"}
		}
		return setupAction{dep: dep, cmd: "brew install " + pkgName(dep.name, pmBrew)}
	}
	switch pm {
	case pmApt:
		return setupAction{dep: dep, cmd: "sudo apt install -y " + pkgName(dep.name, pmApt)}
	case pmDnf:
		return setupAction{dep: dep, cmd: "sudo dnf install -y " + pkgName(dep.name, pmDnf)}
	case pmPacman:
		return setupAction{dep: dep, cmd: "sudo pacman -S --noconfirm " + pkgName(dep.name, pmPacman)}
	default:
		return setupAction{dep: dep, note: "no supported package manager (apt/dnf/pacman) found; install " + dep.name + " manually"}
	}
}

// setupIO is the set of seams setup depends on, injected so tests never touch
// the real host (LookPath, shell, or stdin).
type setupIO struct {
	look    func(string) (string, error) // exec.LookPath in production
	run     func(cmd string) error       // runs a shell install command
	confirm func(prompt string) bool     // y/N prompt; true == proceed
	goos    string                       // runtime.GOOS in production
}

// runSetup verifies the install (reusing doctor's binary checks), installs the
// missing deps with confirm-each (or unconditionally when yes), then re-runs the
// checks and prints a doctor-style report of the final state.
func runSetup(out io.Writer, sio setupIO, yes bool) error {
	// VERIFY FIRST: reuse doctor's check logic to find what is missing.
	results := checkBinaries(sio.look)
	resultByName := make(map[string]checkResult, len(results))
	for _, r := range results {
		resultByName[r.name] = r
	}
	var missing []setupDep
	for _, dep := range setupDeps() {
		if r, ok := resultByName[dep.name]; ok && !r.ok {
			missing = append(missing, dep)
		}
	}

	if len(missing) == 0 {
		fmt.Fprintln(out, "All dependencies are already installed — nothing to do.")
		fmt.Fprint(out, formatReport(doctorVersion, results))
		return nil
	}

	pm, pmPresent := detectPkgManager(sio.goos, sio.look)
	brewPresent := pmPresent && pm == pmBrew // only meaningful on macOS

	fmt.Fprintf(out, "Missing dependencies (%d): %s\n", len(missing), depNames(missing))

	for _, dep := range missing {
		label := "optional"
		if dep.required {
			label = "required"
		}
		action := resolveAction(dep, sio.goos, pm, brewPresent)

		if action.cmd == "" {
			// Print-only fallback: nothing safe to run automatically.
			fmt.Fprintf(out, "\n%s (%s): cannot auto-install\n  %s\n", dep.name, label, action.note)
			continue
		}

		fmt.Fprintf(out, "\n%s (%s) installs with:\n  %s\n", dep.name, label, action.cmd)
		if !yes && !sio.confirm(fmt.Sprintf("Install %s? [y/N]: ", dep.name)) {
			fmt.Fprintf(out, "skipped %s\n", dep.name)
			continue
		}
		fmt.Fprintf(out, "running: %s\n", action.cmd)
		if err := sio.run(action.cmd); err != nil {
			fmt.Fprintf(out, "failed to install %s: %v\n", dep.name, err)
		}
	}

	// AFTER: re-run the checks and print the doctor-style report so the user
	// sees the final dependency state. (Setup owns host binaries only; for the
	// full environment report — daemon, data dir — run `wd doctor`.)
	fmt.Fprintln(out, "\nFinal dependency check:")
	fmt.Fprint(out, formatReport(doctorVersion, checkBinaries(sio.look)))
	return nil
}

// depNames joins dep names for display.
func depNames(deps []setupDep) string {
	names := make([]string, len(deps))
	for i, d := range deps {
		names[i] = d.name
	}
	return strings.Join(names, ", ")
}

func newSetupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Install missing dependencies (tmux, git, claude; optional gh, ollama)",
		Long: "Verify the current install with the same checks as `warden doctor`, then\n" +
			"install whatever is missing. setup is idempotent: it only touches deps that\n" +
			"are not already on PATH.\n\n" +
			"For each missing dependency it prints the exact install command and prompts\n" +
			"before running it (use --yes for non-interactive/automation). Required deps\n" +
			"(tmux, git, claude) are offered first, then optional ones (gh, ollama).\n\n" +
			"Package managers: Homebrew on macOS (never auto-bootstrapped — if brew is\n" +
			"missing setup prints the instruction and skips brew installs), and apt, dnf,\n" +
			"or pacman on Linux (auto-detected). Claude Code and Ollama use their official\n" +
			"installers. After installing, setup re-runs the checks and prints the report.\n\n" +
			"setup is CLI-only by design (it installs host packages) and is not exposed\n" +
			"over MCP or the daemon.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			yes, _ := cmd.Flags().GetBool("yes")
			out := cmd.OutOrStdout()
			reader := bufio.NewReader(cmd.InOrStdin())
			sio := setupIO{
				look: exec.LookPath,
				run: func(c string) error {
					sh := exec.Command("sh", "-c", c)
					sh.Stdout = out
					sh.Stderr = cmd.ErrOrStderr()
					sh.Stdin = cmd.InOrStdin()
					return sh.Run()
				},
				confirm: func(prompt string) bool {
					fmt.Fprint(out, prompt)
					line, _ := reader.ReadString('\n')
					line = strings.TrimSpace(line)
					return line == "y" || line == "Y" || line == "yes"
				},
				goos: runtime.GOOS,
			}
			return runSetup(out, sio, yes)
		},
	}
	cmd.Flags().Bool("yes", false, "install all missing dependencies without prompting (non-interactive)")
	return cmd
}
