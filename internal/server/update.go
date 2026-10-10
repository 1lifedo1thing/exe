package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"exe/internal/release"
)

// Software Update: the desktop's way to do what `exe update` does in a
// shell (Apple menu → Software Update…).
//
//	GET  /v1/update[?check=1]   how this exe stands against the releases
//	POST /v1/update             {"restart": true}: install the newest, then restart
//
// The work is not done twice. The binary in place runs its own `update`
// as a child of the daemon, exactly as a person would at a prompt, so the
// download, the checksums, the probe of the new binary and the last-step
// rename are the command's (cmd/exe/update.go) and behave the same on
// every system. What the daemon adds is what a shell has no need of:
//
//   - One job. An update started from one desktop is the update a second
//     Update Now joins, and a tab that was closed finds it still going.
//   - What is on disk. The binary is replaced before any restart, so a
//     restart that was put off, or a shell's `exe update`, leaves a newer
//     exe in place under a daemon still running the old one. Asking the
//     file for its `version` says so: "installed, restart to use it".
//   - What the restart was for. The daemon that comes back has no memory,
//     so the one that goes down writes update.json first: from, to, when,
//     the VMs that were running, and anything the installer said that
//     still stands. The new daemon reads it and reports each of those VMs
//     as it is now, and whether the version that runs is the one meant.
//
// A check is one request, for where /releases/latest redirects to, made
// when a desktop asks and the last answer is more than updateCheckEvery
// old. A daemon nobody looks at asks nobody.
//
// An exe built from a checkout is never updated in place (release.Version
// is empty): it says so, and still reports the newest release.

const (
	updateCheckEvery  = 12 * time.Hour
	updateRecordFile  = "update.json"
	updateRecentFor   = 15 * time.Minute // how long the VMs' return is reported after a restart
	updateRestartWait = 90 * time.Second // a restart that has not happened by then did not
)

// updateRecord is what a daemon leaves for the one that replaces it.
type updateRecord struct {
	From  string    `json:"from"`
	To    string    `json:"to"`
	At    time.Time `json:"at"`
	VMs   []string  `json:"vms,omitempty"`
	Notes string    `json:"notes,omitempty"`
}

// updateJob is the one update in hand.
type updateJob struct {
	Target  string    `json:"target"`
	State   string    `json:"state"` // "installing", "restarting", "failed"
	Started time.Time `json:"started"`
	Message string    `json:"message,omitempty"` // the installer's last line
	Error   string    `json:"error,omitempty"`
	Restart bool      `json:"restart"`
}

type updateState struct {
	mu      sync.Mutex
	checkMu sync.Mutex // one check at a time; the others wait for its answer

	latest   string
	checked  time.Time
	checks   int // looks made, so one that waited for another can tell
	checkErr string
	notes    map[string]string // a release's notes, once read

	job *updateJob
	// notes the installer left when nobody restarted (the helper): they
	// ride with the restart whenever it comes
	pending string

	exe      string      // the binary this daemon runs
	launched os.FileInfo // that file as it was at the first look
	diskStat os.FileInfo // the file the answer below is about
	diskSaid string      // its `version`, a release version or ""
	relooked string      // the release on disk the address was asked again for

	// seams, set by tests
	version func() string
	client  *release.Client
	probe   func(bin string) (string, error)
	install func(ctx context.Context, exe string, line func(string)) error
	restart func(running []string)
	now     func() time.Time
	wait    time.Duration // updateRestartWait, shorter in a test
}

func (u *updateState) init() {
	if u.version == nil {
		u.version = func() string { return release.Version }
	}
	if u.client == nil {
		u.client = release.NewClient()
	}
	if u.probe == nil {
		u.probe = func(bin string) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, bin, "version").Output()
			return strings.TrimSpace(string(out)), err
		}
	}
	if u.install == nil {
		u.install = runInstaller
	}
	if u.now == nil {
		u.now = time.Now
	}
	if u.notes == nil {
		u.notes = map[string]string{}
	}
	if u.wait == 0 {
		u.wait = updateRestartWait
	}
	if u.exe == "" {
		if p, err := os.Executable(); err == nil {
			if real, err := filepath.EvalSymlinks(p); err == nil {
				p = real
			}
			u.exe = p
		}
	}
	if u.launched == nil && u.exe != "" {
		u.launched, _ = os.Stat(u.exe)
	}
}

