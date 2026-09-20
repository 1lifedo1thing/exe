package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"exe/internal/config"
	"exe/internal/proxy"
)

func TestPublishRedirect(t *testing.T) {
	p, err := proxy.New(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Proxy: p}
	s.cfg.Store(&config.Config{Cloudflare: config.CloudflareConfig{Domain: "example.com"}})
	for _, host := range []string{"example.com", " WWW.Example.com. "} {
		body, _ := json.Marshal(map[string]string{"host": host, "target": "https://exe.example.com"})
		w := httptest.NewRecorder()
		s.handleRedirectPublish(w, httptest.NewRequest("POST", "/v1/routes/redirect", strings.NewReader(string(body))))
		if w.Code != http.StatusOK {
			t.Fatalf("publish %s: %d %s", host, w.Code, w.Body.String())
		}
	}
	for _, host := range []string{"example.com", "www.example.com"} {
		w := httptest.NewRecorder()
		p.Handler().ServeHTTP(w, httptest.NewRequest("GET", "http://"+host+"/docs/?from=alias", nil))
		if w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != "https://exe.example.com/docs/?from=alias" {
			t.Fatalf("published redirect %s: %d %s", host, w.Code, w.Header().Get("Location"))
		}
	}
	// Validation happens before publishing, leaving existing routes intact.
	for _, body := range []string{
		`{"host":"evil-example.com","target":"https://exe.example.com"}`,
		`{"host":"example.com.evil.com","target":"https://exe.example.com"}`,
		`{"host":"*.example.com","target":"https://exe.example.com"}`,
		`{"host":"https://www.example.com","target":"https://exe.example.com"}`,
		`{"host":"a..example.com","target":"https://exe.example.com"}`,
		`{"host":"-a.example.com","target":"https://exe.example.com"}`,
		`{"host":"example.com","target":"https://example.com"}`,
		`{"host":"example.com","target":"https://exe.example.com/docs"}`,
		`{`,
	} {
		w := httptest.NewRecorder()
		s.handleRedirectPublish(w, httptest.NewRequest("POST", "/v1/routes/redirect", strings.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", body, w.Code)
		}
	}
	if routes := p.Snapshot(); len(routes) != 2 || routes["example.com"] != proxy.Redirect+"https://exe.example.com" {
		t.Fatalf("invalid request changed routes: %v", routes)
	}
	s.cfg.Store(&config.Config{})
	w := httptest.NewRecorder()
	s.handleRedirectPublish(w, httptest.NewRequest("POST", "/v1/routes/redirect", strings.NewReader(`{"host":"example.com","target":"https://exe.example.com"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unconfigured domain: %d", w.Code)
	}
}
