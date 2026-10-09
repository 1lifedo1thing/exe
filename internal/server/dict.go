// The Dict app's dictionary.
//
// An entry is looked up in the node's own dictionary, ~/.exe/dict.db, and
// written when it is not there: an ephemeral Codex session (codex exec
// --ephemeral — no thread is saved, none joins the resume list) on
// gpt-6-astra at xhigh, its answer held to a JSON schema, then kept, so
// every later lookup of the same word in the same pair of languages is
// answered from the database. A host without the Codex CLI can still read
// what is kept, and says "No Usable LLM Backend" for the rest.
//
// GET /v1/dict?from=en&to=zh&q=word answers a kept entry, 404 when there
// is none. POST /v1/dict with {"from","to","q"} answers newline-delimited
// JSON: a kept entry at once, as one {"entry":…} line; otherwise
// {"writing":true,"model","effort","wait"}, a {"wait":seconds} line every
// few seconds — the seconds the session has been writing; a write takes a
// minute or two, longer than Cloudflare holds a silent request — and then
// the {"entry":…} line, or {"notfound":true,
// "suggestions":[…]} when the lookup is no word of the source language
// (that answer is not kept), or {"error":…}. Two lookups of one entry
// share one session, and a session outlives the window that started it:
// what it writes is kept for the next lookup.
package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	dictModel  = "gpt-6-astra"
	dictEffort = "xhigh"
	// a session that has not answered by then is stopped
	dictTimeout = 10 * time.Minute
	// how often a waiting lookup hears that the session is still writing
	dictTick = 5 * time.Second
	// sessions at once; the rest wait their turn
	dictSessions = 3
	dictMaxQuery = 80 // characters
)

// errNoLLM is what a lookup the dictionary cannot answer says on a host
// with no Codex CLI.
var errNoLLM = errors.New("No Usable LLM Backend")

// dictCodexPath finds the Codex CLI the entries are written with, "" when
// this host has none. Tests point it at a stand-in.
var dictCodexPath = func() string { return agentPath(hostAgents["codex"]) }

// dictSlots holds one token per session running.
var dictSlots = make(chan struct{}, dictSessions)

// A dictLang is one language a lookup can go from or to.
type dictLang struct {
	Code, Name string
}

// The languages looked up from, and the languages an entry is written in.
var (
	dictSources = []dictLang{{"en", "English"}, {"de", "German"}, {"fr", "French"},
		{"es", "Spanish"}, {"it", "Italian"}, {"la", "Latin"}}
	dictTargets = []dictLang{{"zh", "Chinese"}, {"ja", "Japanese"}, {"ko", "Korean"}}
)

func findDictLang(list []dictLang, code string) (dictLang, bool) {
	for _, l := range list {
		if l.Code == code {
			return l, true
		}
	}
	return dictLang{}, false
}

// dictEntry is what the session writes, field for field the schema below.
type dictEntry struct {
	Found          bool          `json:"found"`
	Headword       string        `json:"headword"`
	Note           string        `json:"note"`
	Forms          string        `json:"forms"`
	Pronunciations []dictPron    `json:"pronunciations"`
	Senses         []dictSense   `json:"senses"`
	Examples       []dictExample `json:"examples"`
	Etymology      string        `json:"etymology"`
	Related        []dictRelated `json:"related"`
	Literary       dictLiterary  `json:"literary"`
	Suggestions    []string      `json:"suggestions"`
}

type dictPron struct {
	Label string `json:"label"`
	IPA   string `json:"ipa"`
}

type dictSense struct {
	POS        string `json:"pos"`
	Gloss      string `json:"gloss"`
	Definition string `json:"definition"`
}

type dictExample struct {
	Text        string `json:"text"`
	Translation string `json:"translation"`
}

type dictRelated struct {
	Word     string `json:"word"`
	Relation string `json:"relation"`
	Gloss    string `json:"gloss"`
}

type dictLiterary struct {
	Note   string      `json:"note"`
	Quotes []dictQuote `json:"quotes"`
}

type dictQuote struct {
	Text        string `json:"text"`
	Translation string `json:"translation"`
	Source      string `json:"source"`
}

