package server

// The homepage's documentation, at /docs/ — the detail that used to live
// only in the README, as pages of its own in the same Platinum blocks as
// the homepage. The pages are Markdown in site/docs, rendered here as
// the page is served: no script, nothing to build, and a change to a
// page is a change to the binary like the rest of the site.
//
// "Using exe" is not a copy of anything: it renders internal/server/docs.md,
// the manual the desktop's Help menu opens, so the two can never drift.

import (
	"embed"
	"html"
	"html/template"
	"net/http"
	"regexp"
	"strings"
)

//go:embed site/docs/*.md
var siteDocsFS embed.FS

// siteDoc is one page of the documentation.
type siteDoc struct {
	Slug  string // the URL: /docs/<slug>
	Title string // its heading, and the window's name
	Blurb string // one line, the index's description
	src   func() string
}

// siteDocs are the pages, in the order they are meant to be read. The
// index lists them in this order and each page's foot leads to the next.
var siteDocs = []siteDoc{
	{Slug: "getting-started", Title: "Getting Started",
		Blurb: "Build it, open the desktop, make a VM, publish a port.",
		src:   docFile("getting-started")},
	{Slug: "ssh", Title: "SSH as an Interface",
		Blurb: "The lobby on :2222, a VM's own shell, scp and tunnels.",
		src:   docFile("ssh")},
	{Slug: "desktop", Title: "The Desktop and the API",
		Blurb: "The web UI, the skill guide agents read, and the Mac OS 9 sound runtime.",
		src:   docFile("desktop")},
	{Slug: "vms", Title: "How VMs Work",
		Blurb: "What a VM is made of on each platform, and where the boundary is.",
		src:   docFile("vms")},
	{Slug: "config", Title: "Configuration",
		Blurb: "Every key of ~/.exe/config.json, and the Cloudflare setup.",
		src:   docFile("config")},
	{Slug: "using", Title: "Using exe",
		Blurb: "The desktop's own manual — the same text its Help menu opens.",
		src:   func() string { return string(docsMD) }},
}

func docFile(slug string) func() string {
	return func() string {
		b, err := siteDocsFS.ReadFile("site/docs/" + slug + ".md")
		if err != nil {
			return "# Missing\n\nThis page is not in this build."
		}
		return string(b)
	}
}

// siteDocPage is what the template draws.
type siteDocPage struct {
	Title  string
	Blurb  string
	Body   template.HTML
	Pages  []siteDoc // the index's list; empty on a page
	Next   *siteDoc
	Online int
}

