package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exe/internal/config"
	"exe/internal/proxy"
	"exe/internal/vmm"
)

type anVMs struct{ vmm.Manager }

func (anVMs) List(context.Context) ([]*vmm.Info, error) {
	return []*vmm.Info{{Name: "hubvm", IP: "172.30.0.2"}}, nil
}

type anCall struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

// anServer is a node publishing four hosts of example.com — a VM's port,
// a named local service, the homepage and a redirect — whose Cloudflare
// GraphQL answers come from reply.
func anServer(t *testing.T, reply func(anCall) string) (*Server, *[]anCall) {
	t.Helper()
	calls := &[]anCall{}
	old := anHTTPClient
	anHTTPClient = &http.Client{Transport: cfStatsTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.cloudflare.com/client/v4/graphql" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("request %s %s", r.URL, r.Header.Get("Authorization"))
		}
		var c anCall
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			t.Fatal(err)
		}
		*calls = append(*calls, c)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(reply(c))), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { anHTTPClient = old })
	px, err := proxy.New(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	px.SetBuiltin(SiteBackend, http.NotFoundHandler())
	for host, backend := range map[string]string{
		"hub.example.com":  "http://172.30.0.2:7788",
		"blog.example.com": "http://127.0.0.1:7799",
		"exe.example.com":  SiteBackend,
		"example.com":      "redirect:https://exe.example.com",
	} {
		if err := px.Set(host, backend); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{VMs: anVMs{}, Proxy: px, StateDir: t.TempDir()}
	s.cfg.Store(&config.Config{
		Services:   map[string]string{"planet": "http://127.0.0.1:7799"},
		Cloudflare: config.CloudflareConfig{APIToken: "secret", ZoneID: "zone", Domain: "example.com"},
	})
	return s, calls
}

func anGet(t *testing.T, s *Server, query string) (int, anResponse, string) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleCFAnalytics(w, httptest.NewRequest("GET", "/v1/cloudflare/analytics?"+query, nil))
	var v anResponse
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Error("answer may be cached by the browser")
		}
	}
	return w.Code, v, w.Body.String()
}

const anReply = `{"data":{"viewer":{"zones":[{
 "hosts":[
  {"count":100,"sum":{"visits":40,"edgeResponseBytes":5000},"ratio":{"status4xx":0.1,"status5xx":0.02},"dimensions":{"clientRequestHTTPHost":"hub.example.com"}},
  {"count":5,"sum":{"visits":0,"edgeResponseBytes":10},"ratio":{"status4xx":1,"status5xx":0},"dimensions":{"clientRequestHTTPHost":"hub.example.com:8443"}},
  {"count":7,"sum":{"visits":3,"edgeResponseBytes":70},"ratio":{"status4xx":0,"status5xx":0},"dimensions":{"clientRequestHTTPHost":"example.com"}}],
 "total":[{"count":105,"sum":{"visits":40,"edgeResponseBytes":5010},"ratio":{"status4xx":0.2,"status5xx":0.04},"avg":{"sampleInterval":1}}],
 "botTotal":[{"count":12}],
 "prevTotal":[{"count":50,"sum":{"visits":20,"edgeResponseBytes":100},"ratio":{"status4xx":0,"status5xx":0}}],
 "prevBots":[{"count":5}],
 "series":[
  {"count":30,"sum":{"visits":9},"dimensions":{"datetimeMinute":"2026-09-29T11:00:00Z"}},
  {"count":20,"sum":{"visits":8},"dimensions":{"datetimeMinute":"2026-09-29T11:59:00Z"}},
  {"count":99,"sum":{"visits":9},"dimensions":{"datetimeMinute":"2026-09-29T10:00:00Z"}}],
 "paths":[
  {"count":60,"dimensions":{"clientRequestHTTPHost":"hub.example.com","clientRequestPath":"/stats"}},
  {"count":9,"dimensions":{"clientRequestHTTPHost":"example.com","clientRequestPath":"/"}},
  {"count":5,"dimensions":{"clientRequestHTTPHost":"hub.example.com:8443","clientRequestPath":"/stats"}}],
 "errors":[{"count":4,"dimensions":{"clientRequestHTTPHost":"hub.example.com","clientRequestPath":"/.env","edgeResponseStatus":404}}],
 "countries":[{"count":80,"dimensions":{"clientCountryName":"SG"}},{"count":2,"dimensions":{"clientCountryName":"T1"}}],
 "status":[{"count":90,"dimensions":{"edgeResponseStatus":200}},{"count":3,"dimensions":{"edgeResponseStatus":522}}],
 "cache":[{"count":90,"dimensions":{"cacheStatus":"dynamic"}}],
 "types":[{"count":90,"dimensions":{"edgeResponseContentTypeName":"html"}}],
 "bots":[{"count":12,"dimensions":{"verifiedBotCategory":"AI Crawler"}}],
 "browsers":[{"count":70,"dimensions":{"userAgentBrowser":"ChromeMobile"}}],
 "systems":[{"count":70,"dimensions":{"userAgentOS":"MacOSX"}}],
 "devices":[{"count":70,"dimensions":{"clientDeviceType":"desktop"}}]
}]}},"errors":null}`

