package server

import (
	"encoding/json"
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
