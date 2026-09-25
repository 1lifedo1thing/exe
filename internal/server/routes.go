package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"exe/internal/proxy"
)

// localBackend accepts an http(s) origin a published hostname may route to
// without a VM — a daemon on this machine, such as exe-planet on
// 127.0.0.1:7799 — with no path, query, fragment or credentials, so the
// request's own path reaches the backend unchanged. Redirects and the
// daemon's builtins have routes of their own.
func localBackend(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("invalid backend url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("backend must be an http(s) origin")
	}
	if u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", errors.New("backend must be an origin with no path, query, fragment or credentials")
	}
	return u.Scheme + "://" + u.Host, nil
}

// handleRoutePublish publishes a hostname straight to a local backend:
//
//	POST /v1/routes {"host": "charts.example.com", "backend": "http://127.0.0.1:7799"}
//
// The same DNS record, tunnel ingress and proxy route a VM's Expose tab
// creates, for a service that is not a VM. DELETE /v1/routes/{host}
// removes it.
func (s *Server) handleRoutePublish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host    string `json:"host"`
		Backend string `json:"backend"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	host, err := zoneHost(req.Host, s.Config().Cloudflare.Domain)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.HasPrefix(req.Backend, proxy.Redirect) || strings.HasPrefix(req.Backend, proxy.Builtin) {
		writeErr(w, http.StatusBadRequest, errors.New("backend must be an http(s) origin; redirects use /v1/routes/redirect"))
		return
	}
	backend, err := localBackend(req.Backend)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.publishHost(r.Context(), host, backend)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
