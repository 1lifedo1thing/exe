package server

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"exe/internal/config"
)

// The test binary is its own stand-in for the Codex CLI: run with
// EXE_DICT_FAKE set to a folder, it answers as `codex app-server` would
// (fakeCodexAppServer) instead of running the tests.
func TestMain(m *testing.M) {
	if dir := os.Getenv("EXE_DICT_FAKE"); dir != "" {
		fakeCodexAppServer(dir)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeDictCodex points the dictionary at the stand-in: a script that runs
// this test binary as a Codex app server working in dir, whose answer is
// answer ("" for a session that fails).
func fakeDictCodex(t *testing.T, dir, answer string) {
	t.Helper()
	if answer != "" {
		if err := os.WriteFile(filepath.Join(dir, "answer.json"), []byte(answer), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "codex")
	script := "#!/bin/sh\nEXE_DICT_FAKE=" + shQuote(dir) + " exec " + shQuote(self) + " \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	was := dictCodexPath
	dictCodexPath = func() string { return bin }
	t.Cleanup(func() { dictCodexPath = was })
}

// fakeCodexAppServer speaks just enough of the app server's JSON-RPC: it
// notes its arguments and what thread/start and turn/start asked for in
// dir, a line per run in runs, then plays one turn — a reasoning pass with
// a summary, the answer in fragments, the tokens — or, with no answer.json,
// a turn that fails.
func fakeCodexAppServer(dir string) {
	note := func(name, text string) {
		f, _ := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		f.WriteString(text)
		f.Close()
	}
	note("args", strings.Join(os.Args[1:], " ")+"\n")
	note("runs", "run\n")
	time.Sleep(300 * time.Millisecond) // long enough for a second lookup to join
	enc := json.NewEncoder(os.Stdout)
	say := func(method string, params any) { enc.Encode(map[string]any{"method": method, "params": params}) }
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage
			Method string
			Params json.RawMessage
		}
		json.Unmarshal(sc.Bytes(), &m)
		switch m.Method {
		case "initialize":
			enc.Encode(map[string]any{"id": m.ID, "result": map[string]any{"userAgent": "fake"}})
		case "thread/start":
			note("thread", string(m.Params))
			enc.Encode(map[string]any{"id": m.ID, "result": map[string]any{"thread": map[string]any{"id": "t1"}}})
		case "turn/start":
			note("turn", string(m.Params))
			enc.Encode(map[string]any{"id": m.ID, "result": map[string]any{"turn": map[string]any{"id": "u1"}}})
			answer, err := os.ReadFile(filepath.Join(dir, "answer.json"))
			if err != nil {
				say("error", map[string]any{"error": map[string]any{"message": "boom"}, "willRetry": false})
				say("turn/completed", map[string]any{"turn": map[string]any{"status": "failed", "error": map[string]any{"message": "boom"}}})
				continue
			}
			say("item/started", map[string]any{"item": map[string]any{"type": "reasoning", "id": "r1"}})
			say("item/reasoning/summaryTextDelta", map[string]any{"itemId": "r1", "summaryIndex": 0, "delta": "**Checking the senses**"})
			say("item/completed", map[string]any{"item": map[string]any{"type": "reasoning", "id": "r1"}})
			say("item/started", map[string]any{"item": map[string]any{"type": "agentMessage", "id": "m1", "text": ""}})
			for r := []rune(string(answer)); len(r) > 0; {
				n := min(len(r), 10)
				say("item/agentMessage/delta", map[string]any{"itemId": "m1", "delta": string(r[:n])})
				r = r[n:]
			}
			say("item/completed", map[string]any{"item": map[string]any{"type": "agentMessage", "id": "m1", "text": string(answer)}})
			say("thread/tokenUsage/updated", map[string]any{"tokenUsage": map[string]any{"total": map[string]any{
				"totalTokens": 100, "inputTokens": 80, "cachedInputTokens": 0, "outputTokens": 20, "reasoningOutputTokens": 5}}})
			say("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}})
		}
	}
}

func dictLines(t *testing.T, resp *http.Response) []map[string]any {
	t.Helper()
	var lines []map[string]any
	dec := json.NewDecoder(resp.Body)
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, m)
	}
	return lines
}