// runInstaller is `exe update` with nobody at a keyboard: it installs,
// and leaves the restart to whoever asked. Each line it prints is handed
// on as it comes.
func runInstaller(ctx context.Context, exe string, line func(string)) error {
	cmd := exec.CommandContext(ctx, exe, "update")
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			line(sc.Text())
		}
		io.Copy(io.Discard, pr)
	}()
	err := cmd.Run()
	pw.Close()
	<-done
	return err
}

// versionSaid reads the release version out of a binary's `version` line:
// "exe 2026.10.11 (linux/arm64)". A build from source has none.
func versionSaid(line string) string {
	f := strings.Fields(line)
	if len(f) >= 2 && f[0] == "exe" && release.Valid(f[1]) {
		return f[1]
	}
	return ""
}

// onDisk is the release version of the binary in place when that is not
// the file this daemon started from, and "" when it is, or cannot be told.
func (u *updateState) onDisk() string {
	if u.exe == "" || u.launched == nil {
		return ""
	}
	st, err := os.Stat(u.exe)
	if err != nil || os.SameFile(st, u.launched) && st.ModTime().Equal(u.launched.ModTime()) && st.Size() == u.launched.Size() {
		return ""
	}
	if u.diskStat != nil && os.SameFile(st, u.diskStat) && st.ModTime().Equal(u.diskStat.ModTime()) && st.Size() == u.diskStat.Size() {
		return u.diskSaid
	}
	said, err := u.probe(u.exe)
	if err != nil {
		return ""
	}
	u.diskStat, u.diskSaid = st, versionSaid(said)
	return u.diskSaid
}

