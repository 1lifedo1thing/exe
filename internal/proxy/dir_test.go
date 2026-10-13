package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// a site: a page, a stylesheet, a folder of downloads behind a password,
// a folder with no index, and a dot-file.
func dirSite(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	must := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("index.html", "<title>home</title>")
	must("site.css", "body{}")
	must("pictures/a.png", "png")
	must("dl/.password", "open sesame\n")
	must("dl/game.toast", strings.Repeat("x", 1000))
	must("dl/readme.txt", "read me")
	must(".secret", "no")
	return root
}

func get(t *testing.T, h http.Handler, target string, hdr ...string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func TestDirHandlerServesPagesAndFiles(t *testing.T) {
	h := DirHandler(dirSite(t))
	res := get(t, h, "/")
	if b, _ := io.ReadAll(res.Body); res.StatusCode != 200 || string(b) != "<title>home</title>" {
		t.Fatalf("/: %d %q", res.StatusCode, b)
	}
	if cc := res.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("a page is no-cache, got %q", cc)
	}
	res = get(t, h, "/site.css")
	if res.StatusCode != 200 || res.Header.Get("Cache-Control") != "public, max-age=600" {
		t.Fatalf("site.css: %d %q", res.StatusCode, res.Header.Get("Cache-Control"))
	}
	// a directory without an index is not listed
	if res = get(t, h, "/pictures/"); res.StatusCode != 404 {
		t.Fatalf("a listing was served: %d", res.StatusCode)
	}
	// dot-files are not there
	for _, p := range []string{"/.secret", "/dl/.password", "/dl/../.secret"} {
		if res = get(t, h, p); res.StatusCode != 404 {
			t.Fatalf("%s: %d, want 404", p, res.StatusCode)
		}
	}
	if res = get(t, h, "/nothing.html"); res.StatusCode != 404 {
		t.Fatalf("missing page: %d", res.StatusCode)
	}
}

func TestDirHandlerPassword(t *testing.T) {
	h := DirHandler(dirSite(t))
	res := get(t, h, "/dl/game.toast")
	if res.StatusCode != 401 || !strings.HasPrefix(res.Header.Get("WWW-Authenticate"), `Basic realm="dl"`) {
		t.Fatalf("without a password: %d %q", res.StatusCode, res.Header.Get("WWW-Authenticate"))
	}
	req := httptest.NewRequest(http.MethodGet, "/dl/game.toast", nil)
	req.SetBasicAuth("anyone", "wrong")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("wrong password: %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/dl/game.toast", nil)
	req.SetBasicAuth("anyone", "open sesame")
	req.Header.Set("Range", "bytes=10-19")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res = rec.Result()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode != 206 || len(b) != 10 {
		t.Fatalf("a range with the password: %d, %d bytes", res.StatusCode, len(b))
	}
	if res.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("gated files are never stored, got %q", res.Header.Get("Cache-Control"))
	}
	if res.Header.Get("Content-Disposition") != `attachment; filename="game.toast"` || res.Header.Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("a disk image is a download: %q %q", res.Header.Get("Content-Disposition"), res.Header.Get("Content-Type"))
	}
	// the gate covers everything under the directory
	if res = get(t, h, "/dl/readme.txt"); res.StatusCode != 401 {
		t.Fatalf("readme under the gate: %d", res.StatusCode)
	}
	// and nothing beside it
	if res = get(t, h, "/site.css"); res.StatusCode != 200 {
		t.Fatalf("site.css beside the gate: %d", res.StatusCode)
	}
}

func TestDirBackendRoute(t *testing.T) {
	root := dirSite(t)
	p, err := New(filepath.Join(t.TempDir(), "routes.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Set("site.example.com", Dir+root); err != nil {
		t.Fatal(err)
	}
	if err := p.Set("bad.example.com", Dir+filepath.Join(root, "nowhere")); err == nil {
		t.Fatal("a missing directory was accepted")
	}
	if err := p.Set("bad.example.com", Dir+"relative/path"); err == nil {
		t.Fatal("a relative path was accepted")
	}
	h := p.Handler()
	req := httptest.NewRequest(http.MethodGet, "http://site.example.com/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "home") {
		t.Fatalf("through the proxy: %d %q", rec.Code, rec.Body.String())
	}
	if b, err := DirBackend(root + "/"); err != nil || b != Dir+root {
		t.Fatalf("DirBackend cleans its path: %q %v", b, err)
	}
}
