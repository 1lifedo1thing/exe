package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"exe/internal/proxy"
)

// zoneHost accepts a hostname exe may publish, lower-cased and without a
// trailing dot, and names the Cloudflare zone its DNS record goes in: the
// configured zone for the configured domain and its subdomains, otherwise
// another zone the API token holds, such as a second domain on the account.
func (s *Server) zoneHost(ctx context.Context, host string) (string, string, error) {
	cfg := s.Config()
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(cfg.Cloudflare.Domain)), ".")
	if domain == "" {
		return "", "", errors.New("cloudflare.domain is not configured")
	}
	if len(host) > 253 {
		return "", "", errors.New("hostname is too long")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", "", errors.New("invalid hostname")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", "", errors.New("invalid hostname")
			}
		}
	}
	if host == domain || strings.HasSuffix(host, "."+domain) {
		return host, cfg.Cloudflare.ZoneID, nil
	}
	outside := errors.New("host must be in the configured domain or another zone the Cloudflare token holds")
	cfc := cfClient(cfg)
	if !cfc.Configured() {
		return "", "", outside
	}
	zone, err := cfc.ZoneFor(ctx, host)
	if err != nil {
		return "", "", fmt.Errorf("zone lookup: %w", err)
	}
	if zone == "" {
		return "", "", outside
	}
	return host, zone, nil
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
	host, zone, err := s.zoneHost(r.Context(), req.Host)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	backend, err := proxy.RedirectBackend(host, req.Target)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.publishHost(r.Context(), host, zone, backend)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
