// One-shot model completions for desktop apps.
//
// POST /v1/chat/complete asks a model one question and streams the answer
// back: no session, no tools, no history. The app holds the prompt; the
// daemon holds the endpoint and the key, so a page in the browser never
// sees either. Blue Pencil's grammar pass runs on it, and any app can. The
// call runs on the Ollama endpoint — the one the user pointed exe at for
// local work — unless the body says "provider": "openai", which runs it on
// the ChatGPT subscription signed in under Configuration → OpenAI, the way
// the Chat window does.
//
// A body with "share": true joins an identical call already running
// instead of starting its own — see completeFlight.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"exe/internal/agent"
	"exe/internal/codex"
)

const (
	completeMaxPrompt = 64 << 10
	completeMaxSystem = 8 << 10
	// a shared answer stays on hand this long after it finished, and at
	// most this many of them
	completeKeep    = 10 * time.Minute
	completeKeepMax = 128
)

// completeCall runs one model turn on a backend, fragment by fragment.
type completeCall func(ctx context.Context, msgs []agent.Message, onDelta func(string)) error

// handleChatComplete streams newline-delimited JSON: {"delta":"…"} per
// content fragment, then {"done":true,"model":"…","provider":"…"}. The
// body carries the prompts plus optional provider, model, effort and
// Ollama options overrides. A failure before the first fragment is a plain
// JSON error (400/502/503); one mid-stream lands as a final {"error":"…"}
// line. Closing the request cancels the model call — a shared one once its
// last asker has closed. An asker that joined a call already running, or
// got an answer kept from one, reads "shared":true on its done line.
func (s *Server) handleChatComplete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		// Provider is "ollama" (the default) or "openai".
		Provider string `json:"provider"`
		System   string `json:"system"`
		Prompt   string `json:"prompt"`
		Model    string `json:"model"`
		Effort   string `json:"effort"`
		// Options go to Ollama as-is: temperature, seed, num_predict…
		Options map[string]any `json:"options"`
		// Share lets this call be one with every identical call.
		Share bool `json:"share"`
	}
	body := io.LimitReader(r.Body, completeMaxPrompt+completeMaxSystem+4096)
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("prompt is empty"))
		return
	}
	if len(req.Prompt) > completeMaxPrompt || len(req.System) > completeMaxSystem {
		writeErr(w, http.StatusRequestEntityTooLarge,
			fmt.Errorf("prompt over %d bytes", completeMaxPrompt))
		return
	}
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if provider == "" {
		provider = "ollama"
	}
	model := strings.TrimSpace(req.Model)
	effort := strings.ToLower(strings.TrimSpace(req.Effort))
	cfg := s.Config()

	// call runs the one model turn on the chosen backend; it is built before
	// any byte is written so a backend not ready to run is a plain error.
	var call completeCall
	endpoint := "" // what else tells two backends apart, for the share key
	switch provider {
	case "ollama":
		if cfg.Ollama.BaseURL == "" {
			writeErr(w, http.StatusServiceUnavailable, errors.New("ollama.base_url is not configured"))
			return
		}
		acfg := agent.Config{BaseURL: cfg.Ollama.BaseURL, APIKey: cfg.Ollama.APIKey,
			Model: cfg.Ollama.Model, Effort: cfg.Ollama.Effort, Options: req.Options}
		if model != "" {
			acfg.Model = model
		}
		if effort != "" {
			acfg.Effort = effort
		}
		if acfg.Model == "" {
			writeErr(w, http.StatusServiceUnavailable, errors.New("ollama.model is not configured"))
			return
		}
		model, effort, endpoint = acfg.Model, acfg.Effort, acfg.BaseURL
		call = func(ctx context.Context, msgs []agent.Message, onDelta func(string)) error {
			_, err := agent.ChatStream(ctx, acfg, msgs, nil, onDelta)
			return err
		}
	case "openai":
		if s.codexCreds() == nil {
			writeErr(w, http.StatusServiceUnavailable, errors.New("not signed in to ChatGPT (Configuration → OpenAI)"))
			return
		}
		if model == "" {
			model = cfg.OpenAI.Model
		}
		if effort == "" {
			effort = cfg.OpenAI.Effort
		}
		if model == "" {
			writeErr(w, http.StatusServiceUnavailable, errors.New("openai.model is not configured"))
			return
		}
		// The ChatGPT path resolves (and auto-refreshes) the token per call
		// and retries once after a 401 in case the token was revoked out
		// from under us — a 401 comes before any fragment, so nothing has
		// streamed yet. The cache key names the system prompt: every call
		// an app makes with the same instructions lands on the same cache.
		ccfg := codex.ClientConfig{Model: model, Effort: effort, SessionKey: completeCacheKey(req.System)}
		call = func(ctx context.Context, msgs []agent.Message, onDelta func(string)) error {
			creds, err := s.codexToken(ctx, false)
			if err != nil {
				return err
			}
			ccfg.AccessToken, ccfg.AccountID = creds.AccessToken, creds.AccountID
			_, err = codex.ChatStream(ctx, ccfg, msgs, nil, onDelta)
			if errors.Is(err, codex.ErrUnauthorized) {
				if creds, err = s.codexToken(ctx, true); err != nil {
					return err
				}
				ccfg.AccessToken, ccfg.AccountID = creds.AccessToken, creds.AccountID
				_, err = codex.ChatStream(ctx, ccfg, msgs, nil, onDelta)
			}
			return err
		}
	default:
		writeErr(w, http.StatusBadRequest, fmt.Errorf("unknown provider %q", req.Provider))
		return
	}
	var msgs []agent.Message
	if req.System != "" {
		msgs = append(msgs, agent.Message{Role: "system", Content: req.System})
	}
	msgs = append(msgs, agent.Message{Role: "user", Content: req.Prompt})

	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	started := false
	emit := func(v any) {
		if !started {
			started = true
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
		}
		enc.Encode(v)
		if fl != nil {
			fl.Flush()
		}
	}
	onDelta := func(delta string) { emit(map[string]string{"delta": delta}) }
	var err error
	shared := false
	if req.Share {
		key := completeShareKey(provider, endpoint, model, effort, req.System, req.Prompt, req.Options)
		var f *completeFlight
		f, shared = s.joinComplete(key, call, msgs)
		err = f.follow(r.Context(), onDelta)
		s.leaveComplete(key, f)
	} else {
		err = call(r.Context(), msgs, onDelta)
	}
	if err != nil {
		if !started {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		emit(map[string]string{"error": err.Error()})
		return
	}
	done := map[string]any{"done": true, "model": model, "provider": provider}
	if shared {
		done["shared"] = true
	}
	emit(done)
}

