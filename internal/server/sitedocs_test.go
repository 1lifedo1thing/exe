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
	// the manual is the one the desktop opens, not a copy of it
	_, using := getDoc(t, h, "/docs/using")
	if !strings.Contains(using, "Using exe") || !strings.Contains(using, "The desktop") {
		t.Error("the manual page is not the desktop's manual")
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
