package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"exe/internal/config"
	"exe/internal/release"
)

// extFixture is a daemon in a scratch folder with nothing beside it: the
// configuration is kept in memory (never the real config.json), nothing
// is looked for outside the fixture, and a release is whatever the test
// publishes.
type extFixture struct {
	t       *testing.T
	s       *Server
	h       http.Handler
	dir     string
	saves   int
	running map[string]bool // daemons that answer, by address
	fetched int
}

func newExtFixture(t *testing.T, version string) *extFixture {
	t.Helper()
	f := &extFixture{t: t, dir: t.TempDir(), running: map[string]bool{}}
	f.s = New(&config.Config{}, nil, nil, "", filepath.Join(f.dir, "state"))
	e := &f.s.ext
	e.version = func() string { return version }
	e.store = func(nc *config.Config) error { f.saves++; f.s.cfg.Store(nc); return nil }
	e.near = func() []string { return []string{filepath.Join(f.dir, "src")} }
	e.probe = func(url string) bool { return f.running[url] }
	f.h = f.s.Handler()
	return f
}

// bundle makes an app's folder.
func bundle(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	for file, body := range map[string]string{"app.json": `{"title":"` + name + `"}`, "index.html": "<p>" + name} {
		if err := os.WriteFile(filepath.Join(dir, name, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *extFixture) get() map[string]extraInfo {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/extras", nil))
	if rec.Code != 200 {
		f.t.Fatalf("GET /v1/extras: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Extras []extraInfo `json:"extras"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		f.t.Fatal(err)
	}
	m := map[string]extraInfo{}
	for _, x := range out.Extras {
		m[x.Name] = x
	}
	if len(m) != 4 {
		f.t.Fatalf("extras: %v", out.Extras)
	}
	return m
}

func (f *extFixture) post(name, body string) (int, extraInfo, string) {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/extras/"+name, strings.NewReader(body)))
	var info extraInfo
	var fail struct {
		Error string `json:"error"`
	}
	json.Unmarshal(rec.Body.Bytes(), &info)
	json.Unmarshal(rec.Body.Bytes(), &fail)
	return rec.Code, info, fail.Error
}

func (f *extFixture) apps() []string {
	var names []string
	for _, m := range f.s.listApps() {
		if m.SystemIcon == "" { // the built-in ones are not the test's
			names = append(names, m.Name)
		}
	}
	return names
}

// A program beside exe: missing until its folder is named, then on, off
// and on again without being asked where it is.
func TestExtrasAFolder(t *testing.T) {
	f := newExtFixture(t, "")
	all := f.get()
	for name, x := range all {
		if x.State != "missing" || x.Dir != "" || len(x.Have) != 0 {
			t.Fatalf("%s on an empty machine: %+v", name, x)
		}
	}
	if all["planet"].Service != "http://127.0.0.1:7799" || all["apps"].Service != "" || all["planet"].Running != nil {
		t.Fatalf("services: %+v", all)
	}
	if code, _, why := f.post("planet", `{"on":true}`); code != http.StatusConflict || !strings.Contains(why, "exe-planet") {
		t.Fatalf("turning on what is not here: %d %s", code, why)
	}
	if code, _, _ := f.post("nosuch", `{"on":true}`); code != http.StatusNotFound {
		t.Fatalf("an extra nobody knows: %d", code)
	}

	// a checkout, somewhere exe would not look
	root := filepath.Join(f.dir, "elsewhere", "exe-planet")
	bundle(t, filepath.Join(root, "apps"), "Planet")
	bundle(t, filepath.Join(root, "apps"), "Writer")
	if code, _, why := f.post("planet", `{"check":true,"dir":"`+filepath.Join(f.dir, "nowhere")+`"}`); code != http.StatusBadRequest ||
		!strings.Contains(why, "exe-planet is not in that folder") {
		t.Fatalf("a folder without it: %d %s", code, why)
	}
	if code, _, _ := f.post("planet", `{"check":true,"dir":"relative/path"}`); code != http.StatusBadRequest {
		t.Fatalf("a relative folder: %d", code)
	}
	// the checkout's own folder is as good as its apps folder; a check
	// changes nothing, and says whether the copy's daemon runs
	code, x, _ := f.post("planet", `{"check":true,"dir":"`+root+`"}`)
	if code != 200 || x.State != "off" || x.From != "folder" || x.Dir != filepath.Join(root, "apps") || x.Running == nil || *x.Running {
		t.Fatalf("check: %d %+v", code, x)
	}
	if f.saves != 0 || len(f.s.Config().AppsDirs) != 0 || f.get()["planet"].State != "missing" {
		t.Fatal("a check changed something")
	}

	f.running["http://127.0.0.1:7799/v1/status"] = true
	if _, x, _ = f.post("planet", `{"check":true,"dir":"`+root+`"}`); x.Running == nil || !*x.Running {
		t.Fatalf("a found copy's daemon runs, and the check did not say so: %+v", x)
	}
	code, x, _ = f.post("planet", `{"on":true,"dir":"`+root+`"}`)
	if code != 200 || x.State != "on" || x.From != "folder" || !reflect.DeepEqual(x.Have, []string{"Planet", "Writer"}) ||
		x.Running == nil || !*x.Running {
		t.Fatalf("on: %d %+v", code, x)
	}
	cfg := f.s.Config()
	if !reflect.DeepEqual(cfg.AppsDirs, []string{filepath.Join(root, "apps")}) || cfg.Services["planet"] != "http://127.0.0.1:7799" {
		t.Fatalf("the configuration: %v %v", cfg.AppsDirs, cfg.Services)
	}
	if got := f.apps(); !reflect.DeepEqual(got, []string{"Planet", "Writer"}) && !reflect.DeepEqual(got, []string{"Writer", "Planet"}) {
		t.Fatalf("the desk's apps: %v", got)
	}
	// on again is nothing
	saves := f.saves
	if code, x, _ = f.post("planet", `{"on":true}`); code != 200 || x.State != "on" || f.saves != saves {
		t.Fatalf("on twice: %d %+v, %d saves", code, x, f.saves-saves)
	}

	// a service at another address is kept through off and on
	nc := *f.s.Config()
	nc.Services = map[string]string{"planet": "http://127.0.0.1:7801", "other": "http://127.0.0.1:9"}
	nc.AppsDirs = append([]string{"/somewhere/else"}, nc.AppsDirs...)
	f.s.cfg.Store(&nc)
	code, x, _ = f.post("planet", `{"on":false}`)
	if code != 200 || x.State != "off" || x.Dir != filepath.Join(root, "apps") || x.Service != "http://127.0.0.1:7801" || len(x.Have) != 0 || len(x.Kept) != 0 {
		t.Fatalf("off: %d %+v", code, x)
	}
	cfg = f.s.Config()
	if !reflect.DeepEqual(cfg.AppsDirs, []string{"/somewhere/else"}) || !reflect.DeepEqual(cfg.Services, map[string]string{"other": "http://127.0.0.1:9"}) {
		t.Fatalf("off left: %v %v", cfg.AppsDirs, cfg.Services)
	}
	if len(f.apps()) != 0 {
		t.Fatalf("apps after off: %v", f.apps())
	}
	if _, err := os.Stat(filepath.Join(root, "apps", "Planet", "app.json")); err != nil {
		t.Fatal("off touched the checkout")
	}
	// and on needs no folder now
	code, x, _ = f.post("planet", `{"on":true}`)
	if code != 200 || x.State != "on" || f.s.Config().Services["planet"] != "http://127.0.0.1:7801" {
		t.Fatalf("on from memory: %d %+v %v", code, x, f.s.Config().Services)
	}
	if got := f.s.Config().AppsDirs; !reflect.DeepEqual(got, []string{"/somewhere/else", filepath.Join(root, "apps")}) {
		t.Fatalf("apps_dirs: %v", got)
	}
}

// A checkout by its own name beside what exe knows is found unasked, and
// apps in a folder with no daemon are an extra too.
func TestExtrasFoundNearby(t *testing.T) {
	f := newExtFixture(t, "")
	src := filepath.Join(f.dir, "src")
	bundle(t, filepath.Join(src, "exe-easel", "apps"), "easel")
	bundle(t, filepath.Join(src, "exe-apps"), "Notes")
	bundle(t, filepath.Join(src, "exe-apps"), "Todo")
	all := f.get()
	if x := all["easel"]; x.State != "off" || x.Dir != filepath.Join(src, "exe-easel", "apps") || x.Running == nil || *x.Running {
		t.Fatalf("easel: %+v", x)
	}
	if x := all["apps"]; x.State != "off" || x.From != "folder" || x.Dir != filepath.Join(src, "exe-apps") {
		t.Fatalf("apps: %+v", x)
	}
	if all["planet"].State != "missing" {
		t.Fatalf("planet: %+v", all["planet"])
	}
	code, x, _ := f.post("apps", `{"on":true}`)
	if code != 200 || x.State != "on" || !reflect.DeepEqual(x.Have, []string{"Notes", "Todo"}) || len(f.s.Config().Services) != 0 {
		t.Fatalf("apps on: %d %+v", code, x)
	}
	// its apps named but not its daemon: that is not on, and on mends it
	nc := *f.s.Config()
	nc.AppsDirs = append(nc.AppsDirs, filepath.Join(src, "exe-easel", "apps"))
	f.s.cfg.Store(&nc)
	if x := f.get()["easel"]; x.State != "off" || len(x.Have) != 1 {
		t.Fatalf("easel half named: %+v", x)
	}
	if code, x, _ = f.post("easel", `{"on":true}`); code != 200 || x.State != "on" || f.s.Config().Services["easel"] != "http://127.0.0.1:7794" || len(f.s.Config().AppsDirs) != 2 {
		t.Fatalf("easel on: %d %+v %v", code, x, f.s.Config())
	}
}

// Projects is the fourth: an app whose folder has a lowercase name, and a
// daemon asked after at its own address and path.
func TestExtrasProjects(t *testing.T) {
	f := newExtFixture(t, "")
	apps := filepath.Join(f.dir, "src", "exe-projects", "apps")
	bundle(t, apps, "projects")
	f.running["http://127.0.0.1:7789/v1/status"] = true
	x := f.get()["projects"]
	if x.State != "off" || x.From != "folder" || x.Dir != apps || x.Service != "http://127.0.0.1:7789" || x.Running == nil || !*x.Running {
		t.Fatalf("a checkout nearby, its daemon running: %+v", x)
	}
	code, x, why := f.post("projects", `{"on":true}`)
	if code != 200 || x.State != "on" || x.From != "folder" || !reflect.DeepEqual(x.Have, []string{"projects"}) {
		t.Fatalf("on: %d %+v %s", code, x, why)
	}
	cfg := f.s.Config()
	if !reflect.DeepEqual(cfg.AppsDirs, []string{apps}) || !reflect.DeepEqual(cfg.Services, map[string]string{"projects": "http://127.0.0.1:7789"}) {
		t.Fatalf("the configuration: %v %v", cfg.AppsDirs, cfg.Services)
	}
	if got := f.apps(); !reflect.DeepEqual(got, []string{"projects"}) {
		t.Fatalf("the desk's apps: %v", got)
	}
	if code, x, _ = f.post("projects", `{"on":false}`); code != 200 || x.State != "off" || x.Dir != apps || len(f.s.Config().AppsDirs) != 0 || len(f.s.Config().Services) != 0 {
		t.Fatalf("off: %d %+v %v", code, x, f.s.Config())
	}
}

// A folder where someone keeps apps of their own beside a copy of Notes is
// theirs: it is not the extra's folder to name, and off does not take its
// name away.
func TestExtrasLeaveAFolderOfSomeonesOwn(t *testing.T) {
	f := newExtFixture(t, "")
	mine := filepath.Join(f.dir, "mine")
	bundle(t, mine, "Notes")
	bundle(t, mine, "Ledger")
	if code, _, why := f.post("apps", `{"check":true,"dir":"`+mine+`"}`); code != http.StatusBadRequest {
		t.Fatalf("a mixed folder was taken for exe-apps: %d %s", code, why)
	}
	nc := config.Config{AppsDirs: []string{mine}}
	f.s.cfg.Store(&nc)
	x := f.get()["apps"]
	if x.State != "on" || x.From != "apps" || x.Dir != mine || !reflect.DeepEqual(x.Have, []string{"Notes"}) {
		t.Fatalf("a Notes of someone's own: %+v", x)
	}
	code, x, _ := f.post("apps", `{"on":false}`)
	if code != 200 || x.State != "on" || !reflect.DeepEqual(x.Kept, []string{"Notes"}) || f.saves != 0 {
		t.Fatalf("off: %d %+v, %d saves", code, x, f.saves)
	}
	if got := f.s.Config().AppsDirs; !reflect.DeepEqual(got, []string{mine}) {
		t.Fatalf("off took the folder's name away: %v", got)
	}
}

// A folder under the home folder is told with "~", and taken back so.
func TestExtrasTellHomeAsTilde(t *testing.T) {
	f := newExtFixture(t, "")
	home := filepath.Join(f.dir, "home")
	t.Setenv("HOME", home)
	bundle(t, filepath.Join(home, "src", "exe-easel", "apps"), "easel")
	code, x, why := f.post("easel", `{"check":true,"dir":"~/src/exe-easel"}`)
	if code != 200 || x.Dir != "~/src/exe-easel/apps" {
		t.Fatalf("check: %d %+v %s", code, x, why)
	}
	if code, x, _ = f.post("easel", `{"on":true,"dir":"`+x.Dir+`"}`); code != 200 || x.State != "on" || x.Dir != "~/src/exe-easel/apps" {
		t.Fatalf("on: %d %+v", code, x)
	}
	// the configuration keeps the whole path, which every reader of it takes
	if got := f.s.Config().AppsDirs; !reflect.DeepEqual(got, []string{filepath.Join(home, "src", "exe-easel", "apps")}) {
		t.Fatalf("apps_dirs: %v", got)
	}
	if x = f.get()["easel"]; x.Dir != "~/src/exe-easel/apps" {
		t.Fatalf("told: %+v", x)
	}
	// off and on again, by the folder it remembers
	if code, x, _ = f.post("easel", `{"on":false}`); code != 200 || x.State != "off" || x.Dir != "~/src/exe-easel/apps" {
		t.Fatalf("off: %d %+v", code, x)
	}
	if code, x, _ = f.post("easel", `{"on":true}`); code != 200 || x.State != "on" {
		t.Fatalf("on again: %d %+v", code, x)
	}
}

// publish serves a release whose apps tarball holds the named bundles.
func (f *extFixture) publish(version string, names ...string) {
	f.t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		for file, body := range map[string]string{"app.json": `{"title":"` + name + `"}`, "index.html": "<p>" + name + " " + version} {
			tw.WriteHeader(&tar.Header{Name: name + "/" + file, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
			tw.Write([]byte(body))
		}
	}
	tw.Close()
	gz.Close()
	sum := sha256.Sum256(buf.Bytes())
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/download/"+version+"/"+release.AppsAsset, func(w http.ResponseWriter, r *http.Request) {
		f.fetched++
		w.Write(buf.Bytes())
	})
	mux.HandleFunc("/releases/download/"+version+"/"+release.SumsAsset, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + release.AppsAsset + "\n"))
	})
	srv := httptest.NewServer(mux)
	f.t.Cleanup(srv.Close)
	f.s.ext.client = &release.Client{Base: srv.URL + "/releases", HTTP: srv.Client()}
}

