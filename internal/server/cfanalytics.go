package server

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	stats "github.com/livid/exe-stats"

	"exe/internal/cf"
	"exe/internal/proxy"
)

// The Analytics system app: Cloudflare's own count of the traffic to every
// hostname this node publishes (the proxy's routes, the same registry My
// Apps shows), read from the GraphQL Analytics API with the node's
// Cloudflare token — which needs Zone → Analytics → Read besides the
// wizard's DNS and Tunnel grants.
//
// GET /v1/cloudflare/analytics?range=1h|24h|7d|30d&host=<host>&tz=<IANA zone>
//
// One GraphQL request answers a view: every exposed host's totals, and for
// the chosen host (or all of them) the chart, the totals of the span before
// and the ranked lists. A Host header that carries a port (scanners try
// Cloudflare's other HTTPS ports: tides.v2core.com:2096) is counted with its
// host. The httpRequestsAdaptiveGroups dataset reaches back 31 days and
// spans 30 at most, so 30 days has no span before to compare with. Answers
// are kept a minute per view and a view asked for twice at once is fetched
// once, whoever asks.

type anRange struct {
	Key  string
	Span time.Duration
	Step time.Duration
	Dim  string // the dataset's time dimension the chart is grouped by
	Days bool   // hours folded into the viewer's calendar days
	Prev bool   // the span before is inside the dataset's reach
}

var anRanges = []anRange{
	{Key: "1h", Span: time.Hour, Step: time.Minute, Dim: "datetimeMinute", Prev: true},
	{Key: "24h", Span: 24 * time.Hour, Step: 15 * time.Minute, Dim: "datetimeFifteenMinutes", Prev: true},
	{Key: "7d", Span: 7 * 24 * time.Hour, Step: time.Hour, Dim: "datetimeHour", Prev: true},
	{Key: "30d", Span: 30 * 24 * time.Hour, Step: time.Hour, Dim: "datetimeHour", Days: true},
}

// anMaxSpan is the dataset's longest query (maxDuration, 30 days), less a
// minute so a request built at the edge is never refused.
const anMaxSpan = 30*24*time.Hour - time.Minute

// anLag is how long Cloudflare takes to count a request: buckets that end
// inside it are drawn as still filling.
const anLag = 3 * time.Minute

const anTTL = time.Minute

var anHTTPClient = &http.Client{Timeout: 20 * time.Second}
var anNow = time.Now

type anTotals struct {
	Requests int64 `json:"requests"`
	Visits   int64 `json:"visits"`
	Bytes    int64 `json:"bytes"`
	Errors   int64 `json:"errors"`  // 5xx
	Refused  int64 `json:"refused"` // 4xx
	Bots     int64 `json:"bots"`
}

type anPoint struct {
	T        time.Time `json:"t"`
	Requests int64     `json:"requests"`
	Visits   int64     `json:"visits"`
}

type anHost struct {
	Host     string `json:"host"`
	To       string `json:"to"`
	Requests int64  `json:"requests"`
	Visits   int64  `json:"visits"`
	Bytes    int64  `json:"bytes"`
	Errors   int64  `json:"errors"`
}

type anRow struct {
	Label string `json:"label"`
	Tag   string `json:"tag,omitempty"`
	Title string `json:"title,omitempty"`
	N     int64  `json:"n"`
}

type anResponse struct {
	Range     string             `json:"range"`
	Host      string             `json:"host"`
	Zone      string             `json:"zone"`
	From      time.Time          `json:"from"`
	To        time.Time          `json:"to"`
	Step      string             `json:"step"`
	Filling   int                `json:"filling"`
	Totals    anTotals           `json:"totals"`
	Previous  *anTotals          `json:"previous"`
	Series    []anPoint          `json:"series"`
	Hosts     []anHost           `json:"hosts"`
	Lists     map[string][]anRow `json:"lists"`
	Sampled   float64            `json:"sample_interval"`
	FetchedAt time.Time          `json:"fetched_at"`
}

