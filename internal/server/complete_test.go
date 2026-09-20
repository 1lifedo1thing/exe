package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"exe/internal/config"
)

// A completion streams the model's fragments as NDJSON and ends with a done
// line; the backend sees the system prompt ahead of the user prompt, the
// configured model, thinking switched off and the options the caller sent.
func TestChatComplete(t *testing.T) {
	var seen map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&seen)
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"Hello, "},"done":false}`)
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"world."},"done":true}`)
	}))
	defer backend.Close()

	s := New(&config.Config{Ollama: config.OllamaConfig{BaseURL: backend.URL, Model: "m"}}, nil, nil, "", t.TempDir())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/chat/complete", "application/json",
		strings.NewReader(`{"system":"fix it","prompt":"helo wrold","effort":"off","options":{"temperature":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content-type = %q", ct)
	}
	var lines []map[string]any
	dec := json.NewDecoder(resp.Body)
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, m)
	}
	if len(lines) != 3 || lines[0]["delta"] != "Hello, " || lines[1]["delta"] != "world." ||
		lines[2]["done"] != true || lines[2]["model"] != "m" {
		t.Fatalf("lines = %v", lines)
	}

	if seen["model"] != "m" {
		t.Fatalf("backend model = %v", seen["model"])
	}
	if seen["think"] != false {
		t.Fatalf("backend think = %v, want false", seen["think"])
	}
	if opts, _ := seen["options"].(map[string]any); opts["temperature"] != 0.0 {
		t.Fatalf("backend options = %v, want temperature 0", seen["options"])
	}
	msgs, _ := seen["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("backend messages = %v", msgs)
	}
	sys, _ := msgs[0].(map[string]any)
	usr, _ := msgs[1].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "fix it" || usr["role"] != "user" || usr["content"] != "helo wrold" {
		t.Fatalf("backend messages = %v", msgs)
	}
}

// Failures that happen before any fragment streams are plain JSON errors,
// so an app can tell "nothing configured" from "the model choked".
func TestChatCompleteErrors(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
	}))
	defer backend.Close()

	post := func(s *Server, body string) (int, string) {
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()
		resp, err := http.Post(ts.URL+"/v1/chat/complete", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out.Error
	}

	s := New(&config.Config{Ollama: config.OllamaConfig{BaseURL: backend.URL, Model: "m"}}, nil, nil, "", t.TempDir())
	if code, msg := post(s, `{"prompt":"   "}`); code != http.StatusBadRequest || msg == "" {
		t.Fatalf("empty prompt: %d %q", code, msg)
	}
	if code, msg := post(s, `{"prompt":"hi"}`); code != http.StatusBadGateway || !strings.Contains(msg, "model not found") {
		t.Fatalf("backend failure: %d %q", code, msg)
	}
	if code, msg := post(s, `{"prompt":"`+strings.Repeat("x", completeMaxPrompt+1)+`"}`); code != http.StatusRequestEntityTooLarge || msg == "" {
		t.Fatalf("oversize prompt: %d %q", code, msg)
	}

	unset := New(&config.Config{}, nil, nil, "", t.TempDir())
	if code, msg := post(unset, `{"prompt":"hi"}`); code != http.StatusServiceUnavailable || !strings.Contains(msg, "base_url") {
		t.Fatalf("unconfigured: %d %q", code, msg)
	}
}

// The ChatGPT provider needs a sign-in on file before anything streams,
// and a provider nobody knows is a bad request; the default stays Ollama.
func TestChatCompleteProvider(t *testing.T) {
	s := New(&config.Config{Ollama: config.OllamaConfig{BaseURL: "http://127.0.0.1:1", Model: "m"},
		OpenAI: config.OpenAIConfig{Model: "gpt-5.6-sol"}}, nil, nil, "", t.TempDir())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	post := func(body string) (int, string) {
		resp, err := http.Post(ts.URL+"/v1/chat/complete", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out.Error
	}
	if code, msg := post(`{"prompt":"hi","provider":"openai"}`); code != http.StatusServiceUnavailable || !strings.Contains(msg, "ChatGPT") {
		t.Fatalf("openai without a sign-in: %d %q", code, msg)
	}
	if code, msg := post(`{"prompt":"hi","provider":"bogus"}`); code != http.StatusBadRequest || !strings.Contains(msg, "bogus") {
		t.Fatalf("unknown provider: %d %q", code, msg)
	}
	// "" and "ollama" both reach the Ollama endpoint (unreachable here: 502, not 503)
	for _, p := range []string{"", "ollama"} {
		if code, _ := post(`{"prompt":"hi","provider":"` + p + `"}`); code != http.StatusBadGateway {
			t.Fatalf("provider %q: %d, want 502 from the dead Ollama endpoint", p, code)
		}
	}
	if k := completeCacheKey("fix it"); !strings.HasPrefix(k, "complete-") || len(k) != len("complete-")+16 {
		t.Fatalf("cache key = %q", k)
	}
}

// shareBackend is a fake Ollama that counts its calls and holds each answer
// until release is closed, so a test decides who is asking while it runs.
type shareBackend struct {
	*httptest.Server
	mu      sync.Mutex
	calls   int
	gone    int // calls whose asker hung up before the answer was let go
	started chan struct{}
	release chan struct{}
	failing bool // every call fails while set (the agent retries a failure with another think field)
}

func newShareBackend() *shareBackend {
	b := &shareBackend{started: make(chan struct{}, 16), release: make(chan struct{})}
	b.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.calls++
		fail := b.failing
		release := b.release
		b.mu.Unlock()
		if fail {
			http.Error(w, `{"error":"model choked"}`, http.StatusInternalServerError)
			return
		}
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"Hello, "},"done":false}`)
		w.(http.Flusher).Flush()
		b.started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			b.mu.Lock()
			b.gone++
			b.mu.Unlock()
			return
		}
		fmt.Fprintln(w, `{"message":{"role":"assistant","content":"world."},"done":true}`)
	}))
	return b
}