// ---- shared calls ----
//
// The same backend, model, effort, prompts and options is the same
// question, so every asker of it reads one answer: the fragments so far at
// once, the rest as they stream. Blue Pencil asks this way — each desk
// showing a draft checks the same paragraphs, and a second window must not
// cost a second model call. The call belongs to the flight, not to the
// request that started it: it outlives any one asker and is cancelled when
// the last has gone. A finished answer is kept for completeKeep, so an
// asker a moment too late to join still gets it without a call; a failed
// one is dropped at once, so the next ask tries again.
type completeFlight struct {
	mu      sync.Mutex
	deltas  []string
	done    bool
	err     error
	at      time.Time     // when it finished
	changed chan struct{} // closed, and replaced, whenever the fields above move
	askers  int
	cancel  context.CancelFunc
}

// completeShareKey names a question: a digest of everything the answer
// depends on, after the configured defaults are filled in.
func completeShareKey(provider, endpoint, model, effort, system, prompt string, options map[string]any) string {
	b, _ := json.Marshal([]any{provider, endpoint, model, effort, system, prompt, options}) // map keys marshal sorted
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// joinComplete returns the flight answering key, starting the call if
// nobody has asked yet; joined reports that somebody had. Pair it with
// leaveComplete.
func (s *Server) joinComplete(key string, call completeCall, msgs []agent.Message) (f *completeFlight, joined bool) {
	s.completeMu.Lock()
	defer s.completeMu.Unlock()
	if f = s.completeFlights[key]; f != nil {
		f.mu.Lock()
		stale := f.done && time.Since(f.at) > completeKeep
		if !stale {
			f.askers++
		}
		f.mu.Unlock()
		if !stale {
			return f, true
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	f = &completeFlight{changed: make(chan struct{}), askers: 1, cancel: cancel}
	if s.completeFlights == nil {
		s.completeFlights = map[string]*completeFlight{}
	}
	s.completeFlights[key] = f
	go func() {
		err := call(ctx, msgs, func(delta string) {
			f.mu.Lock()
			f.deltas = append(f.deltas, delta)
			f.wake()
			f.mu.Unlock()
		})
		cancel()
		f.mu.Lock()
		f.done, f.err, f.at = true, err, time.Now()
		f.wake()
		f.mu.Unlock()
		s.completeMu.Lock()
		if err != nil && s.completeFlights[key] == f {
			delete(s.completeFlights, key)
		}
		s.pruneComplete()
		s.completeMu.Unlock()
	}()
	return f, false
}

// wake tells every follower the flight moved; the caller holds f.mu.
func (f *completeFlight) wake() {
	close(f.changed)
	f.changed = make(chan struct{})
}

// follow feeds onDelta the answer from its start, and returns once it is
// whole (or failed), or ctx — the asker — is gone.
func (f *completeFlight) follow(ctx context.Context, onDelta func(string)) error {
	for i := 0; ; {
		f.mu.Lock()
		batch := f.deltas[i:]
		i = len(f.deltas)
		done, err, changed := f.done, f.err, f.changed
		f.mu.Unlock()
		for _, d := range batch {
			onDelta(d)
		}
		if done {
			return err
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// leaveComplete is an asker going; the last one to leave a call still
// running takes the call with it.
func (s *Server) leaveComplete(key string, f *completeFlight) {
	s.completeMu.Lock()
	defer s.completeMu.Unlock()
	f.mu.Lock()
	f.askers--
	orphan := f.askers == 0 && !f.done
	f.mu.Unlock()
	if orphan {
		f.cancel()
		if s.completeFlights[key] == f {
			delete(s.completeFlights, key)
		}
	}
}

// pruneComplete drops the kept answers past completeKeep, then the oldest
// over completeKeepMax; the caller holds completeMu.
func (s *Server) pruneComplete() {
	kept := 0
	for k, f := range s.completeFlights {
		f.mu.Lock()
		old := f.done && time.Since(f.at) > completeKeep
		if f.done && !old {
			kept++
		}
		f.mu.Unlock()
		if old {
			delete(s.completeFlights, k)
		}
	}
	for ; kept > completeKeepMax; kept-- {
		oldest, at := "", time.Time{}
		for k, f := range s.completeFlights {
			f.mu.Lock()
			if f.done && (oldest == "" || f.at.Before(at)) {
				oldest, at = k, f.at
			}
			f.mu.Unlock()
		}
		delete(s.completeFlights, oldest)
	}
}

// completeCacheKey is the ChatGPT prompt cache key for one-shot calls: a
// digest of the system prompt, so an app's calls share a cache without
// the app naming one.
func completeCacheKey(system string) string {
	sum := sha256.Sum256([]byte(system))
	return "complete-" + hex.EncodeToString(sum[:8])
}
