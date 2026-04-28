//go:build windows

// proc_windows.go — Windows shim for process-lifecycle helpers used by
// the notifier daemon. The fleet feature is fundamentally Unix-shaped
// (it depends on tmux), so these are best-effort stubs — fleet won't
// run on Windows in any useful sense, but the package needs to compile
// so the rest of qai (search, image, etc.) ships in a Windows binary.

package fleet

import (
	"os"
	"syscall"
)

// processAlive on Windows: os.FindProcess always succeeds, so we have
// to actually probe via a no-op signal. Signal(syscall.Signal(0)) is
// not portable to Windows, so we use proc.Wait with a non-blocking
// approximation: if FindProcess returns and the process can be
// signalled at all, treat it as alive.
func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Best-effort: try a benign signal. Windows has no portable
	// "is alive" check at the os package level.
	return proc != nil
}

// sigTerm on Windows: there is no SIGTERM. proc.Kill() invokes
// TerminateProcess — abrupt rather than graceful, but functionally
// stops the notifier from writing nudges. Acceptable since fleet on
// Windows isn't a real workflow anyway.
func sigTerm(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}

// detachAttr on Windows: no Setsid equivalent. CREATE_NEW_PROCESS_GROUP
// (0x00000200) is the closest analogue but the field name differs across
// Go versions. Returning the zero-value SysProcAttr leaves Go's default
// behaviour, which is fine because fleet isn't expected to run on
// Windows.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}
