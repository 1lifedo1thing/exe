package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"exe/internal/config"
	"exe/internal/release"
	"exe/internal/vmm"
)

// updVMs is a VM list that can be changed under the server.
type updVMs struct {
	vmm.Manager
	mu  sync.Mutex
	vms map[string]string
}

func (v *updVMs) List(context.Context) ([]*vmm.Info, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []*vmm.Info
	for name, st := range v.vms {
		out = append(out, &vmm.Info{Name: name, State: st})
	}
	return out, nil
}

// updFixture is a server whose Software Update reads a release address of
// the test's, runs an installer of the test's, and reports the version the
// test says it is.
type updFixture struct {
	s        *Server
	h        http.Handler
	dir      string
	latest   atomic.Value // the newest release's version
	asked    atomic.Int32 // times the address was asked for it
	installs atomic.Int32
	restarts chan []string
	vms      *updVMs
}

func newUpdFixture(t *testing.T, version, latest string) *updFixture {
	t.Helper()
	f := &updFixture{dir: t.TempDir(), restarts: make(chan []string, 4), vms: &updVMs{vms: map[string]string{}}}
	f.latest.Store(latest)
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		f.asked.Add(1)
		v := f.latest.Load().(string)
		if v == "" {
			http.Redirect(w, r, "/releases", http.StatusFound)
			return
		}
		http.Redirect(w, r, "/releases/tag/"+v, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/2026.10.11/NOTES.md") {
			w.Write([]byte("Since 2026.10.10:\n\n- Software Update\n"))
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// the binary "in place": a file whose words are its version line
	exe := filepath.Join(f.dir, "exe")
	writeExe(t, exe, version)
	f.s = New(&config.Config{}, f.vms, nil, "", filepath.Join(f.dir, "state"))
	u := &f.s.upd
	u.version = func() string { return version }
	u.client = &release.Client{Base: srv.URL + "/releases", HTTP: srv.Client()}
	u.exe = exe
	u.probe = func(bin string) (string, error) {
		b, err := os.ReadFile(bin)
		return strings.TrimSpace(string(b)), err
	}
	// the installer: what `exe update` prints, and the new file in place
	u.install = func(ctx context.Context, exe string, line func(string)) error {
		f.installs.Add(1)
		v := f.latest.Load().(string)
		line("Downloading exe " + v + "…")
		time.Sleep(60 * time.Millisecond) // long enough to be joined
		writeExe(t, exe, v)
		for _, l := range []string{"Installed exe " + v + " at ~/.local/bin/exe", "",
			"This release has a new network helper. VMs use the old one until you run:", "  sudo install …", "",
			"The daemon keeps running " + version + " until you run: systemctl --user restart exe"} {
			line(l)
		}
		return nil
	}
	u.restart = func(running []string) { f.restarts <- running }
	u.wait = time.Hour // a restart "happens": the job never comes back from it
	f.h = f.s.Handler()
	return f
}

// writeExe puts a file in place the way an update does: a new file, renamed
// over the old one.
func writeExe(t *testing.T, path, version string) {
	t.Helper()
	line := "exe, built from source (linux/arm64)"
	if version != "" {
		line = "exe " + version + " (linux/arm64)"
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(line+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

type updAnswer struct {
	Version   string `json:"version"`
	Source    bool   `json:"source"`
	Latest    string `json:"latest"`
	Available bool   `json:"available"`
	Installed string `json:"installed"`
	Notes     string `json:"notes"`
	Error     string `json:"error"`
	Checked   string `json:"checked"`
	Running   []string
	Job       *updateJob `json:"job"`
	Last      *struct {
		From, To, Notes string
		OK, Recent      bool
		VMs             []map[string]string `json:"vms"`
	} `json:"last"`
}

func (f *updFixture) call(t *testing.T, method, path, body string) (int, updAnswer) {
	t.Helper()
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, httptest.NewRequest(method, path, strings.NewReader(body)))
	var a updAnswer
	json.Unmarshal(w.Body.Bytes(), &a)
	return w.Code, a
}

func (f *updFixture) waitFor(t *testing.T, what string, ok func(updAnswer) bool) updAnswer {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); ; {
		_, a := f.call(t, "GET", "/v1/update", "")
		if ok(a) {
			return a
		}
		if time.Now().After(end) {
			t.Fatalf("waited for %s; the daemon says %+v, job %+v", what, a, a.Job)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A desktop that asks learns the newest release and what changed in it,
// and the answer is kept: the address is asked again only on request.
func TestUpdateCheck(t *testing.T) {
	f := newUpdFixture(t, "2026.10.10", "2026.10.11")
	code, a := f.call(t, "GET", "/v1/update", "")
	if code != 200 || a.Version != "2026.10.10" || a.Latest != "2026.10.11" || !a.Available || a.Source || a.Installed != "" || a.Job != nil {
		t.Fatalf("first answer: %d %+v", code, a)
	}
	if a.Notes != "Since 2026.10.10:\n\n- Software Update" || a.Checked == "" {
		t.Errorf("notes %q, checked %q", a.Notes, a.Checked)
	}
	f.call(t, "GET", "/v1/update", "")
	if n := f.asked.Load(); n != 1 {
		t.Errorf("the release address was asked %d times for two looks", n)
	}
	f.latest.Store("2026.10.12")
	if _, a := f.call(t, "GET", "/v1/update", ""); a.Latest != "2026.10.11" {
		t.Errorf("a kept answer was not kept: %+v", a)
	}
	// Check Now asks, whenever it is pressed
	if _, a := f.call(t, "GET", "/v1/update?check=1", ""); a.Latest != "2026.10.12" || f.asked.Load() != 2 {
		t.Errorf("Check Now: %+v after %d asks", a, f.asked.Load())
	}
	// and half a day on, a plain look asks by itself
	f.latest.Store("2026.10.13")
	f.s.upd.now = func() time.Time { return time.Now().Add(13 * time.Hour) }
	if _, a := f.call(t, "GET", "/v1/update", ""); a.Latest != "2026.10.13" {
		t.Errorf("a stale answer was served: %+v", a)
	}
}

// The newest release running: nothing is offered, and Update Now does nothing.
func TestUpdateWhenCurrent(t *testing.T) {
	f := newUpdFixture(t, "2026.10.11", "2026.10.11")
	if code, a := f.call(t, "POST", "/v1/update", `{}`); code != 200 || a.Available || a.Job != nil || f.installs.Load() != 0 {
		t.Fatalf("%d %+v, %d installs", code, a, f.installs.Load())
	}
}

// An exe built from a checkout says so, still names the newest release,
// and is never updated in place.
func TestUpdateOnASourceBuild(t *testing.T) {
	f := newUpdFixture(t, "", "2026.10.11")
	if _, a := f.call(t, "GET", "/v1/update", ""); !a.Source || a.Version != "" || a.Latest != "2026.10.11" || a.Available || a.Installed != "" {
		t.Fatalf("%+v", a)
	}
	if code, _ := f.call(t, "POST", "/v1/update", `{}`); code != http.StatusConflict || f.installs.Load() != 0 {
		t.Fatalf("Update Now on a source build: %d, %d installs", code, f.installs.Load())
	}
}

// A release address that does not answer is said, not hidden, and leaves
// what was known before in place.
func TestUpdateCheckThatFails(t *testing.T) {
	f := newUpdFixture(t, "2026.10.10", "")
	if _, a := f.call(t, "GET", "/v1/update", ""); a.Error != "No release has been published yet." || a.Latest != "" || a.Available {
		t.Fatalf("%+v", a)
	}
	if code, _ := f.call(t, "POST", "/v1/update", `{}`); code != http.StatusBadGateway || f.installs.Load() != 0 {
		t.Fatalf("Update Now with nothing known: %d", code)
	}
}

// Update Now: one installer run however many ask, the binary on disk is
// the target before any restart, the restart is told which VMs ran, and
// the record it leaves is what the next daemon reports from.
func TestUpdateNow(t *testing.T) {
	f := newUpdFixture(t, "2026.10.10", "2026.10.11")
	f.vms.vms = map[string]string{"lab": "running", "idle": "stopped"}
	code, a := f.call(t, "POST", "/v1/update", `{"restart": true}`)
	if code != http.StatusAccepted || a.Job == nil || a.Job.State != "installing" || a.Job.Target != "2026.10.11" {
		t.Fatalf("Update Now: %d %+v", code, a.Job)
	}
	// a second desktop's Update Now joins it
	if code, a := f.call(t, "POST", "/v1/update", `{"restart": true}`); code != http.StatusAccepted || a.Job == nil || a.Job.Target != "2026.10.11" {
		t.Fatalf("the second Update Now: %d %+v", code, a.Job)
	}
	var running []string
	select {
	case running = <-f.restarts:
	case <-time.After(5 * time.Second):
		t.Fatal("no restart was asked for")
	}
	if n := f.installs.Load(); n != 1 {
		t.Fatalf("the installer ran %d times", n)
	}
	if len(running) != 1 || running[0] != "lab" {
		t.Errorf("the restart was told %v ran", running)
	}
	_, a = f.call(t, "GET", "/v1/update", "")
	if a.Job == nil || a.Job.State != "restarting" || a.Installed != "2026.10.11" {
		t.Errorf("while restarting: job %+v, installed %q", a.Job, a.Installed)
	}
	if f.asked.Load() != 1 {
		t.Errorf("the release address was asked %d times during one update", f.asked.Load())
	}

	// the daemon that comes back: the new version, the same state folder
	next := New(&config.Config{}, f.vms, nil, "", filepath.Join(f.dir, "state"))
	next.upd.version = func() string { return "2026.10.11" }
	next.upd.client, next.upd.exe, next.upd.probe = f.s.upd.client, f.s.upd.exe, f.s.upd.probe
	f.vms.mu.Lock()
	f.vms.vms["lab"] = "starting"
	f.vms.mu.Unlock()
	g := &updFixture{s: next, h: next.Handler()}
	_, a = g.call(t, "GET", "/v1/update", "")
	if a.Last == nil || !a.Last.OK || !a.Last.Recent || a.Last.From != "2026.10.10" || a.Last.To != "2026.10.11" {
		t.Fatalf("after the restart: %+v", a.Last)
	}
	if len(a.Last.VMs) != 1 || a.Last.VMs[0]["name"] != "lab" || a.Last.VMs[0]["state"] != "starting" {
		t.Errorf("the VM that ran: %v", a.Last.VMs)
	}
	if !strings.HasPrefix(a.Last.Notes, "This release has a new network helper.") || strings.Contains(a.Last.Notes, "keeps running") {
		t.Errorf("what the installer said that still stands: %q", a.Last.Notes)
	}
	if a.Available || a.Installed != "" || a.Job != nil || a.Version != "2026.10.11" {
		t.Errorf("the new daemon: %+v", a)
	}
	// a quarter of an hour on, the VMs' return is no longer news
	next.upd.now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	if _, a = g.call(t, "GET", "/v1/update", ""); a.Last == nil || a.Last.Recent || a.Last.VMs != nil {
		t.Errorf("later: %+v", a.Last)
	}
	// a daemon that came back as something else says so
	next.upd.version = func() string { return "2026.10.10" }
	if _, a = g.call(t, "GET", "/v1/update", ""); a.Last == nil || a.Last.OK {
		t.Errorf("back as the old version: %+v", a.Last)
	}
}

// Installed without a restart — a shell's `exe update` put off, or Update
// Now told not to: the daemon says a newer exe is in place, and a restart
// is all that is left to do.
func TestUpdateInstalledWaitsForARestart(t *testing.T) {
	f := newUpdFixture(t, "2026.10.10", "2026.10.11")
	code, _ := f.call(t, "POST", "/v1/update", `{"restart": false}`)
	if code != http.StatusAccepted {
		t.Fatalf("install only: %d", code)
	}
	a := f.waitFor(t, "the install", func(a updAnswer) bool { return a.Job == nil && a.Installed != "" })
	if a.Installed != "2026.10.11" || !a.Available || a.Version != "2026.10.10" {
		t.Fatalf("installed, not restarted: %+v", a)
	}
	select {
	case <-f.restarts:
		t.Fatal("a restart nobody asked for")
	default:
	}
	if _, err := os.Stat(f.s.updateRecordPath()); err == nil {
		t.Error("a record was left with no restart")
	}
	// Restart Now: no second install, and the helper's lines ride along
	if code, a := f.call(t, "POST", "/v1/update", `{"restart": true}`); code != http.StatusAccepted || a.Job == nil || a.Job.State != "restarting" {
		t.Fatalf("Restart Now: %d %+v", code, a.Job)
	}
	select {
	case <-f.restarts:
	case <-time.After(5 * time.Second):
		t.Fatal("no restart")
	}
	if f.installs.Load() != 1 {
		t.Errorf("the installer ran %d times", f.installs.Load())
	}
	if rec := f.s.readUpdateRecord(); rec == nil || rec.To != "2026.10.11" || rec.From != "2026.10.10" || !strings.Contains(rec.Notes, "network helper") {
		t.Errorf("the record: %+v", rec)
	}
}

// A shell's `exe update` can put a release in place that the daemon has
// not heard of yet: its last look was minutes ago and found the one
// before. The release on disk being newer than the newest known is reason
// enough to look again, so the panel does not name an older release as
// the newest beside "installed, restart to use it".
func TestUpdateOnDiskNewerThanKnown(t *testing.T) {
	f := newUpdFixture(t, "2026.10.10", "2026.10.10")
	if _, a := f.call(t, "GET", "/v1/update", ""); a.Latest != "2026.10.10" || a.Installed != "" {
		t.Fatalf("before: %+v", a)
	}
	f.latest.Store("2026.10.11")
	writeExe(t, f.s.upd.exe, "2026.10.11") // the shell's update
	_, a := f.call(t, "GET", "/v1/update", "")
	if a.Installed != "2026.10.11" || a.Latest != "2026.10.11" || a.Notes == "" || f.asked.Load() != 2 {
		t.Fatalf("after: %+v (asked %d times)", a, f.asked.Load())
	}
	// and that is one look, not one at every question from then on
	f.call(t, "GET", "/v1/update", "")
	if f.asked.Load() != 2 {
		t.Errorf("asked %d times", f.asked.Load())
	}
	// nor where the address never comes to name it: a build nobody published
	writeExe(t, f.s.upd.exe, "2026.10.12")
	for i := 0; i < 3; i++ {
		f.s.upd.now = func() time.Time { return time.Now().Add(time.Duration(i+1) * time.Minute) }
		f.call(t, "GET", "/v1/update", "")
	}
	if _, a := f.call(t, "GET", "/v1/update", ""); a.Installed != "2026.10.12" || a.Latest != "2026.10.11" || f.asked.Load() != 3 {
		t.Errorf("a release the address does not know: %+v (asked %d times)", a, f.asked.Load())
	}
}

// A restart that does not happen is a failure the panel can show: the
// release is installed, the daemon is the old one, and no record claims
// otherwise.
func TestUpdateRestartThatDoesNotHappen(t *testing.T) {
	f := newUpdFixture(t, "2026.10.10", "2026.10.11")
	f.s.upd.wait = 20 * time.Millisecond
	f.call(t, "POST", "/v1/update", `{}`)
	a := f.waitFor(t, "the failure", func(a updAnswer) bool { return a.Job != nil && a.Job.State == "failed" })
	if !strings.Contains(a.Job.Error, "did not restart") || a.Installed != "2026.10.11" || a.Last != nil {
		t.Fatalf("%+v, job %+v", a, a.Job)
	}
	// and Update Now again is a restart, not another download
	f.s.upd.wait = time.Hour
	<-f.restarts
	f.call(t, "POST", "/v1/update", `{}`)
	select {
	case <-f.restarts:
	case <-time.After(5 * time.Second):
		t.Fatal("no second restart")
	}
	if f.installs.Load() != 1 {
		t.Errorf("the installer ran %d times", f.installs.Load())
	}
}

// An installer that stops says why in its own last words, nothing is
// restarted, and the next Update Now starts over.
func TestUpdateInstallerFails(t *testing.T) {
	f := newUpdFixture(t, "2026.10.10", "2026.10.11")
	good := f.s.upd.install
	f.s.upd.install = func(ctx context.Context, exe string, line func(string)) error {
		line("Downloading exe 2026.10.11…")
		line("error: exe-linux-arm64.tar.gz does not match its checksum")
		return os.ErrInvalid
	}
	f.call(t, "POST", "/v1/update", `{}`)
	a := f.waitFor(t, "the failure", func(a updAnswer) bool { return a.Job != nil && a.Job.State == "failed" })
	if a.Job.Error != "exe-linux-arm64.tar.gz does not match its checksum" || a.Installed != "" {
		t.Fatalf("%+v, job %+v", a, a.Job)
	}
	f.s.upd.install = good
	if code, a := f.call(t, "POST", "/v1/update", `{}`); code != http.StatusAccepted || a.Job == nil || a.Job.State != "installing" {
		t.Fatalf("Update Now after a failure: %d %+v", code, a.Job)
	}
	select {
	case <-f.restarts:
	case <-time.After(5 * time.Second):
		t.Fatal("no restart after the second try")
	}
}

func TestVersionSaid(t *testing.T) {
	for line, want := range map[string]string{
		"exe 2026.10.11 (linux/arm64)":         "2026.10.11",
		"exe 2026.10.10.2 (windows/amd64)":     "2026.10.10.2",
		"exe, built from source (linux/arm64)": "",
		"exe latest (linux/arm64)":             "",
		"":                                     "",
	} {
		if got := versionSaid(line); got != want {
			t.Errorf("versionSaid(%q) = %q, want %q", line, got, want)
		}
	}
}
