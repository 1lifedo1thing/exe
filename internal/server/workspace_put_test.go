package server

import (
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exe/internal/peer"
)

func TestWorkspaceCapIsWhatSyncCarries(t *testing.T) {
	// a Workspace file the API takes and sync would not stays on one node
	if got := (*peer.Engine)(nil).MaxFile(); got != workspaceMax {
		t.Fatalf("sync carries %d bytes a file, the Workspace takes %d", got, workspaceMax)
	}
}

func TestWorkspaceTakesMoreThanAppData(t *testing.T) {
	n := newTestNode(t)
	n.installBundle(t, "Paint")
	put := func(path string, size int64, length int64) (int, string) {
		r := httptest.NewRequest("PUT", path, io.LimitReader(zeros{}, size))
		r.ContentLength = length
		w := httptest.NewRecorder()
		n.srv.Handler().ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	over := int64(fileMax + 1) // over what app data takes
	if code, body := put("/v1/workspace/big.bin", over, over); code != 200 {
		t.Fatalf("a Workspace file over 100 MB: %d %s", code, body)
	}
	if fi, err := os.Stat(filepath.Join(n.srv.workspaceDir(), "big.bin")); err != nil || fi.Size() != over {
		t.Fatalf("big.bin landed with %v, %v", fi, err)
	}
	if code, body := put("/v1/apps/Paint/data/big.bin", over, over); code != 413 || !strings.Contains(body, "100 MB") {
		t.Fatalf("an app-data file over 100 MB: %d %s", code, body)
	}
	if code, body := put("/v1/workspace/huge.bin", 16, workspaceMax+1); code != 413 || !strings.Contains(body, "1 GB") {
		t.Fatalf("a Workspace file said to be over 1 GB: %d %s", code, body)
	}
	if code, body := put("/v1/workspace/unsaid.bin", over, -1); code != 200 {
		t.Fatalf("a body of no stated length: %d %s", code, body)
	}
	ents, _ := os.ReadDir(n.srv.workspaceDir())
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".put-") {
			t.Fatalf("a staged body was left behind: %s", e.Name())
		}
	}
}

func TestWorkspaceUploadHoldsNoLockWhileItArrives(t *testing.T) {
	// The file lock holds every app's saves and every sync apply. An upload
	// takes it only to rename its file into place and version it.
	n := newTestNode(t)
	pr, pw := io.Pipe()
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		w := httptest.NewRecorder()
		n.srv.Handler().ServeHTTP(w, httptest.NewRequest("PUT", "/v1/workspace/slow.bin", pr))
		done <- w
	}()
	pw.Write([]byte("first half, "))

	locked := make(chan struct{})
	go n.eng.LockFiles(func() { close(locked) })
	select {
	case <-locked:
	case <-time.After(2 * time.Second):
		t.Fatal("the file lock is held while an upload's body arrives")
	}
	pw.Write([]byte("second half"))
	pw.Close()
	if w := <-done; w.Code != 200 {
		t.Fatalf("the upload: %d %s", w.Code, w.Body.String())
	}
	if b, _ := os.ReadFile(filepath.Join(n.srv.workspaceDir(), "slow.bin")); string(b) != "first half, second half" {
		t.Fatalf("slow.bin holds %q", b)
	}
}
