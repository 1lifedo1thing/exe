package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"exe/internal/config"
	"exe/internal/hostmon"
)

func TestHostMonitorEndpoint(t *testing.T) {
	s := New(config.Default(), nil, nil, "", t.TempDir())
	w := httptest.NewRecorder()
	s.handleHostMonitor(w, httptest.NewRequest("GET", "/v1/host/monitor?after=0", nil))
	var got struct {
		Supported bool             `json:"supported"`
		Interval  int64            `json:"interval"`
		Info      hostmon.Info     `json:"info"`
		Samples   []hostmon.Sample `json:"samples"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" || got.Supported != hostmon.Supported {
		t.Fatalf("answer: %s", w.Body.String())
	}
	if !got.Supported {
		return
	}
	// a cold monitor has no samples yet but knows what it covers
	if got.Interval != 2000 || got.Info.Host == "" || got.Info.Cores < 1 || got.Info.MemoryTotal == 0 || got.Samples == nil || len(got.Samples) != 0 {
		t.Fatalf("cold answer: %s", w.Body.String())
	}
}

// The Activity Monitor shows another node's figures: the desk asks its own
// daemon, which asks the node, signed; the node answers a joined node only.
func TestPeersMonitor(t *testing.T) {
	a, b := newTestNode(t), newTestNode(t)
	pairNodes(t, a, b)
	get := func(url string) (int, map[string]any) {
		t.Helper()
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var got map[string]any
		json.NewDecoder(resp.Body).Decode(&got)
		return resp.StatusCode, got
	}
	code, got := get(a.ts.URL + "/v1/peers/" + b.eng.SelfID() + "/monitor?after=0")
	if code != 200 || got["supported"] != hostmon.Supported {
		t.Fatalf("through a: %d %v", code, got)
	}
	if hostmon.Supported {
		info, _ := got["info"].(map[string]any)
		if cpus, _ := info["cpus"].([]any); len(cpus) == 0 {
			t.Fatalf("no detail from the node: %v", got)
		}
	}
	// not signed: refused, whoever asks
	if code, _ := get(b.ts.URL + "/v1/peer/monitor?after=0"); code != http.StatusUnauthorized {
		t.Fatalf("unsigned: %d", code)
	}
	if code, _ := get(a.ts.URL + "/v1/peers/0123456789abcdef/monitor"); code != http.StatusNotFound {
		t.Fatalf("a node never joined: %d", code)
	}
}
