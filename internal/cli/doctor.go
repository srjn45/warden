package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/agentstore"
	"github.com/srjn45/warden/internal/backendstore"
	"github.com/srjn45/warden/internal/client"
	"github.com/srjn45/warden/internal/config"
	"github.com/srjn45/warden/internal/daemon"
	"github.com/srjn45/warden/internal/pipeline"
	"github.com/srjn45/warden/internal/planstore"
	"github.com/srjn45/warden/internal/projectstore"
	"github.com/srjn45/warden/internal/store"
)

// doctorVersion is the reported warden version. There is no build-stamped
// version var for the binary yet, so this stays "dev" until one exists.
const doctorVersion = "dev"

// External tools warden shells out to. Required ones must resolve on PATH;
// optional ones are warn-only (gh is only used for some convenience flows).
var (
	requiredBinaries = []string{"tmux", "git", "claude"}
	optionalBinaries = []string{"gh"}
)

// checkResult is the outcome of one preflight check.
type checkResult struct {
	name     string
	ok       bool
	required bool
	detail   string
}

// checkBinary resolves name on PATH. look is injected for testability
// (exec.LookPath in production).
func checkBinary(name string, required bool, look func(string) (string, error)) checkResult {
	path, err := look(name)
	if err != nil {
		return checkResult{name: name, ok: false, required: required, detail: "not found on PATH"}
	}
	return checkResult{name: name, ok: true, required: required, detail: path}
}

// checkBinaries checks every required and optional binary.
func checkBinaries(look func(string) (string, error)) []checkResult {
	out := make([]checkResult, 0, len(requiredBinaries)+len(optionalBinaries))
	for _, b := range requiredBinaries {
		out = append(out, checkBinary(b, true, look))
	}
	for _, b := range optionalBinaries {
		out = append(out, checkBinary(b, false, look))
	}
	return out
}

