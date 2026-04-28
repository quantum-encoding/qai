//go:build !windows

// proc_unix.go — process-lifecycle helpers used by the notifier daemon.
// Unix-only counterpart to proc_windows.go. Splits out the syscall-bound
// bits so the rest of the fleet package stays portable.

package fleet

import "syscall"

// processAlive reports whether `pid` is a live process owned by the
// current uid (or signalable from us). Unix uses signal 0 — a no-op
// signal that returns nil if the target exists and ESRCH if it doesn't.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// sigTerm asks the process to exit cleanly. The notifier handles SIGTERM
// by removing its pidfile and returning from RunNotifier.
func sigTerm(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// detachAttr returns the SysProcAttr that puts a child into its own
// session, surviving when the parent (`qai fleet up`) returns. Windows
// has no Setsid; the windows variant is a no-op.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
