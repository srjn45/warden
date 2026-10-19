package stateclassifier

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// OSProcessInspector inspects real operating system process trees.
type OSProcessInspector struct {
	// Run executes a command (e.g. ps); nil defaults to exec.CommandContext.
	Run func(ctx context.Context, dir, name string, args ...string) (string, error)
}

// NewOSProcessInspector builds a default OSProcessInspector.
func NewOSProcessInspector() *OSProcessInspector {
	return &OSProcessInspector{
		Run: func(ctx context.Context, dir, name string, args ...string) (string, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			return string(out), err
		},
	}
}

// Inspect gathers liveness and child process info for rootPID.
func (p *OSProcessInspector) Inspect(ctx context.Context, rootPID int) (ProcessInfo, error) {
	if rootPID <= 0 {
		return ProcessInfo{RootAlive: false}, nil
	}
	// Check root liveness via signal 0
	alive := isProcessAlive(rootPID)
	if !alive {
		return ProcessInfo{RootAlive: false}, nil
	}

	// Read ps table to discover child processes
	out, err := p.Run(ctx, "", "ps", "-axo", "pid=,ppid=,comm=")
	if err != nil {
		// Degrade gracefully: root is alive, children unknown
		return ProcessInfo{RootAlive: true}, nil
	}

	children, comms := parsePSHierarchy(out)
	var (
		descendants []int
		visited     = make(map[int]bool)
	)
	var walk func(pid int)
	walk = func(pid int) {
		for _, child := range children[pid] {
			if !visited[child] {
				visited[child] = true
				descendants = append(descendants, child)
				walk(child)
			}
		}
	}
	walk(rootPID)

	var activeCommands []string
	for _, d := range descendants {
		if c, ok := comms[d]; ok {
			activeCommands = append(activeCommands, c)
		}
	}

	return ProcessInfo{
		RootAlive:         true,
		HasActiveChildren: len(descendants) > 0,
		ChildCount:        len(descendants),
		ChildCommands:     activeCommands,
	}, nil
}

func isProcessAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil
}

func parsePSHierarchy(raw string) (map[int][]int, map[int]string) {
	children := make(map[int][]int)
	comms := make(map[int]string)
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		comm := strings.Join(f[2:], " ")
		children[ppid] = append(children[ppid], pid)
		comms[pid] = comm
	}
	return children, comms
}
