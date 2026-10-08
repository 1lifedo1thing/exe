package server

import (
	"net/http"
	"strconv"

	"exe/internal/hostmon"
)

// handleHostMonitor answers the Control Strip's activity module: what the
// samples cover and every sample newer than ?after= (unix ms). Each read
// keeps the sampler going; with no reader for ten minutes it stops. A
// platform it cannot read says so, and the module hides.
func (s *Server) handleHostMonitor(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !hostmon.Supported {
		writeJSON(w, http.StatusOK, map[string]any{"supported": false})
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	s.hostMonOnce.Do(func() { s.hostMon = hostmon.New() })
	info, samples := s.hostMon.Read(after)
	writeJSON(w, http.StatusOK, map[string]any{
		"supported": true, "interval": hostmon.Interval.Milliseconds(), "info": info, "samples": samples,
	})
}
