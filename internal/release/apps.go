package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// The extra desktop apps (github.com/livid/exe-apps) come with a release
// as one tarball of bundles and are unpacked into the apps folder, where
// the daemon serves them from disk — and where their owner may edit them.
// So what was installed is written down, bundle by bundle, and an update
// replaces only a bundle that is still what it installed: an app edited,
// removed or put there by its owner is left alone.
//
// That record has to survive a run that stops halfway. A manifest saved
// only at the end forgets the bundles that were already placed: the next
// run would find them changed, or find a new one it has no record of, and
// take them for their owner's — for good. So before the first bundle is
// placed, what is about to be placed is written beside the manifest (the
// pending plan), and it is removed once the manifest is saved. A later run
// reads a plan it finds as a claim to check, not a fact: a bundle is ours
// by the plan only if it stands there with exactly the contents the plan
// meant to give it.

// AppsManifest is what was installed: bundle name → TreeSum of it.
type AppsManifest map[string]string

// ReadAppsManifest reads a manifest; a missing file is an empty one.
func ReadAppsManifest(file string) (AppsManifest, error) {
	b, err := os.ReadFile(file)
	if os.IsNotExist(err) {
		return AppsManifest{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := AppsManifest{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return m, nil
}

// Write saves the manifest, whole or not at all.
func (m AppsManifest) Write(file string) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// pendingFile is where the plan of a run that has not finished is kept.
func pendingFile(manifestFile string) string { return manifestFile + ".pending" }

// AppsTracked reports whether the installer ever brought apps here: it
// left a manifest, or the plan of an install that did not finish. Apps
// that came with the install follow it — an update refreshes them, an
// uninstall takes them away; apps that were declined are never touched.
func AppsTracked(manifestFile string) bool {
	for _, f := range []string{manifestFile, pendingFile(manifestFile)} {
		if _, err := os.Stat(f); err == nil {
			return true
		}
	}
	return false
}

// owned is what the installer knows it put in appsDir: the manifest, and
// of an unfinished run's plan every bundle that stands there exactly as
// that run meant to place it. plan is that run's plan as it was written.
func owned(appsDir, manifestFile string) (m, plan AppsManifest, err error) {
	if m, err = ReadAppsManifest(manifestFile); err != nil {
		return nil, nil, err
	}
	if plan, err = ReadAppsManifest(pendingFile(manifestFile)); err != nil {
		return nil, nil, err
	}
	for name, sum := range plan {
		if !filepath.IsLocal(name) {
			continue
		}
		if now, err := TreeSum(filepath.Join(appsDir, name)); err == nil && now == sum {
			m[name] = sum
		}
	}
	return m, plan, nil
}

// TreeSum names a folder's contents: a hash over every file's path and
// bytes, in order.
func TreeSum(dir string) (string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, rel := range files {
		sum, err := FileSum(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%s\n", rel, sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// AppChange says what happened to one bundle.
type AppChange struct {
	Name   string
	Action string // "installed", "updated", "kept", "current"
	Why    string // for "kept": why it was left alone
}

// InstallApps brings the bundles in src (one folder each, with an
// app.json) into appsDir, and keeps the record of it in manifestFile.
// staging is a scratch folder of its own on the same disk as appsDir.
//
// It can be run again after any failure, with the same release or a later
// one, and finishes what the failed run began: the changes it returns
// then name the bundles already in place as "current".
func InstallApps(src, appsDir, staging, manifestFile string) ([]AppChange, error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return nil, err
	}
	m, plan, err := owned(appsDir, manifestFile)
	if err != nil {
		return nil, err
	}
	if len(plan) > 0 {
		// what the unfinished run placed is on record before its plan
		// gives way to this run's
		if err := m.Write(manifestFile); err != nil {
			return nil, err
		}
	}
	os.RemoveAll(staging) // what a run that died left in its scratch

	type job struct{ name, from, to, sum, action string }
	var jobs []job
	var changes []AppChange
	for _, e := range entries {
		name := e.Name()
		from := filepath.Join(src, name)
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(from, "app.json")); err != nil {
			continue // not a bundle
		}
		sum, err := TreeSum(from)
		if err != nil {
			return nil, err
		}
		to := filepath.Join(appsDir, name)
		was, ours := m[name]
		_, planned := plan[name]
		_, statErr := os.Stat(to)
		switch {
		case statErr != nil && !os.IsNotExist(statErr):
			return nil, statErr
		case statErr != nil && ours && !planned:
			changes = append(changes, AppChange{name, "kept", "you removed it"})
			continue
		case statErr != nil:
			// new here — or the run that planned it stopped before, or
			// in the middle of, putting it in place
			jobs = append(jobs, job{name, from, to, sum, "installed"})
			continue
		case !ours:
			changes = append(changes, AppChange{name, "kept", "an app of that name was already there"})
			continue
		}
		now, err := TreeSum(to)
		if err != nil {
			return nil, err
		}
		switch {
		case now == sum:
			// first, before "edited": a bundle this release would place
			// is this release's, whatever the record said of it
			m[name] = sum
			changes = append(changes, AppChange{name, "current", ""})
		case now != was:
			changes = append(changes, AppChange{name, "kept", "you edited it"})
		default:
			jobs = append(jobs, job{name, from, to, sum, "updated"})
		}
	}

	// the plan goes to disk before the first bundle does
	pending := pendingFile(manifestFile)
	if len(jobs) > 0 {
		next := AppsManifest{}
		for _, j := range jobs {
			next[j.name] = j.sum
		}
		if err := next.Write(pending); err != nil {
			return nil, err
		}
	}
	for _, j := range jobs {
		if err := placeTree(j.from, j.to, staging); err != nil {
			// the plan stays for the next run; saving what was placed
			// is a kindness to it, not what it relies on
			m.Write(manifestFile)
			return changes, fmt.Errorf("%s: %w", j.name, err)
		}
		m[j.name] = j.sum
		changes = append(changes, AppChange{j.name, j.action, ""})
	}
	if err := m.Write(manifestFile); err != nil {
		return changes, err
	}
	if err := os.Remove(pending); err != nil && !os.IsNotExist(err) {
		return changes, err
	}
	sort.Slice(changes, func(i, k int) bool { return changes[i].Name < changes[k].Name })
	return changes, nil
}

// RemoveApps takes away the bundles that are still exactly what was
// installed — an unfinished install's among them. One that was edited is
// its owner's now, and stays.
func RemoveApps(appsDir, manifestFile string) (removed, kept []string, err error) {
	m, _, err := owned(appsDir, manifestFile)
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		dir := filepath.Join(appsDir, name)
		if !filepath.IsLocal(name) {
			continue
		}
		now, serr := TreeSum(dir)
		switch {
		case os.IsNotExist(serr):
		case serr != nil:
			return removed, kept, serr
		case now != m[name]:
			kept = append(kept, name)
		default:
			if err := os.RemoveAll(dir); err != nil {
				return removed, kept, err
			}
			removed = append(removed, name)
		}
	}
	return removed, kept, nil
}

// placeTree puts a copy of from at to, whole or not at all: the copy is
// made in staging and renamed into place, and what stood there before
// goes only once the new one stands. A variable so a test can make one
// bundle fail.
var placeTree = func(from, to, staging string) error {
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(staging, "app-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	fresh := filepath.Join(tmp, "new")
	if err := copyTree(from, fresh); err != nil {
		return err
	}
	old := filepath.Join(tmp, "old")
	if err := os.Rename(to, old); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(fresh, to); err != nil {
		os.Rename(old, to) // put back what was there
		return err
	}
	return nil
}

func copyTree(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(dst, 0o755)
		case d.Type().IsRegular():
			return CopyFile(p, dst, 0o644)
		}
		return nil // a release holds nothing else (Extract)
	})
}

// CopyFile writes a copy of from at to with the given mode.
func CopyFile(from, to string, mode os.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
