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

// appsRig is an apps folder with its record, and releases to bring into it.
type appsRig struct {
	t                             *testing.T
	root, apps, staging, manifest string
}

func newAppsRig(t *testing.T) *appsRig {
	root := t.TempDir()
	return &appsRig{t: t, root: root, apps: filepath.Join(root, "apps"),
		staging: filepath.Join(root, "release", "staging"), manifest: filepath.Join(root, "release", "apps.json")}
}

// release makes a release folder whose every bundle holds body.
func (r *appsRig) release(body string, names ...string) string {
	dir := filepath.Join(r.root, "release-"+body)
	for _, n := range names {
		bundle(r.t, dir, n, body)
	}
	return dir
}

func (r *appsRig) install(src string) (map[string]string, error) {
	changes, err := InstallApps(src, r.apps, r.staging, r.manifest)
	return actions(changes), err
}

func (r *appsRig) read(name string) string {
	b, _ := os.ReadFile(filepath.Join(r.apps, name, "index.html"))
	return string(b)
}

func (r *appsRig) recorded() AppsManifest {
	m, err := ReadAppsManifest(r.manifest)
	if err != nil {
		r.t.Fatal(err)
	}
	return m
}

func (r *appsRig) pending() bool {
	_, err := os.Stat(pendingFile(r.manifest))
	return err == nil
}

// failOn makes placing the named bundle fail (or, with die, panic the way
// a killed process stops: nothing after it runs) until the returned
// function is called.
func failOn(t *testing.T, name string, die bool) (restore func()) {
	t.Helper()
	real := placeTree
	placeTree = func(from, to, staging string) error {
		if filepath.Base(to) == name {
			if die {
				panic("killed while placing " + name)
			}
			return errors.New("disk full")
		}
		return real(from, to, staging)
	}
	restore = func() { placeTree = real }
	t.Cleanup(restore)
	return restore
}

// An update replaces a bundle only while it is still what was installed.
func TestInstallApps(t *testing.T) {
	r := newAppsRig(t)
	// the owner's own Paint is there before anything is installed
	bundle(t, r.apps, "Paint", "mine")

	v1 := r.release("one", "Notes", "Todo", "Tides", "Paint", "World Clock")
	os.WriteFile(filepath.Join(v1, "README.md"), []byte("not an app"), 0o644)
	os.MkdirAll(filepath.Join(v1, "docs"), 0o755) // nor is a folder without app.json

	got, err := r.install(v1)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"Notes", "Todo", "Tides", "World Clock"} {
		if got[n] != "installed" || r.read(n) != "one" {
			t.Errorf("%s: %s, %q", n, got[n], r.read(n))
		}
	}
	if got["Paint"] != "kept" || r.read("Paint") != "mine" {
		t.Errorf("an app that was already there: %s, %q", got["Paint"], r.read("Paint"))
	}
	m := r.recorded()
	if _, ok := m["Paint"]; ok {
		t.Error("the owner's Paint was written down as installed")
	}
	if _, err := os.Stat(filepath.Join(r.apps, "docs")); err == nil {
		t.Error("a folder that is no bundle was installed")
	}
	if r.pending() {
		t.Error("a finished install left its plan behind")
	}

	// then: Notes is edited, Tides removed, and the release moves on
	os.WriteFile(filepath.Join(r.apps, "Notes", "index.html"), []byte("edited"), 0o644)
	os.RemoveAll(filepath.Join(r.apps, "Tides"))
	v2 := r.release("two", "Notes", "Todo", "Tides", "Paint", "Weather")
	bundle(t, v2, "World Clock", "one") // unchanged in this release

	got, err = r.install(v2)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"Notes":       {"kept", "edited"},
		"Todo":        {"updated", "two"},
		"Tides":       {"kept", ""},
		"Paint":       {"kept", "mine"},
		"Weather":     {"installed", "two"},
		"World Clock": {"current", "one"},
	}
	for n, w := range want {
		if got[n] != w[0] || r.read(n) != w[1] {
			t.Errorf("%s: %s with %q, want %s with %q", n, got[n], r.read(n), w[0], w[1])
		}
	}
	if r.recorded()["Notes"] != m["Notes"] {
		t.Error("an edited app's record changed, so a later update would take it for untouched")
	}
	if left, _ := os.ReadDir(r.staging); len(left) != 0 {
		t.Errorf("staging was left with %d entries", len(left))
	}
	if none, err := ReadAppsManifest(filepath.Join(r.root, "nothing.json")); err != nil || len(none) != 0 {
		t.Fatalf("a missing manifest: %v, %v", none, err)
	}
}

