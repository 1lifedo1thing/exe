package server

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"exe/internal/cf"
)

const cfStatsTTL = 4 * time.Second
const cfConnectionsTTL = 30 * time.Second

var cfStatsHTTPClient = &http.Client{Timeout: 4 * time.Second}

type cfTunnelStats struct {
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	Replicas    int       `json:"replicas"`
	Connections int       `json:"connections"`
	Locations   []string  `json:"locations"`
	CheckedAt   time.Time `json:"checked_at"`
	Stale       bool      `json:"stale"`
}

type cfLocalStats struct {
	*cf.LocalMetrics
	RequestsPerSecond      *float64 `json:"requests_per_second"`
	SentBytesPerSecond     *float64 `json:"sent_bytes_per_second"`
	ReceivedBytesPerSecond *float64 `json:"received_bytes_per_second"`
	UptimeSeconds          float64  `json:"uptime_seconds"`
}

type cfStatsResponse struct {
	SampledAt   time.Time      `json:"sampled_at"`
	Tunnel      *cfTunnelStats `json:"tunnel"`
	TunnelError string         `json:"tunnel_error,omitempty"`
	Local       *cfLocalStats  `json:"local"`
	LocalError  string         `json:"local_error,omitempty"`
}

type cfStatsState struct {
	mu                sync.Mutex
	key               string
	result            cfStatsResponse
	tunnel            *cf.Tunnel
	tunnelAttempt     time.Time
	verifiedConnector string
	previous          *cf.LocalMetrics
	previousAt        time.Time
}

func tunnelStats(t *cf.Tunnel, now time.Time) *cfTunnelStats {
	s := &cfTunnelStats{Name: t.Name, Status: t.Status, CheckedAt: now, Locations: []string{}}
	clients, locations, edges := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, c := range t.Connections {
		if c.PendingReconnect {
			continue
		}
		if c.ID != "" && edges[c.ID] {
			continue
		}
		edges[c.ID] = true
		s.Connections++
		if c.ClientID != "" {
			clients[c.ClientID] = true
		}
		if c.Location != "" {
			locations[c.Location] = true
		}
	}
	s.Replicas = len(clients)
	for location := range locations {
		s.Locations = append(s.Locations, location)
	}
	slices.Sort(s.Locations)
	return s
}

func localStats(current, previous *cf.LocalMetrics, now, previousAt time.Time) *cfLocalStats {
	s := &cfLocalStats{LocalMetrics: current, UptimeSeconds: max(0, float64(now.UnixMilli())/1000-current.Started)}
	seconds := now.Sub(previousAt).Seconds()
	if previous != nil && seconds >= 1 && seconds <= 30 && current.ConnectorID == previous.ConnectorID &&
		current.Started == previous.Started {
		if current.Requests >= previous.Requests {
			rate := (current.Requests - previous.Requests) / seconds
			s.RequestsPerSecond = &rate
		}
		s.SentBytesPerSecond, s.ReceivedBytesPerSecond = current.ByteRates(previous, seconds)
	}
	return s
}

// Connections describe all replicas of the configured tunnel. Request
// counters come only from a verified cloudflared connector on this host.
// Polling multiple browsers shares both caches and one in-flight sample.
func (s *Server) handleCFStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	c := s.Config().Cloudflare
	key := strings.Join([]string{c.APIToken, c.AccountID, c.TunnelID, c.MetricsURL}, "|")
	state := &s.cfStats
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.key != key {
		state.key, state.result, state.tunnel = key, cfStatsResponse{}, nil
		state.previous, state.verifiedConnector = nil, ""
		state.tunnelAttempt = time.Time{}
	}
	if !state.result.SampledAt.IsZero() && time.Since(state.result.SampledAt) < cfStatsTTL {
		writeJSON(w, http.StatusOK, state.result)
		return
	}
	res := cfStatsResponse{}
	if c.APIToken == "" || c.AccountID == "" || c.TunnelID == "" {
		res.TunnelError, res.LocalError = "Cloudflare is not configured.", "No tunnel selected."
	} else {
		res.Tunnel, res.TunnelError = state.result.Tunnel, state.result.TunnelError
		if time.Since(state.tunnelAttempt) >= cfConnectionsTTL {
			client := &cf.Client{Token: c.APIToken, AccountID: c.AccountID, HTTPC: cfStatsHTTPClient}
			ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
			t, err := client.GetTunnel(ctx, c.TunnelID)
			cancel()
			state.tunnelAttempt = time.Now()
			if err != nil {
				res.TunnelError = "Cloudflare connection counts are unavailable."
				if res.Tunnel != nil {
					stale := *res.Tunnel
					stale.Stale = true
					res.Tunnel = &stale
				}
			} else {
				state.tunnel = t
				res.Tunnel, res.TunnelError = tunnelStats(t, time.Now()), ""
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		m, err := cf.ReadLocalMetrics(ctx, c.MetricsURL, cfStatsHTTPClient)
		cancel()
		if err != nil {
			res.LocalError = err.Error()
		} else {
			matched := m.ConnectorID == state.verifiedConnector
			if state.tunnel != nil {
				for _, edge := range state.tunnel.Connections {
					if edge.ClientID == m.ConnectorID {
						matched = true
						break
					}
				}
			}
			if !matched {
				res.LocalError = "The local connector has not been identified in this tunnel."
			} else {
				state.verifiedConnector = m.ConnectorID
				now := time.Now()
				res.Local = localStats(m, state.previous, now, state.previousAt)
				state.previous, state.previousAt = m, now
			}
		}
	}
	if res.Local == nil {
		state.previous = nil
	}
	res.SampledAt = time.Now().UTC()
	state.result = res
	writeJSON(w, http.StatusOK, res)
}