// One GraphQL request answers a view: every published host (a Host header
// with a port folded onto its host), the chosen hosts' chart bucketed by
// the range's step, the span before, and labelled lists. The answer is kept
// a minute.
func TestCFAnalyticsView(t *testing.T) {
	old := anNow
	anNow = func() time.Time { return time.Date(2026, 9, 29, 11, 59, 30, 0, time.UTC) }
	t.Cleanup(func() { anNow = old })
	s, calls := anServer(t, func(anCall) string { return anReply })

	code, v, body := anGet(t, s, "range=1h")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if len(*calls) != 1 {
		t.Fatalf("%d GraphQL calls", len(*calls))
	}
	c := (*calls)[0]
	sel, _ := json.Marshal(c.Variables["sel"])
	for _, want := range []string{`"clientRequestHTTPHost_like":"hub.example.com:%"`, `"datetime_geq":"2026-09-29T11:00:00Z"`,
		`"datetime_lt":"2026-09-29T11:59:30Z"`, `"requestSource":"eyeball"`, `"blog.example.com"`} {
		if !strings.Contains(string(sel), want) {
			t.Errorf("filter lacks %s: %s", want, sel)
		}
	}
	if !strings.Contains(c.Query, "prevTotal:") || !strings.Contains(c.Query, "datetimeMinute_ASC") {
		t.Errorf("query: %s", c.Query)
	}
	if psel, _ := json.Marshal(c.Variables["psel"]); !strings.Contains(string(psel), `"datetime_geq":"2026-09-29T10:00:30Z","datetime_lt":"2026-09-29T11:00:00Z"`) {
		t.Errorf("span before: %s", psel)
	}

	want := []anHost{
		{Host: "hub.example.com", To: "hubvm:7788", Requests: 105, Visits: 40, Bytes: 5010, Errors: 2},
		{Host: "example.com", To: "→ exe.example.com", Requests: 7, Visits: 3, Bytes: 70},
		{Host: "blog.example.com", To: "planet"},
		{Host: "exe.example.com", To: "homepage"},
	}
	if len(v.Hosts) != len(want) {
		t.Fatalf("hosts %+v", v.Hosts)
	}
	for i := range want {
		if v.Hosts[i] != want[i] {
			t.Errorf("host %d = %+v, want %+v", i, v.Hosts[i], want[i])
		}
	}
	if v.Totals != (anTotals{Requests: 105, Visits: 40, Bytes: 5010, Errors: 4, Refused: 21, Bots: 12}) {
		t.Errorf("totals %+v", v.Totals)
	}
	if v.Previous == nil || v.Previous.Requests != 50 || v.Previous.Bots != 5 {
		t.Errorf("previous %+v", v.Previous)
	}
	if len(v.Series) != 60 || v.Series[0].Requests != 30 || v.Series[59].Requests != 20 || v.Series[59].Visits != 8 {
		t.Errorf("series %d: %+v … %+v", len(v.Series), v.Series[0], v.Series[len(v.Series)-1])
	}
	if !v.Series[0].T.Equal(time.Date(2026, 9, 29, 11, 0, 0, 0, time.UTC)) || v.Step != "minute" {
		t.Errorf("first bucket %v step %s", v.Series[0].T, v.Step)
	}
	// 11:56 ends 11:57, inside the three minutes still being counted
	if v.Filling != 4 {
		t.Errorf("filling %d", v.Filling)
	}
	if p := v.Lists["paths"]; len(p) != 2 || p[0] != (anRow{Label: "/stats", Tag: "hub", Title: "https://hub.example.com/stats", N: 65}) || p[1].Tag != "example.com" {
		t.Errorf("paths %+v", p)
	}
	if e := v.Lists["errors"]; len(e) != 1 || e[0].Tag != "404 hub" {
		t.Errorf("errors %+v", e)
	}
	for list, first := range map[string]anRow{
		"countries": {Label: "Singapore", Tag: "SG", N: 80},
		"cache":     {Label: "Dynamic", N: 90},
		"types":     {Label: "HTML", N: 90},
		"bots":      {Label: "AI Crawler", N: 12},
		"browsers":  {Label: "Chrome Mobile", N: 70},
		"systems":   {Label: "macOS", N: 70},
		"devices":   {Label: "Desktop", N: 70},
	} {
		if l := v.Lists[list]; len(l) == 0 || l[0] != first {
			t.Errorf("%s: %+v", list, l)
		}
	}
	if l := v.Lists["countries"]; len(l) != 2 || l[1].Label != "Tor" {
		t.Errorf("countries %+v", l)
	}
	if l := v.Lists["status"]; len(l) != 2 || l[1].Label != "522 Connection Timed Out" {
		t.Errorf("status %+v", l)
	}

	anGet(t, s, "range=1h")
	if len(*calls) != 1 {
		t.Errorf("a view asked for again within the minute went to Cloudflare: %d calls", len(*calls))
	}
	// one host: its own filter, and paths without the host's name
	code, v, _ = anGet(t, s, "range=1h&host=HUB.example.com")
	if code != 200 || len(*calls) != 2 || v.Host != "hub.example.com" || v.Lists["paths"][0].Tag != "" {
		t.Fatalf("one host: %d %d %+v", code, len(*calls), v.Lists["paths"])
	}
	sel, _ = json.Marshal((*calls)[1].Variables["sel"])
	if strings.Contains(string(sel), "blog") || !strings.Contains(string(sel), `"clientRequestHTTPHost_in":["hub.example.com"]`) {
		t.Errorf("one host's filter: %s", sel)
	}
	if all, _ := json.Marshal((*calls)[1].Variables["all"]); !strings.Contains(string(all), "blog.example.com") {
		t.Errorf("the host list still counts every host: %s", all)
	}
}