// A run that fails on its second bundle has already placed the first. The
// next run must know that bundle for its own — not find it "edited", keep
// it, and hold it back from every later release.
func TestInstallAppsAgainAfterAFailure(t *testing.T) {
	r := newAppsRig(t)
	if _, err := r.install(r.release("one", "Notes", "Todo")); err != nil {
		t.Fatal(err)
	}
	v2 := r.release("two", "Notes", "Todo")

	restore := failOn(t, "Todo", false)
	if _, err := r.install(v2); err == nil {
		t.Fatal("the install went through a bundle that could not be placed")
	}
	if r.read("Notes") != "two" || r.read("Todo") != "one" {
		t.Fatalf("after the failure: Notes %q, Todo %q", r.read("Notes"), r.read("Todo"))
	}
	if !r.pending() {
		t.Fatal("the failed run left no plan for the next one")
	}

	restore()
	got, err := r.install(v2)
	if err != nil {
		t.Fatal(err)
	}
	if got["Notes"] != "current" || got["Todo"] != "updated" || r.read("Todo") != "two" {
		t.Errorf("the run after the failure: %v, Todo %q", got, r.read("Todo"))
	}
	if r.pending() {
		t.Error("the plan outlived the run that finished it")
	}
	// and both still follow the next release
	got, err = r.install(r.release("three", "Notes", "Todo"))
	if err != nil {
		t.Fatal(err)
	}
	if got["Notes"] != "updated" || got["Todo"] != "updated" || r.read("Notes") != "three" || r.read("Todo") != "three" {
		t.Errorf("the release after: %v, Notes %q, Todo %q", got, r.read("Notes"), r.read("Todo"))
	}
}

// The same with a bundle that is new in the release: it was placed, the
// run failed on the next, and no record names it. Standing there with
// exactly what the plan meant to give it, it is the installer's.
func TestInstallAppsNewBundleThenAFailure(t *testing.T) {
	r := newAppsRig(t)
	if _, err := r.install(r.release("one", "Todo")); err != nil {
		t.Fatal(err)
	}
	v2 := r.release("two", "Atlas", "Todo") // Atlas is new and comes first

	restore := failOn(t, "Todo", false)
	if _, err := r.install(v2); err == nil {
		t.Fatal("the failure did not surface")
	}
	if r.read("Atlas") != "two" {
		t.Fatalf("Atlas was not placed before the failure: %q", r.read("Atlas"))
	}
	restore()
	got, err := r.install(v2)
	if err != nil {
		t.Fatal(err)
	}
	if got["Atlas"] != "current" || got["Todo"] != "updated" {
		t.Errorf("the run after the failure: %v", got)
	}
	if _, ok := r.recorded()["Atlas"]; !ok {
		t.Fatal("the new bundle is on no record")
	}
	got, err = r.install(r.release("three", "Atlas", "Todo"))
	if err != nil {
		t.Fatal(err)
	}
	if got["Atlas"] != "updated" || r.read("Atlas") != "three" {
		t.Errorf("the new bundle did not follow the next release: %v, %q", got, r.read("Atlas"))
	}
}

// A process killed between placing a bundle and saving the manifest leaves
// only the plan. The next run may be a later release than the one that
// died, so "it is what this release would place" cannot be the test: the
// plan's own checksum is.
func TestInstallAppsAfterBeingKilled(t *testing.T) {
	r := newAppsRig(t)
	if _, err := r.install(r.release("one", "Notes", "Todo")); err != nil {
		t.Fatal(err)
	}
	before := r.recorded()

	restore := failOn(t, "Todo", true)
	func() {
		defer func() { recover() }()
		r.install(r.release("two", "Atlas", "Notes", "Todo"))
	}()
	restore()
	if r.read("Atlas") != "two" || r.read("Notes") != "two" || r.read("Todo") != "one" {
		t.Fatalf("at the kill: Atlas %q, Notes %q, Todo %q", r.read("Atlas"), r.read("Notes"), r.read("Todo"))
	}
	if m := r.recorded(); m["Notes"] != before["Notes"] || len(m) != len(before) {
		t.Fatal("the manifest was saved, so this is not the case the test is for")
	}

	got, err := r.install(r.release("three", "Atlas", "Notes", "Todo"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"Atlas", "Notes", "Todo"} {
		if got[n] != "updated" || r.read(n) != "three" {
			t.Errorf("%s after the kill: %s, %q", n, got[n], r.read(n))
		}
	}
	if r.pending() {
		t.Error("the plan was left behind")
	}
}

// A bundle the plan named, and that is not there, was being put in place
// when the run died — not removed by its owner.
func TestInstallAppsKilledInTheMiddleOfABundle(t *testing.T) {
	r := newAppsRig(t)
	if _, err := r.install(r.release("one", "Notes", "Todo")); err != nil {
		t.Fatal(err)
	}
	v2 := r.release("two", "Notes", "Todo")
	// the state a kill between placeTree's two renames leaves: the plan
	// written, Notes moved away into staging, nothing yet in its place
	sums := AppsManifest{}
	for _, n := range []string{"Notes", "Todo"} {
		sums[n], _ = TreeSum(filepath.Join(v2, n))
	}
	if err := sums.Write(pendingFile(r.manifest)); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(r.staging, "app-1"), 0o755)
	os.Rename(filepath.Join(r.apps, "Notes"), filepath.Join(r.staging, "app-1", "old"))

	got, err := r.install(v2)
	if err != nil {
		t.Fatal(err)
	}
	if got["Notes"] != "installed" || r.read("Notes") != "two" || got["Todo"] != "updated" {
		t.Errorf("after the kill: %v, Notes %q", got, r.read("Notes"))
	}
	if left, _ := os.ReadDir(r.staging); len(left) != 0 {
		t.Errorf("the dead run's scratch was left: %d entries", len(left))
	}

	// with no plan, a bundle that is gone was removed by its owner
	os.RemoveAll(filepath.Join(r.apps, "Todo"))
	got, err = r.install(r.release("three", "Notes", "Todo"))
	if err != nil {
		t.Fatal(err)
	}
	if got["Todo"] != "kept" || got["Notes"] != "updated" {
		t.Errorf("a removed bundle: %v", got)
	}
	if _, err := os.Stat(filepath.Join(r.apps, "Todo")); err == nil {
		t.Error("a bundle its owner removed came back")
	}
}

