package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDeclarativePushPayload(t *testing.T) {
	for _, msg := range []pushMessage{
		agentTurnEndMessage(hostAgents["claude"], "exe-claude-2", nil),
		agentTurnEndMessage(hostAgents["codex"], "exe-codex-3", nil),
		chatEndMessage("abc123", "Review the patch", "", ""),
		{Title: "Rain soon", Body: "Rain in twenty minutes", Tag: "rain-example", URL: "/"},
		{Title: "SOL moved", Body: "A rare move", Tag: "px-SOL"},
		{},
	} {
		t.Run(msg.Title, func(t *testing.T) {
			b, err := pushPayload(msg, "https://desk.example.com")
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			n := got["notification"].(map[string]any)
			if got["web_push"] != float64(8030) || got["mutable"] != true || n["mutable"] != true {
				t.Fatalf("missing declarative marker or mutable compatibility: %s", b)
			}
			for _, field := range []string{"title", "body", "tag"} {
				if got[field] != n[field] {
					t.Fatalf("old and new workers disagree on %s: %s", field, b)
				}
			}
			path := msg.URL
			if path == "" {
				path = "/"
			}
			if n["title"] == "" || n["navigate"] != "https://desk.example.com"+path || n["icon"] != "https://desk.example.com/ui/icon-192.png" {
				t.Fatalf("invalid fallback notification: %s", b)
			}
			if n["data"].(map[string]any)["show"] != msg.Show {
				t.Fatalf("session lost: %s", b)
			}
		})
	}
	msg := pushMessage{Title: "Codex finished", Body: "Review", URL: "/#show=codex:exe-codex", Show: "codex:exe-codex"}
	for _, origin := range []string{"", "not an origin", "https://desk.example.com/path", "http://remote.example.com"} {
		b, err := pushPayload(msg, origin)
		if err != nil {
			t.Fatal(err)
		}
		want, _ := json.Marshal(msg)
		if string(b) != string(want) {
			t.Fatalf("unknown origin must keep the old payload: %s", b)
		}
	}
}

func TestDeclarativePushSize(t *testing.T) {
	for _, content := range []string{strings.Repeat("界🌍", 1500), strings.Repeat("<>&\n", 1500)} {
		msg := pushMessage{Title: strings.Repeat("Long title ", 200), Body: content, Tag: "agent-exe-codex", URL: "/#show=codex:exe-codex", Show: "codex:exe-codex"}
		b, err := pushPayload(msg, "https://desk.example.com")
		if err != nil {
			t.Fatal(err)
		}
		if len(b)+86+17 > 4096 || !utf8.Valid(b) {
			t.Fatalf("invalid wire size or UTF-8: %d", len(b))
		}
		var got map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		n := got["notification"].(map[string]any)
		if got["body"] != n["body"] || got["title"] != n["title"] || strings.Contains(got["body"].(string), "�") {
			t.Fatalf("bad shortening: %s", b)
		}
		if got["show"] != msg.Show || got["url"] != msg.URL || n["navigate"] != "https://desk.example.com"+msg.URL {
			t.Fatal("shortened routing data")
		}
	}
	if _, err := pushPayload(pushMessage{Tag: strings.Repeat("t", 5000)}, "https://desk.example.com"); err == nil {
		t.Fatal("oversized routing data accepted")
	}
	for _, path := range []string{"//other.example/", "/\\other.example/", "https://other.example/", "/\n"} {
		if _, err := pushPayload(pushMessage{URL: path}, "https://desk.example.com"); err == nil {
			t.Fatalf("unsafe path accepted: %q", path)
		}
	}
}

