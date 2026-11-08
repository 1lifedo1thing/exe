package peer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// EncodeVector renders a version's vector for the X-Exe-Ver wire header
// (base64 of its JSON), and ParseVersion reconstructs it on the other side.
func EncodeVector(v Version) string {
	b, _ := json.Marshal(v.Vec)
	return base64.StdEncoding.EncodeToString(b)
}

func ParseVersion(verB64 string, deleted bool) Version {
	vec := map[string]int64{}
	if b, err := base64.StdEncoding.DecodeString(verB64); err == nil {
		json.Unmarshal(b, &vec)
	}
	return Version{Vec: vec, Deleted: deleted}
}

// WorkspaceNS is the manifest namespace for the shared workspace tree.
// App names can't start with "@" (validAppName), so workspace keys
// ("@workspace/rel") can never collide with app-data keys ("App/rel").
const WorkspaceNS = "@workspace"

// maxSyncFile caps what sync will carry per file: 1 GB, what the Workspace
// takes, so nothing is versioned, pushed or served that the receiving side
// rejects. A file goes disk to disk (stage.go), so the cap is no memory's.
// A peer says its own cap (manifest max_file, the X-Exe-Max-File of a
// fetch): 100 MB for the releases before this one. One that says none is
// older still and takes legacySyncFile, the 10 MB every release held files
// to before that.
const (
	maxSyncFile    = 1 << 30
	legacySyncFile = 10 << 20
)

// stallTimeout gives up a file transfer that has moved no bytes for this
// long — connecting, sending or receiving — in place of the client's flat
// 15 s, which would fail a big file on a slow link every time; the next
// reconcile tries again. Once a push has handed over its last byte, the
// answer gets answerTimeout: the last MB still drain from the socket
// buffers, and then the peer writes the file. Vars so a test can shorten
// them.
var (
	stallTimeout  = 15 * time.Second
	answerTimeout = time.Minute
)

// bigPushes is how many pushes of a file over legacySyncFile run at once: a
// scan that finds many starts a push for each, and they would split the
// link between them. Smaller files never wait for them.
const bigPushes = 2

// errTooLarge is the refusal of a file over the sync cap, by openSyncFile or
// stage.
var errTooLarge = errors.New("file exceeds the sync cap")

// errBusy turns down a push of a file that is on its way in already.
var errBusy = errors.New("this file is on its way in from another node")

// claim marks key's content as on its way in, by a push or a fetch, and
// says whether nothing else was bringing it; release ends the claim. A file
// that several peers hold is taken once: when a laptop wakes, each of them
// pushes it on its own reconcile, and over a slow link a 1 GB file would
// cross it once a peer.
func (e *Engine) claim(key string) (release func(), ok bool) {
	e.inMu.Lock()
	defer e.inMu.Unlock()
	if e.incoming[key] {
		return nil, false
	}
	e.incoming[key] = true
	var once sync.Once
	return func() {
		once.Do(func() {
			e.inMu.Lock()
			delete(e.incoming, key)
			e.inMu.Unlock()
		})
	}, true
}

// EngineConfig wires the engine into the daemon.
type EngineConfig struct {
	StateDir     string
	DataDir      string // ~/.exe/appdata — the app-data tree being synced
	WorkspaceDir string // ~/.exe/workspace — synced under WorkspaceNS; empty disables
	Self         *Identity
	PortFn       func() string // our API port; peers dial <tailscale-ip>:<port>
	// OnApply fires after a remote change lands on disk so the server can
	// tell open app windows to reload (SSE -> desktop -> iframe).
	OnApply func(app, rel string, deleted bool)
	// OnConflict fires on its own goroutine after a concurrent-edit conflict
	// was resolved and the losing copy preserved under sync-conflicts.
	OnConflict func(app, rel string)
	Logf       func(format string, args ...any)
}

// Engine syncs ~/.exe/appdata and ~/.exe/workspace with every enrolled peer:
// local writes push immediately, a reconcile loop diffs manifests to catch
// anything missed, and mergeable app documents (todos.json, notes.json)
// union item-by-item instead of last-writer-wins.
type Engine struct {
	cfg   EngineConfig
	peers *Store
	man   *Manifest

	client *http.Client
	files  *http.Client  // file transfers, under the stall timer (doFile)
	big    chan struct{} // slots for pushes of a big file (bigPushes)

	fileMu sync.Mutex // serializes apply/scan against each other

	inMu     sync.Mutex
	incoming map[string]bool // keys whose content is on its way in (claim)

	codeMu sync.Mutex
	codes  map[string]time.Time // active join codes -> expiry

	statusMu sync.Mutex
	status   map[string]*Status
	statusAt time.Time

	kick chan struct{}
	stop chan struct{}
}

// validKey guards every file path sync touches: a key must be "App/relpath"
// (or "@workspace/relpath") with a filesystem-local relative portion. Peer
// manifests are untrusted input, so a "../config.json" key that would escape
// DataDir is rejected before it ever reaches filePath/os.Remove/
// writeFileAtomic. Hidden segments are rejected too — dot paths (.Trash,
// .put-* temp files) are node-local by the same rule the scanner uses, so a
// peer can neither plant them nor tombstone them.
func (e *Engine) validKey(key string) bool {
	if !strings.Contains(key, "/") || !filepath.IsLocal(filepath.FromSlash(key)) {
		return false
	}
	for _, seg := range strings.Split(key, "/") {
		if strings.HasPrefix(seg, ".") {
			return false
		}
	}
	// "@" first segments are reserved for non-appdata namespaces; only the
	// workspace one exists, and only while a workspace dir is configured.
	if ns, _, _ := strings.Cut(key, "/"); strings.HasPrefix(ns, "@") {
		return ns == WorkspaceNS && e.cfg.WorkspaceDir != ""
	}
	return true
}

