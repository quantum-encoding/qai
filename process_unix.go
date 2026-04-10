//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// detachProcess sets process group so child doesn't die with parent.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalProcess sends a signal to a process.
func signalProcess(proc *os.Process, sig string) error {
	switch sig {
	case "term":
		return proc.Signal(syscall.SIGTERM)
	case "kill":
		return proc.Signal(syscall.SIGKILL)
	default:
		return proc.Signal(syscall.SIGTERM)
	}
}
