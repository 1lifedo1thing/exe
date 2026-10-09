package server

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// App windows follow their app's code. Every app in /v1/apps carries a
// version — a fingerprint of the files its window runs — and while any desk
// listens on the app-data stream the daemon looks at the disk bundles every
// appCodeEvery and announces one whose files changed ({"app": "@apps",
// "path": name, "version": v}). The desk reloads a window that runs an older
// version once nobody is using it. A window left open on a sleeping iPad ran
// the Notes from before colours for hours and wrote every note back without
// its colour (2026-10-09); a window that reloads when its app does cannot.
const appCodeEvery = 2 * time.Second

// appCodeVersion fingerprints a disk bundle: every file's path, size and
// modification time (dot-files and dot-folders aside, at most 5000 files).
func appCodeVersion(dir string) string {
	h := sha1.New()
	n := 0
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p != dir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if n++; n > 5000 {
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		fmt.Fprintf(h, "%s\x00%d\x00%d\n", filepath.ToSlash(rel), info.Size(), info.ModTime().UnixNano())
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// sysAppVersions caches the fingerprint of each app built into the binary:
// its files never change while the daemon runs.
var sysAppVersions sync.Map

// sysAppVersion fingerprints an embedded app by its files' contents.
func sysAppVersion(name string) string {
	if v, ok := sysAppVersions.Load(name); ok {
		return v.(string)
	}
	h := sha1.New()
	root := "sysapps/" + name
	fs.WalkDir(sysAppsFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(sysAppsFS, p)
		if err != nil {
			return nil
		}
		fmt.Fprintf(h, "%s\x00%d\n", p, len(b))
		h.Write(b)
		return nil
	})
	v := hex.EncodeToString(h.Sum(nil))[:12]
	sysAppVersions.Store(name, v)
	return v
}

// watchAppCode starts the look at the disk bundles if it is not running; it
// stops by itself once no desk is listening, and the next one starts it.
func (s *Server) watchAppCode() {
	s.appEv.mu.Lock()
	if s.appEv.watching {
		s.appEv.mu.Unlock()
		return
	}
	s.appEv.watching = true
	s.appEv.mu.Unlock()
	go func() {
		seen := map[string]string{}
		for _, m := range s.listApps() {
			seen[m.Name] = m.Version
		}
		t := time.NewTicker(appCodeEvery)
		defer t.Stop()
		for range t.C {
			s.appEv.mu.Lock()
			if len(s.appEv.subs) == 0 {
				s.appEv.watching = false
				s.appEv.mu.Unlock()
				return
			}
			s.appEv.mu.Unlock()
			for _, m := range s.listApps() {
				if old, ok := seen[m.Name]; ok && old != m.Version {
					s.broadcastAppEvent(map[string]any{"app": "@apps", "path": m.Name, "version": m.Version})
				}
				seen[m.Name] = m.Version
			}
		}
	}()
}

// broadcastAppEvent sends one event to every desk on the app-data stream.
func (s *Server) broadcastAppEvent(ev map[string]any) {
	b, _ := json.Marshal(ev)
	s.appEv.mu.Lock()
	defer s.appEv.mu.Unlock()
	for ch := range s.appEv.subs {
		select {
		case ch <- b:
		default:
		}
	}
}
