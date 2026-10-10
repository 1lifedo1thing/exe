//go:build linux || darwin

package main

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"

	"exe/internal/release"
)

// helperState describes a network helper on disk (Linux): its checksum,
// and whether it is root's and carries a capability.
func helperState(path string) (sum string, rooted bool) {
	st, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	sum, _ = release.FileSum(path)
	sys, ok := st.Sys().(*syscall.Stat_t)
	n, _ := unix.Getxattr(path, "security.capability", make([]byte, 64))
	return sum, ok && sys.Uid == 0 && n > 0
}

// newWinHost: there is no Windows to ask off Windows.
func newWinHost() *winHost { return nil }
