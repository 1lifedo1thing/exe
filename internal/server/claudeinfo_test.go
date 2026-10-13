package server

import (
	"strconv"
	"testing"
	"time"
)

func TestClaudeSessionInfo(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 10, 12, 20, 0, 0, 0, time.UTC)
	id := "81afe071-7e89-4ee7-9522-b7e0ca11f6b6"
	path := writeClaudeTranscript(t, home, "-www-exe", id, "cli", "/www/exe", "add a tornado to the city", now)
	// one reply in three blocks, sharing the id and the usage: counted once
	reply := func(id, model string, in, out, cw, cr int, block string) string {
		return `{"type":"assistant","timestamp":"2026-09-23T06:54:00.000Z","message":{"id":"` + id + `","model":"` + model + `","role":"assistant","content":[` + block +
			`],"usage":{"input_tokens":` + strconv.Itoa(in) + `,"output_tokens":` + strconv.Itoa(out) + `,"cache_creation_input_tokens":` + strconv.Itoa(cw) + `,"cache_read_input_tokens":` + strconv.Itoa(cr) + `}},"uuid":"a2"}`
	}
	for _, b := range []string{`{"type":"thinking","thinking":"hm"}`, `{"type":"text","text":"On it."}`, `{"type":"tool_use","id":"t2","name":"Bash","input":{}}`} {
		appendLine(t, path, reply("msg_1", "claude-fable-5-1", 10, 100, 1000, 5000, b), now)
	}
	// a tool's result is the tool's turn, text beside it or not
	appendLine(t, path, `{"type":"user","timestamp":"2026-09-23T06:54:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"done"},{"type":"text","text":"and this"}]},"uuid":"u3"}`, now)
	// a synthetic entry — the CLI's own words in the model's place — is no reply
	appendLine(t, path, `{"type":"assistant","message":{"id":"msg_x","model":"<synthetic>","role":"assistant","content":[{"type":"text","text":"API error"}],"usage":{"input_tokens":0,"output_tokens":0}}}`, now)
	// a sub-agent's prompt is not the person's
	appendLine(t, path, `{"type":"user","isSidechain":true,"message":{"role":"user","content":"search the code for tornadoes"},"uuid":"u4"}`, now)
	appendLine(t, path, reply("msg_2", "claude-haiku-4-5", 5, 20, 0, 0, `{"type":"text","text":"found"}`), now)
	// the person's second turn, a paste with a second line, and a title for it all
	appendLine(t, path, `{"type":"user","timestamp":"2026-09-23T07:00:00.000Z","message":{"role":"user","content":"make it bigger\nmuch bigger"},"uuid":"u5"}`, now)
	appendLine(t, path, reply("msg_3", "claude-fable-5-1", 7, 30, 200, 6000, `{"type":"text","text":"Bigger."}`), now)
	appendLine(t, path, `{"type":"ai-title","aiTitle":"Tornado in the city","sessionId":"`+id+`"}`, now)

	i := claudeSessionInfoOf(path)
	if i == nil {
		t.Fatal("no info")
	}
	if i.ID != id || i.Title != "Tornado in the city" || i.Summary != "Tornado in the city" {
		t.Errorf("id/title/summary: %q %q %q", i.ID, i.Title, i.Summary)
	}
	if i.Prompt != "add a tornado to the city" || i.LastPrompt != "make it bigger" {
		t.Errorf("prompts: %q, last %q", i.Prompt, i.LastPrompt)
	}
	if i.Turns != 2 || i.Replies != 3 {
		t.Errorf("turns %d replies %d, want 2 and 3", i.Turns, i.Replies)
	}
	if want := (claudeTokens{Input: 22, Output: 150, CacheRead: 11000, CacheWrite: 1200}); i.Tokens != want {
		t.Errorf("tokens %+v, want %+v", i.Tokens, want)
	}
	if len(i.Models) != 2 || i.Models[0] != "claude-fable-5-1" || i.Models[1] != "claude-haiku-4-5" {
		t.Errorf("models %v", i.Models)
	}
	if i.Cwd != "/www/exe" || i.Origin != "a terminal" || i.Created != time.Date(2026, 9, 23, 6, 53, 13, 0, time.UTC).Unix() || i.Activity != now.Unix() {
		t.Errorf("cwd %q origin %q created %d activity %d", i.Cwd, i.Origin, i.Created, i.Activity)
	}
	// the cache answers while the file is the size it was read at, and
	// reads again once it grew
	if again := claudeSessionInfoOf(path); again != i {
		t.Error("a second reading, same size: want the cached one")
	}
	appendLine(t, path, `{"type":"custom-title","customTitle":"My tornado","sessionId":"`+id+`"}`, now.Add(time.Minute))
	grown := claudeSessionInfoOf(path)
	if grown == i || grown.Title != "My tornado" || grown.Summary != "Tornado in the city" || grown.Activity != now.Add(time.Minute).Unix() {
		t.Errorf("after growth: %+v", grown)
	}
	// a session with a summary of the CLI's own shows that
	appendLine(t, path, `{"type":"summary","summary":"The city gets a tornado, then a bigger one.","leafUuid":"a9"}`, now.Add(2*time.Minute))
	if i := claudeSessionInfoOf(path); i.Summary != "The city gets a tornado, then a bigger one." {
		t.Errorf("summary %q", i.Summary)
	}
	// a transcript with no prompt yet is no conversation
	empty := writeClaudeTranscript(t, home, "-www-exe", "c8570b33-0815-5847-9e9b-97c75876f60f", "cli", "/www/exe", "", now)
	if claudeSessionInfoOf(empty) != nil {
		t.Error("a transcript with no prompt: want nil")
	}
	if claudeSessionInfoOf(home+"/none.jsonl") != nil {
		t.Error("a missing file: want nil")
	}
}
