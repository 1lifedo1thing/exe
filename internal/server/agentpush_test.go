package server

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestAgentStateSession(t *testing.T) {
	for file, want := range map[string]string{
		"claude.state":       "exe-claude",
		"claude-12.state":    "exe-claude-12",
		"codex.state":        "exe-codex",
		"codex-3.state":      "exe-codex-3",
		"claude-1.state":     "", // the icon's own carries no number
		"claude.state.tmp":   "",
		"claude.status.json": "",
		"other-2.state":      "",
	} {
		_, got, ok := agentStateSession(file)
		if got != want || ok != (want != "") {
			t.Errorf("agentStateSession(%q) = %q, %v; want %q", file, got, ok, want)
		}
	}
}

func TestAgentTurnEnds(t *testing.T) {
	t0 := time.Unix(1790000000, 0)
	t1 := t0.Add(3 * time.Second)
	first := map[string]agentStateStamp{"claude.state": {t0, agentStateDone}}
	if ends := agentTurnEnds(nil, first); len(ends) != 0 {
		t.Fatalf("the first pass only takes the stamps, got %v", ends)
	}
	if ends := agentTurnEnds(first, first); len(ends) != 0 {
		t.Fatalf("an unmoved stamp is no turn end, got %v", ends)
	}
	cases := []struct {
		name      string
		prev, cur agentStateStamp
		fresh     bool // no file on the pass before
		want      bool
	}{
		{name: "working then done", prev: agentStateStamp{t0, agentStateWorking}, cur: agentStateStamp{t1, agentStateDone}, want: true},
		{name: "Codex: done again, a new file", prev: agentStateStamp{t0, agentStateDone}, cur: agentStateStamp{t1, agentStateDone}, want: true},
		{name: "a session's first turn", cur: agentStateStamp{t1, agentStateDone}, fresh: true, want: true},
		{name: "a turn begins", prev: agentStateStamp{t0, agentStateDone}, cur: agentStateStamp{t1, agentStateWorking}},
		{name: "done, then the idle prompt", prev: agentStateStamp{t0, agentStateDone}, cur: agentStateStamp{t1, agentStateWaiting}},
		{name: "a permission waits", prev: agentStateStamp{t0, agentStateWorking}, cur: agentStateStamp{t1, agentStateWaiting}},
	}
	for _, c := range cases {
		prev := map[string]agentStateStamp{}
		if !c.fresh {
			prev["claude-2.state"] = c.prev
		}
		ends := agentTurnEnds(prev, map[string]agentStateStamp{"claude-2.state": c.cur})
		if got := len(ends) == 1 && ends[0] == "claude-2.state"; got != c.want {
			t.Errorf("%s: turn end = %v (%v), want %v", c.name, got, ends, c.want)
		}
	}
}