// Status is one row of the Node Sync dialog's peer table.
type Status struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Icon      string `json:"icon,omitempty"` // its node icon, as it last said
	Addr      string `json:"addr"`
	Online    bool   `json:"online"`
	LatencyMS int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
	LastSeen  int64  `json:"last_seen,omitempty"` // unix ms

	takes int64           // the max_file its last manifest named; 0 for none
	held  map[string]bool // the files its last manifest listed; nil before one
}

// PeerInfo is the wire form of a peer entry (pairing + mesh gossip).
type PeerInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	PubKey string `json:"pubkey"`
	Addr   string `json:"addr"`
}

type PairRequest struct {
	Code   string `json:"code"`
	ID     string `json:"id"`
	Name   string `json:"name"`
	PubKey string `json:"pubkey"`
	Port   string `json:"port"`
}

type PairResponse struct {
	ID     string     `json:"id"`
	Name   string     `json:"name"`
	PubKey string     `json:"pubkey"`
	Port   string     `json:"port"`
	Peers  []PeerInfo `json:"peers"`
}

type manifestResponse struct {
	ID    string             `json:"id"`
	Name  string             `json:"name"`
	Icon  string             `json:"icon"` // its node icon; "" from an older release, exe's own
	Port  string             `json:"port"`
	Files map[string]Version `json:"files"`
	Peers []PeerInfo         `json:"peers"`
	// MaxFile is the biggest file the peer takes; an older release omits it
	MaxFile int64 `json:"max_file"`
}

const (
	codeTTL       = 10 * time.Minute
	reconcileTick = 30 * time.Second
	statusTTL     = 5 * time.Second
)

func NewEngine(cfg EngineConfig) (*Engine, error) {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	store, err := LoadStore(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("peers.json: %w", err)
	}
	man, err := LoadManifest(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("sync-manifest.json: %w", err)
	}
	e := &Engine{
		cfg:      cfg,
		peers:    store,
		man:      man,
		client:   &http.Client{Timeout: 15 * time.Second},
		files:    &http.Client{},
		big:      make(chan struct{}, bigPushes),
		codes:    map[string]time.Time{},
		status:   map[string]*Status{},
		incoming: map[string]bool{},
		kick:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}
	e.clearStaging()
	return e, nil
}

// Start runs the reconcile loop until Stop.
func (e *Engine) Start() {
	go func() {
		t := time.NewTicker(reconcileTick)
		defer t.Stop()
		// first pass shortly after boot, once listeners are up
		first := time.NewTimer(2 * time.Second)
		defer first.Stop()
		for {
			select {
			case <-e.stop:
				return
			case <-first.C:
			case <-t.C:
			case <-e.kick:
			}
			e.ScanLocal()
			for _, p := range e.peers.List() {
				e.reconcile(p)
			}
		}
	}()
}

func (e *Engine) Stop() { close(e.stop) }

// ReconcileNow schedules an immediate pass (after pairing).
func (e *Engine) ReconcileNow() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

func (e *Engine) SelfID() string     { return e.cfg.Self.ID }
func (e *Engine) SelfName() string   { return e.cfg.Self.Name }
func (e *Engine) SelfPubKey() string { return e.cfg.Self.PubKey() }
func (e *Engine) Port() string       { return e.cfg.PortFn() }

func (e *Engine) ListPeers() []*Peer { return e.peers.List() }

// VerifyPeer authenticates an inbound signed peer request.
func (e *Engine) VerifyPeer(r *http.Request, body []byte) (*Peer, error) {
	return VerifyRequest(e.peers, r, body, e.cfg.Self.ID)
}

// LockFiles runs fn while holding the mutex that serializes remote applies,
// so a local app-data write and its versioning can't interleave with an
// ApplyRemote of the same file (last-writer-loses clobbers). The server's
// app-data PUT/DELETE handlers wrap their write + LocalWrite/LocalDelete in
// this.
func (e *Engine) LockFiles(fn func()) {
	e.fileMu.Lock()
	defer e.fileMu.Unlock()
	fn()
}

// ManifestFor is the manifest peer id is served: ours, less each file it
// cannot take and does not hold. Offered one, an older release asks for it
// every reconcile and is turned down every time; once its release takes
// the file, the next reconcile here learns so and offers it. Until we have
// read the peer's own manifest, it is served all of ours: it may hold an
// earlier version of a file that has grown since.
func (e *Engine) ManifestFor(id string) map[string]Version {
	e.statusMu.Lock()
	var held map[string]bool
	if st := e.status[id]; st != nil {
		held = st.held
	}
	e.statusMu.Unlock()
	if held == nil {
		return e.man.Snapshot()
	}
	return e.man.SnapshotTaking(e.takes(id), held)
}

// MaxFile is the biggest file this node takes from a peer, for its manifest.
func (e *Engine) MaxFile() int64 { return maxSyncFile }

// Takes is the biggest file the asker of r takes, by its X-Exe-Max-File;
// an older release sends none and takes legacySyncFile.
func Takes(r *http.Request) int64 {
	n, err := strconv.ParseInt(r.Header.Get(hdrMaxFile), 10, 64)
	if err != nil || n <= 0 {
		return legacySyncFile
	}
	return min(n, maxSyncFile)
}

// takes is the biggest file peer id takes, by its last manifest: what it
// named, or legacySyncFile while it has named nothing.
func (e *Engine) takes(id string) int64 {
	e.statusMu.Lock()
	defer e.statusMu.Unlock()
	if st := e.status[id]; st != nil && st.takes > 0 {
		return min(st.takes, maxSyncFile)
	}
	return legacySyncFile
}

