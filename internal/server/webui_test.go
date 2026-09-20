package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exe/internal/config"
)

// The UI files ship in the binary and change with every deploy: each one
// carries an ETag and no-cache, so a reload revalidates with a 304 instead
// of downloading the vendor scripts again; the service worker is served
// from the root, stamped with the build.
func TestUIFilesRevalidate(t *testing.T) {
	s := New(&config.Config{}, nil, nil, "", t.TempDir())
	mux := http.NewServeMux()
	mux.Handle("GET /ui/", uiStatic)
	mux.HandleFunc("GET /sw.js", handleServiceWorker)
	mux.HandleFunc("GET /", s.handleUI)
	get := func(path, ifNoneMatch string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		if ifNoneMatch != "" {
			req.Header.Set("If-None-Match", ifNoneMatch)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	for _, p := range []string{"/", "/ui/manifest.json", "/ui/vendor/xterm.js", "/ui/offline.html", "/sw.js"} {
		rec := get(p, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d", p, rec.Code)
		}
		tag := rec.Header().Get("ETag")
		if tag == "" || rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: want an ETag and no-cache, got %q / %q", p, tag, rec.Header().Get("Cache-Control"))
		}
		if again := get(p, tag); again.Code != http.StatusNotModified {
			t.Errorf("%s: conditional GET got %d, want 304", p, again.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: empty body", p)
		}
	}
	if ct := get("/", "").Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("index content type %q", ct)
	}
	sw := get("/sw.js", "")
	if ct := sw.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("worker content type %q", ct)
	}
	body := sw.Body.String()
	if strings.Contains(body, "__EXE_BUILD__") || !strings.Contains(body, `"`+uiBuild+`"`) {
		t.Errorf("worker is not stamped with the build %q", uiBuild)
	}
	if !strings.Contains(body, "/ui/offline.html") {
		t.Errorf("worker does not precache the offline page")
	}
	if len(uiBuild) != 12 {
		t.Errorf("build stamp %q", uiBuild)
	}
	// the desktop page carries the build it was served from, and every
	// response names the build the daemon ships, so an open page can tell
	// when it has gone stale
	if len(deskBuild) != 12 {
		t.Errorf("desk build stamp %q", deskBuild)
	}
	index := get("/", "").Body.String()
	if strings.Contains(index, "__EXE_BUILD__") || !strings.Contains(index, `const UI_BUILD = "`+deskBuild+`"`) {
		t.Errorf("index is not stamped with the build %q", deskBuild)
	}
	for _, p := range []string{"/", "/healthz", "/v1/ui/state"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if got := rec.Header().Get("X-Exe-Build"); got != deskBuild {
			t.Errorf("%s: X-Exe-Build %q, want %q", p, got, deskBuild)
		}
	}
	state := httptest.NewRecorder()
	s.Handler().ServeHTTP(state, httptest.NewRequest("GET", "/v1/ui/state", nil))
	s.ui.mu.Lock()
	ev := string(s.ui.eventLocked(""))
	s.ui.mu.Unlock()
	if !strings.Contains(ev, `"build":"`+deskBuild+`"`) {
		t.Errorf("layout event does not name the build: %s", ev)
	}
	if rec := get("/ui/sw.js", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unstamped worker reachable at /ui/sw.js: %d", rec.Code)
	}
	if rec := get("/somewhere", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path got %d", rec.Code)
	}
}
