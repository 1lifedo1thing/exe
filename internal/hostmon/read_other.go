//go:build !linux

package hostmon

import "time"

// Supported: only Linux is read so far; elsewhere the Control Strip hides
// the module.
const Supported = false

func readHost() counters { return counters{at: time.Now()} }
