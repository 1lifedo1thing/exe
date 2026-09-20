package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestRedirectSurvivesRestart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "routes.json")
	p, err := New(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"example.com", "www.example.com"} {
		if err := p.Set(host, Redirect+"https://exe.example.com/"); err != nil {
			t.Fatal(err)
		}
	}
	p, err = New(file)
	if err != nil {
		t.Fatal(err)
	}
	h := p.Handler()
	for _, host := range []string{"example.com", "WWW.EXAMPLE.COM:80"} {
		for _, method := range []string{"GET", "HEAD", "POST"} {
			for _, path := range []string{"/", "/docs/using?from=www&a=1&a=2", "/a%2Fb/%E4%BD%A0?next=%2Fdocs%3Fa%3Db", "//other.example/path", "/?"} {
				r := httptest.NewRequest(method, "http://"+host+path, nil)
				// Incoming origin requests do not have a URL host/scheme.
				r.URL.Host, r.URL.Scheme = "", ""
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != http.StatusPermanentRedirect || w.Header().Get("Location") != "https://exe.example.com"+path {
					t.Errorf("%s %s%s: %d %q", method, host, path, w.Code, w.Header().Get("Location"))
				}
				if method == "HEAD" && w.Body.Len() != 0 {
					t.Error("HEAD returned a body")
				}
			}
		}
	}
}

func TestRedirectRejectsInvalidTargets(t *testing.T) {
	p, err := New(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"", "/relative", "//exe.example.com", "ftp://exe.example.com", "javascript:alert(1)",
		"https://", "https://user:password@exe.example.com", "https://exe.example.com/docs",
		"https://exe.example.com/?q=x", "https://exe.example.com/?", "https://exe.example.com/#x",
		"https://exe.example.com/#", "https://example.com", "https://EXAMPLE.COM.:443/",
		"https://exe.example.com:99999", "https://exe.example.com:", "https://exe.example.com/%2f",
		"https://exe.example.com\r\nLocation: bad",
	} {
		if err := p.Set("example.com", Redirect+target); err == nil {
			t.Errorf("accepted %q", target)
		}
	}
	if len(p.Snapshot()) != 0 {
		t.Fatal("invalid target changed the routes")
	}
	// A manually edited or old file cannot create a self-redirect loop.
	p.routes["example.com"] = Redirect + "https://example.com"
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://example.com/", nil)
	r.URL.Host, r.URL.Scheme = "", ""
	p.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("persisted self redirect answered %d", w.Code)
	}
}
