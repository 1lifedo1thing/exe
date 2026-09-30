package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"exe/internal/config"
	"exe/internal/proxy"
)

// 1×1 PNG
var iconPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\xcf\xc0\xf0\x1f\x00\x05\x00\x01\xff\x89\x99=\x1d\x00\x00\x00\x00IEND\xaeB`\x82")

// A host the daemon answers itself is read through its handler; a redirect
// to a published host wears that host's icon, a redirect elsewhere none; an
// app with nothing but /favicon.ico still has an icon.
func TestAppIconBuiltinRedirectFavicon(t *testing.T) {
	px, err := proxy.New(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	pages := 0
	px.SetBuiltin(SiteBackend, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "exe.example.com" {
			t.Errorf("builtin asked as %q", r.Host)
		}
		switch r.URL.Path {
		case "/":
			pages++
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<link rel="apple-touch-icon" href="/v1/icon-192.png">`))
		case "/v1/icon-192.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write(iconPNG)
		default:
			http.NotFound(w, r)
		}
	}))
	bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/favicon.ico" {
			w.Header().Set("Content-Type", "image/x-icon")
			w.Write([]byte("\x00\x00\x01\x00icon"))
			return
		}
		http.NotFound(w, r)
	}))
	defer bare.Close()
	for host, backend := range map[string]string{
		"exe.example.com":   SiteBackend,
		"example.com":       "redirect:https://exe.example.com",
		"away.example.com":  "redirect:https://elsewhere.example.org",
		"plain.example.com": bare.URL,
	} {
		if err := px.Set(host, backend); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{Proxy: px, StateDir: t.TempDir()}
	s.cfg.Store(&config.Config{})
	get := func(host string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/v1/appicons/"+host, nil)
		r.SetPathValue("host", host)
		w := httptest.NewRecorder()
		s.handleAppIcon(w, r)
		return w
	}
	for _, host := range []string{"exe.example.com", "example.com"} {
		if w := get(host); w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Body.Len() != len(iconPNG) {
			t.Errorf("%s: %d %s %d bytes", host, w.Code, w.Header().Get("Content-Type"), w.Body.Len())
		}
	}
	if pages != 1 {
		t.Errorf("the redirect fetched its target's icon again: %d page reads", pages)
	}
	if w := get("away.example.com"); w.Code != 404 {
		t.Errorf("a redirect off this node: %d", w.Code)
	}
	if w := get("plain.example.com"); w.Code != 200 || w.Header().Get("Content-Type") != "image/x-icon" {
		t.Errorf("/favicon.ico alone: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
}
