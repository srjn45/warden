package daemon

import (
	"fmt"
	"os"
	"testing"

	"github.com/srjn45/warden/internal/ptyholder"
)

// TestMain lets this test binary stand in for the warden binary's hidden
// `ptyholder` subcommand (the holder is a re-exec of the daemon's own binary).
func TestMain(m *testing.M) {
	if len(os.Args) >= 3 && os.Args[1] == "__ptyholder" {
		var ready *os.File
		if os.Getenv("WARDEN_PTYHOLDER_READY_FD") == "3" {
			ready = os.NewFile(3, "ready")
		}
		if err := ptyholder.Run(os.Args[2], ready); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
