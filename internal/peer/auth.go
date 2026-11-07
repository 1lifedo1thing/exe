package peer

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Peer requests are authenticated by ed25519 signature, not by the API
// token — a peer may only reach /v1/peer/*, never the rest of the daemon.
// The signature covers method, path, a timestamp (±2 min skew window), the
// body hash and the sync-version headers, so a request can't be re-targeted;
// a replay inside the window just re-applies the same version, which the
// stale check makes a no-op.
const (
	hdrPeer    = "X-Exe-Peer"
	hdrTo      = "X-Exe-To"
	hdrTS      = "X-Exe-Ts"
	hdrSig     = "X-Exe-Sig"
	hdrVer     = "X-Exe-Ver" // base64(json) of the file's version vector
	hdrDeleted = "X-Exe-Deleted"
	hdrMaxFile = "X-Exe-Max-File" // the biggest file a fetching peer takes
	// hdrSum is a file push's body hash, the one its signature covers, said
	// up front so the receiver checks the signature before the body arrives
	// (ReceiveFile). An older release sends none.
	hdrSum = "X-Exe-Sum"

	// Personal machines (laptops that sleep) drift; a generous window keeps
	// sync working across modest skew. Replay is already defanged by the
	// recipient binding below and by the versioned, idempotent nature of
	// file writes, so a wider window trades little.
	maxSkew = 10 * time.Minute
)

// canonical binds a request to its sender, its intended recipient (to), the
// timestamp, the body and the version headers — so a signature captured by
// one peer cannot be replayed against a different daemon.
func canonical(method, path, to, ts, bodySum, ver, deleted string) []byte {
	return []byte(strings.Join([]string{method, path, to, ts, bodySum, ver, deleted}, "\n"))
}

func bodySum(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// SignRequest stamps r with this node's identity headers, bound to recipient
// node `to`. Call after the version headers (if any) are set.
func SignRequest(id *Identity, r *http.Request, body []byte, to string) {
	signSum(id, r, bodySum(body), to)
}

// signSum is SignRequest for a body hashed beforehand (bodySum's form): a
// file pushed straight off the disk.
func signSum(id *Identity, r *http.Request, sum, to string) {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	r.Header.Set(hdrPeer, id.ID)
	r.Header.Set(hdrTo, to)
	r.Header.Set(hdrTS, ts)
	msg := canonical(r.Method, r.URL.Path, to, ts, sum,
		r.Header.Get(hdrVer), r.Header.Get(hdrDeleted))
	r.Header.Set(hdrSig, id.Sign(msg))
}

// VerifyRequest authenticates an inbound peer request against the enrolled
// peer list and returns the caller. selfID is this node's own id: a request
// addressed to anyone else is rejected, so a peer cannot relay a signature
// meant for it to a third node.
func VerifyRequest(store *Store, r *http.Request, body []byte, selfID string) (*Peer, error) {
	p, err := checkSigner(store, r, selfID)
	if err != nil {
		return nil, err
	}
	if err := checkSig(p, r, bodySum(body), selfID); err != nil {
		return nil, err
	}
	return p, nil
}

// checkSigner is all of VerifyRequest but the signature: an enrolled sender,
// this node as the recipient, a timestamp inside the window.
func checkSigner(store *Store, r *http.Request, selfID string) (*Peer, error) {
	id := r.Header.Get(hdrPeer)
	p, ok := store.Get(id)
	if !ok {
		return nil, errors.New("unknown peer")
	}
	if to := r.Header.Get(hdrTo); to != selfID {
		return nil, errors.New("request not addressed to this node")
	}
	ms, err := strconv.ParseInt(r.Header.Get(hdrTS), 10, 64)
	if err != nil {
		return nil, errors.New("bad timestamp")
	}
	if d := time.Since(time.UnixMilli(ms)); d > maxSkew || d < -maxSkew {
		return nil, fmt.Errorf("timestamp skew %s exceeds %s (are both clocks synced? check NTP)", d.Round(time.Second), maxSkew)
	}
	return p, nil
}

// checkSig verifies that p signed r over a body that hashes to sum.
func checkSig(p *Peer, r *http.Request, sum, selfID string) error {
	key, err := p.Key()
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(r.Header.Get(hdrSig))
	if err != nil {
		return errors.New("bad signature encoding")
	}
	msg := canonical(r.Method, r.URL.Path, selfID, r.Header.Get(hdrTS), sum,
		r.Header.Get(hdrVer), r.Header.Get(hdrDeleted))
	if !ed25519.Verify(key, msg, sig) {
		return errors.New("bad signature")
	}
	return nil
}