// check asks for the newest release, unless an answer young enough is in
// hand; it reads that release's notes once.
func (s *Server) updateCheck(ctx context.Context, force bool) {
	u := &s.upd
	u.mu.Lock()
	before := u.checks
	u.mu.Unlock()
	u.checkMu.Lock()
	defer u.checkMu.Unlock()
	u.mu.Lock()
	fresh := !u.checked.IsZero() && u.now().Sub(u.checked) < updateCheckEvery
	if force { // an asker that waited here for another one's look has its answer
		fresh = u.checks != before
	}
	u.mu.Unlock()
	if fresh {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	latest, err := u.client.Latest(ctx)
	var notes string
	var haveNotes bool
	if err == nil {
		u.mu.Lock()
		_, haveNotes = u.notes[latest]
		u.mu.Unlock()
		if !haveNotes {
			var nerr error
			if notes, nerr = u.client.Notes(ctx, latest); nerr != nil {
				log.Printf("update: notes of %s: %v", latest, nerr)
			} else {
				haveNotes = true
			}
		} else {
			haveNotes = false // already kept
		}
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.checked = u.now()
	u.checks++
	if err != nil {
		u.checkErr = err.Error()
		if errors.Is(err, release.ErrNoRelease) {
			u.checkErr = "No release has been published yet."
		}
		return
	}
	u.checkErr, u.latest = "", latest
	if haveNotes {
		u.notes[latest] = notes
	}
}

func (s *Server) updateRecordPath() string { return filepath.Join(s.StateDir, updateRecordFile) }

func (s *Server) readUpdateRecord() *updateRecord {
	b, err := os.ReadFile(s.updateRecordPath())
	if err != nil {
		return nil
	}
	rec := &updateRecord{}
	if json.Unmarshal(b, rec) != nil || !release.Valid(rec.To) {
		return nil
	}
	return rec
}

// updateStatus is the whole answer a desktop draws its panel from.
func (s *Server) updateStatus(ctx context.Context) map[string]any {
	u := &s.upd
	u.mu.Lock()
	version := u.version()
	res := map[string]any{
		"version": version,
		"source":  version == "",
		"latest":  u.latest,
		"error":   u.checkErr,
	}
	if !u.checked.IsZero() {
		res["checked"] = u.checked.UTC()
	}
	if u.latest != "" {
		res["notes"] = u.notes[u.latest]
		res["page"] = release.DefaultBase + "/tag/" + u.latest
		if version != "" {
			res["available"] = release.Compare(u.latest, version) > 0
		}
	}
	if version != "" {
		if disk := u.onDisk(); disk != "" && release.Compare(disk, version) != 0 {
			res["installed"] = disk
			if u.pending != "" {
				res["installed_notes"] = u.pending
			}
		}
	}
	if u.job != nil {
		j := *u.job
		res["job"] = j
	}
	now := u.now()
	u.mu.Unlock()

	// the VMs a restart would stop, and after one, how each came back
	states := map[string]string{}
	var running []string
	if vms, err := s.VMs.List(ctx); err == nil {
		for _, v := range vms {
			states[v.Name] = v.State
			if v.State == "running" {
				running = append(running, v.Name)
			}
		}
	}
	res["running"] = running
	// The record is about the restart it was written for. An exe that has
	// since moved past that release (a shell's update, say) is not a failed
	// restart, and the record has nothing more to tell.
	if rec := s.readUpdateRecord(); rec != nil && (version == "" || release.Compare(version, rec.To) <= 0) {
		last := map[string]any{"from": rec.From, "to": rec.To, "at": rec.At.UTC(), "notes": rec.Notes,
			"ok": version != "" && release.Compare(version, rec.To) == 0}
		if now.Sub(rec.At) < updateRecentFor {
			last["recent"] = true
			vms := []map[string]string{}
			for _, name := range rec.VMs {
				st, ok := states[name]
				if !ok {
					st = "gone"
				}
				vms = append(vms, map[string]string{"name": name, "state": st})
			}
			last["vms"] = vms
		}
		res["last"] = last
	}
	return res
}

func (s *Server) handleUpdateGet(w http.ResponseWriter, r *http.Request) {
	s.upd.init()
	force := r.URL.Query().Get("check") != ""
	s.upd.mu.Lock()
	busy := s.upd.job != nil && s.upd.job.State != "failed"
	stale := s.upd.checked.IsZero() || s.upd.now().Sub(s.upd.checked) >= updateCheckEvery
	// A release on disk that is newer than the newest one known: a shell
	// installed it since the last look, and what is known is out of date.
	// One look for each such release: where the address goes on naming an
	// older one (a build nobody published), asking again changes nothing.
	if s.upd.version() == "" {
		// a build from a checkout is replaced by every `make build`: not this
	} else if disk := s.upd.onDisk(); disk != "" && disk != s.upd.relooked && (s.upd.latest == "" || release.Compare(disk, s.upd.latest) > 0) {
		s.upd.relooked = disk
		force = true
	}
	s.upd.mu.Unlock()
	// an update in hand already knows what it is after
	if !busy && (force || stale) {
		s.updateCheck(r.Context(), force)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.updateStatus(r.Context()))
}

// handleUpdatePost starts the update, or joins the one in hand. With
// "restart" (the default) the daemon restarts into the new release when
// it is installed; a release already installed is only restarted into.
func (s *Server) handleUpdatePost(w http.ResponseWriter, r *http.Request) {
	u := &s.upd
	u.init()
	req := struct {
		Restart *bool `json:"restart"`
	}{}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil && err != io.EOF {
		writeErr(w, http.StatusBadRequest, errors.New(`expected {"restart": true} or {"restart": false}`))
		return
	}
	restart := req.Restart == nil || *req.Restart
	if u.version() == "" {
		writeErr(w, http.StatusConflict, errors.New("this exe was built from source: it is updated there, with git pull and make build"))
		return
	}
	u.mu.Lock()
	busy := u.job != nil && u.job.State != "failed"
	known := u.latest != ""
	u.mu.Unlock()
	if !busy && !known {
		s.updateCheck(r.Context(), true)
	}

	u.mu.Lock()
	if u.job != nil && u.job.State != "failed" {
		// one update: this asker joins it, and may add the restart to it
		if restart {
			u.job.Restart = true
		}
		u.mu.Unlock()
		writeJSON(w, http.StatusAccepted, s.updateStatus(r.Context()))
		return
	}
	version, latest, disk := u.version(), u.latest, u.onDisk()
	switch {
	case disk != "" && release.Compare(disk, version) != 0 && (latest == "" || release.Compare(disk, latest) >= 0):
		// the newest is installed already and waits for a restart
		if !restart {
			u.mu.Unlock()
			writeJSON(w, http.StatusOK, s.updateStatus(r.Context()))
			return
		}
		notes := u.pending
		u.job = &updateJob{Target: disk, State: "restarting", Started: u.now(), Restart: true}
		u.mu.Unlock()
		go s.updateRestart(disk, notes)
	case latest != "" && release.Compare(latest, version) > 0:
		u.job = &updateJob{Target: latest, State: "installing", Started: u.now(), Restart: restart}
		u.mu.Unlock()
		go s.updateRun(latest)
	default:
		err := u.checkErr
		u.mu.Unlock()
		if latest == "" && err != "" {
			writeErr(w, http.StatusBadGateway, errors.New(err))
			return
		}
		writeJSON(w, http.StatusOK, s.updateStatus(r.Context()))
		return
	}
	writeJSON(w, http.StatusAccepted, s.updateStatus(r.Context()))
}

// updateRun is the job: the installer, a look at what it left on disk,
// then the restart if one was asked for by the time it is done.
func (s *Server) updateRun(target string) {
	u := &s.upd
	var out []string
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	err := u.install(ctx, u.exe, func(line string) {
		out = append(out, line)
		// its closing word on the restart it left alone is not the job's
		// status: the restart is the daemon's next step
		if strings.TrimSpace(line) == "" || installerClosing(line) {
			return
		}
		u.mu.Lock()
		u.job.Message = strings.TrimSpace(line)
		u.mu.Unlock()
	})
	fail := func(why string) {
		log.Printf("update to %s: %s", target, why)
		u.mu.Lock()
		u.job.State, u.job.Error = "failed", why
		u.mu.Unlock()
	}
	if err != nil {
		fail(installerError(out, err))
		return
	}
	u.mu.Lock()
	u.diskStat = nil // look again: the file is another one now
	disk := u.onDisk()
	u.mu.Unlock()
	if disk != target {
		why := "the installer finished, and the exe in place is still " + u.version()
		if disk != "" {
			why = "the installer finished, and the exe in place is " + disk + ", not " + target
		}
		fail(why)
		return
	}
	notes := installerNotes(out)
	log.Printf("update: exe %s is installed at %s", target, u.exe)
	u.mu.Lock()
	restart := u.job.Restart
	if !restart {
		u.job, u.pending = nil, notes
	} else {
		u.job.State, u.job.Message = "restarting", ""
	}
	u.mu.Unlock()
	if restart {
		s.updateRestart(target, notes)
	}
}

// updateRestart leaves the record and restarts. It returns only when the
// restart did not happen.
func (s *Server) updateRestart(target, notes string) {
	u := &s.upd
	running := s.RunningVMNames(context.Background())
	rec := updateRecord{From: u.version(), To: target, At: u.now().UTC(), VMs: running, Notes: notes}
	b, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(s.updateRecordPath(), append(b, '\n'), 0o600); err != nil {
		log.Printf("update: record: %v", err)
	}
	log.Printf("update: restarting into exe %s (running VMs: %s)", target, strings.Join(running, ","))
	if u.restart != nil {
		u.restart(running)
	} else {
		s.RestartDaemon(400*time.Millisecond, running)
	}
	// Under a service manager the daemon was only asked to stop, and is on
	// its way down; handing over by itself, it is gone by now. Still here
	// after a while, it is not going.
	time.Sleep(u.wait)
	os.Remove(s.updateRecordPath()) // nothing was restarted into
	u.mu.Lock()
	u.pending = notes
	u.job.State = "failed"
	u.job.Error = "exe " + target + " is installed, but exe did not restart. Restart it to use the new release."
	u.mu.Unlock()
	log.Printf("update: the restart into %s did not happen", target)
}

// installerError is why the installer stopped, in its own last words.
func installerError(out []string, err error) string {
	for i := len(out) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(out[i]); l != "" {
			return strings.TrimPrefix(l, "error: ")
		}
	}
	return "the installer stopped: " + err.Error()
}

// installerNotes is what the installer said after "Installed exe …" that
// still stands once the daemon has restarted: on Linux, the two sudo lines
// for a network helper that changed. Its closing word on the restart it
// left alone is not that.
func installerNotes(out []string) string {
	var keep []string
	seen := false
	for _, l := range out {
		switch {
		case !seen:
			seen = strings.HasPrefix(l, "Installed exe ")
		case installerClosing(l):
			return strings.TrimSpace(strings.Join(keep, "\n"))
		default:
			keep = append(keep, l)
		}
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

// installerClosing is the installer's last line, on the restart it did not
// do: said to a person at a prompt, and of no use once the daemon restarts
// by itself.
func installerClosing(line string) bool {
	return strings.HasPrefix(line, "The daemon keeps running") || strings.HasPrefix(line, "exe is not running")
}
