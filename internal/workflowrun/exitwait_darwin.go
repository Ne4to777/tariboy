package workflowrun

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

// waitExit blocks until the child pid has exited, without reaping it: kqueue
// reports NOTE_EXIT and leaves the child for wait.
func waitExit(pid int) {
	kq, err := unix.Kqueue()
	if err != nil {
		pollExit(pid)
		return
	}
	defer unix.Close(kq)
	change := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC,
		Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil); err != nil {
		// ESRCH: the child has already exited and waits to be reaped.
		if !errors.Is(err, unix.ESRCH) {
			pollExit(pid)
		}
		return
	}
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(kq, nil, events, nil)
		if n > 0 || (err != nil && !errors.Is(err, unix.EINTR)) {
			return
		}
	}
}

// sZomb is SZOMB of <sys/proc.h>: a process that exited and waits to be reaped.
const sZomb = 5

// pollExit waits for the child to become a zombie, for when kqueue fails.
func pollExit(pid int) {
	for {
		info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil || info.Proc.P_pid != int32(pid) || info.Proc.P_stat == sZomb {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
