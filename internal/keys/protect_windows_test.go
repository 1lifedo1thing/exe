//go:build windows

package keys

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This runs on a real Windows only (the test binary is cross-built and
// run there, docs/release.md "Windows"): the judge is Windows' own
// OpenSSH, which is what refuses a key.

// opensshTakes asks ssh-keygen for the public half of a private key. It
// reads the file by the rule ssh reads it by, and says so of a key it will
// not use. Its words go to a file: read through a pipe, in a session with
// no console, it does not come back.
func opensshTakes(t *testing.T, key string) (bool, string) {
	t.Helper()
	keygen := filepath.Join(os.Getenv("SystemRoot"), "System32", "OpenSSH", "ssh-keygen.exe")
	if _, err := os.Stat(keygen); err != nil {
		t.Skipf("no OpenSSH on this Windows: %v", err)
	}
	said, err := os.CreateTemp(t.TempDir(), "said")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, keygen, "-y", "-f", key)
	cmd.Stdout, cmd.Stderr = said, said
	runErr := cmd.Run()
	said.Close()
	b, _ := os.ReadFile(said.Name())
	out := strings.TrimSpace(string(b))
	return runErr == nil && strings.HasPrefix(out, "ssh-ed25519 "), out
}

func acl(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("icacls", path).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls %s: %v\n%s", path, err, out)
	}
	return string(out)
}

func TestOnRealWindowsAKeyIsKeptToItsUser(t *testing.T) {
	state := t.TempDir()
	// what Codex's sandbox does to the folders at the top of a home
	// folder, with a group every Windows has: Users may read this folder
	// and all that is made in it
	if out, err := exec.Command("icacls", state, "/grant", "*S-1-5-32-545:(OI)(CI)RX").CombinedOutput(); err != nil {
		t.Fatalf("icacls /grant: %v\n%s", err, out)
	}

	// a new key is written with a list of its own
	key, _, err := Ensure(state)
	if err != nil {
		t.Fatal(err)
	}
	if ok, said := opensshTakes(t, key); !ok {
		t.Errorf("OpenSSH refuses a key Ensure has just made: %s\n%s", said, acl(t, key))
	}
	if a := acl(t, key); strings.Contains(a, "(I)") || strings.Contains(a, `BUILTIN\Users`) {
		t.Errorf("the new key still takes its folder's permissions:\n%s", a)
	}

	// the fault itself, so that the test is known to see it: the same key
	// written the plain way inherits the folder's list, and is refused
	b, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, said := opensshTakes(t, key); ok {
		t.Fatalf("OpenSSH takes a key that Users may read: this test would not see the fault\n%s", acl(t, key))
	} else if !strings.Contains(said, "ermissions") {
		t.Errorf("refused, but not for its permissions: %s", said)
	}

	// and a key that is already there, in that state, is put right by the
	// next start: this is every install from before the fix
	if _, _, err := Ensure(state); err != nil {
		t.Fatal(err)
	}
	if ok, said := opensshTakes(t, key); !ok {
		t.Errorf("OpenSSH still refuses an existing key after Ensure: %s\n%s", said, acl(t, key))
	}

	// the gate's host key is a private key in the same folder
	if _, err := EnsureHostKey(state); err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(state, "ssh", "host_ed25519")
	if ok, said := opensshTakes(t, host); !ok {
		t.Errorf("OpenSSH refuses the host key: %s\n%s", said, acl(t, host))
	}

	// the daemon still reads what it protected
	if _, err := EnsureHostKey(state); err != nil {
		t.Errorf("the host key cannot be read back: %v", err)
	}
	// Protect of a file that is not there says so, and nothing else
	if err := Protect(filepath.Join(state, "ssh", "none")); err == nil {
		t.Error("Protect of a missing file reported nothing")
	}
}
