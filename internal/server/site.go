package server

import (
	"embed"
	"net/http"
	"strings"
)

// The project homepage — the public front door at https://<sub>.<domain>,
// as opposed to the desktop, which is this daemon's own UI. It is one
// static page in the desktop's Platinum blocks, and the daemon serves it
// out of the binary: a route whose backend is the builtin name below goes
// here instead of to a VM, so publishing it is `exe site` and nothing on
// any disk has to be kept in step.
//
// The page's icons are the desktop's own (uiFS), served here so the site
// host is self-contained and the repository keeps one copy of each.

//go:embed site/index.html site/screenshot.png
var siteFS embed.FS

// SiteBackend is the proxy backend that names this handler. `exe site`
// writes it into the route table; the proxy serves it from the daemon.
const SiteBackend = "exe:site"

// siteFile is one file of the homepage: where its bytes come from, what
// it is, and how long a browser and the edge may keep it.
type siteFile struct {
	fs     embed.FS
	name   string
	kind   string
	maxAge string
	fromUI bool
}

var siteFiles = map[string]siteFile{
	"/":               {fs: siteFS, name: "site/index.html", kind: "text/html; charset=utf-8", maxAge: "no-cache"},
	"/screenshot.png": {fs: siteFS, name: "site/screenshot.png", kind: "image/png", maxAge: "max-age=14400"},
	"/icon.svg":       {name: "ui/icon.svg", kind: "image/svg+xml", maxAge: "max-age=14400", fromUI: true},
	"/icon-192.png":   {name: "ui/icon-192.png", kind: "image/png", maxAge: "max-age=14400", fromUI: true},
}

// SiteHandler serves the homepage on the proxy listener. The page itself
// is revalidated on every visit, so a new binary shows at once; its
// pictures may sit in a cache for four hours.
func SiteHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := siteFiles[strings.TrimSuffix(r.URL.Path, "index.html")]
		if !ok {
			http.Error(w, "exe site: no such page", http.StatusNotFound)
			return
		}
		var b []byte
		var err error
		if f.fromUI {
			b, err = uiFS.ReadFile(f.name)
		} else {
			b, err = f.fs.ReadFile(f.name)
		}
		if err != nil {
			http.Error(w, "exe site: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", f.kind)
		w.Header().Set("Cache-Control", f.maxAge)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Write(b)
	})
}