type anEntry struct {
	done chan struct{}
	res  *anResponse
	err  error
	at   time.Time
}

type anState struct {
	mu      sync.Mutex
	entries map[string]*anEntry
}

// anError is a failure the app shows as it is, with the status it gets.
type anError struct {
	code int
	msg  string
}

func (e *anError) Error() string { return e.msg }

func (s *Server) handleCFAnalytics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	key := q.Get("range")
	if key == "" {
		key = "24h"
	}
	i := slices.IndexFunc(anRanges, func(rg anRange) bool { return rg.Key == key })
	if i < 0 {
		writeErr(w, http.StatusBadRequest, errors.New("range must be 1h, 24h, 7d or 30d"))
		return
	}
	rg := anRanges[i]
	c := s.Config().Cloudflare
	if c.APIToken == "" || c.ZoneID == "" {
		writeErr(w, http.StatusConflict, errors.New("Cloudflare is not set up. Run Special → Cloudflare Setup Wizard… first."))
		return
	}
	var routes map[string]string
	if s.Proxy != nil {
		routes = s.Proxy.Snapshot()
	}
	host := strings.ToLower(strings.TrimSpace(q.Get("host")))
	if _, ok := routes[host]; host != "" && !ok {
		writeErr(w, http.StatusNotFound, fmt.Errorf("%s is not published from this node", host))
		return
	}
	loc := time.UTC
	if rg.Days {
		if l, err := time.LoadLocation(q.Get("tz")); err == nil && q.Get("tz") != "" {
			loc = l
		}
	}
	hosts := make([]string, 0, len(routes))
	for h := range routes {
		hosts = append(hosts, h)
	}
	slices.Sort(hosts)
	ck := strings.Join([]string{c.APIToken, c.ZoneID, rg.Key, host, loc.String(), strings.Join(hosts, ",")}, "|")

	st := &s.cfAnalytics
	st.mu.Lock()
	if st.entries == nil {
		st.entries = map[string]*anEntry{}
	}
	for k, e := range st.entries {
		if e.res != nil || e.err != nil {
			if time.Since(e.at) > 10*time.Minute {
				delete(st.entries, k)
			}
		}
	}
	e := st.entries[ck]
	fresh := e != nil && (e.res == nil && e.err == nil || // in flight
		e.err == nil && time.Since(e.at) < anTTL ||
		e.err != nil && time.Since(e.at) < 10*time.Second)
	if !fresh {
		e = &anEntry{done: make(chan struct{})}
		st.entries[ck] = e
		st.mu.Unlock()
		// the fetch is the view's, not this request's: a reader who
		// closes the window must not fail it for another waiting on it
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		res, err := s.fetchAnalytics(ctx, rg, host, loc, routes)
		cancel()
		st.mu.Lock()
		e.res, e.err, e.at = res, err, time.Now()
		if err != nil {
			e.res = nil
		}
		close(e.done)
	}
	st.mu.Unlock()
	select {
	case <-e.done:
	case <-r.Context().Done():
		return
	}
	if e.err != nil {
		var ae *anError
		if errors.As(e.err, &ae) {
			writeErr(w, ae.code, ae)
			return
		}
		writeErr(w, http.StatusBadGateway, e.err)
		return
	}
	writeJSON(w, http.StatusOK, e.res)
}

// anWindow is the view's span and its chart buckets: n steps ending with the
// one now falls in (still filling), or for 30 days the viewer's last 30
// calendar days, today the last.
func anWindow(rg anRange, now time.Time, loc *time.Location) (from time.Time, buckets []time.Time) {
	if rg.Days {
		l := now.In(loc)
		today := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
		for i := 29; i >= 0; i-- {
			buckets = append(buckets, today.AddDate(0, 0, -i))
		}
		from = buckets[0]
	} else {
		last := now.Truncate(rg.Step)
		n := int(rg.Span / rg.Step)
		for i := n - 1; i >= 0; i-- {
			buckets = append(buckets, last.Add(-time.Duration(i)*rg.Step))
		}
		from = buckets[0]
	}
	if now.Sub(from) > anMaxSpan {
		from = now.Add(-anMaxSpan)
	}
	return from, buckets
}

