//go:build !darwin && !linux && !windows

package vmm

import (
	"fmt"
	"runtime"
)

// Backend names what runs this platform's VMs: nothing here.
const Backend = ""

// New returns ErrNoBackend on platforms without a VM backend.
func New(opts Options) (Manager, error) {
	return nil, noBackend(fmt.Errorf("no vm backend for %s/%s", runtime.GOOS, runtime.GOARCH))
}
