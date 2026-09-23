package server

import (
	"context"
	"strings"
	"testing"

	"exe/internal/vmm"
)

// The memory file is replaced wholesale, capped, and removed when cleared —
// the anti-rot contract the remember tool's description promises.
func TestVMMemory(t *testing.T) {
	s := &Server{StateDir: t.TempDir()}

	if got := s.readVMMemory("demo"); got != "" {
		t.Fatalf("fresh memory = %q", got)
	}
	if err := s.writeVMMemory("demo", "app lives in ~/app, port 8000"); err != nil {
		t.Fatal(err)
	}
	if got := s.readVMMemory("demo"); got != "app lives in ~/app, port 8000" {
		t.Fatalf("memory = %q", got)
	}
	if err := s.writeVMMemory("demo", "rewritten"); err != nil {
		t.Fatal(err)
	}
	if got := s.readVMMemory("demo"); got != "rewritten" {
		t.Fatalf("memory after rewrite = %q", got)
	}
	if err := s.writeVMMemory("demo", strings.Repeat("x", memoryMax+1)); err == nil {
		t.Fatal("oversized memory accepted")
	}
	if got := s.readVMMemory("demo"); got != "rewritten" {
		t.Fatalf("memory clobbered by refused write: %q", got)
	}
	if err := s.writeVMMemory("demo", " \n"); err != nil {
		t.Fatal(err)
	}
	if got := s.readVMMemory("demo"); got != "" {
		t.Fatalf("memory after clear = %q", got)
	}
	if err := s.writeVMMemory("demo", ""); err != nil {
		t.Fatal(err) // clearing twice must not error on the missing file
	}
}

type briefingVM struct {
	vmm.Manager
	info *vmm.Info
}

func (v briefingVM) Get(context.Context, string) (*vmm.Info, error) { return v.info, nil }

// The briefing names the system the VM's recorded image means, so an agent
// on an Alpine guest is told apk and doas before it reaches for apt-get;
// the default image is named too, never left to a guess.
func TestVMBriefingNamesTheSystem(t *testing.T) {
	for _, tc := range []struct{ image, want, never string }{
		{"alpine", "System: Alpine 3.24 — doas, not sudo", "Debian"},
		{"", "System: Debian 13 — sudo, apt-get, bash, systemd.", "Alpine"},
		{"debian", "System: Debian 13 — sudo", "Alpine"},
	} {
		s := &Server{StateDir: t.TempDir(), VMs: briefingVM{info: &vmm.Info{Name: "smol", State: "stopped", Image: tc.image}}}
		got := s.vmBriefing(context.Background(), "smol", "")
		if !strings.Contains(got, tc.want) {
			t.Errorf("image %q: briefing lacks %q:\n%s", tc.image, tc.want, got)
		}
		if strings.Contains(got, tc.never) {
			t.Errorf("image %q: briefing names %s:\n%s", tc.image, tc.never, got)
		}
	}
}

// A VM list for a model names every entry's image, the default included,
// without touching the manager's records.
func TestWithImages(t *testing.T) {
	in := []*vmm.Info{{Name: "test"}, {Name: "smol", Image: "alpine"}}
	out := withImages(in)
	if out[0].Image != "debian" || out[1].Image != "alpine" {
		t.Fatalf("images = %q, %q", out[0].Image, out[1].Image)
	}
	if in[0].Image != "" {
		t.Fatal("the manager's record was changed")
	}
}
