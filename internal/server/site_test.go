package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The homepage comes out of the binary: the page itself, its picture, and
// the desktop's icons, each with the caching a public page wants; nothing
// else is served from that host.
func TestSiteHandler(t *testing.T) {
	h := SiteHandler()
	for _, tc := range []struct {
		path, kind, cache, want string
	}{
		{"/", "text/html; charset=utf-8", "no-cache", "<title>exe"},
		{"/index.html", "text/html; charset=utf-8", "no-cache", "<title>exe"},
		{"/screenshot.png", "image/png", "max-age=14400", "PNG"},
		{"/icon.svg", "image/svg+xml", "max-age=14400", "<svg"},
		{"/icon-192.png", "image/png", "max-age=14400", "PNG"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "http://exe.example.com"+tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", tc.path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != tc.kind {
			t.Errorf("%s: type %q, want %q", tc.path, got, tc.kind)
		}
		if got := rec.Header().Get("Cache-Control"); got != tc.cache {
			t.Errorf("%s: cache %q, want %q", tc.path, got, tc.cache)
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: body does not carry %q", tc.path, tc.want)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://exe.example.com/secrets", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown path answered %d", rec.Code)
	}
}

// The page the daemon ships is the one in the repository, with its own
// assets and no absolute link back to a machine.
func TestSitePageAssets(t *testing.T) {
	b, err := siteFS.ReadFile("site/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	for _, want := range []string{`src="icon.svg"`, `src="screenshot.png"`, `href="icon-192.png"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not reference %s", want)
		}
	}
	if strings.Contains(page, "127.0.0.1:7777/ui") {
		t.Error("the page links to a local desktop")
	}
}
