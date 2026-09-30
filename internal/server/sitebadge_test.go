package server

import (
	"bytes"
	"html"
	"image/gif"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	stats "github.com/livid/exe-stats"
)

// The badge is an 88×31 GIF that plays for good, served under its plain
// address for other pages to link and under the build's stamp for the
// site's own.
func TestSiteBadgeGIF(t *testing.T) {
	h := SiteHandler(nil)
	for _, path := range []string{"/badge.gif", "/v" + siteBuild + "/badge.gif"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://exe.example"+path, nil))
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/gif" {
			t.Fatalf("%s answered %d as %q", path, w.Code, w.Header().Get("Content-Type"))
		}
		g, err := gif.DecodeAll(bytes.NewReader(w.Body.Bytes()))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if b := g.Config; b.Width != 88 || b.Height != 31 {
			t.Errorf("%s is %d×%d, want 88×31", path, b.Width, b.Height)
		}
		if len(g.Image) < 2 || g.LoopCount != 0 {
			t.Errorf("%s has %d frames and loops %d times; want an animation that plays for good", path, len(g.Image), g.LoopCount)
		}
		for i, d := range g.Delay {
			if d < 2 {
				t.Errorf("%s frame %d waits %d cs; a browser would slow it to its own floor", path, i, d)
			}
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://exe.example/badge.gif", nil))
	if got := w.Header().Get("Cache-Control"); got != "max-age=14400" {
		t.Errorf("the linked address is kept as %q", got)
	}
}

// The homepage ends with the badge, and it leads to the badge's page.
func TestSiteBadgeOnTheHomepage(t *testing.T) {
	_, body := getDoc(t, SiteHandler(nil), "/")
	want := `<p class="badge"><a href="/badge/"`
	i := strings.Index(body, want)
	if i < 0 {
		t.Fatal("the homepage has no badge leading to /badge/")
	}
	if !strings.Contains(body[i:], `<img src="/v`+siteBuild+`/badge.gif" width="88" height="31"`) {
		t.Error("the homepage's badge is not this build's GIF at its own size")
	}
	if strings.Contains(body[i:], `class="window"`) || strings.Contains(body[i:], `class="foot"`) {
		t.Error("the badge is not the last thing on the homepage")
	}
}

// /badge/ hands out the badge: HTML and Markdown that link the GIF from
// this site and point at it, drawn in the site's own chrome.
func TestSiteBadgePage(t *testing.T) {
	h := SiteHandler(nil)
	code, body := getDoc(t, h, "/badge/")
	if code != http.StatusOK {
		t.Fatalf("/badge/ answered %d", code)
	}
	for _, want := range []string{
		`href="/v` + siteBuild + `/site.css"`,
		`<a class="tbox" href="/"`,
		`src="/v` + siteBuild + `/badge.gif" width="88" height="31"`,
		`<a href="/badge.gif" download="exe-badge.gif">`,
		`<button class="btn default" data-from="html">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/badge/ has no %s", want)
		}
	}
	if strings.Contains(body, "{{") {
		t.Error("/badge/ went out with an unrendered action in it")
	}
	snip := func(id string) string {
		m := regexp.MustCompile(`<pre class="term snip" id="` + id + `">([^<]*)</pre>`).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("/badge/ has no %s snippet", id)
		}
		return html.UnescapeString(m[1])
	}
	if got, want := snip("html"), `<a href="https://exe.v2core.com/"><img src="https://exe.v2core.com/badge.gif" width="88" height="31" alt="exe: a personal VM cloud"></a>`; got != want {
		t.Errorf("the HTML snippet is\n%s\nwant\n%s", got, want)
	}
	if got, want := snip("md"), `[![exe: a personal VM cloud](https://exe.v2core.com/badge.gif)](https://exe.v2core.com/)`; got != want {
		t.Errorf("the Markdown snippet is\n%s\nwant\n%s", got, want)
	}
	// the status line reads the GIF it describes
	if !strings.Contains(body, "88 × 31, "+strconv.Itoa(siteBadgeFrames)+" frames, "+siteBadgeSize) {
		t.Error("the status line does not describe this build's GIF")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "http://exe.example/badge", nil))
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/badge/" {
		t.Errorf("/badge answered %d to %q", w.Code, w.Header().Get("Location"))
	}
}

// A reader of /badge/ is counted, as a page of its own.
func TestSiteBadgeCounted(t *testing.T) {
	an := SiteStats(t.TempDir())
	if an == nil {
		t.Fatal("no stats over a fresh state directory")
	}
	defer an.Stop()
	h := SiteHandler(an)
	for _, path := range []string{"/badge/", "/badge.gif"} {
		r := httptest.NewRequest("GET", "http://exe.example"+path, nil)
		r.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 Chrome/128.0 Safari/537.36")
		r.Header.Set("Sec-Fetch-Dest", "document")
		r.Header.Set("Accept", "text/html")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s answered %d", path, w.Code)
		}
		if path == "/badge/" && !strings.Contains(w.Body.String(), `<a href="/stats">1 online</a>`) {
			t.Error("/badge/ does not say who is reading")
		}
	}
	if err := an.Flush(); err != nil {
		t.Fatal(err)
	}
	sum, err := an.DB().Summary(stats.Filter{To: time.Now().UnixMilli() + 1000})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Pageviews != 1 {
		t.Errorf("counted %d page views, want the page and not the picture", sum.Pageviews)
	}
}
