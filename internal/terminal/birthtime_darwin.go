//go:build darwin

package terminal

import (
	"os"
	"syscall"
	"time"
)

// fileCreatedAt returns a file's creation (birth) time on macOS. Birth time is
// stable for the life of the file, unlike mtime which an active Claude session
// bumps on every write — so it correctly identifies when a transcript (and thus
// its session) was launched.
func fileCreatedAt(info os.FileInfo) time.Time {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Birthtimespec.Sec, st.Birthtimespec.Nsec)
	}
	return info.ModTime()
}
