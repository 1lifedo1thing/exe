package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A push when an agent's turn ends: the daemon tells every subscribed
// browser (webpush.go — the same subscriptions the price and weather
// alerts use, and no switch of its own) that a Claude Code or Codex
// session has finished its turn, or that a Chat run has ended, and a tap
// on the notification opens that window on that conversation (sw.js
// hands the desk the message's Show).
//
// For the agent windows the news is already on disk: each session's
// hooks write the word "done" to its state file at the end of a turn
// (agentStateHooks for Claude Code, codexArgs' notify for Codex), written
// beside the file and moved into place, so every turn end is a fresh
// file whatever the word was before — Codex writes nothing else there.
// RunAgentPush polls those files; a new stamp on one that says "done" is
// a turn end. A Chat run reports its own end (pushChatEnd, from
// startChatRun).
//
// One rule keeps the pushes from nagging: a turn that ends within
// agentPushQuiet of the person's last word in that window — a key typed
// into the session from a desk, a message sent or queued in the chat —
// stays quiet. They are still there.

const agentPushQuiet = time.Minute

// agentPushPoll is how often the state files are looked at; a variable
// so the test can hurry it.
var agentPushPoll = 2 * time.Second

// agentStateStamp is one state file as a pass over the folder saw it.
type agentStateStamp struct {
	mod  time.Time
	word string
}

// agentStateName matches a state file's name: the agent's app, then the
// session's number when it is not the icon's own ("claude.state",
// "codex-12.state").
var agentStateName = regexp.MustCompile(`^([a-z]+)(?:-([0-9]+))?\.state$`)

// agentStateSession is the tmux session a state file belongs to, ok
// false for any other file in the folder.
func agentStateSession(file string) (a hostAgent, session string, ok bool) {
	m := agentStateName.FindStringSubmatch(file)
	if m == nil {
		return a, "", false
	}
	a, ok = hostAgents[m[1]]
	if !ok {
		return a, "", false
	}
	n := 1
	if m[2] != "" {
		if n, _ = strconv.Atoi(m[2]); n < 2 {
			return a, "", false
		}
	}
	return a, agentSessionName(a, n), true
}

// agentTurnEnds compares two passes over the state files and names the
// files whose turn ended in between: a stamp that moved, on a file that
// now says "done". prev nil is the first pass after the daemon started,
// which only takes the stamps — a turn that ended while it was down is
// old news by now.
func agentTurnEnds(prev, cur map[string]agentStateStamp) []string {
	if prev == nil {
		return nil
	}
	var ends []string
	for file, c := range cur {
		if p, seen := prev[file]; c.word == agentStateDone && (!seen || !p.mod.Equal(c.mod)) {
			ends = append(ends, file)
		}
	}
	return ends
}

// readAgentStates is one pass over the agents folder.
func readAgentStates(dir string) map[string]agentStateStamp {
	cur := map[string]agentStateStamp{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return cur
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".state") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		cur[e.Name()] = agentStateStamp{mod: info.ModTime(), word: readAgentState(filepath.Join(dir, e.Name()))}
	}
	return cur
}