func dictPost(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url+"/v1/dict", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

const dictAnswer = `{"found":true,"headword":"serendipity","note":"","forms":"",
"pronunciations":[{"label":"英","ipa":"/ˌserənˈdɪpəti/"},{"label":"美","ipa":"/ˌsɛrənˈdɪpəti/"}],
"senses":[{"pos":"n.","gloss":"意外收获；机缘巧合","definition":"偶然发现美好事物的机缘。"}],
"examples":[{"text":"It was pure serendipity.","translation":"这纯属机缘巧合。"},
 {"text":"Although she was looking for a date, serendipity led her to a letter.","translation":"她原本在找一个日期，却偶然发现了一封信。"}],
"etymology":"沃波尔于1754年创造。","related":[{"word":"serendipitous","relation":"派生词","gloss":"机缘巧合的"}],
"literary":{"note":"书面色彩。","quotes":[]},"suggestions":[]}`

// A word the dictionary does not keep is written by one ephemeral Codex
// session on Astra at xhigh, kept, and answered from the database after.
func TestDictWritesThenKeeps(t *testing.T) {
	dir := t.TempDir()
	fakeDictCodex(t, dir, dictAnswer)
	s := New(&config.Config{}, nil, nil, "", t.TempDir())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	if r, _ := http.Get(ts.URL + "/v1/dict?from=en&to=zh&q=serendipity"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("GET before = %d, want 404", r.StatusCode)
	}

	resp := dictPost(t, ts.URL, `{"from":"en","to":"zh","q":"  Serendipity "}`)
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content-type = %q", ct)
	}
	lines := dictLines(t, resp)
	if len(lines) < 3 || lines[0]["writing"] != true || lines[0]["model"] != "gpt-6-astra" || lines[0]["effort"] != "xhigh" {
		t.Fatalf("lines = %v", lines)
	}
	// on the way: the session's steps, and the entry a fragment at a time
	var kinds []string
	var text strings.Builder
	summary := ""
	for _, l := range lines[1 : len(lines)-1] {
		if st, ok := l["step"].(map[string]any); ok {
			kinds = append(kinds, st["kind"].(string))
			if st["kind"] == "summary" {
				summary, _ = st["text"].(string)
			}
		} else if d, ok := l["text"].(string); ok {
			text.WriteString(d)
		}
	}
	if got := strings.Join(kinds, " "); got != "session think summary thought answer usage" {
		t.Errorf("steps = %q", got)
	}
	if summary != "Checking the senses" {
		t.Errorf("summary = %q", summary)
	}
	if text.String() != dictAnswer {
		t.Errorf("streamed text = %q", text.String())
	}
	got, _ := lines[len(lines)-1]["entry"].(map[string]any)
	if got == nil || got["q"] != "serendipity" || got["written"] != true || got["model"] != "gpt-6-astra" {
		t.Fatalf("entry line = %v", lines[len(lines)-1])
	}
	if e, _ := got["entry"].(map[string]any); e == nil || e["headword"] != "serendipity" {
		t.Fatalf("entry = %v", got["entry"])
	}
	if tok, _ := got["tokens"].(map[string]any); tok == nil || tok["totalTokens"] != 100.0 {
		t.Errorf("tokens = %v", got["tokens"])
	}

	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	for _, want := range []string{"app-server", "--disable shell_tool", "--disable unified_exec"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("codex args %q lack %q", args, want)
		}
	}
	var thread, turn struct {
		Ephemeral           bool
		Model, Sandbox, Cwd string
		ApprovalPolicy      string
		Effort, Summary     string
		OutputSchema        map[string]any
		Input               []struct{ Text string }
	}
	b, _ := os.ReadFile(filepath.Join(dir, "thread"))
	json.Unmarshal(b, &thread)
	if !thread.Ephemeral || thread.Model != "gpt-6-astra" || thread.Sandbox != "read-only" || thread.ApprovalPolicy != "never" {
		t.Errorf("thread/start = %s", b)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "turn"))
	json.Unmarshal(b, &turn)
	if turn.Effort != "xhigh" || turn.Summary != "detailed" || turn.OutputSchema["type"] != "object" || len(turn.Input) != 1 {
		t.Fatalf("turn/start = %s", b)
	}
	prompt := turn.Input[0].Text
	for _, want := range []string{"English–Chinese", `"serendipity"`, "Simplified Chinese", `"英" and "美"`} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}

	// kept: a GET answers it, and a second lookup starts no session
	r, err := http.Get(ts.URL + "/v1/dict?from=en&to=zh&q=SERENDIPITY")
	if err != nil || r.StatusCode != http.StatusOK {
		t.Fatalf("GET after = %v %v", r.StatusCode, err)
	}
	r.Body.Close()
	lines = dictLines(t, dictPost(t, ts.URL, `{"from":"en","to":"zh","q":"serendipity"}`))
	if len(lines) != 1 || lines[0]["entry"] == nil {
		t.Fatalf("kept lines = %v", lines)
	}
	if e := lines[0]["entry"].(map[string]any); e["written"] != nil {
		t.Fatalf("a kept entry says written: %v", e)
	}
	if runs, _ := os.ReadFile(filepath.Join(dir, "runs")); strings.Count(string(runs), "run") != 1 {
		t.Fatalf("codex ran %d times", strings.Count(string(runs), "run"))
	}
	// another pair of languages is another entry
	if r, _ := http.Get(ts.URL + "/v1/dict?from=en&to=ja&q=serendipity"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("en→ja = %d, want 404", r.StatusCode)
	}
}

