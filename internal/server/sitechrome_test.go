package server

import (
	"regexp"
	"strings"
	"testing"
)

// The desktop is the source of truth for this look. The site's chrome is
// copied from it, and this is what keeps the copy honest: every visual
// declaration of a shared block must say what the desktop says. Only the
// properties that place a window on a desk — where it sits and how big it
// may be — are the site's own.
func TestSiteChromeFollowsTheDesktop(t *testing.T) {
	desk := cssBlocks(string(uiHTML))
	site := cssBlocks(string(mustSiteFile("site/site.css")))

	// what a page places for itself: a window is centred in a column
	// here, not dragged around a desk
	ours := map[string]bool{
		"position": true, "z-index": true, "min-width": true, "max-width": true,
		"margin": true, "transform-origin": true, "transition": true, "display": true,
		"cursor": true, "justify-content": true, "overflow": true, "white-space": true,
	}
	for _, sel := range []string{
		".window", ".window::before", ".titlebar", ".titlebar .stripe",
		".titlebar .stripe::before", ".titlebar .stripe::after", ".titlebar .title",
		".tbox", ".tbox:active::after", ".statusbar",
	} {
		d, okD := desk[sel]
		s, okS := site[sel]
		if !okD {
			t.Errorf("the desktop has no %s to copy (has it been renamed?)", sel)
			continue
		}
		if !okS {
			t.Errorf("the site does not carry the desktop's %s", sel)
			continue
		}
		for prop, want := range d {
			if ours[prop] {
				continue
			}
			got, ok := s[prop]
			if !ok {
				t.Errorf("%s: the site drops %s (the desktop says %q)", sel, prop, want)
				continue
			}
			if got != want {
				t.Errorf("%s: %s is %q here and %q on the desktop", sel, prop, got, want)
			}
		}
	}
}

var cssRule = regexp.MustCompile(`(?s)([^{}]+)\{([^{}]*)\}`)

// cssBlocks reads a stylesheet (or the style element of a page) into its
// rules: selector to property to value, comments and whitespace gone.
func cssBlocks(src string) map[string]map[string]string {
	if i := strings.Index(src, "<style>"); i >= 0 {
		src = src[i+len("<style>"):]
		if j := strings.Index(src, "</style>"); j >= 0 {
			src = src[:j]
		}
	}
	for {
		i := strings.Index(src, "/*")
		if i < 0 {
			break
		}
		j := strings.Index(src[i:], "*/")
		if j < 0 {
			break
		}
		src = src[:i] + src[i+j+2:]
	}
	out := map[string]map[string]string{}
	for _, m := range cssRule.FindAllStringSubmatch(src, -1) {
		sel := strings.Join(strings.Fields(m[1]), " ")
		if strings.HasPrefix(sel, "@") || strings.Contains(sel, "@media") {
			continue
		}
		props := map[string]string{}
		for _, decl := range strings.Split(m[2], ";") {
			k, v, ok := strings.Cut(decl, ":")
			if !ok {
				continue
			}
			props[strings.TrimSpace(k)] = strings.Join(strings.Fields(v), " ")
		}
		for _, one := range strings.Split(sel, ",") {
			one = strings.TrimSpace(one)
			if _, seen := out[one]; seen {
				for k, v := range props {
					out[one][k] = v
				}
				continue
			}
			out[one] = props
		}
	}
	return out
}
