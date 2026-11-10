package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

// Sessions nobody has touched. A Claude Code or Codex session at its
// prompt holds a few hundred megabytes for as long as it lives, and a
// column that is used every day collects sessions nobody will open again
// this month. Put Away gets that memory back and keeps the row; this does
// it for the sessions left alone for put_away_days (seven unless the
// configuration says otherwise, 0 for never). A pass an hour looks at
// each running session's clocks and puts away the ones that have all
// stood still that long:
//
//   - the last key a person typed into it, and the last time a window
//     landed on it — tmux's own stamps (session_activity,
//     session_last_attached);
//   - the last write to its conversation, the transcript or the rollout:
//     a prompt that came over the API, a background task that finished;
//   - the last word of its hooks (the state file), and its start.
//
// The pane's own output is not one of them: Codex repaints at its prompt
// every few minutes, so an idle session's pane is never a week quiet.
//
// Left alone, however old: a session a window is showing; one that is
// working; and one whose turn is still open in its conversation — it
// waits on a permission or a question, and putting it away would lose
// that turn with nobody there to agree to it. One with no conversation
// yet has nothing to continue, and Put Away refuses it itself.
//
// What a pass put away is one line in the Newsfeed, with what the
// machine got back.

const (
	// agentIdleFirst is how long after the daemon starts the first pass
	// runs, agentIdleEvery how often from then on.
	agentIdleFirst = 5 * time.Minute
	agentIdleEvery = time.Hour
)

// sessionClock is tmux's own record of who was last at a session.
type sessionClock struct {
	activity     int64 // the last key from a client
	lastAttached int64 // the last time a client landed on it, 0 for never
	attached     bool  // a client shows it now
}

// tmuxSessionClocks reads every session's clock; an error without tmux,
// or without a server running.
func tmuxSessionClocks() (map[string]sessionClock, error) {
	cmd := tmuxCmd("list-sessions", "-F", "#{session_name}:#{session_activity}:#{session_last_attached}:#{session_attached}")
	if cmd == nil {
		return nil, fmt.Errorf("no tmux on this host")
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	clocks := map[string]sessionClock{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(line, ":")
		if len(f) != 4 {
			continue
		}
		var c sessionClock
		c.activity, _ = strconv.ParseInt(f[1], 10, 64)
		c.lastAttached, _ = strconv.ParseInt(f[2], 10, 64)
		c.attached = f[3] != "0" && f[3] != ""
		clocks[f[0]] = c
	}
	return clocks, nil
}

// agentConversationFile is the file a session's conversation is written
// to — Claude Code's transcript, Codex's rollout — "" when it is not
// known.
func (s *Server) agentConversationFile(a hostAgent, name string) string {
	status := s.agentStatusFile(a, name)
	if !a.notify {
		return claudeTrailPath(status)
	}
	return codexRolloutPath(codexHome(), readAgentThreadID(status))
}

// agentTouched is when a session was last touched: the latest of its
// clocks (above).
func (s *Server) agentTouched(a hostAgent, ss agentSession, c sessionClock) int64 {
	at := max(ss.Created, c.activity, c.lastAttached)
	files := []string{s.agentStateFile(a, ss.Name)}
	if path := s.agentConversationFile(a, ss.Name); path != "" {
		files = append(files, path)
	}
	for _, f := range files {
		if st, err := os.Stat(f); err == nil {
			at = max(at, st.ModTime().Unix())
		}
	}
	return at
}

// RunAgentIdle puts away the sessions nobody has touched, for the life
// of the daemon.
func (s *Server) RunAgentIdle(ctx context.Context) {
	t := time.NewTimer(agentIdleFirst)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.putAwayIdle(agentList(), time.Now())
		t.Reset(agentIdleEvery)
	}
}

// putAwayIdle is one pass over these agents' sessions at now: it puts
// away each one left alone for put_away_days, and says so in the
// Newsfeed. It answers with the sessions it put away.
func (s *Server) putAwayIdle(agents []hostAgent, now time.Time) []string {
	days := s.Config().PutAwayDays
	if days <= 0 {
		return nil
	}
	clocks, err := tmuxSessionClocks()
	if err != nil {
		return nil // no server: nothing runs
	}
	limit := now.Unix() - int64(days)*24*60*60
	var names, titles []string
	var freed, swap uint64
	measured := false
	for _, a := range agents {
		list := s.agentSessions(a)
		s.markAgentStates(a, list)
		for _, ss := range list {
			c, ok := clocks[ss.Name]
			if !ok || c.attached || ss.Working || s.agentTrailFor(a, ss).open {
				continue
			}
			if s.agentTouched(a, ss, c) > limit {
				continue
			}
			m, err := s.putAwayAgentSession(a, ss.Name)
			if err != nil {
				continue // no conversation yet, or it ended meanwhile
			}
			f := m.settle()
			freed, swap, measured = freed+f.Freed, swap+f.Swap, measured || f.Measured
			names = append(names, ss.Name)
			title := strings.TrimSpace(strings.TrimPrefix(ss.Title, "✳"))
			if title == "" {
				title = ss.Name
			}
			titles = append(titles, title)
		}
	}
	if len(names) == 0 {
		return nil
	}
	head, body := idleNews(titles, days, freed, swap, measured)
	log.Printf("agent sessions: %s (%s)", strings.ToLower(head[:1])+head[1:], strings.Join(names, ", "))
	s.PostNews("agent", head, body)
	return names
}

// idleNews is the Newsfeed's line for a pass: how many sessions, which
// (the first three by title), for how long they were left alone, and
// what the machine got back where it could be read.
func idleNews(titles []string, days int, freed, swap uint64, measured bool) (head, body string) {
	plural := func(n int, word string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s", n, word)
		}
		return fmt.Sprintf("%d %ss", n, word)
	}
	head = "Put away " + plural(len(titles), "idle session")
	shown := titles
	if len(shown) > 3 {
		shown = shown[:3]
	}
	for i, t := range shown {
		shown[i] = "“" + t + "”"
	}
	body = strings.Join(shown, ", ")
	if more := len(titles) - len(shown); more > 0 {
		body += fmt.Sprintf(" and %d more", more)
	}
	body += ": not touched for " + plural(days, "day") + "."
	if measured {
		body += " " + newsBytes(freed) + " freed"
		if swap > 0 {
			body += " and " + newsBytes(swap) + " of swap"
		}
		body += "."
	}
	return head, body + " Click a row to continue it."
}

// newsBytes writes a size the way the desktop's toasts do: 185.2 MB.
func newsBytes(n uint64) string {
	units := []string{"bytes", "KB", "MB", "GB", "TB"}
	v, u := float64(n), 0
	for v >= 1024 && u < len(units)-1 {
		v, u = v/1024, u+1
	}
	if u == 0 {
		return fmt.Sprintf("%d bytes", n)
	}
	return fmt.Sprintf("%.1f %s", v, units[u])
}