// 30 days are the viewer's calendar days: hours fold into the day they
// fall on in the zone asked for, and there is no span before to compare.
func TestCFAnalyticsDays(t *testing.T) {
	old := anNow
	anNow = func() time.Time { return time.Date(2026, 9, 29, 18, 0, 0, 0, time.UTC) } // 11:00 in Los Angeles
	t.Cleanup(func() { anNow = old })
	s, calls := anServer(t, func(anCall) string {
		return `{"data":{"viewer":{"zones":[{"series":[
		 {"count":1,"sum":{"visits":1},"dimensions":{"datetimeHour":"2026-09-29T06:00:00Z"}},
		 {"count":2,"sum":{"visits":0},"dimensions":{"datetimeHour":"2026-09-29T07:00:00Z"}},
		 {"count":4,"sum":{"visits":0},"dimensions":{"datetimeHour":"2026-08-31T07:00:00Z"}}]}]}}}`
	})
	code, v, body := anGet(t, s, "range=30d&tz=America/Los_Angeles")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	la, _ := time.LoadLocation("America/Los_Angeles")
	if len(v.Series) != 30 || !v.Series[0].T.Equal(time.Date(2026, 8, 31, 0, 0, 0, 0, la)) || v.Step != "day" {
		t.Fatalf("days %d from %v", len(v.Series), v.Series[0].T)
	}
	// 06:00Z is still Sep 28 in Los Angeles, 07:00Z is Sep 29
	if v.Series[28].Requests != 1 || v.Series[29].Requests != 2 || v.Series[0].Requests != 4 {
		t.Errorf("folded %+v %+v %+v", v.Series[0], v.Series[28], v.Series[29])
	}
	if v.Previous != nil || strings.Contains((*calls)[0].Query, "prevTotal") {
		t.Error("30 days compared with a span Cloudflare no longer holds")
	}
	if all, _ := json.Marshal((*calls)[0].Variables["all"]); !strings.Contains(string(all), `"datetime_geq":"2026-08-31T07:00:00Z"`) {
		t.Errorf("span %s", all)
	}
	// a zone the daemon does not know falls back to UTC days
	_, v, _ = anGet(t, s, "range=30d&tz=Nowhere/Else")
	if len(v.Series) != 30 || !v.Series[29].T.Equal(time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("UTC days end %v", v.Series[29].T)
	}
}

func TestCFAnalyticsRefusals(t *testing.T) {
	s, calls := anServer(t, func(anCall) string {
		return `{"data":null,"errors":[{"message":"not authorized for that account","extensions":{"code":"authz"}}]}`
	})
	for _, tc := range []struct {
		query string
		code  int
		says  string
	}{
		{"range=2d", 400, "range must be"},
		{"host=nowhere.example.com", 404, "not published"},
		{"range=24h", 502, "Zone → Analytics → Read"},
	} {
		code, _, body := anGet(t, s, tc.query)
		if code != tc.code || !strings.Contains(body, tc.says) {
			t.Errorf("%s: %d %s", tc.query, code, body)
		}
	}
	if len(*calls) != 1 {
		t.Errorf("refused requests reached Cloudflare: %d", len(*calls))
	}
	// a failure is kept briefly too, so a window's retries do not hammer the API
	anGet(t, s, "range=24h")
	if len(*calls) != 1 {
		t.Errorf("failure retried at once: %d", len(*calls))
	}
	s.cfg.Store(&config.Config{})
	if code, _, body := anGet(t, s, "range=24h"); code != 409 || !strings.Contains(body, "not set up") {
		t.Errorf("unconfigured: %d %s", code, body)
	}
}