func TestPersonInput(t *testing.T) {
	for in, want := range map[string]bool{
		"a":                                true,
		"\r":                               true,
		"\x1b[A":                           true, // an arrow key
		"\x1b[<0;10;5M":                    true, // the mouse
		"\x1b[200~pasted\x1b[201~":         true,
		"":                                 false,
		"\x1b[12;40R":                      false, // cursor position report
		"\x1b[?1;2c":                       false, // device attributes
		"\x1b[>0;276;0c":                   false,
		"\x1b[I":                           false, // focus in
		"\x1b[O":                           false, // focus out
		"\x1b[?2026;2$y":                   false, // a mode report
		"\x1b]11;rgb:0000/0000/0000\x1b\\": false, // the background colour, asked for
		"\x1b]10;rgb:ffff/ffff/ffff\x07":   false,
		"\x1b[I\x1b[12;40R":                false, // two reports in one frame
		"\x1b[12;40Ry":                     true,  // a report, then a key
	} {
		if got := personInput([]byte(in)); got != want {
			t.Errorf("personInput(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestAgentTurnEndMessage(t *testing.T) {
	claude := hostAgents["claude"]
	list := []agentSession{{Name: "exe-claude-2", Number: 2, Title: "✳ Fix the pager overlap"}, {Name: "exe-claude", Number: 1}}
	got := agentTurnEndMessage(claude, "exe-claude-2", list)
	want := pushMessage{Title: "Claude Code finished", Body: "Fix the pager overlap", Tag: "agent-exe-claude-2",
		URL: "/#show=claude:exe-claude-2", Show: "claude:exe-claude-2"}
	if got != want {
		t.Errorf("titled session:\n got %+v\nwant %+v", got, want)
	}
	if got := agentTurnEndMessage(claude, "exe-claude", list); got.Body != "Claude Code 1" {
		t.Errorf("an untitled session is named as the column names it, got %q", got.Body)
	}
	if got := agentTurnEndMessage(hostAgents["codex"], "exe-codex-4", nil); got.Title != "Codex finished" || got.Body != "Codex 4" || got.Show != "codex:exe-codex-4" {
		t.Errorf("a session tmux no longer lists: %+v", got)
	}
}

func TestChatEndMessage(t *testing.T) {
	got := chatEndMessage("abc123", "Concert calendar", "", "")
	want := pushMessage{Title: "Chat finished", Body: "Concert calendar", Tag: "chat-abc123", URL: "/#show=chat:abc123", Show: "chat:abc123"}
	if got != want {
		t.Errorf("clean end:\n got %+v\nwant %+v", got, want)
	}
	if got := chatEndMessage("abc123", "Concert calendar", "web", "model: 502"); got.Title != "Chat with web stopped" || got.Body != "Concert calendar — model: 502" {
		t.Errorf("a failed run pinned to a VM: %+v", got)
	}
	if got := chatEndMessage("abc123", "", "", ""); got.Body != "Untitled chat" {
		t.Errorf("no title: %+v", got)
	}
}

// pushTestServer is a daemon with one subscribed browser whose push
// service is a local server that counts what it is sent.
func pushTestServer(t *testing.T) (*Server, *atomic.Int32) {
	t.Helper()
	var posts atomic.Int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(ts.Close)
	old := pushClient
	pushClient = ts.Client()
	t.Cleanup(func() { pushClient = old })

	s := &Server{StateDir: t.TempDir()}
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	rand.Read(auth)
	var sub pushSub
	sub.Endpoint = ts.URL + "/push/one"
	sub.Keys.P256dh = b64.EncodeToString(key.PublicKey().Bytes())
	sub.Keys.Auth = b64.EncodeToString(auth)
	b, _ := json.Marshal([]pushSub{sub})
	if err := os.WriteFile(filepath.Join(s.StateDir, pushSubFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return s, &posts
}

// waitPosts waits for the count to reach want, then a little longer to
// see that it stops there.
func waitPosts(t *testing.T, posts *atomic.Int32, want int32, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for posts.Load() < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	if got := posts.Load(); got != want {
		t.Fatalf("%s: %d pushes sent, want %d", what, got, want)
	}
}

// The watcher against real files: the hooks' own write — beside the
// file, then moved into place — and a push service that counts.
func TestRunAgentPush(t *testing.T) {
	s, posts := pushTestServer(t)
	dir := filepath.Join(s.StateDir, "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(file, word string) {
		t.Helper()
		tmp := filepath.Join(dir, file+".tmp")
		if err := os.WriteFile(tmp, []byte(word), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, file)); err != nil {
			t.Fatal(err)
		}
	}
	write("claude-7.state", agentStateDone) // finished before the daemon started: old news

	oldPoll := agentPushPoll
	agentPushPoll = 20 * time.Millisecond
	defer func() { agentPushPoll = oldPoll }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.RunAgentPush(ctx)
	waitPosts(t, posts, 0, "the pass at startup")

	write("claude-7.state", agentStateWorking)
	waitPosts(t, posts, 0, "a turn begins")
	write("claude-7.state", agentStateDone)
	waitPosts(t, posts, 1, "a Claude Code turn ends")
	write("claude-7.state", agentStateWaiting)
	waitPosts(t, posts, 1, "the idle prompt after it")

	write("codex-3.state", agentStateDone)
	waitPosts(t, posts, 2, "a Codex session's first turn ends")
	write("codex-3.state", agentStateDone)
	waitPosts(t, posts, 3, "its second")

	// the quiet rule: someone typed into the session a moment ago
	s.noteAgentInput("exe-claude-7", time.Now())
	write("claude-7.state", agentStateWorking)
	write("claude-7.state", agentStateDone)
	waitPosts(t, posts, 3, "a turn that ends right after a keystroke")
	s.noteAgentInput("exe-claude-7", time.Now().Add(-2*agentPushQuiet))
	write("claude-7.state", agentStateDone)
	waitPosts(t, posts, 4, "a turn that ends long after the last keystroke")
}

func TestPushChatEnd(t *testing.T) {
	s, posts := pushTestServer(t)
	now := time.Now()
	long := now.Add(-5 * time.Minute)
	s.pushChatEnd("abc123", "Concert calendar", "", "", true, long, now)
	waitPosts(t, posts, 0, "a run the person stopped")
	s.pushChatEnd("abc123", "Concert calendar", "", "", false, now.Add(-20*time.Second), now)
	waitPosts(t, posts, 0, "a reply inside the quiet minute")
	s.pushChatEnd("abc123", "Concert calendar", "", "", false, long, now)
	waitPosts(t, posts, 1, "a long run that ended on its own")
	s.pushChatEnd("abc123", "Concert calendar", "web", "model: 502", false, long, now)
	waitPosts(t, posts, 2, "a long run that failed")

	// nobody subscribed: nothing to send, and no error
	os.Remove(filepath.Join(s.StateDir, pushSubFile))
	s.pushChatEnd("abc123", "Concert calendar", "", "", false, long, now)
	waitPosts(t, posts, 2, "no subscriptions")
}
