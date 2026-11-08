package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"exe/internal/hostmon"
	"exe/internal/peer"
)

// handleHostMonitor answers the Control Strip's activity module: what the
// samples cover and every sample newer than ?after= (unix ms), the newest
// ?limit= of them (300, ten minutes, unless asked; at most an hour's
// 1,800), with "more" when older ones were left out. Each read keeps the
// sampler going; with no reader for ten minutes it stops. A platform it
// cannot read says so, and the module hides. ?detail=1 adds each core and
// each graphics card, for the Activity Monitor.
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
	s.hostMonOnce.Do(func() { s.hostMon = hostmon.New() })
	info, samples, more := s.hostMon.ReadNewest(after, detail, limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"supported": true, "interval": hostmon.Interval.Milliseconds(), "info": info, "samples": samples, "more": more,
	})
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
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}
