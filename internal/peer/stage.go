package peer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// A file crosses between nodes by way of the disk, never whole in memory:
// a push is hashed and then sent straight off its file, and what arrives
// (a push received, a fetch answered) is written to a staging file as it
// comes, hashed on the way, and renamed into place by ApplyRemote. Only a
// mergeable document, whose records are merged, is read in.

// Staged is a peer's file landed in the staging folder, waiting for
// ApplyRemote, with the SHA-256 it hashed to on the way in.
type Staged struct {
	path    string
	sum     string // hex, bodySum's form
	size    int64
	release func() // ends ReceiveFile's claim on the key; nil for none
}

// Discard removes the staged file unless ApplyRemote took it, and ends the
// claim on its key. Safe on nil and more than once.
func (s *Staged) Discard() {
	if s == nil {
		return
	}
	if s.path != "" {
		os.Remove(s.path)
		s.path = ""
	}
	if s.release != nil {
		s.release()
		s.release = nil
	}
}

func (s *Staged) bytes() ([]byte, error) { return os.ReadFile(s.path) }

// moveTo puts the staged file at p, which leaves it nothing to discard.
func (s *Staged) moveTo(p string) error {
	if err := moveFile(s.path, p); err != nil {
		return err
	}
	s.path = ""
	return nil
}

// stagingDir is ~/.exe/sync-incoming: on the file system of the app data
// and the Workspace, so a staged file lands by a rename.
func (e *Engine) stagingDir() string { return filepath.Join(e.cfg.StateDir, "sync-incoming") }

// clearStaging removes what an earlier daemon left staged: its transfers
// ended with it.
func (e *Engine) clearStaging() {
	ents, _ := os.ReadDir(e.stagingDir())
	for _, d := range ents {
		if strings.HasPrefix(d.Name(), "in-") {
			os.Remove(filepath.Join(e.stagingDir(), d.Name()))
		}
	}
}

// stage copies r to a new staging file, hashing it as it goes. More than
// max bytes is errTooLarge, and nothing is left behind.
func (e *Engine) stage(r io.Reader, max int64) (*Staged, error) {
	if err := os.MkdirAll(e.stagingDir(), 0o755); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(e.stagingDir(), "in-*")
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, max+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > max {
		err = errTooLarge
	}
	if err != nil {
		os.Remove(f.Name())
		return nil, err
	}
	return &Staged{path: f.Name(), sum: hex.EncodeToString(h.Sum(nil)), size: n}, nil
}

// stageBytes stages content that is already in memory.
func (e *Engine) stageBytes(b []byte) (*Staged, error) {
	return e.stage(bytes.NewReader(b), int64(len(b)))
}

// ReceiveFile authenticates a peer's push of key and stages its body, for
// ApplyRemote; the caller discards what it does not apply (Discard, which
// also lets the next copy of key in). On an error the int is the HTTP
// status to answer with.
//
// The sender, recipient and timestamp are checked before any of the body is
// read, so the timestamp has to be fresh when a push starts, not when a big
// file is finally in. A sender that says the body's hash (hdrSum) has its
// signature checked then too, and the body must hash to it; one from an
// older release says none, and its signature is checked over the staged
// body's hash. A push of a file already on its way in is turned down, 409,
// before its body is read (claim).
func (e *Engine) ReceiveFile(r *http.Request, key string) (*Staged, int, error) {
	p, err := checkSigner(e.peers, r, e.cfg.Self.ID)
	if err != nil {
		return nil, http.StatusUnauthorized, err
	}
	claimed := r.Header.Get(hdrSum)
	if claimed != "" {
		if err := checkSig(p, r, claimed, e.cfg.Self.ID); err != nil {
			return nil, http.StatusUnauthorized, err
		}
	}
	if r.ContentLength > maxSyncFile {
		return nil, http.StatusRequestEntityTooLarge, errTooLarge
	}
	release := func() {}
	if r.Header.Get(hdrDeleted) != "1" {
		var ok bool
		if release, ok = e.claim(key); !ok {
			return nil, http.StatusConflict, errBusy
		}
	}
	var body io.Reader = http.NoBody
	if r.Body != nil {
		body = r.Body
	}
	in, err := e.stage(body, maxSyncFile)
	if err != nil {
		release()
	} else {
		in.release = release
	}
	if errors.Is(err, errTooLarge) {
		return nil, http.StatusRequestEntityTooLarge, err
	}
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if claimed == "" {
		if err := checkSig(p, r, in.sum, e.cfg.Self.ID); err != nil {
			in.Discard()
			return nil, http.StatusUnauthorized, err
		}
	} else if in.sum != claimed { // the file changed while it was sent
		in.Discard()
		return nil, http.StatusBadRequest, errors.New("body does not match its signed hash")
	}
	return in, http.StatusOK, nil
}

// hashFile hashes the first size bytes of f, bodySum's form; a file that
// holds fewer is an error.
func hashFile(f *os.File, size int64) (string, error) {
	h := sha256.New()
	n, err := io.Copy(h, io.NewSectionReader(f, 0, size))
	if err == nil && n != size {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fileSum hashes the file at p, contentHash's form; one that cannot be read
// hashes as empty, as the in-memory comparison read it.
func fileSum(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return bodySum(nil)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return bodySum(nil)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// moveFile puts src at p by a rename, or where that fails (another file
// system) by a copy beside p renamed over it; src is gone either way. p is
// replaced whole, never seen half-written.
func moveFile(src, p string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if os.Rename(src, p) == nil {
		return nil
	}
	if err := copyFile(src, p); err != nil {
		return err
	}
	os.Remove(src)
	return nil
}

// copyFile writes a copy of src at p through a temp file beside it.
func copyFile(src, p string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	return writeAtomic(p, f)
}
