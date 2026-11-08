//go:build !linux && !darwin && !windows

package hostmon

import "time"

// Supported: Linux, macOS and Windows are read; elsewhere the Control Strip hides
// the module.
const Supported = false

func readHost() counters { return counters{at: time.Now()} }
