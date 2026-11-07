//go:build windows

package peer

import (
	"os"
	"syscall"
)

// openForSend opens a file to send to a peer, shared for delete too: a
// transfer can keep it open for minutes, and Go's os.Open would refuse the
// rename that replaces it meanwhile (a desk upload, a newer synced version).
// It falls back to os.Open for what CreateFile cannot take, a long path.
func openForSend(p string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return os.Open(p)
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return os.Open(p)
	}
	return os.NewFile(uintptr(h), p), nil
}
