package ptyholder

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// StartTime is a process's start time in kernel ticks since boot: together with
// the pid it identifies one process incarnation (a recycled pid has a different
// start time). It returns an error when the process does not exist.
func StartTime(pid int) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// The command name may contain spaces and parens: parse after the LAST ')'.
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 {
		return 0, fmt.Errorf("ptyholder: malformed /proc/%d/stat", pid)
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 { // starttime is overall field 22 = index 19 after "pid (comm)"
		return 0, fmt.Errorf("ptyholder: short /proc/%d/stat", pid)
	}
	return strconv.ParseUint(f[19], 10, 64)
}

// peer returns the credentials of the process on the other end of fd.
func peer(fd int) (uid uint32, pid int, err error) {
	c, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return 0, 0, err
	}
	return c.Uid, int(c.Pid), nil
}
