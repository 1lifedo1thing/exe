//go:build linux

package vmm

import (
	"path/filepath"
	"testing"
)

func TestVMDirFollowsTheVMStore(t *testing.T) {
	m := &fcManager{opts: Options{StateDir: "/home/ada/.exe"}}
	if got, want := m.vmDir("demo"), filepath.Join("/home/ada/.exe", "vms", "demo"); got != want {
		t.Errorf("vmDir = %q, want %q", got, want)
	}
	m.opts.VMDir = "/data/exe"
	if got, want := m.vmDir("demo"), filepath.Join("/data/exe", "vms", "demo"); got != want {
		t.Errorf("vmDir with vm_dir = %q, want %q", got, want)
	}
}