// ---- pairing ----------------------------------------------------------------

// MintCode issues a single-use join code another node can pair with.
func (e *Engine) MintCode() (string, time.Time) {
	b := make([]byte, 4)
	rand.Read(b)
	code := hex.EncodeToString(b)
	exp := time.Now().Add(codeTTL)
	e.codeMu.Lock()
	e.codes[code] = exp
	e.codeMu.Unlock()
	return code, exp
}

func (e *Engine) checkCode(code string) bool {
	e.codeMu.Lock()
	defer e.codeMu.Unlock()
	exp, ok := e.codes[code]
	if !ok || time.Now().After(exp) {
		delete(e.codes, code)
		return false
	}
	delete(e.codes, code) // single use
	return true
}

// HandlePair enrolls a joining node (authenticated by join code, since it
// has no key on file yet) and returns our identity plus the peer list so
// the joiner meshes with everyone we know.
func (e *Engine) HandlePair(remoteHost string, req PairRequest) (*PairResponse, error) {
	if !e.checkCode(req.Code) {
		time.Sleep(300 * time.Millisecond) // slow down code guessing
		return nil, errors.New("invalid or expired join code")
	}
	if err := verifyClaimedKey(req.ID, req.PubKey); err != nil {
		return nil, err
	}
	port := req.Port
	if port == "" {
		port = "7777"
	}
	p := &Peer{ID: req.ID, Name: req.Name, PubKey: req.PubKey, Addr: net.JoinHostPort(remoteHost, port)}
	if err := e.peers.Upsert(p); err != nil {
		return nil, err
	}
	e.cfg.Logf("peer: paired with %s (%s) at %s", p.Name, p.ID, p.Addr)
	e.ReconcileNow()
	return &PairResponse{
		ID: e.cfg.Self.ID, Name: e.cfg.Self.Name, PubKey: e.cfg.Self.PubKey(),
		Port: e.cfg.PortFn(), Peers: e.PeerInfos(),
	}, nil
}

// verifyClaimedKey checks a claimed id actually fingerprints the claimed key.
func verifyClaimedKey(id, pubKey string) error {
	p := Peer{PubKey: pubKey}
	key, err := p.Key()
	if err != nil {
		return err
	}
	if Fingerprint(key) != id {
		return errors.New("peer id does not match its key")
	}
	return nil
}

// PeerInfos is the gossiped wire form of the peer list.
func (e *Engine) PeerInfos() []PeerInfo {
	out := []PeerInfo{}
	for _, p := range e.peers.List() {
		out = append(out, PeerInfo{ID: p.ID, Name: p.Name, PubKey: p.PubKey, Addr: p.Addr})
	}
	return out
}

// Join dials addr's pair endpoint with a code its owner read to us.
func (e *Engine) Join(addr, code string) (*Peer, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return nil, errors.New("address required")
	}
	if !strings.Contains(addr, ":") {
		addr = net.JoinHostPort(addr, "7777")
	}
	body, _ := json.Marshal(PairRequest{
		Code: strings.TrimSpace(code), ID: e.cfg.Self.ID, Name: e.cfg.Self.Name,
		PubKey: e.cfg.Self.PubKey(), Port: e.cfg.PortFn(),
	})
	resp, err := e.client.Post("http://"+addr+"/v1/peer/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &apiErr) == nil && apiErr.Error != "" {
			return nil, errors.New(apiErr.Error)
		}
		return nil, fmt.Errorf("pair: HTTP %d", resp.StatusCode)
	}
	var pr PairResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		return nil, err
	}
	if err := verifyClaimedKey(pr.ID, pr.PubKey); err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	port := pr.Port
	if port == "" {
		port = "7777"
	}
	p := &Peer{ID: pr.ID, Name: pr.Name, PubKey: pr.PubKey, Addr: net.JoinHostPort(host, port)}
	if err := e.peers.Upsert(p); err != nil {
		return nil, err
	}
	// transitive mesh: everyone the other node knows becomes our peer too
	for _, pi := range pr.Peers {
		e.adoptPeer(pi)
	}
	e.cfg.Logf("peer: joined %s (%s) at %s", p.Name, p.ID, p.Addr)
	e.ReconcileNow()
	return p, nil
}

// adoptPeer enrolls a gossiped peer, ignoring self and key conflicts.
func (e *Engine) adoptPeer(pi PeerInfo) {
	if pi.ID == e.cfg.Self.ID || pi.ID == "" {
		return
	}
	if err := verifyClaimedKey(pi.ID, pi.PubKey); err != nil {
		e.cfg.Logf("peer: ignoring gossiped %s: %v", pi.ID, err)
		return
	}
	if err := e.peers.Upsert(&Peer{ID: pi.ID, Name: pi.Name, PubKey: pi.PubKey, Addr: pi.Addr}); err != nil {
		e.cfg.Logf("peer: gossiped %s: %v", pi.ID, err)
	}
}

// Leave drops a peer, telling it first (best effort) so it drops us too.
func (e *Engine) Leave(id string) error {
	p, ok := e.peers.Get(id)
	if !ok {
		return errors.New("no such peer")
	}
	req, err := http.NewRequest("POST", "http://"+p.Addr+"/v1/peer/unpair", nil)
	if err == nil {
		SignRequest(e.cfg.Self, req, nil, p.ID)
		if resp, perr := e.client.Do(req); perr == nil {
			resp.Body.Close()
		}
	}
	e.statusMu.Lock()
	delete(e.status, id)
	e.statusMu.Unlock()
	return e.peers.Remove(id)
}