// checkDaemon probes <base>/healthz. get is injected for testability (the
// production caller passes a short-timeout http.Client's Get).
func checkDaemon(base string, get func(string) (*http.Response, error)) checkResult {
	url := base + "/healthz"
	resp, err := get(url)
	if err != nil {
		return checkResult{name: "daemon", ok: false, required: true, detail: fmt.Sprintf("unreachable at %s (%v)", base, err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return checkResult{name: "daemon", ok: false, required: true, detail: fmt.Sprintf("%s returned %d", url, resp.StatusCode)}
	}
	return checkResult{name: "daemon", ok: true, required: true, detail: "reachable at " + base}
}

// checkDataDir verifies dir exists, is a directory, and is writable (proven by
// writing and removing a probe file).
func checkDataDir(dir string) checkResult {
	info, err := os.Stat(dir)
	if err != nil {
		return checkResult{name: "data dir", ok: false, required: true, detail: fmt.Sprintf("%s does not exist (%v)", dir, err)}
	}
	if !info.IsDir() {
		return checkResult{name: "data dir", ok: false, required: true, detail: dir + " is not a directory"}
	}
	probe := filepath.Join(dir, ".doctor-write-test")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return checkResult{name: "data dir", ok: false, required: true, detail: fmt.Sprintf("%s not writable (%v)", dir, err)}
	}
	_ = os.Remove(probe)
	return checkResult{name: "data dir", ok: true, required: true, detail: dir + " (writable)"}
}

// checkAgentStore reports agent-store health. With the daemon up it asks the
// daemon (the only process allowed to open the store); with the daemon down it
// only probes ownership, never opening or mutating the store. It is optional:
// a degraded store is loud in the report but does not fail the preflight, since
// running agents are unaffected and repair is a separate offline procedure.
func checkAgentStore(ctx context.Context, base, dataDir string) checkResult {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	h, err := client.New(base).StoreHealth(cctx)
	switch {
	case err == nil && h.Healthy:
		return checkResult{name: "agent store", ok: true, detail: "healthy (read complete)"}
	case err == nil:
		repair := "automated repair not yet available"
		if h.RepairAvailable {
			repair = "repair available: stop the daemon, then run `warden repair agents`"
		}
		return checkResult{name: "agent store", ok: false, detail: fmt.Sprintf("DEGRADED: %d failure(s); %s; %s", h.FailureCount, repair, h.NextStep)}
	case errors.Is(err, client.ErrDaemonDown):
		if _, serr := os.Stat(dataDir); serr != nil {
			return checkResult{name: "agent store", ok: true, detail: "daemon offline; no data dir yet"}
		}
		if oerr := agentstore.ProbeOwnership(dataDir); oerr != nil {
			var oe *agentstore.OwnershipError
			if errors.As(oerr, &oe) {
				return checkResult{name: "agent store", ok: false, detail: "owned by another warden process although the daemon is not reachable at " + base + "; " + oe.NextStep()}
			}
			return checkResult{name: "agent store", ok: false, detail: "ownership probe failed: " + oerr.Error()}
		}
		return checkResult{name: "agent store", ok: true, detail: "daemon offline; store not owned (safe for offline tools)"}
	default:
		return checkResult{name: "agent store", ok: false, detail: "health unavailable: " + err.Error()}
	}
}

// checkBackendRegistry reports backend-registry integrity (#841) from a
// read-only verification: it never opens, locks or changes the registry, so it
// is safe beside a running daemon. Optional, like the agent-store line: a
// finding is loud but does not fail the preflight; the fix is the offline
// `warden repair backends`.
func checkBackendRegistry(ctx context.Context, dataDir string) checkResult {
	const name = "backend registry"
	dir := filepath.Join(dataDir, "backends")
	if _, err := os.Stat(dir); err != nil {
		return checkResult{name: name, ok: true, detail: "no registry yet (created on first daemon start)"}
	}
	rep, err := backendstore.Verify(ctx, dir)
	if err != nil {
		return checkResult{name: name, ok: false, detail: fmt.Sprintf("verification failed: %v; stop the daemon, then run `%s`", err, backendstore.RepairDryRunCommand)}
	}
	var names []string
	for _, c := range rep.Collections {
		if c.Verdict != backendstore.VerdictClean {
			names = append(names, c.Name+" ("+string(c.Verdict)+")")
		}
	}
	switch {
	case rep.Clean():
		return checkResult{name: name, ok: true, detail: "clean (verify with `" + backendstore.RepairDryRunCommand + "`)"}
	case rep.Recoverable():
		return checkResult{name: name, ok: false, detail: fmt.Sprintf("safely recoverable findings in %s; the daemon repairs them backup-first on its next start, or stop the daemon and run `%s`",
			strings.Join(names, ", "), backendstore.RepairCommand)}
	}
	return checkResult{name: name, ok: false, detail: fmt.Sprintf("RECOVERY REQUIRED in %s; a newer daemon will refuse to start. Stop the daemon, then run `%s` (read-only) and `%s`",
		strings.Join(names, ", "), backendstore.RepairDryRunCommand, backendstore.RepairCommand)}
}

// allRequiredPass reports whether every required check passed (optional
// failures are tolerated).
func allRequiredPass(results []checkResult) bool {
	for _, r := range results {
		if r.required && !r.ok {
			return false
		}
	}
	return true
}

// formatReport renders a human-readable pass/fail report.
func formatReport(version string, results []checkResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "warden doctor (version %s)\n\n", version)
	for _, r := range results {
		mark := "ok  "
		switch {
		case r.ok:
			mark = "ok  "
		case r.required:
			mark = "FAIL"
		default:
			mark = "warn"
		}
		fmt.Fprintf(&b, "  [%s] %-9s %s\n", mark, r.name, r.detail)
	}
	b.WriteString("\n")
	if allRequiredPass(results) {
		b.WriteString("all required checks passed\n")
	} else {
		b.WriteString("one or more required checks FAILED\n")
	}
	return b.String()
}

func newDoctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run preflight checks (required binaries, daemon, data dir, agent store, backend registry integrity)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.Load(configPathFor(cmd))
			if reconcile, _ := cmd.Flags().GetBool("reconcile-membership"); reconcile {
				return runMembershipReconcile(cmd, cfg.DataDir)
			}
			sessions, _ := cmd.Flags().GetBool("sessions")
			if sessions {
				var report *store.RecoveryReport
				err := store.WithOfflineSessionStore(cfg.DataDir, func() error {
					var err error
					report, err = store.DiagnoseSessions(cmd.Context(), cfg.DataDir)
					return err
				})
				if err != nil {
					return fmt.Errorf("session store must be offline: %w", err)
				}
				enrichSessionReconciliation(report)
				return printRecoveryReport(cmd.OutOrStdout(), report, false, true)
			}
			if a, _ := cmd.Flags().GetString("addr"); a != "" {
				cfg.Addr = a
			}

			httpGet := func(url string) (*http.Response, error) {
				return (&http.Client{Timeout: 3 * time.Second}).Get(url)
			}

			results := checkBinaries(exec.LookPath)
			results = append(results, checkDaemon("http://"+cfg.Addr, httpGet))
			results = append(results, checkDataDir(cfg.DataDir))
			results = append(results, checkAgentStore(cmd.Context(), "http://"+cfg.Addr, cfg.DataDir))
			results = append(results, checkBackendRegistry(cmd.Context(), cfg.DataDir))

			fmt.Fprint(cmd.OutOrStdout(), formatReport(doctorVersion, results))
			if !allRequiredPass(results) {
				return fmt.Errorf("doctor: one or more required checks failed")
			}
			return nil
		},
	}
	cmd.Flags().Bool("sessions", false, "diagnose the session store offline without modifying it")
	cmd.Flags().Bool("reconcile-membership", false, "backfill missing project_id and rebuild project membership lists offline (daemon must be stopped)")
	return cmd
}

// runMembershipReconcile is the `warden doctor --reconcile-membership` one-shot: an
// offline backfill/repair of the project↔member edges
// (docs/specs/2026-09-25-project-entity-hierarchy.md D2/§6). It stamps a project_id
// onto any pre-back-ref session/pipeline by path-matching the open projects, then
// rebuilds every project's authoritative agents[]/pipelines[]/plans[] lists
// from those back-refs (Plans from the plan store). Autopilots[] is never inferred.
// It must run with the daemon stopped: each on-disk store
// takes an exclusive writer lock, so opening the session store while the daemon is
// up fails fast (ErrStoreOwned) rather than racing writes. Idempotent — safe to
// re-run; a fully-consistent store reports no changes. The daemon runs the same
// reconcile automatically at boot.
func runMembershipReconcile(cmd *cobra.Command, dataDir string) error {
	sstore, err := agentstore.New(dataDir)
	if err != nil {
		var oe *agentstore.OwnershipError
		if errors.As(err, &oe) {
			return fmt.Errorf("%w\nnext step: %s", err, oe.NextStep())
		}
		return fmt.Errorf("open agent store: %w", err)
	}
	defer sstore.Close()

	pstore, err := pipeline.NewStore(filepath.Join(dataDir, "pipelines"))
	if err != nil {
		return fmt.Errorf("open pipeline store: %w", err)
	}
	defer pstore.Close()

	plans, err := planstore.New(filepath.Join(dataDir, "plans"))
	if err != nil {
		return fmt.Errorf("open plan store: %w", err)
	}
	defer plans.Close()

	projects, err := projectstore.NewStore(filepath.Join(dataDir, "projects"))
	if err != nil {
		return fmt.Errorf("open project store: %w", err)
	}
	defer projects.Close()

	rep, err := daemon.ReconcileProjectMembership(cmd.Context(), sstore, pstore, plans, projects)
	if err != nil {
		return fmt.Errorf("reconcile project membership: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "project membership reconcile complete")
	fmt.Fprintf(out, "  sessions stamped:  %d\n", rep.SessionsStamped)
	fmt.Fprintf(out, "  pipelines stamped: %d\n", rep.PipelinesStamped)
	fmt.Fprintf(out, "  projects rebuilt:  %d\n", rep.ProjectsRebuilt)
	for _, c := range rep.Conflicts {
		fmt.Fprintf(out, "  CONFLICT %s %s: %s (left untouched)\n", c.Kind, c.ID, c.Detail)
	}
	if !rep.Changed() {
		fmt.Fprintln(out, "already consistent — no changes")
	}
	return nil
}
