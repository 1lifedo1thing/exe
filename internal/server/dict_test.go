package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"exe/internal/config"
)

// fakeDictCodex stands in for the Codex CLI: a script that notes its
// arguments and its stdin in dir, one line per run in runs, and writes
// answer as the session's last message.
func fakeDictCodex(t *testing.T, dir, answer string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "answer.json"), []byte(answer), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
dir=` + shQuote(dir) + `
printf '%s\n' "$*" > "$dir/args"
cat > "$dir/prompt"
echo run >> "$dir/runs"
sleep 0.3
while [ $# -gt 0 ]; do
  if [ "$1" = "--output-last-message" ]; then cp "$dir/answer.json" "$2"; fi
  shift
done
`
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	was := dictCodexPath
	dictCodexPath = func() string { return bin }
	t.Cleanup(func() { dictCodexPath = was })
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
	if len(lines) != 2 || lines[0]["writing"] != true || lines[0]["model"] != "gpt-6-astra" || lines[0]["effort"] != "xhigh" {
		t.Fatalf("lines = %v", lines)
	}
	got, _ := lines[1]["entry"].(map[string]any)
	if got == nil || got["q"] != "serendipity" || got["written"] != true || got["model"] != "gpt-6-astra" {
		t.Fatalf("entry line = %v", lines[1])
	}
	if e, _ := got["entry"].(map[string]any); e == nil || e["headword"] != "serendipity" {
		t.Fatalf("entry = %v", got["entry"])
	}

	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	for _, want := range []string{"exec", "--ephemeral", "--model gpt-6-astra", `model_reasoning_effort="xhigh"`,
		"--sandbox read-only", "--output-schema"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("codex args %q lack %q", args, want)
		}
	}
	prompt, _ := os.ReadFile(filepath.Join(dir, "prompt"))
	for _, want := range []string{"English–Chinese", `"serendipity"`, "Simplified Chinese", `"英" and "美"`} {
		if !strings.Contains(string(prompt), want) {
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
	prompt, _ := os.ReadFile(filepath.Join(dir, "prompt"))
	if !strings.Contains(string(prompt), "Latin–Chinese") || !strings.Contains(string(prompt), "macrons") {
		t.Fatalf("prompt:\n%s", prompt)
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
