//go:build windows

package main

import (
	"os"
	"os/exec"
)

// detachProcess is a no-op on Windows — Setpgid isn't available.
func detachProcess(cmd *exec.Cmd) {
	// Windows processes are already detached from the terminal by default
	// when started without a console handle.
}

// signalProcess kills a process on Windows. Windows doesn't have SIGTERM,
// so both "term" and "kill" do a hard kill.
func signalProcess(proc *os.Process, sig string) error {
	return proc.Kill()
}
