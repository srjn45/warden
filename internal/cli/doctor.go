package cli

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/srjn45/warden/internal/config"
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
	return &cobra.Command{
		Use:   "doctor",
		Short: "Run preflight checks (required binaries, daemon, data dir)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := config.Load()
			if a, _ := cmd.Flags().GetString("addr"); a != "" {
				cfg.Addr = a
			}

			httpGet := func(url string) (*http.Response, error) {
				return (&http.Client{Timeout: 3 * time.Second}).Get(url)
			}

			results := checkBinaries(exec.LookPath)
			results = append(results, checkDaemon("http://"+cfg.Addr, httpGet))
			results = append(results, checkDataDir(cfg.DataDir))

			fmt.Fprint(cmd.OutOrStdout(), formatReport(doctorVersion, results))
			if !allRequiredPass(results) {
				return fmt.Errorf("doctor: one or more required checks failed")
			}
			return nil
		},
	}
}
