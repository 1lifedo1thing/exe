package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"exe/internal/codex"
)

// The Control Strip's agent usage module: how many tokens Claude Code and
// Codex have used on this host today and over the last seven days, and
// where each plan's usage windows stand. Both CLIs already write it all
// down, so the daemon only reads:
//
//   - Claude Code keeps a transcript per session under ~/.claude/projects,
//     one JSON line per content block; every assistant line carries the
//     request's usage, the same on each line of one message, so a message
//     counts once, by its id. The plan's five-hour and weekly windows come
//     with the status-line hook's JSON (agentstatus.go): the freshest status
//     file that has them is the node's latest word.
//   - Codex keeps a rollout per thread under ~/.codex/sessions with a
//     token_count event per request: the thread's running total and the
//     request's own share. An event is sometimes sent twice; the second
//     one's total has not moved, and it is skipped — every other event
//     counts its own share (the totals themselves start over after a
//     resume, so they are no measure). The plan's windows are read live on
//     the daemon's ChatGPT sign-in (codexUsage); without one, the newest
//     rate_limits a rollout carries stand in.
//
// A scan reads only what was appended since the last one. The first scan
// of a busy week is hundreds of megabytes, so it runs in the background and
// the answer says "scanning" until it lands.

const (
	agentUsageTTL      = 30 * time.Second // between scans of the logs
	agentUsageDays     = 7                // today and the six days before
	agentUsageFirst    = 2 * time.Second  // how long the first answer waits on the first scan
	agentUsageStatuses = 12               // the newest status files looked at for Claude's windows
	codexLimitsTTL     = time.Minute      // the Codex window's own cadence (openaiUsageEvery)
)

// tokenCount is one agent's tokens over a span. Input is what was sent
// fresh, cache writes included; Cached is input read back from the prompt
// cache; Total is all three.
type tokenCount struct {
	Input    int64 `json:"input"`
	Cached   int64 `json:"cached"`
	Output   int64 `json:"output"`
	Total    int64 `json:"total"`
	Requests int64 `json:"requests"`
}

func (t *tokenCount) add(in, cached, out int64) {
	t.Input += in
	t.Cached += cached
	t.Output += out
	t.Total += in + cached + out
	t.Requests++
}

func (t *tokenCount) sum(o tokenCount) {
	t.Input += o.Input
	t.Cached += o.Cached
	t.Output += o.Output
	t.Total += o.Total
	t.Requests += o.Requests
}

// usageWindow is one of a plan's rolling windows. Rolled says the reset
// time on file has passed: the figure is from the window before, and
// nothing newer has been heard.
type usageWindow struct {
	Name     string  `json:"name"` // five_hour, seven_day, or "" for another length
	Seconds  int64   `json:"seconds"`
	UsedPct  float64 `json:"used_pct"`
	ResetsAt int64   `json:"resets_at,omitempty"` // Unix seconds
	Rolled   bool    `json:"rolled,omitempty"`
}

// agentUsage is one agent's line in the answer. Working: the CLI is on
// this host and has left figures to show.
type agentUsage struct {
	ID           string        `json:"id"`
	Title        string        `json:"title"`
	Installed    bool          `json:"installed"`
	Working      bool          `json:"working"`
	Plan         string        `json:"plan,omitempty"`
	Windows      []usageWindow `json:"windows,omitempty"`
	WindowsAt    int64         `json:"windows_at,omitempty"` // ms, when the windows were read
	LimitReached bool          `json:"limit_reached,omitempty"`
	Today        tokenCount    `json:"today"`
	Week         tokenCount    `json:"week"`
	LastUsed     int64         `json:"last_used,omitempty"` // ms, the newest request counted
}

// usageLogs is the running count over one agent's logs.
type usageLogs struct {
	codex  bool
	files  map[string]*usageLogFile
	days   map[string]*tokenCount // local day, 2006-01-02
	seen   map[string]string      // Claude message id → the day it counted on
	last   time.Time              // the newest request counted
	limits *rolloutLimits         // Codex: the newest rate_limits seen
}

type usageLogFile struct {
	off  int64
	prev int64 // Codex: the thread's running total at off, -1 when unknown
}

// rolloutLimits is the rate_limits object of a Codex token_count event.
type rolloutLimits struct {
	at        time.Time
	Primary   *rolloutWindow `json:"primary"`
	Secondary *rolloutWindow `json:"secondary"`
	PlanType  string         `json:"plan_type"`
	Reached   *string        `json:"rate_limit_reached_type"`
}

type rolloutWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int64   `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

func newUsageLogs(codex bool) *usageLogs {
	return &usageLogs{codex: codex, files: map[string]*usageLogFile{}, days: map[string]*tokenCount{}, seen: map[string]string{}}
}

// agentUsageState is the Server's share: the two counts, the last answer
// and the cached Codex windows.
type agentUsageState struct {
	mu       sync.Mutex
	logs     map[string]*usageLogs
	scanning bool
	scanned  time.Time
	done     chan struct{} // closed when the scan in flight ends

	limMu     sync.Mutex
	codexAt   time.Time
	codexWins []usageWindow
	codexPlan string
	codexHit  bool
}

// claudeLogRoots and codexLogRoots say where the CLIs keep their logs;
// variables so a test can point them at fixtures.
var claudeLogRoots = func() []string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		dir = filepath.Join(home, ".claude")
	}
	return []string{filepath.Join(dir, "projects")}
}

var codexLogRoots = func() []string {
	cfg := codexConfigPath()
	if cfg == "" {
		return nil
	}
	home := filepath.Dir(cfg)
	return []string{filepath.Join(home, "sessions"), filepath.Join(home, "archived_sessions")}
}

// usageSince is the first moment that counts: midnight, six days ago.
func usageSince(now time.Time) time.Time {
	y, m, d := now.Date()
	return time.Date(y, m, d-(agentUsageDays-1), 0, 0, 0, 0, now.Location())
}

// scan reads what the logs under roots gained since the last scan. A log
// untouched since before the week began is never opened. A Codex rollout
// goes by its file name, which is the thread's own, so archiving a thread
// — a move to another folder — does not count it twice; a Claude
// transcript is covered by the message ids.
func (u *usageLogs) scan(roots []string, now time.Time) {
	since := usageSince(now)
	for _, root := range roots {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".jsonl") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			key := p
			if u.codex {
				key = filepath.Base(p)
			}
			f := u.files[key]
			if f == nil {
				if info.ModTime().Before(since) {
					return nil
				}
				f = &usageLogFile{prev: -1}
				u.files[key] = f
			}
			if info.Size() < f.off { // cut short: pick up at its new end
				f.off, f.prev = info.Size(), -1
			}
			if info.Size() > f.off {
				u.read(p, f, since)
			}
			return nil
		})
	}
	// the week moves on: days and ids that fell out of it go
	first := since.Format("2006-01-02")
	for day := range u.days {
		if day < first {
			delete(u.days, day)
		}
	}
	for id, day := range u.seen {
		if day < first {
			delete(u.seen, id)
		}
	}
}

// read takes a log's new lines, whole ones only: a line still being
// written waits for the next scan.
func (u *usageLogs) read(path string, f *usageLogFile, since time.Time) {
	fh, err := os.Open(path)
	if err != nil {
		return
	}
	defer fh.Close()
	if _, err := fh.Seek(f.off, 0); err != nil {
		return
	}
	r := bufio.NewReaderSize(fh, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		f.off += int64(len(line))
		if u.codex {
			u.codexLine(line, f, since)
		} else {
			u.claudeLine(line, since)
		}
	}
}

// count books one request on its local day, when that day is in the week.
func (u *usageLogs) count(ts string, since time.Time, in, cached, out int64) (string, bool) {
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil || at.Before(since) {
		return "", false
	}
	day := at.In(since.Location()).Format("2006-01-02")
	t := u.days[day]
	if t == nil {
		t = &tokenCount{}
		u.days[day] = t
	}
	t.add(in, cached, out)
	if at.After(u.last) {
		u.last = at
	}
	return day, true
}

func (u *usageLogs) claudeLine(line []byte, since time.Time) {
	if !bytes.Contains(line, []byte(`"usage"`)) || !bytes.Contains(line, []byte(`"assistant"`)) {
		return
	}
	var m struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Message   struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				Input      int64 `json:"input_tokens"`
				CacheWrite int64 `json:"cache_creation_input_tokens"`
				CacheRead  int64 `json:"cache_read_input_tokens"`
				Output     int64 `json:"output_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &m) != nil || m.Type != "assistant" || m.Message.Usage == nil || m.Message.Model == "<synthetic>" {
		return
	}
	if id := m.Message.ID; id != "" {
		if _, dup := u.seen[id]; dup {
			return
		}
		us := m.Message.Usage
		if day, ok := u.count(m.Timestamp, since, us.Input+us.CacheWrite, us.CacheRead, us.Output); ok {
			u.seen[id] = day
		}
		return
	}
	us := m.Message.Usage
	u.count(m.Timestamp, since, us.Input+us.CacheWrite, us.CacheRead, us.Output)
}

