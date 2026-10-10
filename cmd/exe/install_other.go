//go:build !linux && !darwin

package main

import "errors"

// The installer, `exe update` and `exe uninstall` belong to the Linux and
// macOS releases. On Windows exe is built from a checkout.
var errLinuxRelease = errors.New("this command belongs to the Linux and macOS releases of exe; here, build from source: https://exe.v2core.com/docs/getting-started")

func cmdSetup([]string) error     { return errLinuxRelease }
func cmdUpdate([]string) error    { return errLinuxRelease }
func cmdUninstall([]string) error { return errLinuxRelease }
