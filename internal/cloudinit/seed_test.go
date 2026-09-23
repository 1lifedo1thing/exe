package cloudinit

import (
	"strings"
	"testing"
)

func TestDocumentsDiskModes(t *testing.T) {
	growing, _ := Documents("vm", "dev", "ssh-ed25519 test", true)
	for _, want := range []string{"name: dev", "ssh-ed25519 test", "mode: auto"} {
		if !strings.Contains(growing, want) {
			t.Fatalf("growing user-data does not contain %q:\n%s", want, growing)
		}
	}
	fixed, _ := Documents("vm", "dev", "ssh-ed25519 test", false)
	for _, want := range []string{"mode: 'off'", "resize_rootfs: false"} {
		if !strings.Contains(fixed, want) {
			t.Fatalf("fixed user-data does not contain %q:\n%s", want, fixed)
		}
	}
}

func TestAlpineDocuments(t *testing.T) {
	userData, metaData := AlpineDocuments("vm", "dev", "ssh-ed25519 test")
	for _, want := range []string{
		"name: dev", "ssh-ed25519 test",
		"shell: /bin/ash", "groups: users, wheel", `doas: ["permit nopass dev"]`,
		"lock_passwd: false", `hashed_passwd: "*"`,
		"mode: 'off'", "resize_rootfs: false",
	} {
		if !strings.Contains(userData, want) {
			t.Fatalf("alpine user-data does not contain %q:\n%s", want, userData)
		}
	}
	if strings.Contains(userData, "/bin/bash") {
		t.Fatalf("alpine user-data names bash:\n%s", userData)
	}
	if !strings.Contains(metaData, "local-hostname: vm") {
		t.Fatalf("alpine meta-data misses the hostname:\n%s", metaData)
	}
}
