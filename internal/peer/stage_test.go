package peer

import (
	"bytes"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// peerServer answers e's file pushes and fetches as the daemon's handlers
// do (internal/server/peers.go).
func peerServer(t *testing.T, e *Engine) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/v1/peer/file/")
		app, rel, _ := strings.Cut(key, "/")
		switch r.Method {
		case "PUT":
			in, code, err := e.ReceiveFile(r)
			if err != nil {
				http.Error(w, err.Error(), code)
				return
			}
			defer in.Discard()
			rv := ParseVersion(r.Header.Get(hdrVer), r.Header.Get(hdrDeleted) == "1")
			out, err := e.ApplyRemote(key, in, rv)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.Write([]byte(out))
		case "GET":
			if _, err := e.VerifyPeer(r, nil); err != nil {
				http.Error(w, err.Error(), 401)
				return
			}
			f, size, v, err := e.FileForPeer(app, rel, Takes(r))
			if err != nil {
				http.Error(w, "not found", 404)
				return
			}
			defer f.Close()
			w.Header().Set(hdrVer, EncodeVector(v))
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			io.CopyN(w, f, size)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// pair enrolls a and b with each other, b answering at addr, and has a
// take b's files up to the cap, as b's manifest would say.
func pair(t *testing.T, a, b *Engine, addr string) *Peer {
	t.Helper()
	pb := &Peer{ID: b.cfg.Self.ID, Name: "b", PubKey: b.cfg.Self.PubKey(), Addr: addr}
	if err := a.peers.Upsert(pb); err != nil {
		t.Fatal(err)
	}
	if err := b.peers.Upsert(&Peer{ID: a.cfg.Self.ID, Name: "a", PubKey: a.cfg.Self.PubKey(), Addr: "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	a.statusMu.Lock()
	a.status[pb.ID] = &Status{takes: maxSyncFile}
	a.statusMu.Unlock()
	return pb
}

// writeNoise puts n bytes that are not all one value at the workspace path
// rel, a megabyte at a time.
func writeNoise(t *testing.T, dir, rel string, n int64) (key string, size, mtime int64) {
	t.Helper()
	p := filepath.Join(dir, "workspace", filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	block := make([]byte, 1<<20)
	rand.Read(block)
	for left := n; left > 0; left -= int64(len(block)) {
		block[0]++ // no two megabytes alike
		f.Write(block[:min(left, int64(len(block)))])
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(p)
	return WorkspaceNS + "/" + rel, fi.Size(), fi.ModTime().UnixNano()
}

// allocated is how many bytes the whole process allocated while fn ran:
// both ends of a transfer, as they share it here.
func allocated(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func stagingEmpty(t *testing.T, e *Engine) {
	t.Helper()
	ents, _ := os.ReadDir(e.stagingDir())
	if len(ents) != 0 {
		t.Fatalf("staging folder keeps %d files", len(ents))
	}
}

func TestBigFileCrossesWithoutBeingHeldInMemory(t *testing.T) {
	const size = 48 << 20 // over legacySyncFile: the big-push path
	a, dirA := newWorkspaceEngine(t)
	b, dirB := newWorkspaceEngine(t)
	pb := pair(t, a, b, strings.TrimPrefix(peerServer(t, b).URL, "http://"))
	pa := pair(t, b, a, strings.TrimPrefix(peerServer(t, a).URL, "http://"))

	key, n, mtime := writeNoise(t, dirA, "Disk Images/big.img", size)
	a.man.Bump(key, a.cfg.Self.ID, false, n, mtime)
	want := fileSum(a.filePath(key))

	// a push, as LocalWrite starts one
	got := allocated(func() { a.pushTo(pb, key) })
	t.Logf("pushing %d MB allocated %d KB", size>>20, got>>10)
	if got > 8<<20 {
		t.Errorf("pushing %d MB allocated %d MB", size>>20, got>>20)
	}
	if got := fileSum(filepath.Join(dirB, "workspace", "Disk Images", "big.img")); got != want {
		t.Fatal("the pushed file arrived with other content")
	}
	if v, ok := b.man.Get(key); !ok || !v.SameVersion(Version{Vec: map[string]int64{a.cfg.Self.ID: 1}}) {
		t.Fatalf("b takes a's version: %+v %v", v, ok)
	}
	stagingEmpty(t, b)

	// a fetch, as reconcile starts one: a's next version, asked for by b
	writeNoise(t, dirA, "Disk Images/big.img", size)
	fi, _ := os.Stat(a.filePath(key))
	rv := a.man.Bump(key, a.cfg.Self.ID, false, fi.Size(), fi.ModTime().UnixNano())
	want = fileSum(a.filePath(key))
	got = allocated(func() { b.fetchFrom(pa, key, rv) })
	t.Logf("fetching %d MB allocated %d KB", size>>20, got>>10)
	if got > 8<<20 {
		t.Errorf("fetching %d MB allocated %d MB", size>>20, got>>20)
	}
	if got := fileSum(filepath.Join(dirB, "workspace", "Disk Images", "big.img")); got != want {
		t.Fatal("the fetched file arrived with other content")
	}
	stagingEmpty(t, b)
}

// tripwire is a body that says whether anything read it.
type tripwire struct {
	r    io.Reader
	read bool
}

func (t *tripwire) Read(p []byte) (int, error) { t.read = true; return t.r.Read(p) }

func TestReceiveFileChecksTheSenderFirst(t *testing.T) {
	a, _ := newWorkspaceEngine(t)
	b, _ := newWorkspaceEngine(t)
	pair(t, a, b, "127.0.0.1:1")
	stranger, _ := LoadIdentity(t.TempDir())
	body := []byte("hello")

	// push is a PUT of body as from signs it; edit changes it after signing.
	push := func(from *Identity, sum bool, edit func(r *http.Request)) (*tripwire, *Staged, int) {
		trip := &tripwire{r: bytes.NewReader(body)}
		r := httptest.NewRequest("PUT", "/v1/peer/file/@workspace/x.txt", trip)
		r.Header.Set(hdrVer, EncodeVector(Version{Vec: map[string]int64{from.ID: 1}}))
		if sum {
			r.Header.Set(hdrSum, bodySum(body))
		}
		SignRequest(from, r, body, b.cfg.Self.ID)
		if edit != nil {
			edit(r)
		}
		in, code, _ := b.ReceiveFile(r)
		return trip, in, code
	}

	for _, sum := range []bool{true, false} {
		_, in, code := push(a.cfg.Self, sum, nil)
		if code != 200 || in == nil || in.sum != bodySum(body) || in.size != int64(len(body)) {
			t.Fatalf("a signed push (sum said: %v) is taken: %d %+v", sum, code, in)
		}
		in.Discard()
	}
	stagingEmpty(t, b)

	for name, tc := range map[string]struct {
		from   *Identity
		sum    bool
		edit   func(r *http.Request)
		code   int
		unread bool // turned down before the body is read
	}{
		"unknown sender": {stranger, true, nil, 401, true},
		"stale": {a.cfg.Self, true, func(r *http.Request) {
			r.Header.Set(hdrTS, strconv.FormatInt(time.Now().Add(-maxSkew-time.Minute).UnixMilli(), 10))
		}, 401, true},
		"other hash said": {a.cfg.Self, true, func(r *http.Request) { r.Header.Set(hdrSum, bodySum([]byte("x"))) }, 401, true},
		"body changed, hash said": {a.cfg.Self, true, func(r *http.Request) {
			r.Body = io.NopCloser(&tripwire{r: strings.NewReader("HELLO")})
		}, 400, false},
		"body changed, older release": {a.cfg.Self, false, func(r *http.Request) {
			r.Body = io.NopCloser(&tripwire{r: strings.NewReader("HELLO")})
		}, 401, false},
		"over the cap": {a.cfg.Self, true, func(r *http.Request) { r.ContentLength = maxSyncFile + 1 }, 413, true},
	} {
		trip, in, code := push(tc.from, tc.sum, tc.edit)
		if code != tc.code || in != nil {
			t.Errorf("%s: want %d, got %d %+v", name, tc.code, code, in)
		}
		if tc.unread && trip.read {
			t.Errorf("%s: the body was read", name)
		}
	}
	stagingEmpty(t, b)
}

func TestNewEngineClearsWhatWasStaged(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "sync-incoming", "in-12345")
	os.MkdirAll(filepath.Dir(stale), 0o755)
	os.WriteFile(stale, []byte("half a file"), 0o600)
	ident, _ := LoadIdentity(dir)
	if _, err := NewEngine(EngineConfig{StateDir: dir, DataDir: filepath.Join(dir, "appdata"), Self: ident}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("a transfer the last daemon left staged is removed: %v", err)
	}
}

func TestOlderReleaseTakesThisReleasesPush(t *testing.T) {
	// An older release reads the whole body and checks the signature over
	// it; the hash this release says up front is one it never looks at.
	a, dirA := newWorkspaceEngine(t)
	b, _ := newWorkspaceEngine(t)
	var got []int
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		code := 200
		if _, err := VerifyRequest(b.peers, r, body, b.cfg.Self.ID); err != nil || string(body) != "hello" && len(body) != 0 {
			code = 401
		}
		got = append(got, code)
		w.WriteHeader(code)
	}))
	defer old.Close()
	pb := pair(t, a, b, strings.TrimPrefix(old.URL, "http://"))

	key := "@workspace/small.txt"
	os.MkdirAll(filepath.Join(dirA, "workspace"), 0o755)
	if err := os.WriteFile(filepath.Join(dirA, "workspace", "small.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	a.man.Bump(key, a.cfg.Self.ID, false, 5, 1)
	a.pushTo(pb, key) // a write
	a.man.Bump(key, a.cfg.Self.ID, true, 0, 0)
	a.pushTo(pb, key) // a delete
	if !slices.Equal(got, []int{200, 200}) {
		t.Fatalf("the write and the delete are taken, got %v", got)
	}
}
