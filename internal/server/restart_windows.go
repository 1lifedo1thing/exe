//go:build windows

package server

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// restartCommand is the daemon that takes over: this binary again.
func restartCommand(exePath string, args []string) *exec.Cmd {
	return exec.Command(exePath, args...)
}

// restartSysProcAttr gives the handed-over daemon a console of its own
// that nobody sees, and its own process group, so a closing terminal
// window (or Ctrl+C in it) cannot take the new daemon down.
//
// It used to get no console at all (DETACHED_PROCESS), which hides it just
// as well — but Windows tells a program that its user is signing out, or
// the PC shutting down, through its console. Without one the daemon is
// simply ended: no clean stop, and no record of which VMs were running, so
// none came back at the next sign-in. The installer starts the daemon the
// same way (winHost.Start in cmd/exe).
func restartSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NEW_CONSOLE | windows.CREATE_NEW_PROCESS_GROUP,
	}
}

// termSelf: no service manager restarts the daemon on Windows; the
// spawn-and-exit handover is the only restart there.
func termSelf() bool { return false }

// startHandover starts the daemon that takes over, outside whatever job
// this one is in (a daemon that a scheduled task started is in the task's
// job). A job that does not allow leaving refuses the flag; the handover
// is then started inside it, as before.
func startHandover(cmd *exec.Cmd) error {
	attr := *cmd.SysProcAttr
	attr.CreationFlags |= windows.CREATE_BREAKAWAY_FROM_JOB
	out := *cmd
	out.SysProcAttr = &attr
	if err := out.Start(); err == nil {
		cmd.Process = out.Process
		return nil
	}
	return cmd.Start()
}
