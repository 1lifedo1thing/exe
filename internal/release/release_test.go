package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	for v, want := range map[string]bool{
		"2026.10.09":     true,
		"2026.10.09.2":   true,
		"2026.1.1":       true,
		"":               false,
		"releases":       false,
		"v2026.10.09":    false,
		"2026.10":        false,
		"2026.10.09.2.1": false,
		"1.2.3":          false, // not a date
		"2026.10.+9":     false,
		"2026..09":       false,
	} {
		if got := Valid(v); got != want {
			t.Errorf("Valid(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestCompare(t *testing.T) {
	order := []string{"2025.12.31", "2026.01.02", "2026.1.10", "2026.10.09", "2026.10.09.2", "2026.10.09.10", "2026.10.10"}
	for i, a := range order {
		for j, b := range order {
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := Compare(a, b); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", a, b, got, want)
			}
		}
	}
	if Compare("2026.10.09", "2026.10.9") != 0 {
		t.Error("a leading zero changed the order")
	}
}

// mirror serves a release address the way GitHub lays one out.
func mirror(t *testing.T, latest string, files map[string][]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		if latest == "" {
			http.Redirect(w, r, "/releases", http.StatusFound)
			return
		}
		http.Redirect(w, r, "/releases/tag/"+latest, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[strings.TrimPrefix(r.URL.Path, "/releases/download/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func sumLine(name string, b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]) + "  " + name + "\n"
}

func TestLatest(t *testing.T) {
	srv := mirror(t, "2026.10.09", nil)
	c := &Client{Base: srv.URL + "/releases", HTTP: srv.Client()}
	got, err := c.Latest(context.Background())
	if err != nil || got != "2026.10.09" {
		t.Fatalf("Latest = %q, %v", got, err)
	}
}

func TestLatestWithNoRelease(t *testing.T) {
	srv := mirror(t, "", nil)
	c := &Client{Base: srv.URL + "/releases", HTTP: srv.Client()}
	if _, err := c.Latest(context.Background()); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("Latest with nothing published: %v", err)
	}
}

func TestLatestRefusesATagThatIsNoVersion(t *testing.T) {
	srv := mirror(t, "nightly", nil)
	c := &Client{Base: srv.URL + "/releases", HTTP: srv.Client()}
	if got, err := c.Latest(context.Background()); err == nil {
		t.Fatalf("Latest took %q for a version", got)
	}
}

func TestFetchVerifies(t *testing.T) {
	good := []byte("the binary")
	files := map[string][]byte{
		"2026.10.09/exe-linux-arm64.tar.gz": good,
		"2026.10.09/SHA256SUMS":             []byte(sumLine("exe-linux-arm64.tar.gz", good)),
		"2026.10.10/exe-linux-arm64.tar.gz": []byte("swapped on the way"),
		"2026.10.10/SHA256SUMS":             []byte(sumLine("exe-linux-arm64.tar.gz", good)),
		"2026.10.11/exe-linux-arm64.tar.gz": good,
		"2026.10.11/SHA256SUMS":             []byte(sumLine("something-else", good)),
	}
	srv := mirror(t, "2026.10.11", files)
	c := &Client{Base: srv.URL + "/releases", HTTP: srv.Client()}
	ctx := context.Background()

	dir := t.TempDir()
	if err := c.Fetch(ctx, "2026.10.09", dir, "exe-linux-arm64.tar.gz"); err != nil {
		t.Fatalf("a good release: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "exe-linux-arm64.tar.gz")); !bytes.Equal(b, good) {
		t.Fatalf("fetched %q", b)
	}

	dir = t.TempDir()
	err := c.Fetch(ctx, "2026.10.10", dir, "exe-linux-arm64.tar.gz")
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("a file that does not match: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "exe-linux-arm64.tar.gz")); !os.IsNotExist(serr) {
		t.Fatal("the file that failed its checksum was left on disk")
	}

	if err := c.Fetch(ctx, "2026.10.11", t.TempDir(), "exe-linux-arm64.tar.gz"); err == nil {
		t.Fatal("a file the checksums do not list was accepted")
	}
	if err := c.Fetch(ctx, "2026.10.12", t.TempDir(), "exe-linux-arm64.tar.gz"); err == nil {
		t.Fatal("a release that does not exist was fetched")
	}
	if err := c.Fetch(ctx, "../latest", t.TempDir(), "x"); err == nil {
		t.Fatal("a tag that is no version was fetched")
	}
}

type entry struct {
	name string
	body string
	kind byte
	mode int64
}

func tarball(t *testing.T, entries ...entry) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.kind, Mode: e.mode, Size: int64(len(e.body))}
		if e.kind == tar.TypeSymlink {
			hdr.Linkname, hdr.Size = e.body, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.kind == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	p := filepath.Join(t.TempDir(), "x.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtract(t *testing.T) {
	dir := t.TempDir()
	tgz := tarball(t,
		entry{"exe", "binary", tar.TypeReg, 0o755},
		entry{"Notes/", "", tar.TypeDir, 0o755},
		entry{"Notes/app.json", "{}", tar.TypeReg, 0o644},
	)
	if err := Extract(tgz, dir); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "exe"))
	if err != nil || st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("exe: %v, mode %v — it has to stay runnable", err, st.Mode())
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "Notes", "app.json")); string(b) != "{}" {
		t.Fatalf("Notes/app.json = %q", b)
	}
}

