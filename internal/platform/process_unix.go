//go:build !windows

package platform

import (
	"os"
	"os/exec"
	"syscall"
)

func DetachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func SignalProcess(proc *os.Process, sig string) error {
	switch sig {
	case "kill":
		return proc.Signal(syscall.SIGKILL)
	default:
		return proc.Signal(syscall.SIGTERM)
	}
}