func TestPushSubscriptionOrigin(t *testing.T) {
	s := &Server{StateDir: t.TempDir()}
	call := func(origin, host string) int {
		r := httptest.NewRequest("POST", "/v1/push/subscribe", strings.NewReader(`{"endpoint":"https://push.example.com/one","keys":{"p256dh":"AA","auth":"BB"},"origin":"https://forged.example.com"}`))
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		s.handlePushSubscribe(w, r)
		return w.Code
	}
	if call("", "desk.example.com") != 200 || s.loadPushSubs()[0].Origin != "" {
		t.Fatal("accepted JSON origin")
	}
	if call("https://desk.example.com", "desk.example.com") != 200 || s.loadPushSubs()[0].Origin != "https://desk.example.com" {
		t.Fatal("browser refresh did not migrate the subscription")
	}
	if call("", "desk.example.com") != 200 || s.loadPushSubs()[0].Origin != "https://desk.example.com" {
		t.Fatal("older client erased known origin")
	}
	for _, origin := range []string{"https://other.example.com", "https://desk.example.com/path", "https://someone@example.com", "http://desk.example.com", "null"} {
		if call(origin, "desk.example.com") != 400 {
			t.Fatalf("accepted origin %q", origin)
		}
	}
	if call("http://127.0.0.1:7797", "127.0.0.1:7797") != 200 {
		t.Fatal("localhost secure context refused")
	}
}

// Decrypt what actually leaves pushAll. Each device must receive a link to
// its own desktop origin, and a pre-migration subscription must stay usable.
func TestPushAllDeclarativeOrigins(t *testing.T) {
	key, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	type delivery struct {
		path    string
		payload map[string]any
	}
	deliveries := make(chan delivery, 3)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ := io.ReadAll(r.Body)
		if len(wire) < 103 {
			t.Error("short encrypted push")
			w.WriteHeader(400)
			return
		}
		sender, err := ecdh.P256().NewPublicKey(wire[21:86])
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		shared, _ := key.ECDH(sender)
		prk, _ := hkdf.Extract(sha256.New, shared, auth)
		ikm, _ := hkdf.Expand(sha256.New, prk, "WebPush: info\x00"+string(key.PublicKey().Bytes())+string(sender.Bytes()), 32)
		prk, _ = hkdf.Extract(sha256.New, ikm, wire[:16])
		cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
		nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
		block, _ := aes.NewCipher(cek)
		gcm, _ := cipher.NewGCM(block)
		plain, err := gcm.Open(nil, nonce, wire[86:], nil)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(plain[:len(plain)-1], &payload); err != nil {
			t.Error(err)
		}
		deliveries <- delivery{r.URL.Path, payload}
		w.WriteHeader(http.StatusCreated)
	}))
	defer ts.Close()
	old := pushClient
	pushClient = ts.Client()
	defer func() { pushClient = old }()
	s := &Server{StateDir: t.TempDir()}
	var subs []pushSub
	for path, origin := range map[string]string{"/one": "https://one.example.com", "/two": "https://two.example.com", "/old": ""} {
		sub := pushSub{Endpoint: ts.URL + path, Origin: origin}
		sub.Keys.P256dh, sub.Keys.Auth = b64.EncodeToString(key.PublicKey().Bytes()), b64.EncodeToString(auth)
		subs = append(subs, sub)
	}
	if err := s.savePushSubs(subs); err != nil {
		t.Fatal(err)
	}
	msg := agentTurnEndMessage(hostAgents["codex"], "exe-codex-3", nil)
	if sent, errs := s.pushAll(context.Background(), msg); sent != 3 || len(errs) != 0 {
		t.Fatalf("sent %d: %v", sent, errs)
	}
	received := map[string]map[string]any{}
	for range 3 {
		d := <-deliveries
		received[d.path] = d.payload
	}
	for path, origin := range map[string]string{"/one": "https://one.example.com", "/two": "https://two.example.com"} {
		n := received[path]["notification"].(map[string]any)
		if n["navigate"] != origin+msg.URL {
			t.Fatalf("wrong origin: %v", n)
		}
	}
	if received["/old"]["web_push"] != nil || received["/old"]["show"] != msg.Show {
		t.Fatalf("old subscription broken: %v", received["/old"])
	}
}