// The plan is a claim to check. An app its owner put there is not adopted
// because a plan names it: only one holding exactly what the plan meant
// to place is.
func TestInstallAppsPlanDoesNotClaimTheOwnersApps(t *testing.T) {
	r := newAppsRig(t)
	v1 := r.release("one", "Atlas", "Todo")
	// a first install that failed before it placed anything …
	restore := failOn(t, "Atlas", false)
	if _, err := r.install(v1); err == nil {
		t.Fatal("the failure did not surface")
	}
	restore()
	if !r.pending() || !AppsTracked(r.manifest) {
		t.Fatal("no plan was left")
	}
	// … after which the owner wrote an Atlas of their own
	bundle(t, r.apps, "Atlas", "mine")

	got, err := r.install(v1)
	if err != nil {
		t.Fatal(err)
	}
	if got["Atlas"] != "kept" || r.read("Atlas") != "mine" || got["Todo"] != "installed" {
		t.Errorf("the owner's app under a planned name: %v, %q", got, r.read("Atlas"))
	}
	if _, ok := r.recorded()["Atlas"]; ok {
		t.Error("the owner's app was written down as installed")
	}
}

// A first install that fails partway has no manifest at all. What it
// placed is still the installer's: the apps are tracked, a rerun finishes
// them, and an uninstall takes them away.
func TestFirstInstallThatFails(t *testing.T) {
	r := newAppsRig(t)
	v1 := r.release("one", "Notes", "Paint", "Todo")
	if AppsTracked(r.manifest) {
		t.Fatal("apps are tracked before any were installed")
	}
	// killed at Todo: Notes and Paint stand, and only the plan says whose they are
	restore := failOn(t, "Todo", true)
	func() {
		defer func() { recover() }()
		r.install(v1)
	}()
	restore()
	if _, err := os.Stat(r.manifest); err == nil {
		t.Fatal("a manifest was saved, so this is not the case the test is for")
	}
	if !AppsTracked(r.manifest) {
		t.Fatal("an install that began is not tracked, so no update or uninstall would finish it")
	}

	// the owner edits Paint; an uninstall now removes Notes and keeps Paint
	os.WriteFile(filepath.Join(r.apps, "Paint", "index.html"), []byte("edited"), 0o644)
	removed, kept, err := RemoveApps(r.apps, r.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != "Notes" || len(kept) != 0 {
		t.Errorf("removed %v, kept %v — Paint was edited, so it is no longer what the plan placed", removed, kept)
	}
	if r.read("Paint") != "edited" {
		t.Error("an edited app was removed")
	}

	// and a rerun, instead of the uninstall, finishes the install
	r = newAppsRig(t)
	v1 = r.release("one", "Notes", "Paint", "Todo")
	restore = failOn(t, "Todo", false)
	r.install(v1)
	restore()
	got, err := r.install(v1)
	if err != nil {
		t.Fatal(err)
	}
	if got["Notes"] != "current" || got["Paint"] != "current" || got["Todo"] != "installed" || len(r.recorded()) != 3 {
		t.Errorf("the rerun: %v, on record %v", got, r.recorded())
	}
}

// Should the plan be lost, a bundle that is already what this release
// would place is still taken for placed — tested before "edited", or the
// first run's own work would read as its owner's.
func TestInstallAppsHealsWithoutAPlan(t *testing.T) {
	r := newAppsRig(t)
	if _, err := r.install(r.release("one", "Notes", "Todo")); err != nil {
		t.Fatal(err)
	}
	v2 := r.release("two", "Notes", "Todo")
	restore := failOn(t, "Todo", true)
	func() {
		defer func() { recover() }()
		r.install(v2)
	}()
	restore()
	os.Remove(pendingFile(r.manifest))

	got, err := r.install(v2)
	if err != nil {
		t.Fatal(err)
	}
	if got["Notes"] != "current" || got["Todo"] != "updated" {
		t.Errorf("without the plan: %v", got)
	}
	if got, _ := r.install(r.release("three", "Notes", "Todo")); got["Notes"] != "updated" {
		t.Errorf("Notes was held back from the next release: %v", got)
	}
}
