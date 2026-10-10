package main

import (
	"fmt"
	"os"
	"runtime"

	"exe/internal/config"
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

// cmdToken shows the API token: what the desk asks for the first time a
// browser opens it, and tells its reader to get by running this. It is
// the api_token line of config.json, so whoever can run this could read
// it there; the command spares them finding the file and reading JSON.
// With no token set it prints nothing on standard output, so a script's
// $(exe token) is empty, and says why beside it.
func cmdToken() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.APIToken == "" {
		fmt.Fprintln(os.Stderr, "No API token is set: this exe asks for none.")
		return nil
	}
	fmt.Println(cfg.APIToken)
	return nil
}

// sourceBuildHint is the answer to setup, update and uninstall on a binary
// nobody released: it came out of a checkout, and that is where it changes.
const sourceBuildHint = "this exe was built from source — update it there with `git pull && make build`"
