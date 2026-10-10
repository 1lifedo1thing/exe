//go:build !windows

package server

import (
	"os"
	"os/exec"
	"syscall"
)

// restartCommand is the daemon that takes over: this binary again.
func restartCommand(exePath string, args []string) *exec.Cmd {
	return exec.Command(exePath, args...)
}

// restartSysProcAttr detaches the handed-over daemon from this process's
// session so it survives the terminal that started the old one.
func restartSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// termSelf sends this process the SIGTERM the service manager would, so the
// daemon's own shutdown path runs (drain, stop and record the VMs, exit 0).
func termSelf() bool {
	return syscall.Kill(os.Getpid(), syscall.SIGTERM) == nil
}

// startHandover starts the daemon that takes over.
func startHandover(cmd *exec.Cmd) error { return cmd.Start() }
