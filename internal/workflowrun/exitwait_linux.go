package workflowrun

import (
	"errors"

	"golang.org/x/sys/unix"
)

// waitExit blocks until the child pid has exited, without reaping it.
func waitExit(pid int) {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return
		}
	}
}
