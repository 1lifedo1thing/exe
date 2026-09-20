package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	stats "github.com/livid/exe-stats"
)

// The homepage comes out of the binary: the page itself, its picture, and
// the desktop's icons, each with the caching a public page wants; nothing
// else is served from that host.
func TestSiteHandler(t *testing.T) {
	h := SiteHandler(nil)
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

// With stats the page is counted and read back at /stats on the same
// host; its pictures are not visits.
func TestSiteStats(t *testing.T) {
	an := SiteStats(t.TempDir())
	if an == nil {
		t.Fatal("no stats over a fresh state directory")
	}
	defer an.Stop()
	h := SiteHandler(an)
	nav := func(path string) int {
		r := httptest.NewRequest("GET", "http://exe.example"+path, nil)
		r.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/128.0 Safari/537.36")
		r.Header.Set("Sec-Fetch-Dest", "document")
		r.Header.Set("Accept", "text/html")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if code := nav("/"); code != http.StatusOK {
		t.Fatalf("the homepage answered %d", code)
	}
	nav("/screenshot.png")
	if err := an.Flush(); err != nil {
		t.Fatal(err)
	}
	sum, err := an.DB().Summary(stats.Filter{To: time.Now().UnixMilli() + 1000})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Pageviews != 1 || sum.Visitors != 1 {
		t.Errorf("counted %+v, want the one page view (the picture is not a visit)", sum)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://exe.example/stats", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Page views") {
		t.Errorf("the desk answered %d", w.Code)
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