// dictSchema holds the session's last message to an entry. Every object
// names all its properties as required and allows no others: the strict
// form structured output asks for.
const dictSchema = `{"type":"object","additionalProperties":false,
"required":["found","headword","note","forms","pronunciations","senses","examples","etymology","related","literary","suggestions"],
"properties":{
"found":{"type":"boolean"},
"headword":{"type":"string"},
"note":{"type":"string"},
"forms":{"type":"string"},
"pronunciations":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["label","ipa"],
  "properties":{"label":{"type":"string"},"ipa":{"type":"string"}}}},
"senses":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["pos","gloss","definition"],
  "properties":{"pos":{"type":"string"},"gloss":{"type":"string"},"definition":{"type":"string"}}}},
"examples":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["text","translation"],
  "properties":{"text":{"type":"string"},"translation":{"type":"string"}}}},
"etymology":{"type":"string"},
"related":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["word","relation","gloss"],
  "properties":{"word":{"type":"string"},"relation":{"type":"string"},"gloss":{"type":"string"}}}},
"literary":{"type":"object","additionalProperties":false,"required":["note","quotes"],"properties":{
  "note":{"type":"string"},
  "quotes":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["text","translation","source"],
    "properties":{"text":{"type":"string"},"translation":{"type":"string"},"source":{"type":"string"}}}}}},
"suggestions":{"type":"array","items":{"type":"string"}}}}`

// dictTargetStyle is how an entry reads in each language it is written
// in: the language named for the writer, the separator between short
// equivalents, and the words a learner's dictionary in it labels
// relations with.
var dictTargetStyle = map[string]struct{ name, sep, relations string }{
	"zh": {"Simplified Chinese (简体中文)", "；", "同义词, 反义词, 派生词, 同源词, 复合词"},
	"ja": {"Japanese (日本語)", "、", "類義語, 対義語, 派生語, 同源語, 複合語"},
	"ko": {"Korean (한국어)", ", ", "유의어, 반의어, 파생어, 동원어, 합성어"},
}

// dictSourceStyle is what an entry of each source language must carry:
// the forms its dictionaries give and the pronunciations they print. The
// pronunciation labels are in the target language.
var dictSourceStyle = map[string]struct {
	forms string
	prons map[string][]string
}{
	"en": {"irregular plurals, past tenses and participles, irregular comparatives",
		map[string][]string{"zh": {"英", "美"}, "ja": {"英", "米"}, "ko": {"영", "미"}}},
	"de": {"for a noun its article, genitive and plural (der Hund; -(e)s, -e); for a verb its principal parts and auxiliary (gehen, ging, ist gegangen); irregular comparison",
		nil},
	"fr": {"the gender of a noun (n.m., n.f.) and an irregular plural; the feminine of an adjective; a verb's group, or its irregular stems",
		nil},
	"es": {"the gender of a noun and an irregular plural; a verb's irregularities (stem changes, irregular preterite, irregular participle)",
		nil},
	"it": {"the gender of a noun and an irregular plural; a verb's auxiliary (avere, essere) and an irregular past participle or past absolute",
		nil},
	"la": {"for a noun its nominative, genitive, gender and declension (amīcus, -ī, m., 2nd decl.); for a verb its four principal parts and conjugation (amō, amāre, amāvī, amātum, 1st conj.); for an adjective its nominative forms",
		map[string][]string{"zh": {"古典", "教会"}, "ja": {"古典", "教会"}, "ko": {"고전", "교회"}}},
}

