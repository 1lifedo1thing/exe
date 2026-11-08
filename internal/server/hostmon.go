package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"exe/internal/hostmon"
	"exe/internal/peer"
)

// handleHostMonitor answers the Control Strip's activity module: what the
// samples cover and every sample newer than ?after= (unix ms), the newest
// ?limit= of them (300, ten minutes, unless asked; at most an hour's
// 1,800), with "more" when older ones were left out. The daemon samples
// from its start (RunHostMonitor); a monitor started by a read alone, as
// a test server's, stops ten minutes after the last. A platform it cannot
// read says so, and the module hides. ?detail=1 adds each core and each
// graphics card, for the Activity Monitor.
//
// ?res= picks the history instead: a row a minute (60), ten minutes (600),
// an hour (3600) or a day (86400), each the slot's averages with its peaks
// in "peak", the slot still filling the newest; "now" is the newest sample
// of the moment, for the figures beside the pictures. Any other res, or
// none, is the samples (and says res 2).
func (s *Server) handleHostMonitor(w http.ResponseWriter, r *http.Request) {
	s.writeHostMonitor(w, r, r.URL.Query().Get("detail") == "1")
}

func (s *Server) writeHostMonitor(w http.ResponseWriter, r *http.Request, detail bool) {
	w.Header().Set("Cache-Control", "no-store")
	if !hostmon.Supported {
		writeJSON(w, http.StatusOK, map[string]any{"supported": false})
		return
	}
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit := hostmon.Newest
	if n, err := strconv.Atoi(q.Get("limit")); err == nil {
		limit = min(max(n, 1), hostmon.Keep)
	}
	m := s.hostMonitor()
	if res, _ := strconv.Atoi(q.Get("res")); slices.Contains(hostmon.Steps(), res) {
		info, rows, more, now := m.ReadHistory(res, after, limit, detail)
		writeMonitorJSON(w, r, map[string]any{
			"supported": true, "interval": res * 1000, "res": res, "info": info, "samples": rows, "more": more, "now": now,
		})
		return
	}
	info, samples, more := m.ReadNewest(after, detail, limit)
	writeMonitorJSON(w, r, map[string]any{
		"supported": true, "interval": hostmon.Interval.Milliseconds(), "res": 2, "info": info, "samples": samples, "more": more,
	})
}

// hostMonitor is the daemon's monitor; one a read starts first (a test
// server's) keeps no history.
func (s *Server) hostMonitor() *hostmon.Monitor {
	s.hostMonOnce.Do(func() { s.hostMon = hostmon.New() })
	return s.hostMon
}

// RunHostMonitor samples this machine from the daemon's start, read or
// not, and keeps the history of it in the state directory's activity
// folder: the Activity Monitor's minutes, hours and days.
func (s *Server) RunHostMonitor() {
	if !hostmon.Supported {
		return
	}
	s.hostMonOnce.Do(func() {
		m, err := hostmon.NewWithHistory(filepath.Join(s.StateDir, "activity"))
		if err != nil {
			log.Printf("activity history: %v — sampling without it", err)
		}
		s.hostMon = m
	})
	s.hostMon.Start()
}

// CloseHostMonitor writes what the history's slots still filling hold.
func (s *Server) CloseHostMonitor() {
	if s.hostMon != nil {
		s.hostMon.Close()
	}
}

// writeMonitorJSON writes an answer gzipped for a reader that takes it: an
// hour of a big machine's cores, or a window of its history, is megabytes
// of JSON that packs to a fifth.
func writeMonitorJSON(w http.ResponseWriter, r *http.Request, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSONBytes(w, r, b)
}

func writeJSONBytes(w http.ResponseWriter, r *http.Request, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Add("Vary", "Accept-Encoding")
	if len(b) < 1024 || !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Write(b)
		return
	}
	w.Header().Set("Content-Encoding", "gzip")
	gz, _ := gzip.NewWriterLevel(w, gzip.BestSpeed)
	gz.Write(b)
	gz.Close()
}

// handlePeerMonitor gives a joined node this machine's activity with its
// detail, as the desk's own Activity Monitor sees it: figures of load,
// clocks, heat and power, and the names of the CPU and the cards. It is
// the one daemon-to-daemon route that is not app-data sync; it reads
// nothing a node keeps and changes nothing.
func (s *Server) handlePeerMonitor(w http.ResponseWriter, r *http.Request) {
	e := s.peerEngine(w)
	if e == nil {
		return
	}
	if _, err := e.VerifyPeer(r, nil); err != nil {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	s.writeHostMonitor(w, r, true)
}

// handlePeersMonitor is the desk's way to another node's activity: this
// daemon asks the node for it, signed, and passes the answer on. A node
// whose daemon is older than the route answers 404, which reaches the
// Activity Monitor as {"old": true}.
func (s *Server) handlePeersMonitor(w http.ResponseWriter, r *http.Request) {
	e := s.peerEngine(w)
	if e == nil {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	path := "/v1/peer/monitor?after=" + strconv.FormatInt(after, 10)
	if n, err := strconv.Atoi(q.Get("limit")); err == nil {
		path += "&limit=" + strconv.Itoa(n) // a node older than the hour gives its ten minutes whatever is asked
	}
	if n, err := strconv.Atoi(q.Get("res")); err == nil {
		path += "&res=" + strconv.Itoa(n) // one older than the history answers with its samples, and no res
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	resp, err := e.Fetch(ctx, r.PathValue("id"), path)
	if err != nil {
		if errors.Is(err, peer.ErrNoSuchPeer) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusBadGateway, errors.New("the node did not answer"))
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		writeJSON(w, http.StatusOK, map[string]any{"supported": false, "old": true})
		return
	case resp.StatusCode != http.StatusOK:
		writeErr(w, http.StatusBadGateway, fmt.Errorf("the node answered HTTP %d", resp.StatusCode))
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20)) // an hour of a big machine's cores
	if err != nil || !json.Valid(body) {
		writeErr(w, http.StatusBadGateway, errors.New("the node's answer was cut short"))
		return
	}
	writeJSONBytes(w, r, body)
}
