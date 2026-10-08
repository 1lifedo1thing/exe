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

func TestLocalMetricsBytes(t *testing.T) {
	m, err := ParseLocalMetrics(strings.NewReader(metricsFixture))
	if err != nil {
		t.Fatal(err)
	}
	if m.SentBytes != nil || m.ReceivedBytes != nil {
		t.Fatalf("an HTTP/2 connector reported bytes: %v %v", m.SentBytes, m.ReceivedBytes)
	}
	quic := metricsFixture + `# TYPE quic_client_sent_bytes counter
quic_client_sent_bytes{conn_index="0"} 1000
quic_client_sent_bytes{conn_index="1"} 500
quic_client_receive_bytes{conn_index="0"} 200
quic_client_receive_bytes{conn_index="1"} 1e2
`
	prev, err := ParseLocalMetrics(strings.NewReader(quic))
	if err != nil {
		t.Fatal(err)
	}
	if *prev.SentBytes != 1500 || *prev.ReceivedBytes != 300 {
		t.Fatalf("totals: sent %v received %v", *prev.SentBytes, *prev.ReceivedBytes)
	}
	// connection 1 reconnected and counts from zero again
	cur, err := ParseLocalMetrics(strings.NewReader(strings.NewReplacer(
		`sent_bytes{conn_index="0"} 1000`, `sent_bytes{conn_index="0"} 2000`,
		`sent_bytes{conn_index="1"} 500`, `sent_bytes{conn_index="1"} 100`,
		`receive_bytes{conn_index="0"} 200`, `receive_bytes{conn_index="0"} 250`).Replace(quic)))
	if err != nil {
		t.Fatal(err)
	}
	sent, received := cur.ByteRates(prev, 5)
	if sent == nil || *sent != 220 || received == nil || *received != 10 {
		t.Fatalf("rates over a reconnect: sent %v received %v", sent, received)
	}
	if s, r := cur.ByteRates(m, 5); s != nil || r != nil {
		t.Error("a rate against a sample without byte counters")
	}
	broken, err := ParseLocalMetrics(strings.NewReader(strings.Replace(quic, "1e2", "NaN", 1)))
	if err != nil {
		t.Fatalf("a bad byte sample failed the whole read: %v", err)
	}
	if broken.ReceivedBytes != nil || broken.SentBytes == nil {
		t.Errorf("a bad byte sample: sent %v received %v", broken.SentBytes, broken.ReceivedBytes)
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