var siteDocTmpl = template.Must(template.New("doc").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<title>{{.Title}} — exe</title>
{{with .Blurb}}<meta name="description" content="{{.}}">{{end}}
<meta name="theme-color" content="#dddddd">
<link rel="icon" href="/icon.svg" type="image/svg+xml">
<link rel="apple-touch-icon" href="/icon-192.png">
<link rel="stylesheet" href="/site.css">
</head>
<body>

<div class="window">
  <div class="titlebar"><span class="tbox"><a href="{{if .Pages}}/{{else}}/docs/{{end}}" title="{{if .Pages}}The homepage{{else}}All the documentation{{end}}"></a></span><span class="stripe"></span><span class="title">{{.Title}}</span><span class="stripe"></span></div>
  <div class="frame">
    <div class="body doc">
{{if .Pages}}<h1>Documentation</h1>
<p>exe is a personal VM cloud in a single Go binary. These pages are the
detail; the <a href="/">homepage</a> is the short version and the
<a href="https://github.com/livid/exe" target="_blank" rel="noopener">source</a> is on GitHub.</p>
<ul class="toc">{{range .Pages}}<li><a href="/docs/{{.Slug}}">{{.Title}}</a><span>{{.Blurb}}</span></li>{{end}}</ul>
{{else}}{{.Body}}{{end}}
    </div>
    <div class="statusbar"><span><a href="/">exe</a>{{if not .Pages}} · <a href="/docs/">Documentation</a>{{end}}</span><span>{{with .Next}}Next: <a href="/docs/{{.Slug}}">{{.Title}}</a>{{else}}{{if .Online}}<a href="/stats">{{.Online}} online</a>{{end}}{{end}}</span></div>
  </div>
</div>

</body>
</html>
`))

// siteDocsHandler serves the index and the pages.
func siteDocsHandler(index bool, an interface{ Online() int }) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := siteDocPage{Title: "Documentation", Blurb: "How exe works: getting started, SSH, VMs, configuration and the desktop's manual."}
		if index {
			p.Pages = siteDocs
		} else {
			slug := strings.TrimPrefix(r.URL.Path, "/docs/")
			found := -1
			for j := range siteDocs {
				if siteDocs[j].Slug == slug {
					found = j
					break
				}
			}
			if found < 0 {
				http.Error(w, "exe docs: no such page", http.StatusNotFound)
				return
			}
			d := siteDocs[found]
			p.Title, p.Blurb = d.Title, d.Blurb
			p.Body = mdRender(d.src())
			if found+1 < len(siteDocs) {
				p.Next = &siteDocs[found+1]
			}
		}
		if an != nil {
			p.Online = an.Online()
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		siteDocTmpl.Execute(w, p)
	}
}

// ---- Markdown, the subset the pages are written in -------------------
//
// Headings, paragraphs, fenced code, bullet and numbered lists, pipe
// tables, and inline code, bold, italic and links. No nesting, no
// blockquotes: what the pages use and nothing else, so what a page says
// is what a reader gets.

var (
	mdHeading = regexp.MustCompile(`^(#{1,3})\s+(.*)$`)
	mdBullet  = regexp.MustCompile(`^[-*]\s+(.*)$`)
	mdNumber  = regexp.MustCompile(`^\d+\.\s+(.*)$`)
	mdImage   = regexp.MustCompile(`^!\[([^\]]*)\]\(([^)\s]+)\)\s*$`)
	mdLink    = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	mdBold    = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	mdItalic  = regexp.MustCompile(`\*([^*\s][^*]*)\*`)
	mdSlugBad = regexp.MustCompile(`[^a-z0-9]+`)
)

func mdRender(src string) template.HTML {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var b strings.Builder
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		switch {
		case strings.TrimSpace(line) == "":
			// a blank line only ends what came before

		case strings.HasPrefix(line, "```"):
			var code []string
			for i++; i < len(lines) && !strings.HasPrefix(lines[i], "```"); i++ {
				code = append(code, lines[i])
			}
			b.WriteString(mdCode(code))

		case mdHeading.MatchString(line):
			m := mdHeading.FindStringSubmatch(line)
			n := len(m[1])
			text := mdInline(m[2])
			id := mdSlug(m[2])
			b.WriteString("<h" + string(rune('0'+n)) + ` id="` + id + `">` + text + "</h" + string(rune('0'+n)) + ">\n")

		case mdImage.MatchString(line):
			m := mdImage.FindStringSubmatch(line)
			b.WriteString(`<p class="shot"><img src="` + html.EscapeString(m[2]) + `" alt="` + html.EscapeString(m[1]) + `"></p>` + "\n")

		case strings.HasPrefix(line, "|"):
			var rows []string
			for ; i < len(lines) && strings.HasPrefix(lines[i], "|"); i++ {
				rows = append(rows, lines[i])
			}
			i--
			b.WriteString(mdTable(rows))

		case mdBullet.MatchString(line) || mdNumber.MatchString(line):
			var out string
			out, i = mdList(lines, i)
			b.WriteString(out)

		default:
			var para []string
			for ; i < len(lines) && strings.TrimSpace(lines[i]) != "" &&
				!strings.HasPrefix(lines[i], "```") && !strings.HasPrefix(lines[i], "|") &&
				!mdHeading.MatchString(lines[i]) && !mdBullet.MatchString(lines[i]) &&
				!mdNumber.MatchString(lines[i]); i++ {
				para = append(para, strings.TrimSpace(lines[i]))
			}
			i--
			b.WriteString("<p>" + mdInline(strings.Join(para, " ")) + "</p>\n")
		}
	}
	return template.HTML(b.String())
}

