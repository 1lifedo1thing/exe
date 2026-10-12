package macos9

import (
	"os"
	"strings"
	"testing"
)

// The Mac OS X Mac has an agent: an RSA key minted on the first ask and
// kept, an account chosen by its short name. The Mac OS 9 Mac has none.
func TestAgentMintsOneRSAKeyAndKeepsIt(t *testing.T) {
	dir := t.TempDir()
	m := NewMacOSX(dir, dir)
	a, err := m.Agent()
	if err != nil || a == nil {
		t.Fatalf("Agent: %v %v", a, err)
	}
	if !strings.HasPrefix(a.Key, "ssh-rsa AAAA") || !strings.HasSuffix(a.Key, " exe-agent") || strings.Contains(a.Key, "\n") {
		t.Errorf("key line: %q", a.Key)
	}
	if a.User != "" || a.Port != 0 {
		t.Errorf("a fresh Mac has no account and no port: %+v", a)
	}
	st, err := os.Stat(m.AgentKeyPath())
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("private key: %v %v", st, err)
	}
	again, _ := m.Agent()
	if again.Key != a.Key {
		t.Error("a second ask minted another key")
	}
	if err := m.SetAgentUser(" exe "); err != nil {
		t.Fatal(err)
	}
	if err := m.SetAgentUser("Liv id"); err == nil {
		t.Error("an account with a space was taken")
	}
	if a, _ = m.Agent(); a.User != "exe" {
		t.Errorf("user after Set: %q", a.User)
	}
	if a, _ := New(dir).Agent(); a != nil {
		t.Errorf("Mac OS 9 has an agent: %+v", a)
	}
}