// Unpair handles the inbound side of Leave.
func (e *Engine) Unpair(id string) error {
	e.statusMu.Lock()
	delete(e.status, id)
	e.statusMu.Unlock()
	return e.peers.Remove(id)
}

// ---- local writes -----------------------------------------------------------

func (e *Engine) filePath(key string) string {
	if rel, ok := strings.CutPrefix(key, WorkspaceNS+"/"); ok {
		return filepath.Join(e.cfg.WorkspaceDir, filepath.FromSlash(rel))
	}
	return filepath.Join(e.cfg.DataDir, filepath.FromSlash(key))
}

// LocalWrite versions a write that just landed through the app-data or
// workspace API (app is WorkspaceNS for the latter) and pushes it to every
// peer. Hidden paths (a move into .Trash) stay node-local, and so does a
// file over maxSyncFile, as the scanner leaves it: a workspace move brings
// one here whatever its size, and a version stamped on it is one every
// reconcile would offer the peers and none of them could take.
func (e *Engine) LocalWrite(app, rel string) {
	key := app + "/" + rel
	if !e.validKey(key) {
		return
	}
	fi, err := os.Stat(e.filePath(key))
	if err != nil || fi.Size() > maxSyncFile {
		return
	}
	e.man.Bump(key, e.cfg.Self.ID, false, fi.Size(), fi.ModTime().UnixNano())
	go e.pushAll(key)
}

// LocalDelete versions an API delete (tombstoned in the manifest).
func (e *Engine) LocalDelete(app, rel string) {
	key := app + "/" + rel
	if !e.validKey(key) {
		return
	}
	e.man.Bump(key, e.cfg.Self.ID, true, 0, 0)
	go e.pushAll(key)
}

// ScanLocal picks up changes that bypassed the API: files edited or dropped
// directly in ~/.exe/appdata or ~/.exe/workspace (agents and VMs do this),
// plus first-boot inventory.
func (e *Engine) ScanLocal() {
	e.fileMu.Lock()
	defer e.fileMu.Unlock()
	e.migrateWorkspaceStrays()
	seen := map[string]bool{}
	e.scanRoot(e.cfg.DataDir, "", seen)
	if e.cfg.WorkspaceDir != "" {
		e.scanRoot(e.cfg.WorkspaceDir, WorkspaceNS+"/", seen)
	}
	for key, v := range e.man.Snapshot() {
		if v.Deleted || seen[key] {
			continue
		}
		if e.cfg.WorkspaceDir == "" && strings.HasPrefix(key, WorkspaceNS+"/") {
			continue // workspace sync off: don't tombstone what we didn't scan
		}
		e.man.Bump(key, e.cfg.Self.ID, true, 0, 0)
		go e.pushAll(key)
	}
}

// migrateWorkspaceStrays heals a tree written by a pre-workspace-sync
// binary: its engine pulled an upgraded peer's @workspace keys and, knowing
// no namespaces, landed the files under appdata/@workspace/. Left there,
// the upgraded scanner would find those manifest keys in neither tree and
// sweep them as deletions — propagating deletes that erase every node's
// real workspace files. Renames keep size+mtime, so the recorded
// fingerprints stay valid and nothing re-versions.
func (e *Engine) migrateWorkspaceStrays() {
	if e.cfg.WorkspaceDir == "" {
		return
	}
	src := filepath.Join(e.cfg.DataDir, WorkspaceNS)
	if _, err := os.Stat(src); err != nil {
		return
	}
	var dirs []string
	filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			dirs = append(dirs, p)
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return nil
		}
		dst := filepath.Join(e.cfg.WorkspaceDir, rel)
		if _, err := os.Stat(dst); err == nil {
			return nil // the workspace already has one; leave the stray be
		}
		if os.MkdirAll(filepath.Dir(dst), 0o755) == nil && os.Rename(p, dst) == nil {
			e.cfg.Logf("peer: moved stray appdata/%s/%s into the workspace", WorkspaceNS, filepath.ToSlash(rel))
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i]) // children first; fails harmlessly when not empty
	}
}

// scanRoot inventories one synced tree. prefix "" is appdata, whose keys are
// App/rel (so a stray file at the root is skipped); WorkspaceNS+"/" prefixes
// workspace keys, where root-level files are the norm.
func (e *Engine) scanRoot(root, prefix string, seen map[string]bool) {
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir // .Trash and friends stay node-local
			}
			return nil // temp files (.put-*, .sync-*) are never inventory
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil
		}
		key := prefix + filepath.ToSlash(rel)
		if prefix == "" && (!strings.Contains(key, "/") || strings.HasPrefix(key, "@")) {
			return nil // appdata strays: root-level files, reserved-@ leftovers
		}
		seen[key] = true
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if fi.Size() > maxSyncFile {
			// stays local (but seen, so never tombstoned). An entry stamped
			// on it before LocalWrite knew the cap goes: while it stands,
			// every reconcile sends this file and every peer asks for it.
			if e.man.DropUnshared(key, e.cfg.Self.ID, maxSyncFile) {
				e.cfg.Logf("peer: %s is over the sync cap and stays local — dropped from the manifest", key)
			}
			return nil
		}
		if !e.man.Fingerprint(key, fi.Size(), fi.ModTime().UnixNano()) {
			e.man.Bump(key, e.cfg.Self.ID, false, fi.Size(), fi.ModTime().UnixNano())
			go e.pushAll(key)
		}
		return nil
	})
}

// ---- push / fetch -----------------------------------------------------------