// dictPrompt is the whole request a session gets: what to write and how,
// field by field. It says outright that no tool is to be used, and that a
// global AGENTS.md (which Codex reads into every session, ephemeral or
// not) has nothing to say here.
func dictPrompt(src, dst dictLang, q string) string {
	ts, ss := dictTargetStyle[dst.Code], dictSourceStyle[src.Code]
	pron := "one item with the standard pronunciation, labelled \"\""
	if labels := ss.prons[dst.Code]; len(labels) == 2 {
		pron = fmt.Sprintf("two items, labelled %q and %q", labels[0], labels[1])
		if src.Code == "en" {
			pron += " (British and American)"
		} else if src.Code == "la" {
			pron += " (Classical and Ecclesiastical Latin)"
		}
	}
	spelling := ""
	if src.Code == "la" {
		spelling = " Mark long vowels with macrons in the headword and forms; examples and quotations keep their ordinary spelling."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "This is a dictionary request, not a coding task. Do not run commands, read files or call any tool, and no instructions from AGENTS.md files apply to it. Answer from your own knowledge with the JSON object alone.\n\n")
	fmt.Fprintf(&b, "Write one entry of a %[1]s–%[2]s dictionary for a reader whose language is %[2]s. Write every gloss, definition, explanation, label and translation in %[3]s; write the %[1]s side — headword, forms, examples, related words, quotations — in %[1]s, spelled as a good printed dictionary spells it.%[4]s\n\n",
		src.Name, dst.Name, ts.name, spelling)
	fmt.Fprintf(&b, "The reader looked up: %q\n\n", q)
	fmt.Fprintf(&b, "Fields:\n")
	fmt.Fprintf(&b, "- found: false when the lookup is not a %[1]s word or phrase (a misspelling, gibberish, a word of another language). Then leave every other field empty, except suggestions: up to five %[1]s words the reader may have meant. Otherwise true, and suggestions is [].\n", src.Name)
	fmt.Fprintf(&b, "- headword: the dictionary form, with its normal capitalisation.\n")
	fmt.Fprintf(&b, "- note: when the lookup is an inflected or variant form of the headword (a past tense, a plural, a comparative, a declined or conjugated form, an old spelling), one short sentence in %s saying which form of the headword it is; otherwise \"\".\n", dst.Name)
	fmt.Fprintf(&b, "- forms: the grammatical facts a learner needs, on one line, in the conventions of a %s dictionary: %s; \"\" when there is nothing to say.\n", src.Name, ss.forms)
	fmt.Fprintf(&b, "- pronunciations: IPA between slashes; %s.\n", pron)
	fmt.Fprintf(&b, "- senses: the meanings, the commonest first, at most six. pos: the part of speech, abbreviated as a %[1]s learner's dictionary of %[2]s abbreviates it. gloss: the short %[1]s equivalents, separated by %[3]q. definition: one or two sentences in %[1]s saying exactly what the word means in that sense.\n", dst.Name, src.Name, ts.sep)
	fmt.Fprintf(&b, "- examples: exactly two sentences in %[1]s using the word, each with a natural %[2]s translation. The first is simple: short and everyday. The second is complex: a longer sentence with subordinate clauses in a formal or literary register.\n", src.Name, dst.Name)
	fmt.Fprintf(&b, "- etymology: a short paragraph in %s tracing the word to its origin: the forms it came through, written in their own spelling (Greek in Greek letters with a transliteration), the language and period of each, and who coined it and when, if known.\n", dst.Name)
	fmt.Fprintf(&b, "- related: up to eight related %[1]s words — synonyms, antonyms, derivatives, cognates, compounds. relation: a short label in %[2]s (%[3]s…). gloss: its short %[2]s meaning.\n", src.Name, dst.Name, ts.relations)
	fmt.Fprintf(&b, "- literary.note: two or three sentences in %s on how the word lives in literature: its register and period, its connotations, writers known for it.\n", dst.Name)
	fmt.Fprintf(&b, "- literary.quotes: up to two passages from published literature that use the word, quoted word for word in %s, each with a %s translation and its source as \"Author, Work (year)\". Quote only passages you know verbatim; a quotation you are unsure of is worse than none, so give fewer or none rather than invent or paraphrase one.\n", src.Name, dst.Name)
	return b.String()
}

// dictKey is the form a lookup is kept under: trimmed, its runs of space
// made one, lower case — "Serendipity " and "serendipity" are one entry.
func dictKey(q string) string {
	return strings.ToLower(strings.Join(strings.Fields(q), " "))
}

// dictQuery reads and checks the three things a lookup names.
func dictQuery(from, to, q string) (src, dst dictLang, key string, err error) {
	var ok bool
	if src, ok = findDictLang(dictSources, from); !ok {
		return src, dst, "", fmt.Errorf("unknown source language %q", from)
	}
	if dst, ok = findDictLang(dictTargets, to); !ok {
		return src, dst, "", fmt.Errorf("unknown target language %q", to)
	}
	key = dictKey(q)
	if key == "" {
		return src, dst, "", errors.New("nothing to look up")
	}
	if !utf8.ValidString(key) || utf8.RuneCountInString(key) > dictMaxQuery {
		return src, dst, "", fmt.Errorf("a lookup is at most %d characters", dictMaxQuery)
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return src, dst, "", errors.New("a lookup is one line of text")
		}
	}
	return src, dst, key, nil
}