// anHostFilter matches the hosts and their port-carrying Host headers.
func anHostFilter(hosts []string) []any {
	or := []any{map[string]any{"clientRequestHTTPHost_in": hosts}}
	for _, h := range hosts {
		if !strings.ContainsAny(h, "%_") {
			or = append(or, map[string]any{"clientRequestHTTPHost_like": h + ":%"})
		}
	}
	return or
}

// anFold drops the port a Host header may carry.
func anFold(h string) string {
	if i := strings.LastIndexByte(h, ':'); i > 0 {
		if _, err := strconv.Atoi(h[i+1:]); err == nil {
			return h[:i]
		}
	}
	return strings.ToLower(h)
}

type anGroup struct {
	Count int64 `json:"count"`
	Sum   struct {
		Visits            int64 `json:"visits"`
		EdgeResponseBytes int64 `json:"edgeResponseBytes"`
	} `json:"sum"`
	Ratio struct {
		Status4xx float64 `json:"status4xx"`
		Status5xx float64 `json:"status5xx"`
	} `json:"ratio"`
	Avg struct {
		SampleInterval float64 `json:"sampleInterval"`
	} `json:"avg"`
	Dimensions map[string]any `json:"dimensions"`
}

func (g anGroup) dim(name string) string {
	switch v := g.Dimensions[name].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

func (g anGroup) totals() anTotals {
	return anTotals{Requests: g.Count, Visits: g.Sum.Visits, Bytes: g.Sum.EdgeResponseBytes,
		Errors:  int64(math.Round(g.Ratio.Status5xx * float64(g.Count))),
		Refused: int64(math.Round(g.Ratio.Status4xx * float64(g.Count)))}
}

const anTotalFields = `count sum { visits edgeResponseBytes } ratio { status4xx status5xx }`

func (s *Server) fetchAnalytics(ctx context.Context, rg anRange, host string, loc *time.Location, routes map[string]string) (*anResponse, error) {
	c := s.Config().Cloudflare
	now := anNow().UTC().Truncate(time.Second)
	from, buckets := anWindow(rg, now, loc)
	res := &anResponse{Range: rg.Key, Host: host, Zone: c.Domain, From: from, To: now,
		Lists: map[string][]anRow{}, Series: []anPoint{}, Hosts: []anHost{}}
	switch {
	case rg.Days:
		res.Step = "day"
	case rg.Step == time.Minute:
		res.Step = "minute"
	case rg.Step == time.Hour:
		res.Step = "hour"
	default:
		res.Step = strconv.Itoa(int(rg.Step/time.Minute)) + "m"
	}
	for _, b := range buckets {
		res.Series = append(res.Series, anPoint{T: b})
	}
	for i := len(buckets) - 1; i >= 0; i-- {
		end := now
		if i+1 < len(buckets) {
			end = buckets[i+1]
		}
		if end.Before(now.Add(-anLag)) {
			break
		}
		res.Filling++
	}
	all := make([]string, 0, len(routes))
	for h := range routes {
		all = append(all, h)
	}
	slices.Sort(all)
	to := s.anDestinations(ctx, routes)
	for _, h := range all {
		res.Hosts = append(res.Hosts, anHost{Host: h, To: to[h]})
	}
	res.FetchedAt = time.Now().UTC()
	if len(all) == 0 {
		return res, nil
	}
	sel := all
	if host != "" {
		sel = []string{host}
	}
	stamp := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	span := func(a, b time.Time, hosts []string, extra map[string]any) map[string]any {
		f := map[string]any{"datetime_geq": stamp(a), "datetime_lt": stamp(b), "requestSource": "eyeball", "OR": anHostFilter(hosts)}
		for k, v := range extra {
			f[k] = v
		}
		return f
	}
	vars := map[string]any{
		"z":    c.ZoneID,
		"all":  span(from, now, all, nil),
		"sel":  span(from, now, sel, nil),
		"bots": span(from, now, sel, map[string]any{"verifiedBotCategory_neq": ""}),
		"errs": span(from, now, sel, map[string]any{"edgeResponseStatus_geq": 400}),
	}
	const F = "ZoneHttpRequestsAdaptiveGroupsFilter_InputObject!"
	decl := "$z: String!, $all: " + F + ", $sel: " + F + ", $bots: " + F + ", $errs: " + F
	g := func(alias string, limit int, filter, fields, dims, order string) string {
		s := alias + ": httpRequestsAdaptiveGroups(limit: " + strconv.Itoa(limit) + ", filter: $" + filter
		if order != "" {
			s += ", orderBy: [" + order + "]"
		}
		s += ") { " + fields
		if dims != "" {
			s += " dimensions { " + dims + " }"
		}
		return s + " }\n"
	}
	var b strings.Builder
	b.WriteString(g("hosts", 1000, "all", anTotalFields, "clientRequestHTTPHost", "count_DESC"))
	b.WriteString(g("total", 1, "sel", anTotalFields+" avg { sampleInterval }", "", ""))
	b.WriteString(g("botTotal", 1, "bots", "count", "", ""))
	b.WriteString(g("series", 2000, "sel", "count sum { visits }", rg.Dim, rg.Dim+"_ASC"))
	b.WriteString(g("paths", 50, "sel", "count", "clientRequestHTTPHost clientRequestPath", "count_DESC"))
	b.WriteString(g("errors", 50, "errs", "count", "clientRequestHTTPHost clientRequestPath edgeResponseStatus", "count_DESC"))
	b.WriteString(g("countries", 10, "sel", "count", "clientCountryName", "count_DESC"))
	b.WriteString(g("status", 10, "sel", "count", "edgeResponseStatus", "count_DESC"))
	b.WriteString(g("cache", 10, "sel", "count", "cacheStatus", "count_DESC"))
	b.WriteString(g("types", 10, "sel", "count", "edgeResponseContentTypeName", "count_DESC"))
	b.WriteString(g("bots", 10, "bots", "count", "verifiedBotCategory", "count_DESC"))
	b.WriteString(g("browsers", 10, "sel", "count", "userAgentBrowser", "count_DESC"))
	b.WriteString(g("systems", 10, "sel", "count", "userAgentOS", "count_DESC"))
	b.WriteString(g("devices", 10, "sel", "count", "clientDeviceType", "count_DESC"))
	if rg.Prev {
		pfrom := from.Add(-now.Sub(from))
		vars["psel"] = span(pfrom, from, sel, nil)
		vars["pbots"] = span(pfrom, from, sel, map[string]any{"verifiedBotCategory_neq": ""})
		decl += ", $psel: " + F + ", $pbots: " + F
		b.WriteString(g("prevTotal", 1, "psel", anTotalFields, "", ""))
		b.WriteString(g("prevBots", 1, "pbots", "count", "", ""))
	}
	query := "query(" + decl + ") { viewer { zones(filter: {zoneTag: $z}) {\n" + b.String() + "} } }"

	client := &cf.Client{Token: c.APIToken, HTTPC: anHTTPClient}
	var out struct {
		Viewer struct {
			Zones []map[string][]anGroup `json:"zones"`
		} `json:"viewer"`
	}
	if err := client.GraphQL(ctx, query, vars, &out); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "authz") || strings.Contains(msg, "not authorized") || strings.Contains(msg, "does not have access") {
			return nil, &anError{http.StatusBadGateway, "The Cloudflare token cannot read this zone's analytics. Add Zone → Analytics → Read to it in the Cloudflare dashboard. (" + msg + ")"}
		}
		return nil, err
	}
	if len(out.Viewer.Zones) == 0 {
		return nil, &anError{http.StatusBadGateway, "Cloudflare returned no data for the zone. Is zone_id right, and can the token read it?"}
	}
	z := out.Viewer.Zones[0]

	byHost := map[string]*anHost{}
	for i := range res.Hosts {
		byHost[res.Hosts[i].Host] = &res.Hosts[i]
	}
	for _, grp := range z["hosts"] {
		h := byHost[anFold(grp.dim("clientRequestHTTPHost"))]
		if h == nil {
			continue
		}
		t := grp.totals()
		h.Requests += t.Requests
		h.Visits += t.Visits
		h.Bytes += t.Bytes
		h.Errors += t.Errors
	}
	slices.SortStableFunc(res.Hosts, func(a, b anHost) int {
		if a.Requests != b.Requests {
			if a.Requests > b.Requests {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Host, b.Host)
	})
	one := func(alias string) (anGroup, bool) {
		if gs := z[alias]; len(gs) > 0 {
			return gs[0], true
		}
		return anGroup{}, false
	}
	if t, ok := one("total"); ok {
		res.Totals = t.totals()
		res.Sampled = t.Avg.SampleInterval
	}
	if t, ok := one("botTotal"); ok {
		res.Totals.Bots = t.Count
	}
	if rg.Prev {
		p := anTotals{}
		if t, ok := one("prevTotal"); ok {
			p = t.totals()
		}
		if t, ok := one("prevBots"); ok {
			p.Bots = t.Count
		}
		res.Previous = &p
	}

	day := map[string]int{}
	for i, bk := range buckets {
		day[bk.Format("2006-01-02")] = i
	}
	for _, grp := range z["series"] {
		t, err := time.Parse(time.RFC3339, grp.dim(rg.Dim))
		if err != nil {
			continue
		}
		i := -1
		if rg.Days {
			if j, ok := day[t.In(loc).Format("2006-01-02")]; ok {
				i = j
			}
		} else if !t.Before(buckets[0]) {
			i = int(t.Sub(buckets[0]) / rg.Step)
		}
		if i < 0 || i >= len(res.Series) {
			continue
		}
		res.Series[i].Requests += grp.Count
		res.Series[i].Visits += grp.Sum.Visits
	}

	short := func(h string) string {
		if d := strings.ToLower(c.Domain); d != "" && strings.HasSuffix(h, "."+d) {
			return strings.TrimSuffix(h, "."+d)
		}
		return h
	}
	// paths are ranked per host, ports folded; with every host shown each
	// path wears its host's short name
	pathRows := func(alias string, status bool) []anRow {
		type k struct{ host, path, status string }
		n, order := map[k]int64{}, []k{}
		for _, grp := range z[alias] {
			key := k{anFold(grp.dim("clientRequestHTTPHost")), grp.dim("clientRequestPath"), ""}
			if status {
				key.status = grp.dim("edgeResponseStatus")
			}
			if _, seen := n[key]; !seen {
				order = append(order, key)
			}
			n[key] += grp.Count
		}
		slices.SortStableFunc(order, func(a, b k) int {
			if n[a] != n[b] {
				if n[a] > n[b] {
					return -1
				}
				return 1
			}
			return 0
		})
		rows := []anRow{}
		for _, key := range order {
			if len(rows) == 10 {
				break
			}
			row := anRow{Label: key.path, N: n[key], Title: "https://" + key.host + key.path}
			if host == "" {
				row.Tag = short(key.host)
			}
			if status {
				row.Tag = strings.TrimSpace(key.status + " " + row.Tag)
			}
			rows = append(rows, row)
		}
		return rows
	}
	res.Lists["paths"] = pathRows("paths", false)
	res.Lists["errors"] = pathRows("errors", true)
	list := func(alias, dim string, label func(string) (string, string)) []anRow {
		rows := []anRow{}
		for _, grp := range z[alias] {
			l, tag := label(grp.dim(dim))
			if l == "" {
				continue
			}
			rows = append(rows, anRow{Label: l, Tag: tag, N: grp.Count})
		}
		return rows
	}
	plain := func(s string) (string, string) { return s, "" }
	res.Lists["countries"] = list("countries", "clientCountryName", func(code string) (string, string) {
		switch code {
		case "", "XX":
			return "Not known", ""
		case "T1":
			return "Tor", ""
		}
		return stats.CountryName(code), code
	})
	res.Lists["status"] = list("status", "edgeResponseStatus", func(code string) (string, string) {
		return code + " " + anStatusText(code), ""
	})
	res.Lists["cache"] = list("cache", "cacheStatus", func(s string) (string, string) { return anTitle(s), "" })
	res.Lists["types"] = list("types", "edgeResponseContentTypeName", func(s string) (string, string) {
		if s == "empty" || s == "unknown" {
			return anTitle(s), ""
		}
		return strings.ToUpper(s), ""
	})
	res.Lists["bots"] = list("bots", "verifiedBotCategory", plain)
	res.Lists["browsers"] = list("browsers", "userAgentBrowser", func(s string) (string, string) { return anWords(s), "" })
	res.Lists["systems"] = list("systems", "userAgentOS", func(s string) (string, string) {
		switch s {
		case "MacOSX":
			return "macOS", ""
		case "iOS", "ChromeOS":
			return s, ""
		}
		return anWords(s), ""
	})
	res.Lists["devices"] = list("devices", "clientDeviceType", func(s string) (string, string) { return anTitle(s), "" })
	res.FetchedAt = time.Now().UTC()
	return res, nil
}

