package proxy

import (
	"bufio"
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// Dir is the prefix of a backend the daemon serves from a directory on
// this machine: "exe:dir:/www/site" answers each request with the file of
// that name under /www/site. A route to a static site then needs no
// process of its own beside the daemon, which is already kept running.
const Dir = Builtin + "dir:"

// DirBackend accepts an absolute path to an existing directory and
// returns the backend that serves it.
func DirBackend(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" || !filepath.IsAbs(dir) {
		return "", errors.New("dir must be an absolute path")
	}
	dir = filepath.Clean(dir)
	st, err := os.Stat(dir)
	if err != nil {
		return "", errors.New("dir: " + err.Error())
	}
	if !st.IsDir() {
		return "", errors.New("dir is not a directory")
	}
	return Dir + dir, nil
}

// passwordFile is the file that gates a directory: when a directory on a
// request's path holds one, the request must carry HTTP Basic
// authentication whose password is the file's first line (any user name).
// Dot-files are never served, so the file itself cannot be read.
const passwordFile = ".password"

// dirHandlers keeps one handler per directory backend; a route table may
// name the same directory under several hosts.
var dirHandlers sync.Map

// dirHandlerFor returns the handler of a "exe:dir:" backend, or false for
// any other.
func dirHandlerFor(backend string) (http.Handler, bool) {
	if !strings.HasPrefix(backend, Dir) {
		return nil, false
	}
	root := strings.TrimPrefix(backend, Dir)
	if h, ok := dirHandlers.Load(root); ok {
		return h.(http.Handler), true
	}
	h, _ := dirHandlers.LoadOrStore(root, DirHandler(root))
	return h.(http.Handler), true
}

// downloadExt lists the kinds a browser is told to save rather than show:
// disk images and archives, which it could not show in any case, and which
// an old browser might otherwise print as text.
var downloadExt = map[string]bool{".toast": true, ".img": true, ".dmg": true, ".sit": true, ".zip": true, ".iso": true, ".bin": true, ".hqx": true}

// DirHandler serves the files under root. A path names a file or, with a
// trailing slash, the index.html of a directory; there are no listings,
// and a path with a dot-file or a dot-directory in it is not found. Byte
// ranges are honoured (http.ServeContent), so a download can resume. A
// page is served no-cache and anything else may be kept for ten minutes;
// what sits behind a .password file is private and never stored.
func DirHandler(root string) http.Handler {
	fs := http.FileServer(dirFS{http.Dir(root)})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		p := path.Clean("/" + r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/") && p != "/" {
			p += "/"
		}
		for _, seg := range strings.Split(p, "/") {
			if strings.HasPrefix(seg, ".") {
				http.NotFound(w, r)
				return
			}
		}
		gated, want := gate(root, p)
		switch {
		case want != "" && !passwordMatches(r, want):
			w.Header().Set("WWW-Authenticate", `Basic realm="`+path.Base(gated)+`", charset="UTF-8"`)
			w.Header().Set("Cache-Control", "private, no-store")
			http.Error(w, "a password is needed here", http.StatusUnauthorized)
			return
		case want != "":
			w.Header().Set("Cache-Control", "private, no-store")
		case strings.HasSuffix(p, "/") || strings.HasSuffix(p, ".html"):
			w.Header().Set("Cache-Control", "no-cache")
		default:
			w.Header().Set("Cache-Control", "public, max-age=600")
		}
		if downloadExt[strings.ToLower(path.Ext(p))] {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="`+path.Base(p)+`"`)
		}
		r.URL.Path = p
		fs.ServeHTTP(w, r)
	})
}

// gate walks the directories on a request path p, root first, and returns
// the request path of the first that holds a .password file and that
// file's password; "" and "" when none does. The last segment of p is a
// file unless p ends with a slash.
func gate(root, p string) (string, string) {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	if !strings.HasSuffix(p, "/") {
		segs = segs[:len(segs)-1]
	}
	dir, at := root, "/"
	if want := readPassword(dir); want != "" {
		return at, want
	}
	for _, seg := range segs {
		if seg == "" {
			continue
		}
		dir = filepath.Join(dir, seg)
		at = path.Join(at, seg)
		if want := readPassword(dir); want != "" {
			return at, want
		}
	}
	return "", ""
}

// dirFS hides what http.Dir would list or show: a directory with no
// index.html is not found rather than listed, and a dot-file is not
// found.
type dirFS struct{ http.Dir }

func (d dirFS) Open(name string) (http.File, error) {
	for _, seg := range strings.Split(path.Clean("/"+name), "/") {
		if strings.HasPrefix(seg, ".") {
			return nil, os.ErrNotExist
		}
	}
	f, err := d.Dir.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		idx, err := d.Dir.Open(path.Join(name, "index.html"))
		if err != nil {
			f.Close()
			return nil, os.ErrNotExist
		}
		idx.Close()
	}
	return f, nil
}

// readPassword returns the first line of a .password file, or "" when the
// directory has none.
func readPassword(dir string) string {
	f, err := os.Open(filepath.Join(dir, passwordFile))
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if sc.Scan() {
		return strings.TrimSpace(sc.Text())
	}
	return ""
}

// passwordMatches compares a request's Basic password with the one wanted,
// in constant time.
func passwordMatches(r *http.Request, want string) bool {
	_, got, ok := r.BasicAuth()
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