// The extra apps of a released exe: fetched from the release that runs,
// placed as the installer places them, removed unless edited.
func TestExtrasAppsFromTheRelease(t *testing.T) {
	f := newExtFixture(t, "2026.10.11")
	f.publish("2026.10.11", "Notes", "Paint", "Tides", "Todo", "Weather", "World Clock")
	x := f.get()["apps"]
	if x.State != "off" || x.From != "release" || x.Release != "2026.10.11" || x.Dir != "" {
		t.Fatalf("before: %+v", x)
	}
	code, x, why := f.post("apps", `{"on":true}`)
	if code != 200 || x.State != "on" || x.From != "release" || len(x.Have) != 6 || f.fetched != 1 {
		t.Fatalf("on: %d %+v %s", code, x, why)
	}
	apps := filepath.Join(f.dir, "state", "apps")
	if !isBundle(apps, "World Clock") || !release.AppsTracked(f.s.appsManifestFile()) {
		t.Fatal("the bundles or their record are not in place")
	}
	if f.saves != 0 {
		t.Fatal("the release's road changed the configuration")
	}
	if len(f.apps()) != 6 {
		t.Fatalf("the desk's apps: %v", f.apps())
	}

	// Notes is edited: it is its owner's now, and stays
	if err := os.WriteFile(filepath.Join(apps, "Notes", "index.html"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, x, why = f.post("apps", `{"on":false}`)
	if code != 200 || !reflect.DeepEqual(x.Kept, []string{"Notes"}) || x.State != "on" || x.From != "apps" || !reflect.DeepEqual(x.Have, []string{"Notes"}) {
		t.Fatalf("off: %d %+v %s", code, x, why)
	}
	if isBundle(apps, "Paint") || !isBundle(apps, "Notes") || release.AppsTracked(f.s.appsManifestFile()) {
		t.Fatal("off: the wrong bundles are left, or the record is")
	}
	// with it gone too the apps are off, and can be fetched again
	os.RemoveAll(filepath.Join(apps, "Notes"))
	if x = f.get()["apps"]; x.State != "off" || x.From != "release" {
		t.Fatalf("after: %+v", x)
	}

	// a release that is not there says so and places nothing
	g := newExtFixture(t, "2026.10.12")
	g.publish("2026.10.11", "Notes")
	if code, _, why := g.post("apps", `{"on":true}`); code != http.StatusInternalServerError || !strings.Contains(why, "fetching the apps of exe 2026.10.12") {
		t.Fatalf("a missing release: %d %s", code, why)
	}
	if g.get()["apps"].State != "off" {
		t.Fatal("a failed fetch left something")
	}

	// an exe from a checkout has no release to take them from
	src := newExtFixture(t, "")
	if x := src.get()["apps"]; x.State != "missing" {
		t.Fatalf("a source build: %+v", x)
	}
}

// Every bundle an extra names is a folder its checkout really has, so the
// list here cannot drift from the four repositories unnoticed. Skipped
// where the checkouts are not beside this one.
func TestExtrasBundlesAreTheCheckouts(t *testing.T) {
	for _, def := range extraDefs {
		root := filepath.Join("..", "..", "..", def.Repo)
		if _, err := os.Stat(root); err != nil {
			t.Logf("%s is not beside this checkout: skipped", def.Repo)
			continue
		}
		dir := extraLocate(mustAbs(t, root), &def)
		if dir == "" {
			t.Errorf("%s holds none of %v", def.Repo, def.Bundles)
			continue
		}
		if have := extraHolds(dir, &def); !reflect.DeepEqual(have, def.Bundles) {
			t.Errorf("%s: its apps folder has %v of %v", def.Repo, have, def.Bundles)
		}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() && isBundle(dir, e.Name()) && !strings.Contains(strings.Join(def.Bundles, "\x00"), e.Name()) {
				t.Errorf("%s has an app %q that extraDefs does not name", def.Repo, e.Name())
			}
		}
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}