func (b *shareBackend) count() (calls, gone int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls, b.gone
}

// shareAnswer is what one asker read: the text, whether its done line said
// shared, and the error line or status that ended it instead.
type shareAnswer struct {
	text   string
	shared bool
	fail   string
}

func askShared(ctx context.Context, url, body string) shareAnswer {
	req, _ := http.NewRequestWithContext(ctx, "POST", url+"/v1/chat/complete", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return shareAnswer{fail: err.Error()}
	}
	defer resp.Body.Close()
	var a shareAnswer
	dec := json.NewDecoder(resp.Body)
	for {
		var m struct {
			Delta, Error string
			Done, Shared bool
		}
		if err := dec.Decode(&m); err != nil {
			if a.fail == "" {
				a.fail = "cut off: " + err.Error()
			}
			return a
		}
		a.text += m.Delta
		if m.Error != "" {
			a.fail = m.Error
			return a
		}
		if m.Done {
			a.shared = m.Shared
			return a
		}
	}
}

// Identical shared calls are one model call: the second asker joins the
// first's, reads it from the start, and an asker who comes after it
// finished gets the kept answer. A different question, or a call that does
// not ask to share, runs on its own.
func TestChatCompleteShare(t *testing.T) {
	b := newShareBackend()
	defer b.Close()
	s := New(&config.Config{Ollama: config.OllamaConfig{BaseURL: b.URL, Model: "m"}}, nil, nil, "", t.TempDir())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	const q = `{"system":"fix it","prompt":"helo wrold","options":{"temperature":0},"share":true}`

	first := make(chan shareAnswer, 1)
	go func() { first <- askShared(context.Background(), ts.URL, q) }()
	<-b.started // the model is mid-answer
	second := make(chan shareAnswer, 1)
	go func() { second <- askShared(context.Background(), ts.URL, q) }()
	waitFor(t, "the second asker to join", func() bool {
		s.completeMu.Lock()
		defer s.completeMu.Unlock()
		for _, f := range s.completeFlights {
			f.mu.Lock()
			n := f.askers
			f.mu.Unlock()
			return n == 2
		}
		return false
	})
	close(b.release)
	a1, a2 := <-first, <-second
	if a1.text != "Hello, world." || a1.shared || a1.fail != "" {
		t.Fatalf("first asker = %+v", a1)
	}
	if a2.text != "Hello, world." || !a2.shared || a2.fail != "" {
		t.Fatalf("second asker = %+v", a2)
	}
	if a3 := askShared(context.Background(), ts.URL, q); a3.text != "Hello, world." || !a3.shared {
		t.Fatalf("late asker = %+v", a3)
	}
	if calls, _ := b.count(); calls != 1 {
		t.Fatalf("model calls = %d, want 1 for three askers", calls)
	}

	// another prompt, another effort, or no "share": calls of their own
	for _, body := range []string{
		`{"system":"fix it","prompt":"helo wrold!","options":{"temperature":0},"share":true}`,
		`{"system":"fix it","prompt":"helo wrold","effort":"low","options":{"temperature":0},"share":true}`,
		`{"system":"fix it","prompt":"helo wrold","options":{"temperature":0}}`,
	} {
		if a := askShared(context.Background(), ts.URL, body); a.text != "Hello, world." || a.shared {
			t.Fatalf("%s = %+v", body, a)
		}
	}
	if calls, _ := b.count(); calls != 4 {
		t.Fatalf("model calls = %d, want 4", calls)
	}
}

