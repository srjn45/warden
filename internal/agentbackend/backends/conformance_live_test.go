// Optional live smoke test for backend CLIs.
//
// The fixture conformance suite (conformance_test.go) is the regression net; it
// runs everywhere with zero external dependencies. THIS file is the complementary
// cheap liveness check: for a backend whose real CLI happens to be installed on
// this machine, it invokes the binary briefly and asserts it actually responds —
// catching the case where a CLI has been upgraded (or removed) locally before the
// fixtures have been refreshed to match.
//
// It is OPT-IN and never runs in the default `go test ./...`:
//   - Gated behind WARDEN_LIVE_BACKEND_TESTS=1. Without it the whole file is a
//     single t.Skip, so CI (which has none of these CLIs installed) stays green.
//   - Even when enabled, each backend whose binary is absent from PATH is skipped
//     cleanly (t.Skip), so it works with whatever subset you happen to have.
//
// Run it locally with, e.g.:
//
//	WARDEN_LIVE_BACKEND_TESTS=1 go test ./internal/agentbackend/backends/ -run TestBackendLiveSmoke -v
//
// This is deliberately a SHALLOW liveness probe (does the installed binary run
// and print something for --help), not a full pane capture — standing up a real
// tmux session per backend is the job of the manual capture workflow that feeds
// the fixtures. If a live CLI's TUI markers have moved, the fix is to recapture
// its pane into testdata/ (see testdata/README.md); this test only tells you the
// binary is present and invocable so that recapture is even possible.
package backends

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/srjn45/warden/internal/agentbackend"
	"github.com/stretchr/testify/require"
)

// liveSmokeEnv, when set to "1", opts this machine into the live backend smoke
// test. It stays off in CI and default local runs.
const liveSmokeEnv = "WARDEN_LIVE_BACKEND_TESTS"

// liveSmokeArg is the flag used to poke each backend's binary for a quick,
// side-effect-free liveness response. Defaults to --help (universally supported
// by these CLIs and fast to exit); override per backend when --help is unsuitable.
var liveSmokeArg = map[string]string{
	// All current backends respond to --help; the map lets a future backend that
	// needs a different probe (e.g. --version) override without touching the loop.
}

// TestBackendLiveSmoke invokes every registered backend's real binary (when
// present on PATH) and asserts it responds. Skipped entirely unless
// WARDEN_LIVE_BACKEND_TESTS=1, and per-backend when the binary is not installed.
func TestBackendLiveSmoke(t *testing.T) {
	if os.Getenv(liveSmokeEnv) != "1" {
		t.Skipf("live backend smoke test is opt-in; set %s=1 to run it", liveSmokeEnv)
	}

	for _, id := range agentbackend.IDs() {
		id := id
		t.Run(id, func(t *testing.T) {
			b, err := agentbackend.Get(id)
			require.NoError(t, err)

			bin := b.Binary()
			path, err := exec.LookPath(bin)
			if err != nil {
				t.Skipf("%s binary %q not on PATH; install it (%s) to smoke-test this backend",
					id, bin, b.InstallHint())
			}

			arg := liveSmokeArg[id]
			if arg == "" {
				arg = "--help"
			}

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			// Combined output; --help exits fast and prints usage. We don't assert
			// exit code (some CLIs exit non-zero on --help), only that the installed
			// binary ran and produced output before the timeout.
			out, runErr := exec.CommandContext(ctx, path, arg).CombinedOutput()
			require.NotErrorIsf(t, ctx.Err(), context.DeadlineExceeded,
				"%s %s did not respond within the timeout — the installed CLI may hang on %s or have changed its interface",
				bin, arg, arg)
			require.NotEmptyf(t, out,
				"%s %s produced no output (runErr=%v) — the installed CLI may have changed its interface; recapture fixtures if its TUI moved (see testdata/README.md)",
				bin, arg, runErr)

			t.Logf("%s: %s %s responded (%d bytes)", id, bin, arg, len(out))
		})
	}
}
