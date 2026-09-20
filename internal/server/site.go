package server

import (
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	stats "github.com/livid/exe-stats"
	_ "modernc.org/sqlite"
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

//go:embed site/index.html site/site.css site/screenshot.png
var siteFS embed.FS

// SiteBackend is the proxy backend that names this handler. `exe site`
// writes it into the route table; the proxy serves it from the daemon.
const SiteBackend = "exe:site"

// siteBuild names this binary's copy of the site's assets: a short hash
// over the bytes of every file a page links to. The pages link them
// under /v<build>/, so a rebuild that changes one changes its address —
// which is the only way to be sure a reader sees it. The edge in front
// of this site raises the lifetime of a stylesheet or a picture to four
// hours whatever the daemon answers (seen on exe.v2core.com: a
// no-cache stylesheet came back max-age=14400), so a stamped address is
// what makes a change arrive at once. The pages themselves are no-cache
// and are not cached, so they always name the current stamp.
var siteBuild = func() string {
	sum := sha256.New()
	for _, n := range []string{"/site.css", "/icon.svg", "/icon-192.png", "/screenshot.png"} {
		f := siteFiles[n]
		var b []byte
		if f.fromUI {
			b, _ = uiFS.ReadFile(f.name)
		} else {
			b, _ = f.fs.ReadFile(f.name)
		}
		sum.Write(b)
	}
	return hex.EncodeToString(sum.Sum(nil))[:10]
}()

// siteStamped is the address a page links an asset by: /v<build>/<name>.
func siteStamped(name string) string { return "/v" + siteBuild + name }

// siteStamp matches an address a page linked, whatever build made it.
var siteStamp = regexp.MustCompile(`^/v[0-9a-f]{6,32}(/.+)$`)

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
	"/site.css":       {fs: siteFS, name: "site/site.css", kind: "text/css; charset=utf-8", maxAge: "no-cache"},
	"/screenshot.png": {fs: siteFS, name: "site/screenshot.png", kind: "image/png", maxAge: "max-age=14400"},
	"/icon.svg":       {name: "ui/icon.svg", kind: "image/svg+xml", maxAge: "max-age=14400", fromUI: true},
	"/icon-192.png":   {name: "ui/icon-192.png", kind: "image/png", maxAge: "max-age=14400", fromUI: true},
}

// SiteStats opens the homepage's own analytics — github.com/livid/exe-stats,
// the same package that draws the hub's /stats, over a database of this
// node's own (stats.db in the state directory, nothing to do with the
// hub's). A failure here only means the page is not counted.
func SiteStats(stateDir string) *stats.Stats {
	db, err := sql.Open("sqlite", filepath.Join(stateDir, "stats.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		log.Printf("site: stats: %v (the homepage will not be counted)", err)
		return nil
	}
	db.SetMaxOpenConns(1)
	an, err := stats.New(db, stats.Options{
		Location:  time.Local,
		Title:     "Stats · exe",
		HomeURL:   "/",
		HomeLabel: "Back to the homepage",
		PathLabel: siteLabel,
	})
	if err != nil {
		log.Printf("site: stats: %v (the homepage will not be counted)", err)
		db.Close()
		return nil
	}
	return an // the caller starts Run
}

// siteLabel names a path in the stats: the homepage as itself and a
// documentation page by its title, so the report reads as the site does.
func siteLabel(path string) string {
	if path == "/" {
		return "/ (the homepage)"
	}
	if slug, ok := strings.CutPrefix(path, "/docs/"); ok {
		if slug == "" {
			return "Docs: all of them"
		}
		if ch, ok := strings.CutPrefix(slug, "using/"); ok {
			if i, ok := manualChapterAt(ch); ok {
				return "Manual: " + manualChapters[i].Title
			}
		}
		for _, d := range siteDocs {
			if d.Slug == slug {
				return "Docs: " + d.Title
			}
		}
	}
	return path
}

// sitePageTmpl is the page with the one thing about it that is not the
// same for everyone: how many are reading it. It is rendered per visit
// (the page is revalidated every time anyway), so the number is there in
// the first paint and nothing moves afterwards.
var sitePageTmpl = template.Must(template.New("site").Parse(string(mustSiteFile("site/index.html"))))

func mustSiteFile(name string) []byte {
	b, err := siteFS.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return b
}

// SiteHandler serves the homepage on the proxy listener, its readers
// counted and read back at /stats. The page itself is revalidated on
// every visit, so a new binary shows at once; its pictures may sit in a
// cache for four hours.
func SiteHandler(an *stats.Stats) http.Handler {
	page := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sitePage(w, r, an) })
	if an == nil {
		// without stats the site is the page and its documentation, and
		// nothing is counted
		mux := http.NewServeMux()
		docs := siteDocsHandler(nil)
		mux.Handle("GET /docs/{$}", docs)
		mux.Handle("GET /docs/{page}", docs)
		mux.Handle("GET /docs/using/{chapter}", docs)
		mux.Handle("GET /docs", http.RedirectHandler("/docs/", http.StatusMovedPermanently))
		mux.Handle("/", page)
		return mux
	}
	mux := http.NewServeMux()
	// the pages are counted; the stylesheet, the pictures and the icons
	// are not a visit
	mux.Handle("GET /{$}", an.Counted("home", page))
	docs := siteDocsHandler(an)
	mux.Handle("GET /docs/{$}", an.Counted("docs", docs))
	mux.Handle("GET /docs/{page}", an.Counted("docs", docs))
	mux.Handle("GET /docs/using/{chapter}", an.Counted("docs", docs))
	mux.Handle("GET /docs", http.RedirectHandler("/docs/", http.StatusMovedPermanently))
	mux.Handle("GET /stats", an.PageHandler())
	mux.Handle("GET /v1/stats", an.JSONHandler())
	mux.Handle("/", page)
	return mux
}

func sitePage(w http.ResponseWriter, r *http.Request, an *stats.Stats) {
	{
		// A bookmark of /index.html is the homepage under a second name.
		// It used to be served as an alias, which meant it arrived through
		// the fallback and was never counted; counting it there instead
		// would split the homepage into two rows in the report. So it is
		// a redirect, and the query goes with it — the navigation lands on
		// "/" as a document fetch and counts once, campaign and all.
		if r.URL.Path == "/index.html" {
			to := *r.URL
			to.Path = "/"
			http.Redirect(w, r, to.RequestURI(), http.StatusMovedPermanently)
			return
		}
		// an asset asked for under a build's stamp is that build's copy
		// for good: the address changes when the bytes do, so it may be
		// kept for as long as anyone likes
		path, stamped := r.URL.Path, false
		if m := siteStamp.FindStringSubmatch(path); m != nil {
			path, stamped = m[1], true
		}
		f, ok := siteFiles[path]
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
		if stamped {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", f.maxAge)
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		if f.name != "site/index.html" {
			w.Write(b)
			return
		}
		// how many are reading: the visitors with a page in the last five
		// minutes. A person reading this very request is one of them — it
		// is being served and will be counted a moment later — so a reader
		// is never told that nobody is here. A crawler is counted as a
		// crawl and belongs to no human number, so it is told what the
		// number really is, as is a curl that counts for nothing.
		online := 0
		if an != nil {
			online = an.Online()
			if _, _, _, bot := stats.Classify(r.UserAgent()); !bot && online < 1 && an.Wanted(r, "home") {
				online = 1
			}
		}
		if err := sitePageTmpl.Execute(w, struct {
			Online int
			Build  string
		}{online, siteBuild}); err != nil {
			log.Printf("site: %v", err)
		}
	}
}
