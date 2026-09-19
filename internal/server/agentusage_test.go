package server

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func claudeLogLine(id string, at time.Time, in, write, read, out int64) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":%q,"model":"claude-fable-5-1","role":"assistant","usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d}}}`+"\n",
		at.UTC().Format(time.RFC3339Nano), id, in, write, read, out)
}

func codexLogLine(at time.Time, total, in, cached, out int64) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":%d},"last_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"total_tokens":%d}},"rate_limits":{"primary":{"used_percent":42.5,"window_minutes":10080,"resets_at":%d},"secondary":null,"plan_type":"prolite","rate_limit_reached_type":null}}}`+"\n",
		at.UTC().Format(time.RFC3339Nano), total, in, cached, out, in+out, at.Add(48*time.Hour).Unix())
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// A message's lines count once, a resumed copy in another transcript not at
// all, last week's not at all; an appended line is picked up by the next
// scan and a half-written one waits for its newline.
func TestClaudeUsageScan(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 9, 19, 15, 0, 0, 0, time.Local)
	today, yesterday, old := now.Add(-time.Hour), now.Add(-26*time.Hour), now.Add(-9*24*time.Hour)
	a := filepath.Join(root, "-www-exe", "a.jsonl")
	appendFile(t, a, claudeLogLine("msg_old", old, 1000, 0, 0, 1000)+
		claudeLogLine("msg_1", yesterday, 10, 90, 900, 50)+
		`{"type":"user","timestamp":"2026-09-19T10:00:00Z","message":{"role":"user","content":"usage"}}`+"\n"+
		claudeLogLine("msg_2", today, 2, 8, 1000, 40)+
		claudeLogLine("msg_2", today, 2, 8, 1000, 40))
	appendFile(t, filepath.Join(root, "-www-exe", "a", "subagents", "agent-1.jsonl"),
		claudeLogLine("msg_2", today, 2, 8, 1000, 40)+claudeLogLine("msg_3", today, 1, 0, 0, 9))

	u := newUsageLogs(false)
	u.scan([]string{root}, now)
	td, wk := u.totals(now)
	if want := (tokenCount{Input: 11, Cached: 1000, Output: 49, Total: 1060, Requests: 2}); td != want {
		t.Fatalf("today = %+v, want %+v", td, want)
	}
	if want := (tokenCount{Input: 111, Cached: 1900, Output: 99, Total: 2110, Requests: 3}); wk != want {
		t.Fatalf("week = %+v, want %+v", wk, want)
	}

	half := claudeLogLine("msg_4", today, 5, 0, 0, 5)
	appendFile(t, a, half[:40])
	u.scan([]string{root}, now)
	if td, _ := u.totals(now); td.Requests != 2 {
		t.Fatalf("a half-written line counted: %+v", td)
	}
	appendFile(t, a, half[40:])
	u.scan([]string{root}, now)
	if td, _ := u.totals(now); td.Requests != 3 || td.Total != 1070 {
		t.Fatalf("after the append today = %+v", td)
	}
	if !u.last.Equal(today) {
		t.Fatalf("last = %v, want %v", u.last, today)
	}
}

// A Codex event sent twice counts once, a total that starts over still
// counts the request, and an archived rollout is the same thread.
func TestCodexUsageScan(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 9, 19, 15, 0, 0, 0, time.Local)
	at := now.Add(-2 * time.Hour)
	name := "rollout-2026-09-19T01-00-00-abc.jsonl"
	live := filepath.Join(home, "sessions", "2026", "09", "19", name)
	appendFile(t, live, codexLogLine(at, 1100, 1000, 800, 100)+
		codexLogLine(at.Add(time.Second), 1100, 1000, 800, 100)+ // sent again
		codexLogLine(at.Add(time.Minute), 3300, 2000, 1500, 200)+
		codexLogLine(at.Add(2*time.Minute), 550, 500, 0, 50)) // the total started over
	roots := []string{filepath.Join(home, "sessions"), filepath.Join(home, "archived_sessions")}

	u := newUsageLogs(true)
	u.scan(roots, now)
	td, _ := u.totals(now)
	if want := (tokenCount{Input: 1200, Cached: 2300, Output: 350, Total: 3850, Requests: 3}); td != want {
		t.Fatalf("today = %+v, want %+v", td, want)
	}
	if u.limits == nil || u.limits.PlanType != "prolite" {
		t.Fatalf("limits = %+v", u.limits)
	}
	w := u.limits.windows(now)
	if len(w) != 1 || w[0].Name != "seven_day" || w[0].UsedPct != 42.5 || w[0].Rolled {
		t.Fatalf("windows = %+v", w)
	}
	if w := u.limits.windows(now.Add(72 * time.Hour)); !w[0].Rolled {
		t.Fatalf("a window past its reset should say so: %+v", w)
	}

	archived := filepath.Join(home, "archived_sessions", name)
	if err := os.MkdirAll(filepath.Dir(archived), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(live, archived); err != nil {
		t.Fatal(err)
	}
	appendFile(t, archived, codexLogLine(at.Add(3*time.Minute), 660, 100, 0, 10))
	u.scan(roots, now)
	if td, _ := u.totals(now); td.Requests != 4 || td.Total != 3960 {
		t.Fatalf("after archiving today = %+v", td)
	}
}

// Claude's windows come from the freshest status file that has them.
func TestClaudeWindows(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(name, body string, age time.Duration) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	write("claude-2.status.json", fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":15,"resets_at":%d},"seven_day":{"used_percentage":46,"resets_at":%q}}}`,
		now.Add(time.Hour).Unix(), now.Add(-time.Minute).UTC().Format(time.RFC3339)), time.Minute)
	write("claude-3.status.json", `{"context_window":{"used_percentage":null}}`, time.Second) // fresh session, no windows yet
	write("codex-2.status.json", `{"rate_limits":{"five_hour":{"used_percentage":99}}}`, 0)
	wins, at := claudeWindows(dir, now)
	if len(wins) != 2 || wins[0].Name != "five_hour" || wins[0].UsedPct != 15 || wins[0].Rolled ||
		wins[1].Name != "seven_day" || wins[1].UsedPct != 46 || !wins[1].Rolled {
		t.Fatalf("windows = %+v", wins)
	}
	if d := now.Sub(at); d < 30*time.Second || d > 2*time.Minute {
		t.Fatalf("read at %v, want the older file's time", at)
	}
}