func TestExtractStaysInside(t *testing.T) {
	for _, e := range []entry{
		{"../outside", "x", tar.TypeReg, 0o644},
		{"/etc/outside", "x", tar.TypeReg, 0o644},
		{"a/../../outside", "x", tar.TypeReg, 0o644},
		{"link", "/etc/passwd", tar.TypeSymlink, 0o777},
	} {
		parent := t.TempDir()
		dir := filepath.Join(parent, "in")
		os.Mkdir(dir, 0o755)
		if err := Extract(tarball(t, e), dir); err == nil {
			t.Errorf("an archive holding %q was unpacked", e.name)
		}
		if _, err := os.Stat(filepath.Join(parent, "outside")); err == nil {
			t.Errorf("%q wrote outside the folder", e.name)
		}
	}
}

func bundle(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"title":"`+name+`"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "index.html"), []byte(body), 0o644)
}

func actions(changes []AppChange) map[string]string {
	m := map[string]string{}
	for _, c := range changes {
		m[c.Name] = c.Action
	}
	return m
}

// An update replaces a bundle only while it is still what was installed.
func TestInstallApps(t *testing.T) {
	root := t.TempDir()
	apps, staging := filepath.Join(root, "apps"), filepath.Join(root, "staging")
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(apps, name, "index.html"))
		return string(b)
	}

	// the owner's own Paint is there before anything is installed
	bundle(t, apps, "Paint", "mine")

	v1 := filepath.Join(root, "v1")
	for _, n := range []string{"Notes", "Todo", "Tides", "Paint", "World Clock"} {
		bundle(t, v1, n, "one")
	}
	os.WriteFile(filepath.Join(v1, "README.md"), []byte("not an app"), 0o644)
	os.MkdirAll(filepath.Join(v1, "docs"), 0o755) // nor is a folder without app.json

	m, changes, err := InstallApps(v1, apps, staging, AppsManifest{})
	if err != nil {
		t.Fatal(err)
	}
	got := actions(changes)
	for _, n := range []string{"Notes", "Todo", "Tides", "World Clock"} {
		if got[n] != "installed" || read(n) != "one" {
			t.Errorf("%s: %s, %q", n, got[n], read(n))
		}
	}
	if got["Paint"] != "kept" || read("Paint") != "mine" {
		t.Errorf("an app that was already there: %s, %q", got["Paint"], read("Paint"))
	}
	if _, ok := m["Paint"]; ok {
		t.Error("the owner's Paint was written down as installed")
	}
	if _, err := os.Stat(filepath.Join(apps, "docs")); err == nil {
		t.Error("a folder that is no bundle was installed")
	}

	// then: Notes is edited, Tides removed, and the release moves on
	os.WriteFile(filepath.Join(apps, "Notes", "index.html"), []byte("edited"), 0o644)
	os.RemoveAll(filepath.Join(apps, "Tides"))
	v2 := filepath.Join(root, "v2")
	for _, n := range []string{"Notes", "Todo", "Tides", "Paint", "Weather"} {
		bundle(t, v2, n, "two")
	}
	bundle(t, v2, "World Clock", "one") // unchanged in this release

	m2, changes, err := InstallApps(v2, apps, staging, m)
	if err != nil {
		t.Fatal(err)
	}
	got = actions(changes)
	want := map[string][2]string{
		"Notes":       {"kept", "edited"},
		"Todo":        {"updated", "two"},
		"Tides":       {"kept", ""},
		"Paint":       {"kept", "mine"},
		"Weather":     {"installed", "two"},
		"World Clock": {"current", "one"},
	}
	for n, w := range want {
		if got[n] != w[0] || read(n) != w[1] {
			t.Errorf("%s: %s with %q, want %s with %q", n, got[n], read(n), w[0], w[1])
		}
	}
	if m2["Notes"] != m["Notes"] {
		t.Error("an edited app's record changed, so a later update would take it for untouched")
	}
	if left, _ := os.ReadDir(staging); len(left) != 0 {
		t.Errorf("staging was left with %d entries", len(left))
	}

	// the record survives the disk
	file := filepath.Join(root, "release", "apps.json")
	if err := m2.Write(file); err != nil {
		t.Fatal(err)
	}
	back, err := ReadAppsManifest(file)
	if err != nil || len(back) != len(m2) || back["Todo"] != m2["Todo"] {
		t.Fatalf("read back %v, %v", back, err)
	}
	if none, err := ReadAppsManifest(filepath.Join(root, "nothing.json")); err != nil || len(none) != 0 {
		t.Fatalf("a missing manifest: %v, %v", none, err)
	}
}