// dictKept is an entry as the dictionary keeps it, and as the API sends it.
type dictKept struct {
	From    string          `json:"from"`
	To      string          `json:"to"`
	Q       string          `json:"q"`
	Entry   json.RawMessage `json:"entry"`
	Model   string          `json:"model"`
	Effort  string          `json:"effort"`
	Created int64           `json:"created"` // unix ms
	// Written is set on the answer of the lookup that wrote it.
	Written bool `json:"written,omitempty"`
}

func (s *Server) dictDatabase() (*sql.DB, error) {
	s.dictMu.Lock()
	defer s.dictMu.Unlock()
	if s.dictDB != nil {
		return s.dictDB, nil
	}
	db, err := sql.Open("sqlite", filepath.Join(s.StateDir, "dict.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS entries (
		src TEXT NOT NULL, dst TEXT NOT NULL, key TEXT NOT NULL,
		headword TEXT NOT NULL, entry TEXT NOT NULL,
		model TEXT NOT NULL, effort TEXT NOT NULL,
		created INTEGER NOT NULL, looked INTEGER NOT NULL, lookups INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (src, dst, key))`); err != nil {
		db.Close()
		return nil, err
	}
	s.dictDB = db
	return db, nil
}

// dictGet reads a kept entry and counts the lookup; nil when there is none.
func (s *Server) dictGet(src, dst, key string) (*dictKept, error) {
	db, err := s.dictDatabase()
	if err != nil {
		return nil, err
	}
	k := &dictKept{From: src, To: dst, Q: key}
	var entry string
	err = db.QueryRow(`SELECT entry, model, effort, created FROM entries WHERE src = ? AND dst = ? AND key = ?`,
		src, dst, key).Scan(&entry, &k.Model, &k.Effort, &k.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	k.Entry = json.RawMessage(entry)
	db.Exec(`UPDATE entries SET looked = ?, lookups = lookups + 1 WHERE src = ? AND dst = ? AND key = ?`,
		time.Now().UnixMilli(), src, dst, key)
	return k, nil
}

func (s *Server) dictPut(k *dictKept, headword string) error {
	db, err := s.dictDatabase()
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT OR REPLACE INTO entries (src, dst, key, headword, entry, model, effort, created, looked, lookups)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		k.From, k.To, k.Q, headword, string(k.Entry), k.Model, k.Effort, k.Created, k.Created)
	return err
}

func (s *Server) handleDictGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	src, dst, key, err := dictQuery(q.Get("from"), q.Get("to"), q.Get("q"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	k, err := s.dictGet(src.Code, dst.Code, key)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if k == nil {
		writeErr(w, http.StatusNotFound, errors.New("not in the dictionary"))
		return
	}
	writeJSON(w, http.StatusOK, k)
}

// A dictFlight is one session writing one entry, for every lookup of it.
type dictFlight struct {
	began    time.Time
	done     chan struct{} // closed when the fields below are set
	kept     *dictKept
	notFound []string // suggestions, when the lookup was no word
	err      error
}

func (s *Server) handleDictLookup(w http.ResponseWriter, r *http.Request) {
	var req struct{ From, To, Q string }
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	src, dst, key, err := dictQuery(req.From, req.To, req.Q)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	emit := func(v any) {
		enc.Encode(v)
		if fl != nil {
			fl.Flush()
		}
	}
	start := func() {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
	}
	k, err := s.dictGet(src.Code, dst.Code, key)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if k != nil {
		start()
		emit(map[string]any{"entry": k})
		return
	}
	f, err := s.dictJoin(src, dst, key)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	start()
	since := func() int { return int(time.Since(f.began).Seconds()) }
	emit(map[string]any{"writing": true, "model": dictModel, "effort": dictEffort, "wait": since()})
	tick := time.NewTicker(dictTick)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return // the session goes on; what it writes is kept
		case <-tick.C:
			emit(map[string]any{"wait": since()})
		case <-f.done:
			switch {
			case f.err != nil:
				emit(map[string]any{"error": f.err.Error()})
			case f.kept == nil:
				sugg := f.notFound
				if sugg == nil {
					sugg = []string{}
				}
				emit(map[string]any{"notfound": true, "suggestions": sugg})
			default:
				emit(map[string]any{"entry": f.kept})
			}
			return
		}
	}
}

// dictJoin is the session writing src→dst key, started now unless one is
// already at it. errNoLLM when there is no Codex CLI to start one with.
func (s *Server) dictJoin(src, dst dictLang, key string) (*dictFlight, error) {
	id := src.Code + "\x00" + dst.Code + "\x00" + key
	s.dictMu.Lock()
	defer s.dictMu.Unlock()
	if f := s.dictFlights[id]; f != nil {
		return f, nil
	}
	bin := dictCodexPath()
	if bin == "" {
		return nil, errNoLLM
	}
	if s.dictFlights == nil {
		s.dictFlights = map[string]*dictFlight{}
	}
	f := &dictFlight{began: time.Now(), done: make(chan struct{})}
	s.dictFlights[id] = f
	go func() {
		defer func() {
			s.dictMu.Lock()
			delete(s.dictFlights, id)
			s.dictMu.Unlock()
			close(f.done)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), dictTimeout)
		defer cancel()
		select {
		case dictSlots <- struct{}{}:
			defer func() { <-dictSlots }()
		case <-ctx.Done():
			f.err = errors.New("the dictionary is busy: try again in a minute")
			return
		}
		// kept meanwhile: by a session that ended between this lookup's
		// miss and its join, or while this one waited for a slot
		if k, err := s.dictGet(src.Code, dst.Code, key); err == nil && k != nil {
			f.kept = k
			return
		}
		e, raw, err := runDictCodex(ctx, bin, dictPrompt(src, dst, key))
		if err != nil {
			f.err = err
			return
		}
		if !e.Found {
			f.notFound = e.Suggestions
			return
		}
		kept := &dictKept{From: src.Code, To: dst.Code, Q: key, Entry: raw,
			Model: dictModel, Effort: dictEffort, Created: time.Now().UnixMilli()}
		if err := s.dictPut(kept, e.Headword); err != nil {
			f.err = err
			return
		}
		written := *kept
		written.Written = true
		f.kept = &written
	}()
	return f, nil
}

// runDictCodex runs one ephemeral Codex session on the prompt and reads its
// last message, the entry, held to dictSchema. The session starts in an
// empty folder of its own, read-only, with its shell tools switched off
// and without the user's config.toml (MCP servers, hooks, plugins) — it is
// a model call, nothing more; the model and effort are said here.
func runDictCodex(ctx context.Context, bin, prompt string) (*dictEntry, json.RawMessage, error) {
	dir, err := os.MkdirTemp("", "exe-dict-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)
	schema, out, work := filepath.Join(dir, "schema.json"), filepath.Join(dir, "entry.json"), filepath.Join(dir, "work")
	if err := os.WriteFile(schema, []byte(dictSchema), 0o600); err != nil {
		return nil, nil, err
	}
	if err := os.Mkdir(work, 0o700); err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(ctx, bin, "exec",
		"--ephemeral", "--ignore-user-config", "--skip-git-repo-check",
		"--sandbox", "read-only", "--disable", "shell_tool", "--disable", "unified_exec",
		"--model", dictModel, "-c", `model_reasoning_effort="`+dictEffort+`"`,
		"--output-schema", schema, "--output-last-message", out,
		"--cd", work, "--color", "never", "-")
	cmd.Dir = work
	cmd.Env = cliEnv(bin)
	cmd.Stdin = strings.NewReader(prompt)
	var stderr tailBuffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, nil, errors.New("Codex took too long to write the entry")
		}
		if why := codexComplaint(stderr.String()); why != "" {
			return nil, nil, fmt.Errorf("Codex: %s", why)
		}
		return nil, nil, fmt.Errorf("Codex: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		return nil, nil, errors.New("Codex finished without an entry")
	}
	var e dictEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return nil, nil, fmt.Errorf("Codex wrote no entry: %v", err)
	}
	if e.Found && (strings.TrimSpace(e.Headword) == "" || len(e.Senses) == 0) {
		return nil, nil, errors.New("Codex wrote an empty entry")
	}
	// kept in the schema's own shape, whatever spacing the model chose
	norm, err := json.Marshal(e)
	if err != nil {
		return nil, nil, err
	}
	return &e, norm, nil
}

// codexComplaint picks what a failed session said: its last line naming
// an error, else its last line.
func codexComplaint(log string) string {
	lines := strings.Split(strings.TrimSpace(log), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.Contains(strings.ToLower(l), "error") {
			return clipText(l, 300)
		}
	}
	if l := strings.TrimSpace(lines[len(lines)-1]); l != "" {
		return clipText(l, 300)
	}
	return ""
}

// tailBuffer keeps the last 8 KB written to it.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if over := len(t.b) - 8<<10; over > 0 {
		t.b = bytes.Clone(t.b[over:])
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }
