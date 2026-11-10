package server

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"exe/internal/config"
)

// The clocks: a session is as lately touched as the latest of them.
func TestAgentTouched(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	s := &Server{StateDir: t.TempDir()}
	a := hostAgent{app: "fake", bin: "fake", title: "Fake", session: "exe-fake"}
	ss := agentSession{Name: "exe-fake-2", Number: 2, Created: 100}
	if got := s.agentTouched(a, ss, sessionClock{}); got != 100 {
		t.Fatalf("no clock but the start: %d", got)
	}
	if got := s.agentTouched(a, ss, sessionClock{activity: 300, lastAttached: 200}); got != 300 {
		t.Fatalf("a key typed: %d", got)
	}
	if got := s.agentTouched(a, ss, sessionClock{activity: 300, lastAttached: 400}); got != 400 {
		t.Fatalf("a window landed on it: %d", got)
	}
	// the hooks' word, then the conversation's last write
	os.MkdirAll(filepath.Dir(s.agentStateFile(a, ss.Name)), 0o755)
	os.WriteFile(s.agentStateFile(a, ss.Name), []byte(agentStateDone), 0o644)
	os.Chtimes(s.agentStateFile(a, ss.Name), time.Unix(500, 0), time.Unix(500, 0))
	if got := s.agentTouched(a, ss, sessionClock{activity: 300}); got != 500 {
		t.Fatalf("the hooks' word: %d", got)
	}
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	os.WriteFile(transcript, []byte("{}\n"), 0o644)
	os.Chtimes(transcript, time.Unix(600, 0), time.Unix(600, 0))
	b, _ := json.Marshal(map[string]string{"session_id": "11111111-2222-4333-8444-555555555555", "transcript_path": transcript})
	os.WriteFile(s.agentStatusFile(a, ss.Name), b, 0o644)
	if got := s.agentTouched(a, ss, sessionClock{activity: 300}); got != 600 {
		t.Fatalf("the conversation's last write: %d", got)
	}
}

func TestIdleNews(t *testing.T) {
	head, body := idleNews([]string{"One"}, 7, 200<<20, 0, true)
	if head != "Put away 1 idle session" || body != "“One”: not touched for 7 days. 200.0 MB freed. Click a row to continue it." {
		t.Fatalf("one: %q %q", head, body)
	}
	head, body = idleNews([]string{"A", "B", "C", "D", "E"}, 1, 3<<30, 300<<20, true)
	if head != "Put away 5 idle sessions" || body != "“A”, “B”, “C” and 2 more: not touched for 1 day. 3.0 GB freed and 300.0 MB of swap. Click a row to continue it." {
		t.Fatalf("five: %q %q", head, body)
	}
	if _, body = idleNews([]string{"A", "B"}, 7, 0, 0, false); body != "“A”, “B”: not touched for 7 days. Click a row to continue it." {
		t.Fatalf("not measured: %q", body)
	}
}

