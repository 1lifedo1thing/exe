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

// Write saves the manifest.
func (m AppsManifest) Write(file string) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	return os.WriteFile(file, append(b, '\n'), 0o644)
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
// app.json) into appsDir. prev is what earlier installs put there; the
// result is what stands there now by this installer's hand. staging is a
// scratch folder on the same disk as appsDir.
func InstallApps(src, appsDir, staging string, prev AppsManifest) (AppsManifest, []AppChange, error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return nil, nil, err
	}
	next := AppsManifest{}
	for name, sum := range prev {
		next[name] = sum
	}
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
			return nil, nil, err
		}
		to := filepath.Join(appsDir, name)
		was, ours := prev[name]
		_, statErr := os.Stat(to)
		switch {
		case statErr != nil && !os.IsNotExist(statErr):
			return nil, nil, statErr
		case statErr != nil && ours:
			changes = append(changes, AppChange{name, "kept", "you removed it"})
			continue
		case statErr != nil:
			if err := placeTree(from, to, staging); err != nil {
				return nil, nil, err
			}
			next[name] = sum
			changes = append(changes, AppChange{name, "installed", ""})
			continue
		case !ours:
			changes = append(changes, AppChange{name, "kept", "an app of that name was already there"})
			continue
		}
		now, err := TreeSum(to)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case now != was:
			changes = append(changes, AppChange{name, "kept", "you edited it"})
		case now == sum:
			changes = append(changes, AppChange{name, "current", ""})
		default:
			if err := placeTree(from, to, staging); err != nil {
				return nil, nil, err
			}
			next[name] = sum
			changes = append(changes, AppChange{name, "updated", ""})
		}
	}
	return next, changes, nil
}

// RemoveApps takes away the bundles that are still exactly what was
// installed. One that was edited is its owner's now, and stays.
func RemoveApps(appsDir string, m AppsManifest) (removed, kept []string, err error) {
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
// goes only once the new one stands.
func placeTree(from, to, staging string) error {
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
