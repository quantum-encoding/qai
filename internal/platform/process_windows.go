//go:build windows

package platform

import (
	"os"
	"os/exec"
)

func DetachProcess(cmd *exec.Cmd) {}

func SignalProcess(proc *os.Process, sig string) error {
	return proc.Kill()
}