// Two lookups of one word at once share one session.
func TestDictSharesASession(t *testing.T) {
	dir := t.TempDir()
	fakeDictCodex(t, dir, dictAnswer)
	s := New(&config.Config{}, nil, nil, "", t.TempDir())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(ts.URL+"/v1/dict", "application/json",
				strings.NewReader(`{"from":"la","to":"zh","q":"amor"}`))
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			var last map[string]any
			dec := json.NewDecoder(resp.Body)
			for dec.More() {
				last = nil
				dec.Decode(&last)
			}
			if last["entry"] == nil {
				t.Errorf("last line = %v", last)
			}
		}()
	}
	wg.Wait()
	if runs, _ := os.ReadFile(filepath.Join(dir, "runs")); strings.Count(string(runs), "run") != 1 {
		t.Fatalf("codex ran %d times", strings.Count(string(runs), "run"))
	}
	turn, _ := os.ReadFile(filepath.Join(dir, "turn"))
	if !strings.Contains(string(turn), "Latin–Chinese") || !strings.Contains(string(turn), "macrons") {
		t.Fatalf("turn/start:\n%s", turn)
	}
}

// A lookup that is no word of the source language comes back with
// suggestions, and is not kept.
func TestDictNotAWord(t *testing.T) {
	dir := t.TempDir()
	fakeDictCodex(t, dir, `{"found":false,"headword":"","note":"","forms":"","pronunciations":[],"senses":[],
"examples":[],"etymology":"","related":[],"literary":{"note":"","quotes":[]},"suggestions":["serendipity"]}`)
	s := New(&config.Config{}, nil, nil, "", t.TempDir())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	lines := dictLines(t, dictPost(t, ts.URL, `{"from":"en","to":"ko","q":"serendipty"}`))
	last := lines[len(lines)-1]
	if last["notfound"] != true || len(last["suggestions"].([]any)) != 1 {
		t.Fatalf("lines = %v", lines)
	}
	if r, _ := http.Get(ts.URL + "/v1/dict?from=en&to=ko&q=serendipty"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("a non-word was kept: %d", r.StatusCode)
	}
}

// A session that fails says why, and nothing is kept.
func TestDictSessionFails(t *testing.T) {
	dir := t.TempDir()
	fakeDictCodex(t, dir, "")
	s := New(&config.Config{}, nil, nil, "", t.TempDir())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	lines := dictLines(t, dictPost(t, ts.URL, `{"from":"fr","to":"zh","q":"maison"}`))
	if last := lines[len(lines)-1]; last["error"] != "Codex: boom" {
		t.Fatalf("lines = %v", lines)
	}
	if r, _ := http.Get(ts.URL + "/v1/dict?from=fr&to=zh&q=maison"); r.StatusCode != http.StatusNotFound {
		t.Fatalf("a failed session kept something: %d", r.StatusCode)
	}
}

// Without the Codex CLI a word the dictionary lacks cannot be written; the
// answer says so in the words the app shows.
func TestDictNoBackend(t *testing.T) {
	was := dictCodexPath
	dictCodexPath = func() string { return "" }
	defer func() { dictCodexPath = was }()
	s := New(&config.Config{}, nil, nil, "", t.TempDir())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp := dictPost(t, ts.URL, `{"from":"de","to":"zh","q":"Haus"}`)
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusServiceUnavailable || body["error"] != "No Usable LLM Backend" {
		t.Fatalf("status %d, body %v", resp.StatusCode, body)
	}
}

func TestDictBadLookups(t *testing.T) {
	was := dictCodexPath
	dictCodexPath = func() string { return "" } // never a real session
	defer func() { dictCodexPath = was }()
	s := New(&config.Config{}, nil, nil, "", t.TempDir())
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	for _, body := range []string{
		`{"from":"xx","to":"zh","q":"word"}`,
		`{"from":"en","to":"fr","q":"word"}`,
		`{"from":"en","to":"zh","q":"   "}`,
		`{"from":"en","to":"zh","q":"` + strings.Repeat("a", 81) + `"}`,
		`{"from":"en","to":"zh","q":"bell\u0007"}`,
	} {
		if r := dictPost(t, ts.URL, body); r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, r.StatusCode)
		}
	}
}
