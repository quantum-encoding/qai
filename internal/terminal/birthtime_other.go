//go:build !darwin

package terminal

import (
	"os"
	"time"
)

// fileCreatedAt falls back to mtime on platforms without a portable birth-time
// syscall. Session correlation is less precise here, but qai's tmux features
// target macOS in practice.
func fileCreatedAt(info os.FileInfo) time.Time {
	return info.ModTime()
}
