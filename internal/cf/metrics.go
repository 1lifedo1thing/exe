package cf

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// LocalMetrics describes one cloudflared process, never the whole tunnel.
type LocalMetrics struct {
	ConnectorID string  `json:"connector_id"`
	Connections float64 `json:"connections"`
	Requests    float64 `json:"requests"`
	Active      float64 `json:"active_requests"`
	Errors      float64 `json:"origin_errors"`
	Started     float64 `json:"-"`
}

var metricSample = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{.*\})?\s+(\S+)(?:\s+\S+)?$`)

// ParseLocalMetrics accepts just the small set of Prometheus samples we
// display. Unknown metrics are ignored; absent/bad samples are not zeros.
func ParseLocalMetrics(r io.Reader) (*LocalMetrics, error) {
	m := &LocalMetrics{}
	fields := map[string]*float64{
		"cloudflared_tunnel_ha_connections":                 &m.Connections,
		"cloudflared_tunnel_total_requests":                 &m.Requests,
		"cloudflared_tunnel_concurrent_requests_per_tunnel": &m.Active,
		"cloudflared_tunnel_request_errors":                 &m.Errors,
		"process_start_time_seconds":                        &m.Started,
	}
	seen := map[string]bool{}
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 4096), 128*1024)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := metricSample.FindStringSubmatch(line)
		if parts == nil {
			continue
		}
		p := fields[parts[1]]
		if p == nil {
			continue
		}
		n, err := strconv.ParseFloat(parts[2], 64)
		if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			return nil, fmt.Errorf("invalid metric %s", parts[1])
		}
		*p += n // cloudflared versions may attach labels to a gauge/counter.
		seen[parts[1]] = true
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	for name := range fields {
		if !seen[name] {
			return nil, fmt.Errorf("missing metric %s", name)
		}
	}
	return m, nil
}

var localMetricsTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return t
}()

// MetricsBaseURL disallows remote hosts, credentials, paths and redirects:
// this optional integration only reads a metrics listener on this machine.
func MetricsBaseURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		raw = "http://127.0.0.1:20241"
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", errors.New("invalid local metrics URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Path != "" && u.Path != "/") || !(u.Hostname() == "localhost" || ip != nil && ip.IsLoopback()) {
		return "", errors.New("metrics URL must be an HTTP loopback address, such as http://127.0.0.1:20241")
	}
	u.Path = ""
	return u.String(), nil
}

func ReadLocalMetrics(ctx context.Context, base string, client *http.Client) (*LocalMetrics, error) {
	base, err := MetricsBaseURL(base)
	if err != nil {
		return nil, err
	}
	// Neither environment proxies nor redirects may turn a loopback metrics
	// read into a request to another machine. Never send Cloudflare API tokens.
	local := *client
	if local.Transport == nil {
		local.Transport = localMetricsTransport
	}
	local.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	get := func(path string, limit int64, allowNotReady bool) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return nil, err
		}
		resp, err := local.Do(req)
		if err != nil {
			return nil, fmt.Errorf("local cloudflared %s is unavailable", path)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK && !(allowNotReady && resp.StatusCode == http.StatusServiceUnavailable) {
			return nil, fmt.Errorf("local cloudflared %s returned HTTP %d", path, resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if err != nil {
			return nil, err
		}
		if int64(len(b)) > limit {
			return nil, errors.New("local metrics response is too large")
		}
		return b, nil
	}
	b, err := get("/ready", 4096, true)
	if err != nil {
		return nil, err
	}
	var ready struct {
		ConnectorID string `json:"connectorId"`
	}
	if err := json.Unmarshal(b, &ready); err != nil || ready.ConnectorID == "" {
		return nil, errors.New("local cloudflared did not identify its connector")
	}
	b, err = get("/metrics", 512*1024, false)
	if err != nil {
		return nil, err
	}
	m, err := ParseLocalMetrics(strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	m.ConnectorID = ready.ConnectorID
	return m, nil
}
