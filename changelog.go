// Package exe is the top of the repository, for the one thing the binary
// needs from there: go:embed reaches no higher than the folder of the
// file that asks.
package exe

import _ "embed"

// Changelog is CHANGELOG.md: what every release said had changed, newest
// first. The site renders it at /docs/changelog (internal/server/sitedocs.go),
// and `deploy/release.sh` adds each release to it.
//
//go:embed CHANGELOG.md
var Changelog string
