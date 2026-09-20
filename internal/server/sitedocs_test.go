package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The subset of Markdown the pages are written in, each construct as the
// page will carry it.
func TestMarkdownRender(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"# A Title", `<h1 id="a-title">A Title</h1>`},
		{"## Linux requirements", `<h2 id="linux-requirements">Linux requirements</h2>`},
		{"plain words", "<p>plain words</p>"},
		{"one line\nand its second", "<p>one line and its second</p>"},
		{"a `flag` in text", "<p>a <code>flag</code> in text</p>"},
		{"**bold** and *soft*", "<p><b>bold</b> and <i>soft</i></p>"},
		{"[the source](https://github.com/livid/exe)", `<a href="https://github.com/livid/exe" target="_blank" rel="noopener">the source</a>`},
		{"[the page](/docs/ssh)", `<a href="/docs/ssh">the page</a>`},
		{"- one\n- two", "<ul>\n<li>one</li>\n<li>two</li>\n</ul>"},
		{"1. first\n2. second", "<ol>\n<li>first</li>\n<li>second</li>\n</ol>"},
		{"- one\n  wrapped on", "<li>one wrapped on</li>"},
		{"| key | meaning |\n|---|---|\n| `listen` | where |", "<th>key</th><th>meaning</th>"},
		{"| key | meaning |\n|---|---|\n| `listen` | where |", "<td><code>listen</code></td><td>where</td>"},
		{"```sh\n$ ./exe serve  # the daemon\n```", `<span class="p">$</span> ./exe serve<span class="c">  # the daemon</span>`},
		{"```\n# a comment\n```", `<span class="c"># a comment</span>`},
		{"![the desktop](screenshot.png)", `<p class="shot"><img src="screenshot.png" alt="the desktop"></p>`},
		// what is not markup is text, and text is escaped
		{"a < b & c", "<p>a &lt; b &amp; c</p>"},
		{"`<script>`", "<code>&lt;script&gt;</code>"},
		{"a *star* but 2 * 3 stays", "<i>star</i>"},
	} {
		got := string(mdRender(c.src))
		if !strings.Contains(got, c.want) {
			t.Errorf("%q\n got %q\nwant it to carry %q", c.src, got, c.want)
		}
	}
	// a paragraph after a list is its own
	got := string(mdRender("- one\n- two\n\nafter"))
	if !strings.Contains(got, "</ul>\n<p>after</p>") {
		t.Errorf("a list running into a paragraph: %q", got)
	}
}

// Every page of the documentation renders, from the index to the
// manual the desktop shares.
func TestSiteDocs(t *testing.T) {
	h := SiteHandler(nil)
	code, body := getDoc(t, h, "/docs/")
	if code != http.StatusOK {
		t.Fatalf("the index answered %d", code)
	}
	for _, d := range siteDocs {
		if !strings.Contains(body, `href="/docs/`+d.Slug+`"`) || !strings.Contains(body, d.Title) {
			t.Errorf("the index does not offer %s", d.Slug)
		}
		code, page := getDoc(t, h, "/docs/"+d.Slug)
		if code != http.StatusOK {
			t.Fatalf("/docs/%s answered %d", d.Slug, code)
		}
		if !strings.Contains(page, "<h1") {
			t.Errorf("/docs/%s has no heading", d.Slug)
		}
		// a fence or a heading mark left in the output would be markdown
		// the renderer did not know (a link may legitimately appear
		// inside a code span, as the manual's own does)
		if strings.Contains(page, "```") || strings.Contains(page, "\n## ") {
			t.Errorf("/docs/%s went out with unrendered markdown in it", d.Slug)
		}
		if !strings.Contains(page, `href="/docs/"`) || !strings.Contains(page, `href="/"`) {
			t.Errorf("/docs/%s has no way back", d.Slug)
		}
	}
	// the manual is read in chapters: its front lists them, each one is a
	// page, and they are the manual's own headings — not a copy of it
	_, using := getDoc(t, h, "/docs/using")
	if len(manualChapters) < 10 {
		t.Fatalf("the manual came apart into %d chapters", len(manualChapters))
	}
	for _, c := range manualChapters {
		if !strings.Contains(using, `href="/docs/using/`+c.Slug+`"`) {
			t.Errorf("the manual's contents do not offer %s", c.Slug)
		}
		code, page := getDoc(t, h, "/docs/using/"+c.Slug)
		if code != http.StatusOK {
			t.Fatalf("/docs/using/%s answered %d", c.Slug, code)
		}
		if !strings.Contains(page, "<h1") || !strings.Contains(page, `href="/docs/using"`) {
			t.Errorf("/docs/using/%s has no heading or no way back", c.Slug)
		}
		if strings.Contains(page, "```") {
			t.Errorf("/docs/using/%s went out with unrendered markdown in it", c.Slug)
		}
	}
	// every word of the manual is on exactly one chapter page
	if strings.Contains(using, "The menu bar works like") {
		t.Error("the manual's front carries a chapter's text as well")
	}
	if _, desk := getDoc(t, h, "/docs/using/the-desktop"); !strings.Contains(desk, "The menu bar works like") {
		t.Error("a chapter lost its text")
	}
	if code, _ := getDoc(t, h, "/docs/using/nothing"); code != http.StatusNotFound {
		t.Errorf("an unknown chapter answered %d", code)
	}
	if code, _ := getDoc(t, h, "/docs/nothing"); code != http.StatusNotFound {
		t.Errorf("an unknown page answered %d", code)
	}
	if code, _ := getDoc(t, h, "/docs"); code != http.StatusMovedPermanently {
		t.Errorf("/docs answered %d, want a redirect to /docs/", code)
	}
	// the chrome both the homepage and the pages wear is served once
	if code, css := getDoc(t, h, "/site.css"); code != http.StatusOK || !strings.Contains(css, ".doc table") {
		t.Errorf("the stylesheet answered %d", code)
	}
}