func peerFileURL(addr, key string) string {
	parts := strings.Split(key, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return "http://" + addr + "/v1/peer/file/" + strings.Join(parts, "/")
}

// openSyncFile opens key's file for the wire, to a peer that takes up to
// max, and says its size; the caller sends that many bytes of it and closes
// it. One bigger is refused on its size alone, before any of it is read or
// sent: the other side takes the first max bytes, turns it down, and the
// next reconcile would send it again. A file replaced while it is sent
// (they are, by a rename) is sent whole as it was opened.
func (e *Engine) openSyncFile(key string, max int64) (*os.File, int64, error) {
	f, err := openForSend(e.filePath(key))
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = errors.New("not a regular file")
	}
	if err == nil && fi.Size() > max {
		err = errTooLarge
	}
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

// progress passes reads through, putting off the transfer's stall timer on
// each one, and by answerTimeout at the end of a body.
type progress struct {
	r io.Reader
	t *time.Timer
}

func (p progress) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if err == io.EOF {
		p.t.Reset(answerTimeout)
	} else {
		p.t.Reset(stallTimeout)
	}
	return n, err
}

// doFile runs a file request under the stall timer: sending its body and
// reading the returned answer both put the timer off. Call done once the
// answer is read.
func (e *Engine) doFile(req *http.Request) (resp *http.Response, answer io.Reader, done func(), err error) {
	ctx, cancel := context.WithCancel(req.Context())
	t := time.AfterFunc(stallTimeout, cancel)
	done = func() { t.Stop(); cancel() }
	if get := req.GetBody; get != nil && req.ContentLength > 0 {
		// GetBody too, as the transport rewinds with it to resend
		req.GetBody = func() (io.ReadCloser, error) {
			b, err := get()
			if err != nil {
				return nil, err
			}
			return io.NopCloser(progress{b, t}), nil
		}
		req.Body, _ = req.GetBody()
	}
	resp, err = e.files.Do(req.WithContext(ctx))
	if err != nil {
		done()
		return nil, nil, nil, err
	}
	return resp, progress{resp.Body, t}, done, nil
}

func (e *Engine) pushAll(key string) {
	for _, p := range e.peers.List() {
		e.pushTo(p, key)
	}
}

// pushTo sends our current version of key to one peer. The receiving side
// answers "stale" when it already has something newer — that's fine, its
// own push or the next reconcile settles it. A file bigger than the peer
// takes is not sent: it waits here until the peer's release takes it.
func (e *Engine) pushTo(p *Peer, key string) {
	v, ok := e.man.Get(key)
	if !ok {
		return
	}
	// the file is hashed for the signature, then sent off the disk
	sum, size := bodySum(nil), int64(0)
	var f *os.File
	if !v.Deleted {
		max := e.takes(p.ID)
		fi, err := os.Stat(e.filePath(key))
		if err != nil || fi.Size() > max {
			return
		}
		if fi.Size() > legacySyncFile {
			e.big <- struct{}{}
			defer func() { <-e.big }()
		}
		if f, size, err = e.openSyncFile(key, max); err != nil {
			return
		}
		defer f.Close()
		if sum, err = hashFile(f, size); err != nil {
			return
		}
	}
	req, err := http.NewRequest("PUT", peerFileURL(p.Addr, key), nil)
	if err != nil {
		return
	}
	if size > 0 {
		req.ContentLength = size
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(io.NewSectionReader(f, 0, size)), nil
		}
		req.Body, _ = req.GetBody()
	}
	req.Header.Set(hdrVer, EncodeVector(v))
	if v.Deleted {
		req.Header.Set(hdrDeleted, "1")
	}
	req.Header.Set(hdrSum, sum)
	signSum(e.cfg.Self, req, sum, p.ID)
	resp, answer, done, err := e.doFile(req)
	if err != nil {
		e.cfg.Logf("peer: push %s to %s: %v", key, p.Name, err)
		return
	}
	defer done()
	io.Copy(io.Discard, io.LimitReader(answer, 4096))
	resp.Body.Close()
	// 409: the peer has this file on its way in from another node already
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusConflict {
		e.cfg.Logf("peer: push %s to %s: HTTP %d", key, p.Name, resp.StatusCode)
	}
}

func (e *Engine) fetchFrom(p *Peer, key string, rv Version) {
	if rv.Deleted {
		if _, err := e.ApplyRemote(key, nil, rv); err != nil {
			e.cfg.Logf("peer: apply delete %s from %s: %v", key, p.Name, err)
		}
		return
	}
	release, ok := e.claim(key)
	if !ok {
		return // on its way in already; the next reconcile looks again
	}
	defer release()
	req, err := http.NewRequest("GET", peerFileURL(p.Addr, key), nil)
	if err != nil {
		return
	}
	req.Header.Set(hdrMaxFile, strconv.FormatInt(maxSyncFile, 10))
	SignRequest(e.cfg.Self, req, nil, p.ID)
	resp, answer, done, err := e.doFile(req)
	if err != nil {
		e.cfg.Logf("peer: fetch %s from %s: %v", key, p.Name, err)
		return
	}
	defer done()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	in, err := e.stage(answer, maxSyncFile)
	if err != nil {
		return
	}
	defer in.Discard()
	got := ParseVersion(resp.Header.Get(hdrVer), false)
	if len(got.Vec) == 0 {
		got = rv // response carried no version header; use the manifest one
	}
	if _, err := e.ApplyRemote(key, in, got); err != nil {
		e.cfg.Logf("peer: apply %s from %s: %v", key, p.Name, err)
	}
}

// FileForPeer opens what a peer's GET is served: the file, its size and our
// version of it, to an asker that takes files up to max (Takes). The caller
// sends size bytes of the file and closes it. A bigger one is refused, so an
// older release that lists it in our manifest asks for it in vain without
// fetching any of it.
func (e *Engine) FileForPeer(app, rel string, max int64) (*os.File, int64, Version, error) {
	key := app + "/" + rel
	v, ok := e.man.Get(key)
	if !ok || v.Deleted {
		return nil, 0, Version{}, os.ErrNotExist
	}
	f, size, err := e.openSyncFile(key, max)
	if err != nil {
		return nil, 0, Version{}, err
	}
	return f, size, v, nil
}

