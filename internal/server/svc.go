package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// serviceURL resolves a configured service name to its base URL: a name
// from the `services` map of config.json, lower-case letters, digits and
// hyphens, pointing at an http(s) origin without a path. The map is read
// on every request, so a service added with PUT /v1/config is reachable
// at once.
func serviceURL(services map[string]string, name string) (*url.URL, error) {
	name = strings.ToLower(name)
	if name == "" {
		return nil, errors.New("service name is required")
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return nil, errors.New("invalid service name")
		}
	}
	raw, ok := services[name]
	if !ok {
		return nil, fmt.Errorf("no service named %q", name)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("service %q has an invalid url: it must be an http(s) origin with no path", name)
	}
	return u, nil
}

// handleServiceRelay carries the desktop's calls to a local service the
// browser itself may have no road to — a daemon on this machine's
// loopback, such as exe-planet on 127.0.0.1:7799, while the desk is
// opened over Tailscale Serve or from a phone:
//
//	/v1/svc/<name>/<the service's path>?<the service's own query>
//
// Every method passes, streamed as it comes, so a service may answer with
// server-sent events. The API token is checked by the daemon in front and
// never forwarded; neither are the desktop's cookies. The service is one
// the operator named in config.json, so what comes back is trusted like
// an app's own page and is not sandboxed — a page it renders may run its
// scripts in a preview frame — but its CORS grants and cookies stay
// behind, so another site cannot read a service through us.
func (s *Server) handleServiceRelay(w http.ResponseWriter, r *http.Request) {
	target, err := serviceURL(s.Config().Services, r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	path := "/" + r.PathValue("path")
	q := r.URL.Query()
	q.Del("token")
	proxy := &httputil.ReverseProxy{
		Transport:     relayTransport,
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = target.Scheme, target.Host
			pr.Out.URL.Path, pr.Out.URL.RawPath = path, ""
			pr.Out.URL.RawQuery = q.Encode()
			pr.Out.Host = target.Host
			for _, h := range []string{"Authorization", "Cookie"} {
				pr.Out.Header.Del(h)
			}
			pr.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			for h := range resp.Header {
				if strings.HasPrefix(h, "Access-Control-") {
					resp.Header.Del(h)
				}
			}
			resp.Header.Del("Set-Cookie")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			writeErr(w, http.StatusBadGateway, fmt.Errorf("service unreachable: %w", err))
		},
	}
	proxy.ServeHTTP(w, r)
}
