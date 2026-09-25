package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"exe/internal/config"
)

// serviceRelayServer mounts the relay the way the daemon does, so both
// patterns and the every-method registration are part of what is tested.
func serviceRelayServer(t *testing.T, services map[string]string) *httptest.Server {
	t.Helper()
	s := &Server{StateDir: t.TempDir()}
	s.cfg.Store(&config.Config{Services: services})
	mux := http.NewServeMux()
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		mux.HandleFunc(m+" /v1/svc/{name}", s.handleServiceRelay)
		mux.HandleFunc(m+" /v1/svc/{name}/{path...}", s.handleServiceRelay)
	}
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// A write reaches the service with its method, body, path and query — the
// desk's token and cookie stay behind — and the answer comes back with its
// cookies and CORS grants stripped but otherwise untouched: no sandbox, so
// a rendered preview may run its scripts.
func TestServiceRelayCarriesAWriteAndKeepsTheAnswer(t *testing.T) {
	var got *http.Request
	var body []byte
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Set-Cookie", "a=b")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("<script>1</script>"))
	}))
	defer svc.Close()
	srv := serviceRelayServer(t, map[string]string{"planet": svc.URL})

	req, _ := http.NewRequest("POST", srv.URL+"/v1/svc/planet/v1/sites/charts/preview?kind=post&token=secret", strings.NewReader(`{"body":"# hi"}`))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Cookie", "desk=1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	answer, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || string(answer) != "<script>1</script>" {
		t.Fatalf("relayed %d %s", resp.StatusCode, answer)
	}
	if got.Method != "POST" || string(body) != `{"body":"# hi"}` || got.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("service saw %s %s %q", got.Method, got.Header.Get("Content-Type"), body)
	}
	if got.URL.Path != "/v1/sites/charts/preview" || got.URL.RawQuery != "kind=post" {
		t.Fatalf("service saw %s?%s", got.URL.Path, got.URL.RawQuery)
	}
	if got.Header.Get("Authorization") != "" || got.Header.Get("Cookie") != "" {
		t.Fatalf("the desktop's credentials reached the service: %v", got.Header)
	}
	if got.Header.Get("X-Forwarded-Host") == "" {
		t.Fatalf("no forwarded host: %v", got.Header)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "" || resp.Header.Get("Set-Cookie") != "" {
		t.Fatalf("service headers leaked: %v", resp.Header)
	}
	if resp.Header.Get("Content-Security-Policy") != "" {
		t.Fatalf("a trusted service was sandboxed: %v", resp.Header)
	}
	if resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("content type changed: %v", resp.Header)
	}
}

// The bare service path is its root; an unknown or ill-formed name is 404
// without a dial; an unreachable service is 502.
func TestServiceRelayEdges(t *testing.T) {
	var path string
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Write([]byte("root"))
	}))
	defer svc.Close()
	srv := serviceRelayServer(t, map[string]string{
		"planet": svc.URL,
		"broken": svc.URL + "/prefix",
		"gone":   "http://127.0.0.1:1",
	})
	for _, url := range []string{srv.URL + "/v1/svc/planet", srv.URL + "/v1/svc/PLANET/"} {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || string(b) != "root" || path != "/" {
			t.Fatalf("%s: %d %s at %s", url, resp.StatusCode, b, path)
		}
	}
	for _, tc := range []struct {
		url  string
		code int
	}{
		{srv.URL + "/v1/svc/nope/v1/x", http.StatusNotFound},
		{srv.URL + "/v1/svc/bad.name/v1/x", http.StatusNotFound},
		{srv.URL + "/v1/svc/broken/v1/x", http.StatusNotFound},
		{srv.URL + "/v1/svc/gone/v1/x", http.StatusBadGateway},
	} {
		resp, err := http.Get(tc.url)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.code {
			t.Errorf("%s: %d, want %d", tc.url, resp.StatusCode, tc.code)
		}
	}
}