func (u *usageLogs) codexLine(line []byte, f *usageLogFile, since time.Time) {
	if !bytes.Contains(line, []byte(`"token_count"`)) {
		return
	}
	type usage struct {
		Input  int64 `json:"input_tokens"`
		Cached int64 `json:"cached_input_tokens"`
		Output int64 `json:"output_tokens"`
		Total  int64 `json:"total_tokens"`
	}
	var m struct {
		Timestamp string `json:"timestamp"`
		Payload   struct {
			Type string `json:"type"`
			Info *struct {
				Total usage `json:"total_token_usage"`
				Last  usage `json:"last_token_usage"`
			} `json:"info"`
			RateLimits *rolloutLimits `json:"rate_limits"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &m) != nil || m.Payload.Type != "token_count" {
		return
	}
	if rl := m.Payload.RateLimits; rl != nil {
		if at, err := time.Parse(time.RFC3339Nano, m.Timestamp); err == nil && (u.limits == nil || at.After(u.limits.at)) {
			rl.at = at
			u.limits = rl
		}
	}
	info := m.Payload.Info
	if info == nil {
		return
	}
	prev := f.prev
	f.prev = info.Total.Total
	if prev >= 0 && info.Total.Total == prev {
		return // the same event, sent again
	}
	// the request's own share is the measure either way; the running total
	// only says whether this event is news
	l := info.Last
	if l.Cached > l.Input {
		l.Cached = l.Input
	}
	u.count(m.Timestamp, since, l.Input-l.Cached, l.Cached, l.Output)
}

// totals are today's count and the week's.
func (u *usageLogs) totals(now time.Time) (today, week tokenCount) {
	day := now.Format("2006-01-02")
	for d, t := range u.days {
		week.sum(*t)
		if d == day {
			today = *t
		}
	}
	return
}

func windowName(seconds int64) string {
	switch seconds {
	case 5 * 3600:
		return "five_hour"
	case 7 * 86400:
		return "seven_day"
	}
	return ""
}

func sortWindows(w []usageWindow) {
	sort.SliceStable(w, func(i, j int) bool { return w[i].Seconds < w[j].Seconds })
}

// claudeWindows reads the plan's windows from the freshest status file
// that has them (a session's first hook call comes before its first reply
// and has none).
func claudeWindows(dir string, now time.Time) ([]usageWindow, time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, time.Time{}
	}
	type stamped struct {
		path string
		at   time.Time
	}
	var files []stamped
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, "claude") && strings.HasSuffix(n, ".status.json") {
			if info, err := e.Info(); err == nil {
				files = append(files, stamped{filepath.Join(dir, n), info.ModTime()})
			}
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].at.After(files[j].at) })
	if len(files) > agentUsageStatuses {
		files = files[:agentUsageStatuses]
	}
	for _, f := range files {
		b, err := os.ReadFile(f.path)
		if err != nil {
			continue
		}
		var h struct {
			RateLimits map[string]struct {
				UsedPercentage float64         `json:"used_percentage"`
				ResetsAt       json.RawMessage `json:"resets_at"`
			} `json:"rate_limits"`
		}
		if json.Unmarshal(b, &h) != nil {
			continue
		}
		var out []usageWindow
		for name, secs := range map[string]int64{"five_hour": 5 * 3600, "seven_day": 7 * 86400} {
			l, ok := h.RateLimits[name]
			if !ok {
				continue
			}
			w := usageWindow{Name: name, Seconds: secs, UsedPct: l.UsedPercentage, ResetsAt: unixOf(l.ResetsAt)}
			w.Rolled = w.ResetsAt > 0 && w.ResetsAt <= now.Unix()
			out = append(out, w)
		}
		if len(out) > 0 {
			sortWindows(out)
			return out, f.at
		}
	}
	return nil, time.Time{}
}

// unixOf reads a reset time written as Unix seconds or as RFC 3339.
func unixOf(raw json.RawMessage) int64 {
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		return int64(n)
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// codexWindows reads the ChatGPT plan's windows on the daemon's sign-in,
// once a minute at most; ok is false without a sign-in or an answer.
func (s *Server) codexWindows(ctx context.Context, force bool) (wins []usageWindow, plan string, hit bool, at time.Time, ok bool) {
	if s.codexCreds() == nil {
		return nil, "", false, time.Time{}, false
	}
	st := &s.agentUsage
	st.limMu.Lock()
	defer st.limMu.Unlock()
	if force || st.codexAt.IsZero() || time.Since(st.codexAt) > codexLimitsTTL {
		u, err := s.codexUsage(ctx)
		if err != nil {
			return nil, "", false, time.Time{}, false
		}
		st.codexWins, st.codexPlan, st.codexHit = nil, u.PlanType, false
		if rl := u.RateLimit; rl != nil {
			st.codexHit = rl.LimitReached
			for _, w := range []*codex.UsageWindow{rl.PrimaryWindow, rl.SecondaryWindow} {
				if w != nil {
					st.codexWins = append(st.codexWins, usageWindow{Name: windowName(w.LimitWindowSeconds), Seconds: w.LimitWindowSeconds, UsedPct: w.UsedPercent, ResetsAt: w.ResetAt})
				}
			}
			sortWindows(st.codexWins)
		}
		st.codexAt = time.Now()
	}
	return st.codexWins, st.codexPlan, st.codexHit, st.codexAt, true
}

// windows turns a rollout's rate_limits into windows.
func (rl *rolloutLimits) windows(now time.Time) []usageWindow {
	var out []usageWindow
	for _, w := range []*rolloutWindow{rl.Primary, rl.Secondary} {
		if w == nil {
			continue
		}
		secs := w.WindowMinutes * 60
		uw := usageWindow{Name: windowName(secs), Seconds: secs, UsedPct: w.UsedPercent, ResetsAt: w.ResetsAt}
		uw.Rolled = uw.ResetsAt > 0 && uw.ResetsAt <= now.Unix()
		out = append(out, uw)
	}
	sortWindows(out)
	return out
}

// scanAgentUsage runs one scan of both agents' logs.
func (s *Server) scanAgentUsage() {
	st := &s.agentUsage
	now := time.Now()
	st.mu.Lock()
	if st.logs == nil {
		st.logs = map[string]*usageLogs{"claude": newUsageLogs(false), "codex": newUsageLogs(true)}
	}
	logs := st.logs
	st.mu.Unlock()
	// only one scan runs at a time (scanning), so the counts need no lock
	// while it reads; the answer takes them under mu once it is done
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); logs["claude"].scan(claudeLogRoots(), now) }()
	go func() { defer wg.Done(); logs["codex"].scan(codexLogRoots(), now) }()
	wg.Wait()
	st.mu.Lock()
	st.scanning, st.scanned = false, time.Now()
	close(st.done)
	st.mu.Unlock()
}

// handleAgentUsage serves the module. The logs are scanned again when the
// last scan is older than agentUsageTTL (?force=1: now); the answer waits
// for that scan, except the very first, which may take a while — then it
// says "scanning" and the desktop asks again.
func (s *Server) handleAgentUsage(w http.ResponseWriter, r *http.Request) {
	st := &s.agentUsage
	force := r.URL.Query().Get("force") == "1"
	st.mu.Lock()
	first := st.scanned.IsZero()
	if !st.scanning && (first || force || time.Since(st.scanned) > agentUsageTTL) {
		st.scanning, st.done = true, make(chan struct{})
		go s.scanAgentUsage()
	}
	done, scanning := st.done, st.scanning
	st.mu.Unlock()
	if scanning {
		wait := 30 * time.Second
		if first {
			wait = agentUsageFirst
		}
		select {
		case <-done:
		case <-time.After(wait):
		case <-r.Context().Done():
			return
		}
	}

	// the windows first, outside the lock: Codex's are a network read
	now := time.Now()
	claudeWins, claudeAt := claudeWindows(filepath.Join(s.StateDir, "agents"), now)
	codexWins, codexPlan, codexHit, codexAt, codexLive := s.codexWindows(r.Context(), force)

	st.mu.Lock()
	defer st.mu.Unlock()
	res := map[string]any{"checked_at": now.UnixMilli(), "days": agentUsageDays}
	if st.scanning || st.scanned.IsZero() {
		res["scanning"] = true
	}
	var agents []agentUsage
	for _, id := range []string{"claude", "codex"} {
		a := hostAgents[id]
		au := agentUsage{ID: id, Title: a.title, Installed: agentPath(a) != ""}
		if logs := st.logs[id]; logs != nil && !st.scanning {
			au.Today, au.Week = logs.totals(now)
			if !logs.last.IsZero() {
				au.LastUsed = logs.last.UnixMilli()
			}
		}
		switch id {
		case "claude":
			if len(claudeWins) > 0 {
				au.Windows, au.WindowsAt = claudeWins, claudeAt.UnixMilli()
			}
		case "codex":
			if codexLive {
				au.Windows, au.Plan, au.LimitReached, au.WindowsAt = codexWins, codexPlan, codexHit, codexAt.UnixMilli()
			} else if logs := st.logs[id]; logs != nil && !st.scanning && logs.limits != nil {
				au.Windows, au.Plan, au.WindowsAt = logs.limits.windows(now), logs.limits.PlanType, logs.limits.at.UnixMilli()
				au.LimitReached = logs.limits.Reached != nil && *logs.limits.Reached != ""
			}
		}
		au.Working = au.Installed && (au.Week.Requests > 0 || len(au.Windows) > 0)
		agents = append(agents, au)
	}
	res["agents"] = agents
	writeJSON(w, http.StatusOK, res)
}
