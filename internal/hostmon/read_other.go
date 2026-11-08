//go:build !linux && !darwin

package hostmon

import "time"

// Supported: Linux and macOS are read; elsewhere the Control Strip hides
// the module.
const Supported = false

func readHost() counters { return counters{at: time.Now()} }
