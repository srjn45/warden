package ptyholder

import (
	"golang.org/x/sys/unix"
)

// StartTime is a process's start time (microseconds since the epoch): together
// with the pid it identifies one process incarnation.
func StartTime(pid int) (uint64, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	t := kp.Proc.P_starttime
	return uint64(t.Sec)*1_000_000 + uint64(t.Usec), nil
}

// peer returns the credentials of the process on the other end of fd.
func peer(fd int) (uid uint32, pid int, err error) {
	x, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, 0, err
	}
	pid, err = unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	if err != nil {
		return 0, 0, err
	}
	return x.Uid, pid, nil
}