// anDestinations says where each published host leads: a VM and its port,
// a named local service, the homepage, or a redirect's target.
func (s *Server) anDestinations(ctx context.Context, routes map[string]string) map[string]string {
	vmByIP := map[string]string{}
	if s.VMs != nil {
		if vms, err := s.VMs.List(ctx); err == nil {
			for _, v := range vms {
				if v.IP != "" {
					vmByIP[v.IP] = v.Name
				}
			}
		}
	}
	svc := map[string]string{}
	for name, raw := range s.Config().Services {
		if u, err := url.Parse(raw); err == nil {
			svc[u.Scheme+"://"+u.Host] = name
		}
	}
	out := map[string]string{}
	for h, backend := range routes {
		switch {
		case backend == SiteBackend:
			out[h] = "homepage"
		case strings.HasPrefix(backend, proxy.Redirect):
			t := strings.TrimPrefix(backend, proxy.Redirect)
			if u, err := url.Parse(t); err == nil && u.Host != "" {
				t = u.Host + strings.TrimSuffix(u.Path, "/")
			}
			out[h] = "→ " + t
		case strings.HasPrefix(backend, proxy.Builtin):
			out[h] = backend
		default:
			u, err := url.Parse(backend)
			if err != nil {
				out[h] = backend
				continue
			}
			if name := vmByIP[u.Hostname()]; name != "" {
				out[h] = name + ":" + u.Port()
			} else if name := svc[u.Scheme+"://"+u.Host]; name != "" {
				out[h] = name
			} else {
				out[h] = u.Host
			}
		}
	}
	return out
}

// anStatusText names a status, Cloudflare's own 52x codes included.
func anStatusText(code string) string {
	switch code {
	case "499":
		return "Client Closed Request"
	case "520":
		return "Unknown Origin Error"
	case "521":
		return "Origin Down"
	case "522":
		return "Connection Timed Out"
	case "523":
		return "Origin Unreachable"
	case "524":
		return "Origin Timeout"
	case "525":
		return "SSL Handshake Failed"
	case "526":
		return "Invalid SSL Certificate"
	case "530":
		return "Origin Error"
	}
	n, _ := strconv.Atoi(code)
	return http.StatusText(n)
}

func anTitle(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// anWords spaces Cloudflare's joined names: ChromeMobile, Chrome Mobile.
func anWords(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' && s[i-1] >= 'a' && s[i-1] <= 'z' {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}
