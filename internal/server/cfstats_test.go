package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"exe/internal/cf"
	"exe/internal/config"
)

type cfStatsTransport func(*http.Request) (*http.Response, error)

func (f cfStatsTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCFStatsScopeCacheAndFailure(t *testing.T) {
	requests, metricsHits, tunnelHits := 100, 0, 0
	connector := "spark"
	localDown, apiDown := false, false
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("Cloudflare token sent to local listener")
		}
		if localDown {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path == "/ready" {
			fmt.Fprintf(w, `{"connectorId":%q}`, connector)
			return
		}
		metricsHits++
		fmt.Fprintf(w, "cloudflared_tunnel_ha_connections 4\ncloudflared_tunnel_total_requests %d\ncloudflared_tunnel_concurrent_requests_per_tunnel 2\ncloudflared_tunnel_request_errors 3\nprocess_start_time_seconds 1700000000\n", requests)
	}))
	defer metrics.Close()
	old := cfStatsHTTPClient
	cfStatsHTTPClient = &http.Client{Transport: cfStatsTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.cloudflare.com" {
			return http.DefaultTransport.RoundTrip(r)
		}
		tunnelHits++
		body, status := `{"success":true,"result":{"name":"planet","status":"healthy","connections":[{"id":"1","client_id":"spark","colo_name":"lax01"},{"id":"2","client_id":"planet","colo_name":"lax12"}]}}`, 200
		if apiDown {
			body, status = `{"success":false,"errors":[{"message":"unavailable"}]}`, 503
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { cfStatsHTTPClient = old })
	cfg := &config.Config{Cloudflare: config.CloudflareConfig{APIToken: "secret", AccountID: "account", TunnelID: "planet", MetricsURL: metrics.URL}}
	s := New(cfg, nil, nil, "", t.TempDir())
	get := func() cfStatsResponse {
		w := httptest.NewRecorder()
		s.handleCFStats(w, httptest.NewRequest("GET", "/v1/cloudflare/stats", nil))
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("response: %d %v", w.Code, w.Header())
		}
		var v cfStatsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	expire := func() { s.cfStats.result.SampledAt = time.Now().Add(-5 * time.Second) }
	v := get()
	if v.Tunnel == nil || v.Tunnel.Replicas != 2 || v.Tunnel.Connections != 2 || v.Local == nil || v.Local.Requests != 100 || v.Local.RequestsPerSecond != nil {
		t.Fatalf("first sample: %+v", v)
	}
	get()
	if metricsHits != 1 || tunnelHits != 1 {
		t.Fatalf("cache missed: local=%d cloud=%d", metricsHits, tunnelHits)
	}
	expire()
	s.cfStats.previousAt = time.Now().Add(-5 * time.Second)
	requests = 110
	v = get()
	if v.Local == nil || v.Local.RequestsPerSecond == nil || *v.Local.RequestsPerSecond < 1.9 || *v.Local.RequestsPerSecond > 2.1 || tunnelHits != 1 {
		t.Fatalf("rate/cloud cache: %+v", v)
	}
	expire()
	connector = "another-tunnel"
	v = get()
	if v.Local != nil || v.LocalError == "" {
		t.Fatal("displayed another tunnel's local traffic")
	}
	expire()
	connector = "spark"
	localDown = true
	apiDown = true
	s.cfStats.tunnelAttempt = time.Time{}
	v = get()
	if v.Local != nil || v.LocalError == "" || v.Tunnel == nil || !v.Tunnel.Stale || v.TunnelError == "" {
		t.Fatalf("failure claimed live data: %+v", v)
	}
	changed := *cfg
	changed.Cloudflare.TunnelID = "other"
	s.cfg.Store(&changed)
	v = get()
	if v.Tunnel != nil || v.Local != nil {
		t.Fatal("configuration change retained old tunnel's stats")
	}
}

func TestCFStatsCounterRestart(t *testing.T) {
	now := time.Now()
	prev := &cf.LocalMetrics{ConnectorID: "a", Requests: 100, Started: 1}
	for _, sample := range []*cf.LocalMetrics{
		{ConnectorID: "b", Requests: 110, Started: 1},
		{ConnectorID: "a", Requests: 110, Started: 2},
		{ConnectorID: "a", Requests: 10, Started: 1},
	} {
		if localStats(sample, prev, now, now.Add(-5*time.Second)).RequestsPerSecond != nil {
			t.Error("computed a rate across a restart/reset")
		}
	}
	if localStats(prev, prev, now, now.Add(-time.Hour)).RequestsPerSecond != nil {
		t.Error("reported an hour-old average as live traffic")
	}
}

func TestCFStatsByteRate(t *testing.T) {
	sample := func(sent string) *cf.LocalMetrics {
		m, err := cf.ParseLocalMetrics(strings.NewReader(`cloudflared_tunnel_ha_connections 4
cloudflared_tunnel_total_requests 10
cloudflared_tunnel_concurrent_requests_per_tunnel 0
cloudflared_tunnel_request_errors 0
process_start_time_seconds 1
quic_client_sent_bytes{conn_index="0"} ` + sent + `
quic_client_receive_bytes{conn_index="0"} 100
`))
		if err != nil {
			t.Fatal(err)
		}
		m.ConnectorID = "a"
		return m
	}
	now := time.Now()
	s := localStats(sample("6000"), sample("1000"), now, now.Add(-5*time.Second))
	if s.SentBytesPerSecond == nil || *s.SentBytesPerSecond != 1000 || s.ReceivedBytesPerSecond == nil || *s.ReceivedBytesPerSecond != 0 {
		t.Fatalf("byte rates: %v %v", s.SentBytesPerSecond, s.ReceivedBytesPerSecond)
	}
	restarted := sample("6000")
	restarted.Started = 2
	if localStats(restarted, sample("1000"), now, now.Add(-5*time.Second)).SentBytesPerSecond != nil {
		t.Error("computed a byte rate across a cloudflared restart")
	}
	b, _ := json.Marshal(s)
	for _, key := range []string{`"sent_bytes":6000`, `"received_bytes":100`, `"sent_bytes_per_second":1000`, `"received_bytes_per_second":0`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("JSON lacks %s: %s", key, b)
		}
	}
}

func TestCFTunnelStats(t *testing.T) {
	s := tunnelStats(&cf.Tunnel{Connections: []cf.Connection{
		{ID: "1", ClientID: "a", Location: "lax12"},
		{ID: "1", ClientID: "a", Location: "lax12"},
		{ID: "2", ClientID: "a", Location: "lax01"},
		{ID: "3", ClientID: "b", Location: "lax01"},
		{ID: "4", ClientID: "gone", Location: "old", PendingReconnect: true},
	}}, time.Now())
	if s.Replicas != 2 || s.Connections != 3 || strings.Join(s.Locations, ",") != "lax01,lax12" {
		t.Fatalf("summary: %+v", s)
	}
}
