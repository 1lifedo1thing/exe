package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"exe/internal/cf"
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
	host, zone, err := s.zoneHost(r.Context(), req.Host)
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
	res, err := s.publishHost(r.Context(), host, zone, backend)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// dnslinkPath is what a DNSLink record may point at: an IPFS path of one
// segment, /ipfs/<cid> or /ipns/<name>.
var dnslinkPath = regexp.MustCompile(`^/(ipfs|ipns)/[A-Za-z0-9][A-Za-z0-9.-]{0,200}$`)

// dnslinkTTL is short because the record moves with every publish.
const dnslinkTTL = 60

func dnslinkName(host string) string { return "_dnslink." + host }

// handleDNSLinkSet points a published hostname's DNSLink record at IPFS
// content:
//
//	PUT /v1/routes/{host}/dnslink {"path": "/ipfs/bafy…"}
//
// Cloudflare then answers TXT _dnslink.<host> "dnslink=/ipfs/bafy…", so
// Kubo and IPFS gateways resolve /ipns/<host> to the same build the
// hostname serves over HTTPS (exe-planet writes each publish's CID here).
// Only a hostname exe routes may have one. DELETE removes it, and so does
// removing the route.
func (s *Server) handleDNSLinkSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !dnslinkPath.MatchString(req.Path) {
		writeErr(w, http.StatusBadRequest, errors.New("path must be /ipfs/<cid> or /ipns/<name>"))
		return
	}
	s.dnslink(w, r, func(ctx context.Context, cfc *cf.Client, name string) (map[string]any, error) {
		value := "dnslink=" + req.Path
		if err := cfc.SetTXT(ctx, name, value, dnslinkTTL); err != nil {
			return nil, err
		}
		return map[string]any{"dnslink": value}, nil
	})
}

func (s *Server) handleDNSLinkDelete(w http.ResponseWriter, r *http.Request) {
	s.dnslink(w, r, func(ctx context.Context, cfc *cf.Client, name string) (map[string]any, error) {
		if err := cfc.DeleteTXT(ctx, name); err != nil {
			return nil, err
		}
		return map[string]any{"status": "removed"}, nil
	})
}

// dnslink checks that {host} is one of exe's routes in a zone Cloudflare
// holds, then runs change on its _dnslink name in that zone.
func (s *Server) dnslink(w http.ResponseWriter, r *http.Request, change func(context.Context, *cf.Client, string) (map[string]any, error)) {
	host, zone, err := s.zoneHost(r.Context(), r.PathValue("host"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if _, ok := s.Proxy.Snapshot()[host]; !ok {
		writeErr(w, http.StatusNotFound, errors.New(host+" is not a published route"))
		return
	}
	cfc := cfClient(s.Config())
	cfc.ZoneID = zone
	if cfc.Token == "" || cfc.ZoneID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("cloudflare is not configured"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	name := dnslinkName(host)
	res, err := change(ctx, cfc, name)
	if err != nil {
		log.Printf("dnslink %s: %v", host, err)
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	res["host"], res["name"] = host, name
	log.Printf("dnslink %s: %v", host, res)
	writeJSON(w, http.StatusOK, res)
}
