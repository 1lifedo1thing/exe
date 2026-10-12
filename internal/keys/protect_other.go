//go:build !windows

package keys

// Protect keeps a private key to its user. Here the mode a key is written
// with (0600) already does, and stays with the file; Windows has the work
// (protect_windows.go).
func Protect(path string) error { return nil }