// mdList renders the list beginning at lines[i] and says which line it
// ended on. A line indented under an item belongs to that item.
func mdList(lines []string, i int) (string, int) {
	num := mdNumber.MatchString(lines[i])
	tag := "ul"
	if num {
		tag = "ol"
	}
	var b strings.Builder
	b.WriteString("<" + tag + ">\n")
	for ; i < len(lines); i++ {
		var item string
		switch {
		case !num && mdBullet.MatchString(lines[i]):
			item = mdBullet.FindStringSubmatch(lines[i])[1]
		case num && mdNumber.MatchString(lines[i]):
			item = mdNumber.FindStringSubmatch(lines[i])[1]
		default:
			b.WriteString("</" + tag + ">\n")
			return b.String(), i - 1
		}
		for i+1 < len(lines) && strings.HasPrefix(lines[i+1], "  ") && strings.TrimSpace(lines[i+1]) != "" {
			i++
			item += " " + strings.TrimSpace(lines[i])
		}
		b.WriteString("<li>" + mdInline(item) + "</li>\n")
	}
	b.WriteString("</" + tag + ">\n")
	return b.String(), i - 1
}

// mdCode is a fenced block, drawn as the desktop draws a terminal: a
// shell prompt keeps its green, a whole-line comment its grey.
func mdCode(lines []string) string {
	var b strings.Builder
	b.WriteString(`<pre class="term">`)
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\n")
		}
		switch {
		case strings.HasPrefix(l, "$ "):
			b.WriteString(`<span class="p">$</span> ` + mdShell(l[2:]))
		case strings.HasPrefix(strings.TrimSpace(l), "#"):
			b.WriteString(`<span class="c">` + html.EscapeString(l) + `</span>`)
		default:
			b.WriteString(mdShell(l))
		}
	}
	b.WriteString("</pre>\n")
	return b.String()
}

// mdShell escapes a command line, greying a comment at its end.
func mdShell(s string) string {
	if i := strings.Index(s, "  #"); i >= 0 {
		return html.EscapeString(s[:i]) + `<span class="c">` + html.EscapeString(s[i:]) + `</span>`
	}
	return html.EscapeString(s)
}

// mdTable renders a pipe table: the first row is the head, the second
// its rule, the rest the body.
func mdTable(rows []string) string {
	cells := func(row string) []string {
		row = strings.TrimSpace(row)
		row = strings.TrimPrefix(row, "|")
		row = strings.TrimSuffix(row, "|")
		out := strings.Split(row, "|")
		for i := range out {
			out[i] = strings.TrimSpace(out[i])
		}
		return out
	}
	var b strings.Builder
	b.WriteString(`<div class="tablebox"><table>`)
	for i, row := range rows {
		if i == 1 && strings.Contains(row, "---") {
			continue // the rule under the head
		}
		tag := "td"
		if i == 0 {
			tag = "th"
		}
		b.WriteString("<tr>")
		for _, c := range cells(row) {
			b.WriteString("<" + tag + ">" + mdInline(c) + "</" + tag + ">")
		}
		b.WriteString("</tr>")
	}
	b.WriteString("</table></div>\n")
	return b.String()
}

// mdInline renders one run of text: code spans first, so nothing inside
// a span is read as markup, then links, then bold, then italic.
func mdInline(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "`")
		if i < 0 {
			break
		}
		j := strings.Index(s[i+1:], "`")
		if j < 0 {
			break
		}
		b.WriteString(mdText(s[:i]))
		b.WriteString("<code>" + html.EscapeString(s[i+1:i+1+j]) + "</code>")
		s = s[i+j+2:]
	}
	b.WriteString(mdText(s))
	return b.String()
}

// mdText is escaped prose with its links, bold and italic.
func mdText(s string) string {
	out := html.EscapeString(s)
	out = mdLink.ReplaceAllStringFunc(out, func(m string) string {
		p := mdLink.FindStringSubmatch(m)
		href := p[2]
		rel := ""
		if strings.HasPrefix(href, "http") {
			rel = ` target="_blank" rel="noopener"`
		}
		return `<a href="` + href + `"` + rel + `>` + p[1] + `</a>`
	})
	out = mdBold.ReplaceAllString(out, "<b>$1</b>")
	out = mdItalic.ReplaceAllString(out, "<i>$1</i>")
	return out
}

// mdSlug is a heading's anchor: its words, lower case, joined by dashes.
func mdSlug(s string) string {
	s = strings.ToLower(mdInline(s))
	s = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(s, "")
	s = mdSlugBad.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}
