package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"exe/internal/proxy"
)

// redirectHost allows the configured zone apex as well as its subdomains.
func redirectHost(host, domain string) (string, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if domain == "" {
		return "", errors.New("cloudflare.domain is not configured")
	}
	if host != domain && !strings.HasSuffix(host, "."+domain) {
		return "", errors.New("redirect host must be the configured domain or one of its subdomains")
	}
	if len(host) > 253 {
		return "", errors.New("redirect hostname is too long")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("invalid redirect hostname")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", errors.New("invalid redirect hostname")
			}
		}
	}
	return host, nil
}

func (s *Server) handleRedirectPublish(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Host   string `json:"host"`
		Target string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	host, err := redirectHost(req.Host, s.Config().Cloudflare.Domain)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	backend, err := proxy.RedirectBackend(host, req.Target)
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