func getDoc(t *testing.T, h http.Handler, path string) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://exe.example"+path, nil))
	return w.Code, w.Body.String()
}

// A path in the report reads as the site does.
func TestSiteLabel(t *testing.T) {
	for path, want := range map[string]string{
		"/":               "/ (the homepage)",
		"/docs/":          "Docs: all of them",
		"/docs/config":    "Docs: Configuration",
		"/docs/using":     "Docs: Using exe",
		"/docs/nothing":   "/docs/nothing",
		"/screenshot.png": "/screenshot.png",
	} {
		if got := siteLabel(path); got != want {
			t.Errorf("%s is named %q, want %q", path, got, want)
		}
	}
}

// The reader's toolbar: it is on the documentation and nowhere else, it
// is drawn only where its buttons work, and the choice is read back
// before the page is drawn.
func TestDocsToolbar(t *testing.T) {
	h := SiteHandler(nil)
	for _, path := range []string{"/docs/", "/docs/config", "/docs/using", "/docs/using/the-hub"} {
		_, body := getDoc(t, h, path)
		for _, want := range []string{
			`<div class="strip tools">`,
			`id="smaller"`, `id="bigger"`, `id="light"`, `id="dark"`,
			`localStorage.getItem("exe-docs-size")`, // before the first paint
			`localStorage.setItem("exe-docs-theme"`, // and kept for the next visit
			`<body class="docs">`,                   // the theme reaches this page only
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s has no %s", path, want)
			}
		}
	}
	// the homepage is not the documentation: it keeps its own chrome
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://exe.example/", nil))
	if strings.Contains(w.Body.String(), `class="strip tools"`) || strings.Contains(w.Body.String(), "exe-docs-theme") {
		t.Error("the homepage carries the documentation's toolbar")
	}
}

// The edge in front of the site keeps a stylesheet and a picture for
// hours whatever the daemon says, so the pages link them under a stamp
// of the build: new bytes, new address, and the old one may be kept for
// ever without ever being wrong.
func TestSiteBuildStamp(t *testing.T) {
	h := SiteHandler(nil)
	if len(siteBuild) < 6 {
		t.Fatalf("the build stamp is %q", siteBuild)
	}
	for _, path := range []string{"/", "/docs/", "/docs/config", "/docs/using/the-hub"} {
		_, body := getDoc(t, h, path)
		if !strings.Contains(body, `href="/v`+siteBuild+`/site.css"`) {
			t.Errorf("%s does not link the stylesheet under this build", path)
		}
	}
	// the stamped address is this build's copy, kept for good
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://exe.example/v"+siteBuild+"/site.css", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), ".doc table") {
		t.Fatalf("the stamped stylesheet answered %d", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("a stamped asset is kept as %q", got)
	}
	if got := w.Header().Get("Content-Type"); got != "text/css; charset=utf-8" {
		t.Errorf("a stamped stylesheet is served as %q", got)
	}
	// an older stamp still serves, and the plain address still works
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://exe.example/vdeadbeef01/icon.svg", nil))
	if w.Code != http.StatusOK {
		t.Errorf("an older stamp answered %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://exe.example/site.css", nil))
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("the plain address answered %d as %q", w.Code, w.Header().Get("Cache-Control"))
	}
	// the stamp follows the bytes: the same build gives the same stamp
	if siteBuild != siteStamped("/x")[2:2+len(siteBuild)] {
		t.Error("the stamp and the address it makes disagree")
	}
}