// A pass on a tmux server of the test's own, eight days from now: the
// session left alone is put away, and the ones with a reason to stay —
// written to since, a turn still open, no conversation, a window on it —
// keep running. The Newsfeed says what went.
func TestPutAwayIdleLive(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux on this host")
	}
	tmuxSocket = fmt.Sprintf("exe-test-idle-%d", os.Getpid())
	killServer := func() {
		if c := tmuxCmd("kill-server"); c != nil {
			c.Run()
		}
	}
	defer func() { killServer(); tmuxSocket = "" }()

	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	work := t.TempDir()
	bin := t.TempDir()
	cli := filepath.Join(bin, "fakecli")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\nexec sleep 600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := hostAgent{app: "fake", bin: cli, title: "Fake", session: "exe-fake"}
	agents := []hostAgent{a}
	s := New(&config.Config{}, nil, nil, "", t.TempDir())
	os.MkdirAll(filepath.Join(s.StateDir, "agents"), 0o755)

	later := time.Now().Add(8 * 24 * time.Hour) // when the pass looks
	// converse gives a session a conversation: a prompt, and the reply
	// that ends the turn unless it is left open
	converse := func(name, id string, open bool) string {
		dir := filepath.Join(home, "projects", "-work")
		os.MkdirAll(dir, 0o755)
		path := filepath.Join(dir, id+".jsonl")
		lines := []map[string]any{{"type": "user", "cwd": work, "sessionId": id, "timestamp": "2026-11-10T01:00:00Z",
			"message": map[string]any{"role": "user", "content": "carry on"}}}
		if !open {
			lines = append(lines, map[string]any{"type": "assistant", "sessionId": id, "timestamp": "2026-11-10T01:01:00Z",
				"message": map[string]any{"role": "assistant", "stop_reason": "end_turn"}})
		}
		var b []byte
		for _, l := range lines {
			j, _ := json.Marshal(l)
			b = append(b, append(j, '\n')...)
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		st, _ := json.Marshal(map[string]string{"session_id": id, "transcript_path": path})
		os.WriteFile(s.agentStatusFile(a, name), st, 0o644)
		os.WriteFile(s.agentStateFile(a, name), []byte(agentStateDone), 0o644) // at its prompt, whatever the pane printed as it started
		return path
	}
	for _, name := range []string{"exe-fake-2", "exe-fake-3", "exe-fake-4", "exe-fake-5"} {
		if err := s.newAgentSession(a, name); err != nil {
			t.Fatal(err)
		}
	}
	converse("exe-fake-2", "11111111-2222-4333-8444-555555555555", false)           // left alone
	recent := converse("exe-fake-3", "22222222-3333-4444-8555-666666666666", false) // written to an hour before the pass
	os.Chtimes(recent, later.Add(-time.Hour), later.Add(-time.Hour))
	converse("exe-fake-4", "33333333-4444-4555-8666-777777777777", true) // waits on a permission: its turn is open
	os.WriteFile(s.agentStateFile(a, "exe-fake-5"), []byte(agentStateDone), 0o644)
	// session 5 has no conversation; the icon's own session has one, and a window on it
	sh, session, err := s.startAgent(a, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()
	go io.Copy(io.Discard, sh)
	converse(session, "44444444-5555-4666-8777-888888888888", false)
	for i := 0; i < 100 && sh.Current() != session; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if sh.Current() != session {
		t.Fatal("the window's client did not attach")
	}
	running := func() []string {
		var names []string
		for _, l := range s.agentSessions(a) {
			names = append(names, l.Name)
		}
		slices.Sort(names)
		return names
	}
	all := running()
	if len(all) != 5 {
		t.Fatalf("sessions before: %v", all)
	}

	// never, and not yet: nothing goes
	s.cfg.Store(&config.Config{PutAwayDays: 0})
	if got := s.putAwayIdle(agents, later); got != nil || len(running()) != 5 {
		t.Fatalf("put_away_days 0 put away %v", got)
	}
	s.cfg.Store(&config.Config{PutAwayDays: 7})
	if got := s.putAwayIdle(agents, time.Now().Add(6*24*time.Hour)); got != nil || len(running()) != 5 {
		t.Fatalf("six days on, put away %v", got)
	}

	got := s.putAwayIdle(agents, later)
	if len(got) != 1 || got[0] != "exe-fake-2" {
		t.Fatalf("put away %v, want session 2 alone", got)
	}
	if left := running(); slices.Contains(left, "exe-fake-2") || len(left) != 4 {
		t.Fatalf("running after the pass: %v", left)
	}
	st := s.stoppedSessions(a)
	if len(st) != 1 || st[0].Name != "exe-fake-2" || !st[0].PutAway {
		t.Fatalf("stopped after the pass: %+v", st)
	}
	// the Newsfeed's line
	file, _ := s.newsSelf()
	var items []newsItem
	b, _ := os.ReadFile(filepath.Join(s.newsDir(), file))
	json.Unmarshal(b, &items)
	if len(items) != 1 || items[0].Title != "Put away 1 idle session" || !strings.Contains(items[0].Body, "“exe-fake-2”: not touched for 7 days.") {
		t.Fatalf("newsfeed: %+v", items)
	}
	// a second pass finds nothing more, and says nothing
	if again := s.putAwayIdle(agents, later); again != nil {
		t.Fatalf("a second pass put away %v", again)
	}
	b, _ = os.ReadFile(filepath.Join(s.newsDir(), file))
	json.Unmarshal(b, &items)
	if len(items) != 1 {
		t.Fatalf("a pass with nothing to do wrote to the Newsfeed: %+v", items)
	}
}