// RunAgentPush follows the agent sessions' state files for the life of
// the daemon and pushes each turn end.
func (s *Server) RunAgentPush(ctx context.Context) {
	dir := filepath.Join(s.StateDir, "agents")
	var prev map[string]agentStateStamp
	t := time.NewTicker(agentPushPoll)
	defer t.Stop()
	for {
		cur := readAgentStates(dir)
		for _, file := range agentTurnEnds(prev, cur) {
			if a, session, ok := agentStateSession(file); ok {
				s.pushAgentTurnEnd(a, session, time.Now())
			}
		}
		prev = cur
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// noteAgentInput stamps a person's input into an agent session from a
// desk window, for the quiet rule.
func (s *Server) noteAgentInput(session string, now time.Time) {
	s.agentInputMu.Lock()
	if s.agentInput == nil {
		s.agentInput = map[string]time.Time{}
	}
	s.agentInput[session] = now
	s.agentInputMu.Unlock()
}

// agentQuiet says whether the person's last input into a session is
// recent enough that its turn end needs no push.
func (s *Server) agentQuiet(session string, now time.Time) bool {
	s.agentInputMu.Lock()
	last, ok := s.agentInput[session]
	s.agentInputMu.Unlock()
	return ok && now.Sub(last) < agentPushQuiet
}

// termReport matches what a terminal sends by itself, with nobody at the
// keys: its answers to the program's queries (cursor position, device
// attributes, window size, colours) and the focus reports. Everything
// else in a window's binary frames is the person: keys, paste, the mouse.
var termReport = regexp.MustCompile(`^(?:\x1b\[[?>=]?[0-9;]*(?:[Rcntuy]|\$y)|\x1b\[[IO]|\x1b[\]P][^\x07\x1b]*(?:\x07|\x1b\\))+$`)

// personInput says whether a frame from an agent window carries
// something the person did.
func personInput(data []byte) bool {
	return len(data) > 0 && !termReport.Match(data)
}

// pushAgentTurnEnd sends the push for one session's turn end, unless
// the quiet rule holds it or nobody is subscribed.
func (s *Server) pushAgentTurnEnd(a hostAgent, session string, now time.Time) {
	if s.agentQuiet(session, now) {
		return
	}
	if !s.pushWanted() {
		return
	}
	msg := agentTurnEndMessage(a, session, s.agentSessions(a))
	go s.pushLogged(a.app+" push", msg)
}

// agentTurnEndMessage words the push: the agent in the title, the
// session's own title — the task the CLI keeps there — in the body, or
// the name the column gives a session without one ("Claude Code 12").
func agentTurnEndMessage(a hostAgent, session string, list []agentSession) pushMessage {
	body := fmt.Sprintf("%s %d", a.title, agentSessionNumber(a, session))
	for _, l := range list {
		if l.Name == session && l.Title != "" {
			body = strings.TrimSpace(strings.TrimPrefix(l.Title, "✳"))
		}
	}
	show := a.app + ":" + session
	return pushMessage{Title: a.title + " finished", Body: body, Tag: "agent-" + session, URL: "/#show=" + show, Show: show}
}

// chatEndMessage words the push for a Chat run that ended on its own:
// the chat's title in the body, or what stopped it when it failed.
func chatEndMessage(id, title, vm, failed string) pushMessage {
	head := "Chat"
	if vm != "" {
		head = "Chat with " + vm
	}
	if title == "" {
		title = "Untitled chat"
	}
	msg := pushMessage{Title: head + " finished", Body: title, Tag: "chat-" + id, URL: "/#show=chat:" + id, Show: "chat:" + id}
	if failed != "" {
		msg.Title = head + " stopped"
		if r := []rune(failed); len(r) > 160 {
			failed = string(r[:160]) + "…"
		}
		msg.Body = title + " — " + failed
	}
	return msg
}

// pushChatEnd sends the push for a Chat run's end. A run the person
// stopped, or one the daemon drained on its way down, is no news; nor is
// one that ends within agentPushQuiet of their last message in it.
func (s *Server) pushChatEnd(id, title, vm, failed string, stopped bool, lastInput, now time.Time) {
	if stopped || now.Sub(lastInput) < agentPushQuiet || !s.pushWanted() {
		return
	}
	go s.pushLogged("chat push", chatEndMessage(id, title, vm, failed))
}

// pushWanted says whether any browser is subscribed at all.
func (s *Server) pushWanted() bool {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	return len(s.loadPushSubs()) > 0
}

// pushLogged sends one message to every subscription and logs what
// became of it.
func (s *Server) pushLogged(what string, msg pushMessage) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	n, errs := s.pushAll(ctx, msg)
	log.Printf("%s: %s — %s (sent to %d)", what, msg.Title, msg.Body, n)
	for _, e := range errs {
		log.Printf("%s: %s", what, e)
	}
}