// The trail at the toolbar's left: where this page sits, each step a
// link, the page itself the last word and not a link. It is navigation,
// so it is there whether or not the script runs.
func TestDocsBreadcrumb(t *testing.T) {
	h := SiteHandler(nil)
	chev := `<svg class="sep" viewBox="0 0 4 7"`
	step := func(url, name string) string {
		return `<span class="crumb"><a href="` + url + `">` + name + `</a>` + chev
	}
	for _, c := range []struct{ path, want string }{
		{"/docs/", step("/", "exe")},
		{"/docs/config", step("/docs/", "Docs")},
		{"/docs/using", step("/docs/", "Docs")},
		{"/docs/using/the-hub", step("/docs/using", "Using exe")},
	} {
		_, body := getDoc(t, h, c.path)
		if !strings.Contains(body, c.want) {
			t.Errorf("%s does not say where it sits:\nwant %s", c.path, c.want)
		}
		if !strings.Contains(body, "<b>"+map[string]string{
			"/docs/": "Docs", "/docs/config": "Configuration",
			"/docs/using": "Using exe", "/docs/using/the-hub": "The Hub"}[c.path]+"</b>") {
			t.Errorf("%s does not end its trail with its own name", c.path)
		}
		// the trail is in the toolbar, and only there
		i, j := strings.Index(body, `class="strip tools"`), strings.Index(body, `class="strip foot"`)
		if k := strings.Index(body, `class="crumbs"`); k < i || k > j {
			t.Errorf("%s puts the trail outside the toolbar", c.path)
		}
		if strings.Count(body, `class="crumbs"`) != 1 {
			t.Errorf("%s draws the trail twice", c.path)
		}
	}
	// a chapter's trail is four steps, and the manual's is three
	_, chapter := getDoc(t, h, "/docs/using/apps")
	if n := strings.Count(chapter[strings.Index(chapter, `class="crumbs"`):strings.Index(chapter, "</nav>")], "<a href"); n != 3 {
		t.Errorf("a chapter's trail has %d links, want exe, Docs and Using exe", n)
	}
}

// The window is held between two strips of one height: the toolbar and
// the foot. The foot says who is reading, with a person before the
// count, and carries the way on as a push button.
func TestDocsFoot(t *testing.T) {
	h := SiteHandler(nil)
	_, body := getDoc(t, h, "/docs/config")
	for _, want := range []string{
		`<div class="strip foot">`,
		`<a class="btn" href="/docs/using">Next<span class="t">: Using exe</span></a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the foot has no %s", want)
		}
	}
	// the last page in the order has nowhere to go on to
	_, last := getDoc(t, h, "/docs/using/"+manualChapters[len(manualChapters)-1].Slug)
	if strings.Contains(last, `>Next<`) {
		t.Error("the last page offers a next one")
	}
	// the person is drawn, not spelt, and only where there is a count
	_, idx := getDoc(t, h, "/docs/")
	if strings.Contains(idx, `class="who"`) {
		t.Error("a page with nobody reading it still draws the reader")
	}
}

// The contents button opens the directory over the page being read: every
// page, the manual's chapters under them, and the page asked from named
// rather than offered. Without the script the same button is a link to
// the index, which is that list on a page of its own.
func TestDocsContents(t *testing.T) {
	h := SiteHandler(nil)
	_, body := getDoc(t, h, "/docs/using/the-hub")
	for _, want := range []string{
		`<a class="btn toc" href="/docs/" title="Contents"`, // a link first, a modal with script
		`<svg class="ix" viewBox="0 0 11 10"`,               // the icon is drawn, not spelt
		`<dialog class="tocbox"`,
		`box.showModal()`,
		`<a href="/docs/using/the-hub" class="at">The Hub</a>`, // where the reader is
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the contents has no %s", want)
		}
	}
	// every page and every chapter is in it
	dir := body[strings.Index(body, `<dialog class="tocbox"`):strings.Index(body, "</dialog>")]
	for _, d := range siteDocs {
		if !strings.Contains(dir, `href="/docs/`+d.Slug+`"`) {
			t.Errorf("the directory is missing %s", d.Slug)
		}
	}
	for _, c := range manualChapters {
		if !strings.Contains(dir, `href="/docs/using/`+c.Slug+`"`) {
			t.Errorf("the directory is missing the chapter %s", c.Slug)
		}
	}
	if n := strings.Count(dir, "<li>"); n != len(siteDocs)+len(manualChapters) {
		t.Errorf("the directory has %d lines, want %d", n, len(siteDocs)+len(manualChapters))
	}
	// the button sits before the way on
	foot := body[strings.Index(body, `<div class="strip foot">`):]
	if i, j := strings.Index(foot, `class="btn toc"`), strings.Index(foot, `>Next<`); i < 0 || j < 0 || i > j {
		t.Error("the contents button does not come before the way on")
	}
}