// ---- applying remote changes ------------------------------------------------

// ApplyRemote lands a peer's version of key locally, using the version
// vectors to separate causal order from concurrency:
//
//	stale       — we already dominate the remote; the sender takes ours
//	applied     — remote strictly dominated ours; adopted verbatim
//	merged      — concurrent mergeable docs unioned item-by-item
//	resolved    — concurrent non-mergeable conflict; deterministic winner
//	              kept, the loser preserved under ~/.exe/sync-conflicts
//	kept-live   — concurrent delete vs. our live file; the live file survives
//	resurrected — concurrent live remote vs. our tombstone; content restored
//
// Every concurrency resolution sets the version to the pointwise max of both
// inputs (no self-increment), so all nodes converge on the same vector with
// no ping-pong. A delete only wins when it causally dominates the local
// version, so a stale tombstone can never delete concurrent real data.
//
// in is the remote content, staged (ReceiveFile, stage); nil for a delete.
// What lands is moved into place, so the caller's Discard finds nothing.
func (e *Engine) ApplyRemote(key string, in *Staged, rv Version) (string, error) {
	if !e.validKey(key) {
		return "", errors.New("invalid sync key")
	}
	if in == nil && !rv.Deleted {
		return "", errors.New("a live version needs its content")
	}
	e.fileMu.Lock()
	defer e.fileMu.Unlock()
	p := e.filePath(key)
	local, exists := e.man.Get(key)
	// Version any on-disk content the manifest hasn't seen yet — data that
	// predates pairing, or direct edits under ~/.exe/appdata — BEFORE
	// comparing, so a push racing the first scan can't clobber unversioned
	// local work (it becomes a concurrent version, not a dominated one).
	if fi, err := os.Stat(p); err == nil {
		if !exists || !e.man.Fingerprint(key, fi.Size(), fi.ModTime().UnixNano()) {
			local = e.man.Bump(key, e.cfg.Self.ID, false, fi.Size(), fi.ModTime().UnixNano())
			exists = true
		}
	}
	app, rel, _ := strings.Cut(key, "/")
	if !exists {
		return e.adoptRemote(key, app, rel, p, in, rv)
	}
	switch {
	case local.SameVersion(rv), local.Dominates(rv):
		return "stale", nil
	case rv.Dominates(local):
		return e.adoptRemote(key, app, rel, p, in, rv)
	default:
		return e.resolveConcurrent(key, app, rel, p, in, rv, local)
	}
}

// adoptRemote takes the remote version verbatim — it dominates ours, or we
// had nothing: its bytes for a write, or a delete of our copy.
func (e *Engine) adoptRemote(key, app, rel, p string, in *Staged, rv Version) (string, error) {
	if rv.Deleted {
		os.Remove(p)
		e.man.Apply(key, rv, 0, 0)
		e.notifyApply(app, rel, true)
		return "applied", nil
	}
	if err := in.moveTo(p); err != nil {
		return "", err
	}
	fi, _ := os.Stat(p)
	e.man.Apply(key, rv, fi.Size(), fi.ModTime().UnixNano())
	e.notifyApply(app, rel, false)
	return "applied", nil
}

// resolveConcurrent handles a genuine conflict — neither version has seen the
// other. The result carries the pointwise-max vector (dominating both), so
// every node converges; then we push so peers that resolved differently or
// not yet catch up.
func (e *Engine) resolveConcurrent(key, app, rel, p string, in *Staged, rv, local Version) (string, error) {
	max := Version{Vec: MaxVec(local.Vec, rv.Vec)}

	// delete vs. live: the live side always survives, so a stale tombstone
	// never deletes data the deleting node had not seen.
	if local.Deleted != rv.Deleted {
		if rv.Deleted { // we are live, remote deleted -> keep ours
			fi, _ := os.Stat(p)
			e.man.Apply(key, max, sizeOf(fi), mtimeOf(fi))
			go e.pushAll(key)
			return "kept-live", nil
		}
		if err := in.moveTo(p); err != nil { // we are a tombstone -> restore
			return "", err
		}
		fi, _ := os.Stat(p)
		e.man.Apply(key, max, fi.Size(), fi.ModTime().UnixNano())
		e.notifyApply(app, rel, false)
		go e.pushAll(key)
		return "resurrected", nil
	}
	if local.Deleted { // both tombstones -> converge the vector, stay deleted
		e.man.Apply(key, Version{Vec: max.Vec, Deleted: true}, 0, 0)
		return "stale", nil
	}

	// both live and mergeable: union the records item-by-item (the one
	// kind of file read into memory)
	if Mergeable(key) {
		body, err := in.bytes()
		if err != nil {
			return "", err
		}
		if out, err, handled := e.mergeConcurrent(key, app, rel, p, body, max); handled {
			return out, err
		}
	}

	// non-mergeable (or unparseable) both-live conflict: deterministic winner
	// by content hash; the loser is preserved under sync-conflicts.
	localSum := fileSum(p)
	if localSum == in.sum { // same content, just converge the vector
		fi, _ := os.Stat(p)
		e.man.Apply(key, max, sizeOf(fi), mtimeOf(fi))
		go e.pushAll(key)
		return "stale", nil
	}
	// A loser that cannot be saved stops the apply: ours stays as it is,
	// the remote's stays on its node, and the next reconcile tries again.
	if in.sum > localSum { // remote wins
		if err := e.backupConflict(app, rel, p, nil); err != nil {
			return "", err
		}
		if err := in.moveTo(p); err != nil {
			return "", err
		}
		fi, _ := os.Stat(p)
		e.man.Apply(key, max, fi.Size(), fi.ModTime().UnixNano())
		e.notifyApply(app, rel, false)
		go e.pushAll(key)
		return "resolved", nil
	}
	if err := e.backupConflict(app, rel, "", in); err != nil { // local wins
		return "", err
	}
	fi, _ := os.Stat(p)
	e.man.Apply(key, max, sizeOf(fi), mtimeOf(fi))
	go e.pushAll(key)
	return "resolved", nil
}

