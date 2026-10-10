package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadPreservesFirecrackerDefaultsForExistingConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("EXE_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"listen":"9000"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if config.Listen != ":9000" {
		t.Fatalf("listen = %q", config.Listen)
	}
	if config.Firecracker.Binary != "firecracker" {
		t.Fatalf("binary = %q", config.Firecracker.Binary)
	}
	if config.Firecracker.NetworkHelper != "/usr/local/libexec/exe-net-helper" {
		t.Fatalf("network helper = %q", config.Firecracker.NetworkHelper)
	}
	if config.Firecracker.NetworkCIDR != "172.30.0.0/16" {
		t.Fatalf("network CIDR = %q", config.Firecracker.NetworkCIDR)
	}
	wantArch := "aarch64"
	if runtime.GOARCH == "amd64" {
		wantArch = "x86_64"
	}
	if !strings.Contains(config.Firecracker.KernelURL, "/"+wantArch+"/") {
		t.Fatalf("kernel URL %q does not select %s", config.Firecracker.KernelURL, wantArch)
	}
}

// Service names are lower-cased and trimmed; empty entries vanish.
func TestNormalizeServices(t *testing.T) {
	c := &Config{Services: map[string]string{" Planet ": " http://127.0.0.1:7799 ", "": "http://x", "gone": " "}}
	c.Normalize()
	if len(c.Services) != 1 || c.Services["planet"] != "http://127.0.0.1:7799" {
		t.Fatalf("services: %v", c.Services)
	}
	c = &Config{}
	c.Normalize()
	if c.Services != nil {
		t.Fatalf("empty services became %v", c.Services)
	}
}

// vm_dir moves the VMs and nothing else; without it they stay where they were.
func TestVMRoot(t *testing.T) {
	t.Setenv("EXE_HOME", t.TempDir())
	c := Default()
	if c.VMRoot() != Dir() {
		t.Errorf("VMRoot = %q, want the state folder %q", c.VMRoot(), Dir())
	}
	c.VMDir = "  /data/exe  "
	c.Normalize()
	if c.VMRoot() != "/data/exe" {
		t.Errorf("VMRoot = %q", c.VMRoot())
	}
}
