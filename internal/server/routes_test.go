package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"exe/internal/config"
	"exe/internal/proxy"
)

// A hostname under the configured domain routes to a local http origin:
// the proxy forwards each request's own path and query to it.
func TestPublishLocalBackend(t *testing.T) {
	var got *http.Request
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Write([]byte("site"))
	}))
	defer backend.Close()
	p, err := proxy.New(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Proxy: p}
	s.cfg.Store(&config.Config{Cloudflare: config.CloudflareConfig{Domain: "example.com"}})

	body, _ := json.Marshal(map[string]string{"host": " Charts.Example.com. ", "backend": backend.URL + "/"})
	w := httptest.NewRecorder()
	s.handleRoutePublish(w, httptest.NewRequest("POST", "/v1/routes", strings.NewReader(string(body))))
	if w.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	var res map[string]any
	json.Unmarshal(w.Body.Bytes(), &res)
	if res["host"] != "charts.example.com" || res["backend"] != backend.URL || res["url"] != "https://charts.example.com" {
		t.Fatalf("published %v", res)
	}
	if routes := p.Snapshot(); routes["charts.example.com"] != backend.URL {
		t.Fatalf("routes: %v", routes)
	}

	w = httptest.NewRecorder()
	p.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://charts.example.com/hello-world/?from=feed", nil))
	b, _ := io.ReadAll(w.Body)
	if w.Code != 200 || string(b) != "site" {
		t.Fatalf("through the proxy: %d %s", w.Code, b)
	}
	if got.URL.Path != "/hello-world/" || got.URL.RawQuery != "from=feed" || got.Host != "charts.example.com" {
		t.Fatalf("backend saw %s %s?%s", got.Host, got.URL.Path, got.URL.RawQuery)
	}

	// Validation happens before publishing, leaving the route intact.
	for _, body := range []string{
		`{"host":"evil-example.com","backend":"http://127.0.0.1:7799"}`,
		`{"host":"charts.example.com","backend":"ftp://127.0.0.1:7799"}`,
		`{"host":"charts.example.com","backend":"http://127.0.0.1:7799/sites/charts"}`,
		`{"host":"charts.example.com","backend":"http://127.0.0.1:7799/?x=1"}`,
		`{"host":"charts.example.com","backend":"http://user:pw@127.0.0.1:7799"}`,
		`{"host":"charts.example.com","backend":"redirect:https://exe.example.com"}`,
		`{"host":"charts.example.com","backend":"exe:site"}`,
		`{"host":"charts.example.com","backend":""}`,
		`{"host":"charts.example.com"}`,
		`{`,
	} {
		w := httptest.NewRecorder()
		s.handleRoutePublish(w, httptest.NewRequest("POST", "/v1/routes", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if routes := p.Snapshot(); len(routes) != 1 || routes["charts.example.com"] != backend.URL {
		t.Fatalf("invalid request changed routes: %v", routes)
	}
	s.cfg.Store(&config.Config{})
	w = httptest.NewRecorder()
	s.handleRoutePublish(w, httptest.NewRequest("POST", "/v1/routes", strings.NewReader(`{"host":"charts.example.com","backend":"http://127.0.0.1:7799"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unconfigured domain: %d", w.Code)
	}
}

// A DNSLink record is only for a hostname exe routes, and only points at
// an IPFS path; each refusal comes before Cloudflare is asked.
func TestDNSLinkRefusals(t *testing.T) {
	p, err := proxy.New(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Set("blog.example.com", "http://127.0.0.1:7799"); err != nil {
		t.Fatal(err)
	}
	s := &Server{Proxy: p}
	s.cfg.Store(&config.Config{Cloudflare: config.CloudflareConfig{Domain: "example.com"}})
	for _, tc := range []struct {
		host, body string
		code       int
	}{
		{"blog.example.com", `{"path":"bafyx"}`, http.StatusBadRequest},
		{"blog.example.com", `{"path":"/ipfs/"}`, http.StatusBadRequest},
		{"blog.example.com", `{"path":"/ipfs/bafyx/index.html"}`, http.StatusBadRequest},
		{"blog.example.com", `{"path":"/ipfs/bafy\"x"}`, http.StatusBadRequest},
		{"blog.example.com", `{"path":"/http/bafyx"}`, http.StatusBadRequest},
		{"blog.example.com", `{`, http.StatusBadRequest},
		{"other.example.com", `{"path":"/ipfs/bafyx"}`, http.StatusNotFound},
		{"evil-example.com", `{"path":"/ipfs/bafyx"}`, http.StatusBadRequest},
		// a route, but no token or zone to write with
		{"blog.example.com", `{"path":"/ipfs/bafyx"}`, http.StatusBadRequest},
	} {
		r := httptest.NewRequest("PUT", "/v1/routes/"+tc.host+"/dnslink", strings.NewReader(tc.body))
		r.SetPathValue("host", tc.host)
		w := httptest.NewRecorder()
		s.handleDNSLinkSet(w, r)
		if w.Code != tc.code {
			t.Errorf("%s %s: %d %s", tc.host, tc.body, w.Code, w.Body.String())
		}
	}
	if !dnslinkPath.MatchString("/ipfs/bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi") || !dnslinkPath.MatchString("/ipns/k51qzi5uqu5dgutdk6i1ynyzgkqngpha5xpgia3a5qqp4jsh0u4csozksxel3r") {
		t.Fatal("a real CID or IPNS name is refused")
	}
}
