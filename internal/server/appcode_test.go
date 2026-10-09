package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func appVersionOf(t *testing.T, base, name string) string {
	t.Helper()
	resp, err := http.Get(base + "/v1/apps")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var apps []appMeta
	json.NewDecoder(resp.Body).Decode(&apps)
	for _, a := range apps {
		if a.Name == name {
			return a.Version
		}
	}
	t.Fatalf("%s not listed", name)
	return ""
}

// Every app carries the version of the code its window runs; a disk bundle's
// moves when one of its files does.
func TestAppsCarryTheirCodeVersion(t *testing.T) {
	a := newTestNode(t)
	a.installBundle(t, "Notes")
	v1 := appVersionOf(t, a.ts.URL, "Notes")
	if len(v1) != 12 {
		t.Fatalf("want a 12-hex version, got %q", v1)
	}
	if v := appVersionOf(t, a.ts.URL, "Notes"); v != v1 {
		t.Fatalf("the version must hold while nothing changes: %q then %q", v1, v)
	}
	idx := filepath.Join(a.dir, "apps", "Notes", "index.html")
	os.WriteFile(idx, []byte("<!doctype html><title>Notes 2</title>"), 0o644)
	if v := appVersionOf(t, a.ts.URL, "Notes"); v == v1 {
		t.Fatal("a changed file must move the version")
	}
	if v := appVersionOf(t, a.ts.URL, "hub"); len(v) != 12 {
		t.Fatalf("a built-in app carries a version too: %q", v)
	}
}

// A desk listening on the app-data stream hears that an app's code changed;
// once no desk listens, the watch stops.
func TestAppCodeChangeIsAnnounced(t *testing.T) {
	a := newTestNode(t)
	a.installBundle(t, "Notes")
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", a.ts.URL+"/v1/apps/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan map[string]any, 8)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data: ") {
				var m map[string]any
				json.Unmarshal([]byte(line[6:]), &m)
				events <- m
			}
		}
	}()
	time.Sleep(300 * time.Millisecond) // the watch takes its first look
	idx := filepath.Join(a.dir, "apps", "Notes", "index.html")
	os.WriteFile(idx, []byte("<!doctype html><title>a newer Notes</title>"), 0o644)
	want := appVersionOf(t, a.ts.URL, "Notes")
	deadline := time.After(3*appCodeEvery + time.Second)
	for got := false; !got; {
		select {
		case ev := <-events:
			if ev["app"] == "@apps" && ev["path"] == "Notes" {
				if ev["version"] != want {
					t.Fatalf("announced %v, the list says %s", ev["version"], want)
				}
				got = true
			}
		case <-deadline:
			t.Fatal("no @apps event for the changed bundle")
		}
	}
	cancel()
	resp.Body.Close()
	deadline = time.After(3*appCodeEvery + time.Second)
	for {
		a.srv.appEv.mu.Lock()
		w := a.srv.appEv.watching
		a.srv.appEv.mu.Unlock()
		if !w {
			return
		}
		select {
		case <-deadline:
			t.Fatal("the watch must stop when no desk listens")
		case <-time.After(100 * time.Millisecond):
		}
	}
}
