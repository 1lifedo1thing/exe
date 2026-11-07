//go:build !windows

package peer

import "os"

// openForSend opens a file to send to a peer; a rename can replace it
// while it is open (open_windows.go has the one system that needs asking).
func openForSend(p string) (*os.File, error) { return os.Open(p) }