// mergeConcurrent unions two mergeable docs. handled=false when our own copy
// won't parse, so the caller falls through to the content-hash path; a
// remote that won't parse (a legacy v1 blob) never erases our v2 doc.
func (e *Engine) mergeConcurrent(key, app, rel, p string, body []byte, max Version) (string, error, bool) {
	localBytes, err := os.ReadFile(p)
	if err != nil {
		return "", nil, false
	}
	if _, lok := CanonicalFile(key, localBytes); !lok {
		return "", nil, false
	}
	if _, rok := CanonicalFile(key, body); !rok { // keep our parseable content
		fi, _ := os.Stat(p)
		e.man.Apply(key, max, sizeOf(fi), mtimeOf(fi))
		go e.pushAll(key)
		return "kept-live", nil, true
	}
	mergedBytes, ok := MergeFile(key, localBytes, body)
	if !ok {
		return "", nil, false
	}
	if bytes.Equal(mergedBytes, localBytes) { // nothing new from the remote
		fi, _ := os.Stat(p)
		e.man.Apply(key, max, sizeOf(fi), mtimeOf(fi))
		go e.pushAll(key)
		return "stale", nil, true
	}
	if err := writeFileAtomic(p, mergedBytes); err != nil {
		return "", err, true
	}
	fi, _ := os.Stat(p)
	e.man.Apply(key, max, fi.Size(), fi.ModTime().UnixNano())
	e.notifyApply(app, rel, false)
	go e.pushAll(key)
	return "merged", nil, true
}

func sizeOf(fi os.FileInfo) int64 {
	if fi == nil {
		return 0
	}
	return fi.Size()
}

func mtimeOf(fi os.FileInfo) int64 {
	if fi == nil {
		return 0
	}
	return fi.ModTime().UnixNano()
}

// backupConflict preserves the losing side of a non-mergeable conflict under
// ~/.exe/sync-conflicts/<app>/, outside appdata so it never itself syncs:
// a copy of our file at local, or the staged remote copy, moved there. An
// error means the loser is not saved, and the caller must not replace it
// or settle the conflict. An empty or missing loser has nothing to save.
func (e *Engine) backupConflict(app, rel, local string, remote *Staged) error {
	src := local
	if remote != nil {
		src = remote.path
	}
	fi, err := os.Stat(src)
	if os.IsNotExist(err) || (err == nil && fi.Size() == 0) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("conflict backup of %s/%s: %w", app, rel, err)
	}
	dir := filepath.Join(e.cfg.StateDir, "sync-conflicts", app)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.cfg.Logf("peer: conflict backup %s/%s failed: %v", app, rel, err)
		return fmt.Errorf("conflict backup of %s/%s: %w", app, rel, err)
	}
	name := fmt.Sprintf("%s.%d.bak", strings.ReplaceAll(rel, "/", "_"), time.Now().UnixNano())
	dst := filepath.Join(dir, name)
	if remote != nil {
		err = remote.moveTo(dst)
	} else {
		err = copyFile(local, dst)
	}
	if err != nil {
		e.cfg.Logf("peer: conflict backup %s/%s failed: %v", app, rel, err)
		return fmt.Errorf("conflict backup of %s/%s: %w", app, rel, err)
	}
	e.cfg.Logf("peer: conflict on %s/%s — losing copy saved to sync-conflicts/%s/%s", app, rel, app, name)
	if e.cfg.OnConflict != nil {
		// own goroutine: the hook may write app data (the Newsfeed journal),
		// which takes the file lock this apply path already holds
		go e.cfg.OnConflict(app, rel)
	}
	return nil
}

func (e *Engine) notifyApply(app, rel string, deleted bool) {
	if e.cfg.OnApply != nil {
		e.cfg.OnApply(app, rel, deleted)
	}
}

func writeFileAtomic(p string, body []byte) error {
	return writeAtomic(p, bytes.NewReader(body))
}

