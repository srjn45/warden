//go:build !unix

package lifecycle

import "os/exec"

// ownProcessGroup is a no-op where process groups are unavailable; exec's
// default cancellation kills the leader process.
func ownProcessGroup(*exec.Cmd) {}
