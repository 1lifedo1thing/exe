package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"exe/internal/config"
)

// The Welcome window (sysapps/welcome) opens by itself on a new install:
// `exe setup` and `exe init` leave a mark in the state directory
// (config.Welcome), /v1/hostinfo tells the desktop it is there, and the
// desktop opens the window each time it loads. The window's check box,
// "Show this window when the desktop opens", is this call: it reads the
// mark and takes it away or puts it back. A node that was installed before
// the window existed has no mark and is never greeted; Help → Welcome…
// opens the window there.

func (s *Server) handleWelcomeGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"show": config.Welcome(s.StateDir)})
}

func (s *Server) handleWelcomePut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Show *bool `json:"show"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil || req.Show == nil {
		writeErr(w, http.StatusBadRequest, errors.New(`expected {"show": true} or {"show": false}`))
		return
	}
	if err := config.SetWelcome(s.StateDir, *req.Show); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"show": *req.Show})
}