// writeAtomic writes src to a temp file beside p and renames it over p.
func writeAtomic(p string, src io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".sync-*")
	if err != nil {
		return err
	}
	if _, err = io.Copy(tmp, src); err == nil {
		err = tmp.Close()
	} else {
		tmp.Close()
	}
	if err == nil {
		err = os.Rename(tmp.Name(), p)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// ---- reconcile --------------------------------------------------------------

// reconcile diffs manifests with one peer: fetch what it has newer, push
// what we have newer, and adopt any peers it knows that we don't (that's
// what makes a third node joining B also sync with A).
func (e *Engine) reconcile(p *Peer) {
	start := time.Now()
	req, err := http.NewRequest("GET", "http://"+p.Addr+"/v1/peer/manifest", nil)
	if err != nil {
		return
	}
	SignRequest(e.cfg.Self, req, nil, p.ID)
	resp, err := e.client.Do(req)
	if err != nil {
		e.setStatus(p, 0, err)
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		e.setStatus(p, 0, fmt.Errorf("HTTP %d", resp.StatusCode))
		return
	}
	var mr manifestResponse
	if err := json.Unmarshal(raw, &mr); err != nil {
		e.setStatus(p, 0, err)
		return
	}
	latency := time.Since(start)
	if mr.ID != p.ID {
		e.setStatus(p, 0, fmt.Errorf("address now answers as %s", mr.ID))
		return
	}
	e.setStatus(p, latency, nil)
	e.peers.SetIcon(p.ID, mr.Icon)
	held := make(map[string]bool, len(mr.Files))
	for key := range mr.Files {
		held[key] = true
	}
	e.statusMu.Lock()
	if st := e.status[p.ID]; st != nil {
		st.takes, st.held = mr.MaxFile, held
	}
	e.statusMu.Unlock()
	if mr.Name != "" && mr.Name != p.Name {
		e.peers.Upsert(&Peer{ID: p.ID, Name: mr.Name, PubKey: p.PubKey})
	}
	for _, pi := range mr.Peers {
		if _, known := e.peers.Get(pi.ID); !known {
			e.adoptPeer(pi)
		}
	}
	localFiles := e.man.Snapshot()
	for key, rv := range mr.Files {
		if !e.validKey(key) {
			e.cfg.Logf("peer: %s offered invalid sync key %q — ignored", p.Name, key)
			continue
		}
		lv, ok := localFiles[key]
		// pull anything we don't have, the peer strictly newer, or a
		// concurrent version we must resolve
		if !ok || rv.Dominates(lv) || rv.Concurrent(lv) {
			e.fetchFrom(p, key, rv)
		}
	}
	for key, lv := range localFiles {
		rv, ok := mr.Files[key]
		if !ok || lv.Dominates(rv) || lv.Concurrent(rv) {
			e.pushTo(p, key)
		}
	}
}

func (e *Engine) setStatus(p *Peer, latency time.Duration, err error) {
	e.statusMu.Lock()
	st := e.status[p.ID]
	if st == nil {
		st = &Status{}
		e.status[p.ID] = st
	}
	wasOnline := st.Online
	st.ID, st.Name, st.Addr = p.ID, p.Name, p.Addr
	if err != nil {
		st.Online, st.Error = false, err.Error()
		e.statusMu.Unlock()
		return
	}
	st.Online, st.Error = true, ""
	st.LatencyMS = latency.Milliseconds()
	st.LastSeen = time.Now().UnixMilli()
	e.statusMu.Unlock()
	// A peer that just came back (e.g. a laptop woke) shouldn't wait up to a
	// full reconcile interval — kick an immediate pass. The latency Probe
	// (every 5s while the Node Sync dialog is open) also drives this transition.
	if !wasOnline {
		e.ReconcileNow()
	}
}

// ErrNoSuchPeer: Fetch was given an id this node has not joined.
var ErrNoSuchPeer = errors.New("no such node")

// Fetch asks peer id for one of its read-only daemon-to-daemon routes
// (path and query, "/v1/peer/monitor?after=0"), signed as a sync request
// is; the Activity Monitor shows another node's figures this way. The
// caller closes the body.
func (e *Engine) Fetch(ctx context.Context, id, pathQuery string) (*http.Response, error) {
	p, ok := e.peers.Get(id)
	if !ok {
		return nil, ErrNoSuchPeer
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+p.Addr+pathQuery, nil)
	if err != nil {
		return nil, err
	}
	SignRequest(e.cfg.Self, req, nil, p.ID)
	return e.client.Do(req)
}

// ---- status / latency -------------------------------------------------------

// StatusCached returns the last known statuses without probing — a probe
// blocks up to the 4 s ping timeout per unreachable peer, too slow for a
// UI read on the dialog-open path. Peers never probed since startup come
// back with Online=false and an empty Error.
func (e *Engine) StatusCached() []*Status {
	return e.statusList()
}

// Probe measures round-trip latency to every peer, cached for 5 s so the
// dialog's poll doesn't hammer the mesh; force bypasses the cache.
func (e *Engine) Probe(force bool) []*Status {
	e.statusMu.Lock()
	fresh := time.Since(e.statusAt) < statusTTL
	e.statusMu.Unlock()
	if !force && fresh {
		return e.statusList()
	}
	var wg sync.WaitGroup
	for _, p := range e.peers.List() {
		wg.Add(1)
		go func(p *Peer) {
			defer wg.Done()
			start := time.Now()
			req, err := http.NewRequest("GET", "http://"+p.Addr+"/v1/peer/ping", nil)
			if err != nil {
				return
			}
			SignRequest(e.cfg.Self, req, nil, p.ID)
			client := &http.Client{Timeout: 4 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				e.setStatus(p, 0, errors.New("unreachable"))
				return
			}
			var pong struct {
				Icon string `json:"icon"`
			}
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				e.setStatus(p, 0, fmt.Errorf("HTTP %d", resp.StatusCode))
				return
			}
			e.setStatus(p, time.Since(start), nil)
			if json.Unmarshal(raw, &pong) == nil {
				e.peers.SetIcon(p.ID, pong.Icon)
			}
		}(p)
	}
	wg.Wait()
	e.statusMu.Lock()
	e.statusAt = time.Now()
	e.statusMu.Unlock()
	return e.statusList()
}

func (e *Engine) statusList() []*Status {
	e.statusMu.Lock()
	defer e.statusMu.Unlock()
	var out []*Status
	for _, p := range e.peers.List() {
		if st, ok := e.status[p.ID]; ok {
			cp := *st
			cp.Name, cp.Addr, cp.Icon = p.Name, p.Addr, p.Icon
			out = append(out, &cp)
		} else {
			out = append(out, &Status{ID: p.ID, Name: p.Name, Addr: p.Addr, Icon: p.Icon})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name+out[i].ID < out[j].Name+out[j].ID })
	return out
}
