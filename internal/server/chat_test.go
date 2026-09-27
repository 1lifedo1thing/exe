package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"exe/internal/config"
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

// A VM window's Agent tab is a launcher: its prompt is the first message
// of a new chat pinned to that VM (POST /v1/chat/send with "vm"), so the
// run takes the backend Configuration has selected, as any chat does. With
// chat_provider "openai" and nobody signed in, the pinned send is refused
// for ChatGPT's reason and the Ollama that is configured and answering
// beside it is never asked; with Ollama selected, it is Ollama's reason.
func TestChatSendPinnedToVMTakesTheSelectedProvider(t *testing.T) {
	var asked atomic.Int32
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"hello"},"done":true}`)
	}))
	defer ollama.Close()

	send := func(cfg *config.Config) (int, string) {
		s := New(cfg, nil, nil, "", t.TempDir())
		rec := httptest.NewRecorder()
		s.handleChatSend(rec, httptest.NewRequest("POST", "/v1/chat/send",
			strings.NewReader(`{"session":"","message":"build a guestbook on port 8000","vm":"demo"}`)))
		return rec.Code, rec.Body.String()
	}

	code, body := send(&config.Config{ChatProvider: "openai",
		Ollama: config.OllamaConfig{BaseURL: ollama.URL, Model: "m"}})
	if code != http.StatusConflict || !strings.Contains(body, "not signed in to ChatGPT") {
		t.Errorf("chat_provider openai, signed out: %d %s, want 409 for ChatGPT's reason", code, body)
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("chat_provider openai: Ollama was asked %d times, want 0", n)
	}

	code, body = send(&config.Config{Ollama: config.OllamaConfig{Model: "m"}})
	if code != http.StatusConflict || !strings.Contains(body, "ollama.base_url is not configured") {
		t.Errorf("chat_provider unset, no Ollama: %d %s, want 409 for Ollama's reason", code, body)
	}
}
