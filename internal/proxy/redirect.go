package proxy

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// Redirect marks a route answered with a permanent redirect by the proxy.
const Redirect = "redirect:"

// RedirectBackend accepts an HTTP(S) origin. Requests keep their own path
// and query; destination paths, credentials, queries and fragments would
// make that contract ambiguous, so they are rejected.
func RedirectBackend(host, target string) (string, error) {
	u, err := url.Parse(target)
	if err != nil || u == nil || (u.Scheme != "https" && u.Scheme != "http") ||
		u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" ||
		u.RawQuery != "" || u.ForceQuery || strings.Contains(target, "#") {
		return "", errors.New("redirect target must be an http:// or https:// origin without credentials, path, query or fragment")
	}
	if strings.ContainsAny(u.Host, "\\ \t\r\n") || strings.HasSuffix(u.Host, ":") {
		return "", errors.New("invalid redirect target host")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("invalid redirect target port")
		}
	}
	if strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), strings.TrimSuffix(host, ".")) {
		return "", errors.New("redirect target must use a different hostname")
	}
	u.Path = ""
	return Redirect + u.String(), nil
}
