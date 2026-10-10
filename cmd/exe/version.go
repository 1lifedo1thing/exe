package main

import (
	"fmt"
	"runtime"

	"exe/internal/release"
)

// versionLine is what `exe version` prints, and what an update reads from
// the binary it is about to install to be sure it is the one it asked for.
func versionLine() string {
	if release.Version == "" {
		return fmt.Sprintf("exe, built from source (%s/%s)", runtime.GOOS, runtime.GOARCH)
	}
	return fmt.Sprintf("exe %s (%s/%s)", release.Version, runtime.GOOS, runtime.GOARCH)
}

func cmdVersion() error {
	fmt.Println(versionLine())
	return nil
}

// sourceBuildHint is the answer to setup, update and uninstall on a binary
// nobody released: it came out of a checkout, and that is where it changes.
const sourceBuildHint = "this exe was built from source — update it there with `git pull && make build`"
