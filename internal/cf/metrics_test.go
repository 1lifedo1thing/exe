package cf

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const metricsFixture = `# HELP cloudflared_tunnel_total_requests Requests
cloudflared_tunnel_ha_connections 4
cloudflared_tunnel_total_requests 1.25e3
cloudflared_tunnel_concurrent_requests_per_tunnel{connection="a"} 2
cloudflared_tunnel_concurrent_requests_per_tunnel{connection="b"} 3
cloudflared_tunnel_request_errors 7
process_start_time_seconds 1700000000.5
unknown_metric NaN
`

func TestParseLocalMetrics(t *testing.T) {
	m, err := ParseLocalMetrics(strings.NewReader(metricsFixture))
	if err != nil {
		t.Fatal(err)
	}
	if m.Requests != 1250 || m.Active != 5 || m.Connections != 4 || m.Errors != 7 || m.Started != 1700000000.5 {
		t.Fatalf("unexpected sample: %+v", m)
	}
	for _, broken := range []string{
		strings.Replace(metricsFixture, "1.25e3", "NaN", 1),
		strings.Replace(metricsFixture, "1.25e3", "+Inf", 1),
		strings.Replace(metricsFixture, "1.25e3", "-1", 1),
		strings.Replace(metricsFixture, "cloudflared_tunnel_total_requests 1.25e3\n", "", 1),
		"<html>not metrics</html>",
	} {
		if _, err := ParseLocalMetrics(strings.NewReader(broken)); err == nil {
			t.Error("accepted invalid/missing counters")
		}
	}
}

func TestLocalMetricsBoundary(t *testing.T) {
	for _, raw := range []string{"http://example.com:20241", "http://127.0.0.1.evil.test", "http://user:secret@127.0.0.1", "file:///tmp/a", "http://127.0.0.1:20241/metrics", "http://127.0.0.1?secret=x"} {
		if _, err := MetricsBaseURL(raw); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
	for _, raw := range []string{"", "http://localhost:20241/", "http://[::1]:20241"} {
		if _, err := MetricsBaseURL(raw); err != nil {
			t.Errorf("rejected %q: %v", raw, err)
		}
	}
	redirect := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("sent a credential to metrics listener")
		}
		if redirect {
			http.Redirect(w, r, "/unexpected", http.StatusFound)
			return
		}
		if r.URL.Path == "/ready" {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"connectorId":"local","readyConnections":0}`)
		} else {
			fmt.Fprint(w, metricsFixture)
		}
	}))
	defer server.Close()
	m, err := ReadLocalMetrics(context.Background(), server.URL, server.Client())
	if err != nil || m.ConnectorID != "local" {
		t.Fatalf("readiness 503 should retain identifiable counters: %v, %v", m, err)
	}
	redirect = true
	if _, err := ReadLocalMetrics(context.Background(), server.URL, server.Client()); err == nil {
		t.Fatal("followed metrics redirect")
	}
}
