package server

import (
	"strings"
	"testing"
)

// The operator is told the system a VM runs from its recorded image: a
// pinned Alpine chat hears doas and apk and never apt-get; a Debian one
// hears sudo and apt-get; the fleet operator hears both and where to look.
func TestChatSystemPromptNamesTheSystem(t *testing.T) {
	alpine := chatSystemPrompt("dev", "", "smol", "alpine")
	for _, want := range []string{"Alpine 3.24", "passwordless doas", "Install packages with doas apk add.", "run under OpenRC", "no bash", "musl"} {
		if !strings.Contains(alpine, want) {
			t.Errorf("alpine prompt lacks %q", want)
		}
	}
	// apt-get, sudo and systemd are named only as what not to use
	for _, never := range []string{"passwordless sudo", "Install packages with sudo", "run under systemd", "Debian"} {
		if strings.Contains(alpine, never) {
			t.Errorf("alpine prompt says %q", never)
		}
	}
	debian := chatSystemPrompt("dev", "", "test", "")
	for _, want := range []string{"Debian 13", "passwordless sudo", "Install packages with sudo apt-get install -y.", "run under systemd"} {
		if !strings.Contains(debian, want) {
			t.Errorf("debian prompt lacks %q", want)
		}
	}
	if strings.Contains(debian, "Alpine") || strings.Contains(debian, "doas") {
		t.Error("debian prompt mentions Alpine")
	}
	fleet := chatSystemPrompt("dev", "example.com", "", "")
	for _, want := range []string{"list_vms entry names its image", "Debian 13", "Alpine 3.24", "sudo apt-get install -y on Debian, doas apk add on Alpine", "https://<subdomain>.example.com"} {
		if !strings.Contains(fleet, want) {
			t.Errorf("fleet prompt lacks %q", want)
		}
	}
	if strings.Contains(fleet, "%!") || strings.Contains(alpine, "%!") || strings.Contains(debian, "%!") {
		t.Error("a prompt has an unfilled verb")
	}
}
