package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func accessLogServer(t *testing.T, h http.Handler) (*AccessLog, *httptest.Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "access.log")
	al, err := OpenAccessLog(path)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(al.Wrap(h))
	ts.Config.ErrorLog = log.New(io.Discard, "", 0)
	ts.Start()
	t.Cleanup(ts.Close)
	return al, ts, path
}

// accessLines waits for path to hold n lines, then returns them: a
// client can hold the whole answer before the handler returns and its
// line is written (a flushed stream; the 101 leaves inside Hijack).
func accessLines(t *testing.T, path string, n int) []string {
	t.Helper()
	var lines []string
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		b, _ := os.ReadFile(path)
		lines = nil
		if len(b) > 0 {
			lines = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		}
		if len(lines) >= n || time.Now().After(deadline) {
			return lines
		}
	}
}

func TestAccessLogLine(t *testing.T) {
	_, ts, path := accessLogServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "hello")
	}))
	req, _ := http.NewRequest("GET", ts.URL+"/v1/vms?a=1&token=s3cret", nil)
	req.Header.Set("User-Agent", "probe/1")
	req.Header.Set("X-Forwarded-For", "100.1.2.3, 10.0.0.1")
	req.Header.Set("Tailscale-User-Login", "livid@github")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	lines := accessLines(t, path, 1)
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %q", lines)
	}
	l := lines[0]
	if strings.Contains(l, "s3cret") {
		t.Fatalf("token leaked: %s", l)
	}
	for _, want := range []string{
		" 127.0.0.1:", " GET /v1/vms?a=1&token=redacted 201 5 ",
		" xff=100.1.2.3,10.0.0.1 ts=livid@github \"probe/1\"",
	} {
		if !strings.Contains(l, want) {
			t.Errorf("line %q lacks %q", l, want)
		}
	}
	if _, err := time.Parse("2006-01-02 15:04:05", l[:19]); err != nil {
		t.Errorf("line does not start with a date: %q", l)
	}
}

func TestAccessLogKeepsFlusher(t *testing.T) {
	_, ts, path := accessLogServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", 500)
			return
		}
		io.WriteString(w, "data: 1\n\n")
		fl.Flush()
	}))
	res, err := http.Get(ts.URL + "/v1/apps/events")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if l := strings.Join(accessLines(t, path, 1), "\n"); !strings.Contains(l, " GET /v1/apps/events 200 9 ") {
		t.Fatalf("line %q", l)
	}
}

// A WebSocket's line lands when it upgrades, while it is still open, and
// the handler's return adds no second one.
func TestAccessLogWebSocket(t *testing.T) {
	closed := make(chan struct{})
	_, ts, path := accessLogServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer close(closed)
		c.Read(context.Background())
		c.CloseNow()
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/v1/host/terminal?cmd=top&token=s3cret", nil)
	if err != nil {
		t.Fatal(err)
	}
	lines := accessLines(t, path, 1)
	if len(lines) != 1 || !strings.Contains(lines[0], " GET /v1/host/terminal?cmd=top&token=redacted 101 0 ") {
		t.Fatalf("open socket: lines %q", lines)
	}
	c.Close(websocket.StatusNormalClosure, "")
	<-closed
	time.Sleep(50 * time.Millisecond) // the handler's deferred log call runs after close(closed)
	if lines := accessLines(t, path, 1); len(lines) != 1 {
		t.Fatalf("closed socket: lines %q", lines)
	}
}

func TestAccessLogPanicIs500(t *testing.T) {
	_, ts, path := accessLogServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	if res, err := http.Get(ts.URL + "/v1/x"); err == nil {
		res.Body.Close()
	}
	if l := strings.Join(accessLines(t, path, 1), "\n"); !strings.Contains(l, " GET /v1/x 500 0 ") {
		t.Fatalf("line %q", l)
	}
}

func TestAccessLogRotates(t *testing.T) {
	al, ts, path := accessLogServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	al.max = 300
	for i := 0; i < 6; i++ {
		res, err := http.Get(ts.URL + "/healthz")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	old := accessLines(t, path+".1", 1)
	cur := accessLines(t, path, 6-len(old))
	if len(old)+len(cur) != 6 || len(cur) == 0 {
		t.Fatalf("old %d + current %d lines, want 6", len(old), len(cur))
	}
	if st, _ := os.Stat(path); st.Size() > al.max {
		t.Fatalf("current file %d bytes over max %d", st.Size(), al.max)
	}
}

// The Access Log tab's ring starts from the file's tail (a whole line
// first, even when the cut falls mid-line) and takes each new request.
func TestAccessLogRing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	var old strings.Builder
	for i := 0; i < 5000; i++ {
		fmt.Fprintf(&old, "2026-09-24 01:00:00 127.0.0.1:1 GET /old/%d 200 0 0s \"x\"\n", i)
	}
	os.WriteFile(path, []byte(old.String()), 0o600)
	al, err := OpenAccessLog(path)
	if err != nil {
		t.Fatal(err)
	}
	backlog, _, cancel := al.Ring.Subscribe()
	cancel()
	if len(backlog) != 1000 || !strings.Contains(backlog[999], " GET /old/4999 ") || !strings.HasPrefix(backlog[0], "2026-09-24 ") {
		t.Fatalf("seeded ring: %d lines, first %q, last %q", len(backlog), backlog[0], backlog[len(backlog)-1])
	}
	ts := httptest.NewServer(al.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})))
	defer ts.Close()
	_, ch, cancel := al.Ring.Subscribe()
	defer cancel()
	res, err := http.Get(ts.URL + "/v1/vms?token=s3cret")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	select {
	case ln := <-ch:
		if !strings.Contains(ln, " GET /v1/vms?token=redacted 200 ") {
			t.Fatalf("live line %q", ln)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no live line")
	}
}

// GET /v1/logs/access streams the ring: backlog, then live lines.
func TestAccessLogStream(t *testing.T) {
	buf := NewLogBuffer(10)
	buf.Write([]byte("one\ntwo\n"))
	s := &Server{AccessLogs: buf}
	ts := httptest.NewServer(http.HandlerFunc(s.handleAccessLogs))
	defer ts.Close()
	res, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	rd := bufio.NewReader(res.Body)
	buf.Write([]byte("three\n"))
	for _, want := range []string{"one", "two", "three"} {
		ln, err := rd.ReadString('\n')
		if err != nil || strings.TrimSpace(ln) != want {
			t.Fatalf("want %q, got %q (%v)", want, ln, err)
		}
	}
	none := httptest.NewServer(http.HandlerFunc((&Server{}).handleAccessLogs))
	defer none.Close()
	res2, err := http.Get(none.URL)
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusNotFound {
		t.Fatalf("no ring: status %d", res2.StatusCode)
	}
}
