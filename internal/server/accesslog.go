package server

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// AccessLog records every request to the API listeners (:7777, the
// tailnet address and its loopback companion) in a file of its own,
// ~/.exe/access.log, so daemon.log stays readable and a question like
// "who reached this machine?" has an answer. One line a request:
//
//	2026-09-24 03:41:07 100.101.2.3:51234 GET /v1/vms 200 1626 3ms xff=… ts=… "UA"
//
// The remote is the TCP peer as the listener saw it, the one field a
// client cannot make up. Tailscale Serve forwards from 127.0.0.1, so its
// requests carry xff= (the client's address) and ts= (the tailnet login);
// on any other remote those two are only what the client sent. A `token`
// query parameter is written as "redacted". A WebSocket is logged when it
// upgrades (101), not when it closes, so a terminal left open still
// leaves its line. Past accessLogMax bytes the file moves to access.log.1
// and starts over.
type AccessLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
	max  int64
	now  func() time.Time

	// Ring holds the latest lines for GET /v1/logs/access (the Log
	// Viewer's Access Log tab): the file's tail at open, then each line.
	Ring *LogBuffer
}

// Two desks polling write about 10 MB a day, so this file and .1 hold
// roughly twelve days.
const accessLogMax = 64 << 20

func OpenAccessLog(path string) (*AccessLog, error) {
	a := &AccessLog{path: path, max: accessLogMax, now: time.Now, Ring: NewLogBuffer(1000)}
	if err := a.open(); err != nil {
		return nil, err
	}
	if tail := fileTail(path, 512<<10); len(tail) > 0 {
		a.Ring.Write(tail)
	}
	return a, nil
}

// fileTail reads up to the last n bytes of path, starting at a line.
func fileTail(path string, n int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	off := max(st.Size()-n, 0)
	b := make([]byte, st.Size()-off)
	if _, err := f.ReadAt(b, off); err != nil && err != io.EOF {
		return nil
	}
	if off > 0 {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
	}
	return b
}

func (a *AccessLog) open() error {
	f, err := os.OpenFile(a.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	a.f, a.size = f, st.Size()
	return nil
}

func (a *AccessLog) write(line string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.Ring.Write([]byte(line))
	if a.f == nil {
		return
	}
	if a.size > 0 && a.size+int64(len(line)) > a.max {
		a.f.Close()
		a.f = nil
		os.Rename(a.path, a.path+".1")
		if err := a.open(); err != nil {
			log.Printf("access.log: %v (API requests go unlogged)", err)
			return
		}
	}
	n, _ := a.f.WriteString(line)
	a.size += int64(n)
}

// Wrap logs each request next serves.
func (a *AccessLog) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := a.now()
		aw := &accessWriter{ResponseWriter: w}
		aw.onHijack = func() { a.write(a.line(r, aw, start)) }
		defer func() {
			if aw.hijacked {
				return
			}
			if p := recover(); p != nil {
				aw.status = 500
				a.write(a.line(r, aw, start))
				panic(p)
			}
			a.write(a.line(r, aw, start))
		}()
		next.ServeHTTP(aw, r)
	})
}

func (a *AccessLog) line(r *http.Request, aw *accessWriter, start time.Time) string {
	status := aw.status
	if status == 0 {
		status = http.StatusOK // the handler wrote nothing: net/http answers 200
	}
	uri := r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		q := r.URL.Query()
		if q.Has("token") {
			q.Set("token", "redacted")
			uri += "?" + q.Encode()
		} else {
			uri += "?" + r.URL.RawQuery
		}
	}
	extra := ""
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		extra += " xff=" + strings.ReplaceAll(v, " ", "")
	}
	if v := r.Header.Get("Tailscale-User-Login"); v != "" {
		extra += " ts=" + v
	}
	return fmt.Sprintf("%s %s %s %s %d %d %s%s %q\n",
		start.Format("2006-01-02 15:04:05"), r.RemoteAddr, r.Method, uri,
		status, aw.bytes, a.now().Sub(start).Round(time.Millisecond), extra, r.UserAgent())
}

// accessWriter notes the status and body size on the way through. It
// keeps Flush and Hijack reachable: the streams assert http.Flusher on
// the writer they are handed, and the WebSocket library looks for
// http.Hijacker (Unwrap covers http.ResponseController).
type accessWriter struct {
	http.ResponseWriter
	status   int
	bytes    int64
	hijacked bool
	onHijack func()
}

func (w *accessWriter) WriteHeader(code int) {
	if w.status == 0 && (code >= 200 || code == http.StatusSwitchingProtocols) {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *accessWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *accessWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *accessWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	c, rw, err := hj.Hijack()
	if err == nil {
		w.status = http.StatusSwitchingProtocols
		w.hijacked = true
		w.onHijack()
	}
	return c, rw, err
}

func (w *accessWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