// A shared call outlives an asker who hangs up while another still waits,
// and is cancelled when the last one has gone; the next ask starts over. A
// failed call is not kept either.
func TestChatCompleteShareLeave(t *testing.T) {
	b := newShareBackend()
	defer b.Close()
	s := New(&config.Config{Ollama: config.OllamaConfig{BaseURL: b.URL, Model: "m"}}, nil, nil, "", t.TempDir())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	const q = `{"prompt":"helo wrold","share":true}`
	askers := func() int {
		s.completeMu.Lock()
		defer s.completeMu.Unlock()
		n := 0
		for _, f := range s.completeFlights {
			f.mu.Lock()
			n += f.askers
			f.mu.Unlock()
		}
		return n
	}

	ctx1, hangUp1 := context.WithCancel(context.Background())
	ctx2, hangUp2 := context.WithCancel(context.Background())
	first, second := make(chan shareAnswer, 1), make(chan shareAnswer, 1)
	go func() { first <- askShared(ctx1, ts.URL, q) }()
	<-b.started
	go func() { second <- askShared(ctx2, ts.URL, q) }()
	waitFor(t, "two askers", func() bool { return askers() == 2 })
	hangUp1() // the one that started the call goes; the call stays
	<-first
	waitFor(t, "one asker left", func() bool { return askers() == 1 })
	if _, gone := b.count(); gone != 0 {
		t.Fatal("the model call went with its first asker")
	}
	hangUp2() // nobody is left: the call is cancelled
	<-second
	waitFor(t, "the model call to be cancelled", func() bool { _, gone := b.count(); return gone == 1 })
	s.completeMu.Lock()
	n := len(s.completeFlights)
	s.completeMu.Unlock()
	if n != 0 {
		t.Fatalf("flights after everyone left = %d", n)
	}

	// asked again: a fresh call, which this time fails — and is not kept
	b.mu.Lock()
	b.failing = true
	b.mu.Unlock()
	if a := askShared(context.Background(), ts.URL, q); !strings.Contains(a.fail, "model choked") {
		t.Fatalf("failed call = %+v", a)
	}
	b.mu.Lock()
	b.failing = false
	b.mu.Unlock()
	close(b.release)
	before, _ := b.count()
	if a := askShared(context.Background(), ts.URL, q); a.text != "Hello, world." || a.shared {
		t.Fatalf("after the failure = %+v", a)
	}
	if calls, _ := b.count(); calls != before+1 {
		t.Fatalf("model calls after the failure = %d, want %d", calls, before+1)
	}
}
