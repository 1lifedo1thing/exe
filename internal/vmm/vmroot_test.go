package vmm

import "testing"

// The VMs' disks and the base images follow vm_dir when one is set, and
// stay in the state folder — where they have always been — when not.
func TestVMRoot(t *testing.T) {
	o := Options{StateDir: "/home/ada/.exe"}
	if got := o.vmRoot(); got != "/home/ada/.exe" {
		t.Errorf("with no vm_dir the VMs are in %q, want the state folder", got)
	}
	o.VMDir = "/data/exe"
	if got := o.vmRoot(); got != "/data/exe" {
		t.Errorf("with vm_dir set the VMs are in %q", got)
	}
}
