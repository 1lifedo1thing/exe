package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"exe/internal/agent"
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

// A chat pinned to the Mac OS X Mac (pin macChatPin) has the shell tools
// and the memory ones, the Workspace upload, and nothing that reads a
// VM's network or record; a VM's chat has upload too.
func TestChatToolsPinnedToTheMac(t *testing.T) {
	names := func(tools []agent.Tool) map[string]bool {
		out := map[string]bool{}
		for _, tl := range tools {
			out[tl.Function.Name] = true
		}
		return out
	}
	mac := names(chatTools(macChatPin))
	for _, want := range []string{"bash", "write_file", "edit_file", "read_file", "upload", "remember", "recall", "plan"} {
		if !mac[want] {
			t.Errorf("the Mac's chat lacks %s", want)
		}
	}
	for _, no := range []string{"list_vms", "create_vm", "delete_vm", "start_vm", "stop_vm", "list_ports", "list_routes", "expose", "unexpose", "github_push"} {
		if mac[no] {
			t.Errorf("the Mac's chat offers %s", no)
		}
	}
	vm := names(chatTools("demo"))
	if !vm["upload"] || !vm["expose"] || vm["list_vms"] {
		t.Errorf("a VM's pinned tools: %v", vm)
	}
	for _, tl := range chatTools(macChatPin) {
		if _, has := tl.Function.Parameters["properties"].(map[string]any)["vm"]; has {
			t.Errorf("%s still carries a vm parameter when pinned", tl.Function.Name)
		}
	}
	args := map[string]any{"source": "a.png", "path": "/Users/exe/Desktop/a.png"}
	pinChatArgs("upload", args, macChatPin)
	if args["vm"] != macChatPin {
		t.Errorf("pinChatArgs left upload's vm %v", args["vm"])
	}
	if got := chatToolSummary("upload", args); got != "upload a.png to Mac OS X:/Users/exe/Desktop/a.png" {
		t.Errorf("summary: %q", got)
	}
}

// POST /v1/chat/send with the Mac's pin asks for no VM record: once the
// provider is there it is refused only while Mac OS X is not installed,
// and with the Mac installed (shut down here) the run starts.
func TestChatSendPinnedToTheMacNeedsNoVM(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"hello"},"done":true}`)
	}))
	defer ollama.Close()
	send := func(dir string) (int, string) {
		s := New(&config.Config{Ollama: config.OllamaConfig{BaseURL: ollama.URL, Model: "m"}}, nil, nil, "", dir)
		rec := httptest.NewRecorder()
		s.handleChatSend(rec, httptest.NewRequest("POST", "/v1/chat/send",
			strings.NewReader(`{"session":"","message":"what version of Tiger is this?","vm":"Mac OS X"}`)))
		return rec.Code, rec.Body.String()
	}
	dir := t.TempDir()
	if code, body := send(dir); code != http.StatusNotFound || !strings.Contains(body, "not set up") {
		t.Errorf("no Mac installed: %d %s", code, body)
	}
	os.MkdirAll(filepath.Join(dir, "mac-osx"), 0o700)
	os.WriteFile(filepath.Join(dir, "mac-osx", "installed.json"), []byte("{}\n"), 0o600)
	if code, body := send(dir); code != http.StatusOK || !strings.Contains(body, "hello") {
		t.Errorf("Mac installed: %d %s, want the run's stream", code, body)
	}
}

// The Mac's memory lives with the Mac, not among the VMs, and the system
// prompt says what Tiger lacks before the model reaches for it.
func TestMacChatPromptAndMemory(t *testing.T) {
	s := New(&config.Config{}, nil, nil, "", t.TempDir())
	if p := s.memoryPath(macChatPin); !strings.HasSuffix(p, filepath.Join("mac-osx", "memory.md")) {
		t.Errorf("memory path %s", p)
	}
	prompt := s.macSystemPrompt()
	for _, want := range []string{"Tiger", "head has no -c", "mktemp needs a template", "osascript", "upload", "not chosen yet"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q", want)
		}
	}
	if strings.Contains(prompt, "%!") {
		t.Error("prompt has an unfilled verb")
	}
}
