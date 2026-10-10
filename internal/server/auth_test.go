package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"exe/internal/config"
)

// The desk asks /v1/auth whether the token it holds is the right one, and
// whether one is asked for at all, before it tells its user anything.
func TestAuthProbe(t *testing.T) {
	for _, tc := range []struct {
		name, set, send string
		want            int
	}{
		{"none asked for", "", "", http.StatusNoContent},
		{"none asked for, one sent", "", "anything", http.StatusNoContent},
		{"asked for, none sent", "secret", "", http.StatusUnauthorized},
		{"asked for, a wrong one", "secret", "wrong", http.StatusUnauthorized},
		{"asked for, the right one", "secret", "secret", http.StatusNoContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := New(&config.Config{APIToken: tc.set}, nil, nil, "", t.TempDir()).Handler()
			r := httptest.NewRequest("GET", "/v1/auth", nil)
			if tc.send != "" {
				r.Header.Set("Authorization", "Bearer "+tc.send)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Errorf("answered %d, want %d", w.Code, tc.want)
			}
			// the desk tells the daemon's 401 from any other by this
			if named := w.Header().Get("WWW-Authenticate") != ""; named != (tc.want == http.StatusUnauthorized) {
				t.Errorf("WWW-Authenticate present: %v", named)
			}
		})
	}
}
