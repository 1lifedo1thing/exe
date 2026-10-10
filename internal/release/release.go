// Package release is what a released exe knows about releases: its own
// version, where the next one is published, and how to fetch one and be
// sure of its bytes.
//
// Releases live on GitHub (github.com/livid/exe/releases). Nothing here
// talks to api.github.com, which allows an address 60 requests an hour:
// the latest version is read from where /releases/latest redirects to,
// and every file comes from /releases/download/<tag>/<name>.
package release

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Version is this binary's release, a date: "2026.10.09", or
// "2026.10.09.2" for the second one that day. The release build sets it
// (-X exe/internal/release.Version=…); a binary built from a checkout has
// none, and is never updated in place.
var Version string

// DefaultBase is where releases are published.
const DefaultBase = "https://github.com/livid/exe/releases"

// BaseEnv names another place that keeps the same layout — a mirror, or
// the local copy a release is tested against before it is published.
const BaseEnv = "EXE_RELEASE_URL"

// The files of a release. A binary tarball holds exe and exe-net-helper.
const (
	AppsAsset = "exe-apps.tar.gz"
	SumsAsset = "SHA256SUMS"
)

// BinaryAsset is the tarball for one architecture ("amd64", "arm64").
func BinaryAsset(goarch string) string { return "exe-linux-" + goarch + ".tar.gz" }

// Base is the release address in use.
func Base() string {
	if v := strings.TrimRight(strings.TrimSpace(os.Getenv(BaseEnv)), "/"); v != "" {
		return v
	}
	return DefaultBase
}

// Valid reports whether v is a release version: three or four dotted
// numbers, the first a year.
func Valid(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) < 3 || len(parts) > 4 {
		return false
	}
	for i, p := range parts {
		if p == "" || len(p) > 4 {
			return false
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || strings.TrimLeft(p, "0123456789") != "" {
			return false
		}
		if i == 0 && n < 2000 {
			return false
		}
	}
	return true
}

// Compare orders two versions number by number: -1 when a is older than
// b, 1 when newer. "2026.10.09" is older than "2026.10.09.2", which is
// older than "2026.10.10".
func Compare(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
	}
	return 0
}

// Client reads one release address.
type Client struct {
	Base string
	HTTP *http.Client
}

// NewClient reads the address in use (Base).
func NewClient() *Client {
	return &Client{Base: Base(), HTTP: &http.Client{Timeout: 10 * time.Minute}}
}

// ErrNoRelease is Latest's answer when nothing has been published.
var ErrNoRelease = errors.New("no release has been published yet")

// Latest is the newest release's version: the tag /latest redirects to.
func (c *Client) Latest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/latest", nil)
	if err != nil {
		return "", err
	}
	// the redirect is the answer; following it would only fetch a page
	hc := *c.HTTP
	hc.Timeout = 30 * time.Second
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("ask %s for the latest release: %w", c.Base, err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", ErrNoRelease
	}
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return "", fmt.Errorf("ask %s for the latest release: %s", c.Base, resp.Status)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		return "", fmt.Errorf("latest release: %w", err)
	}
	// .../releases/tag/<version>; with no release yet GitHub sends the
	// reader back to .../releases
	dir, tag := path.Split(strings.TrimRight(loc.Path, "/"))
	if !strings.HasSuffix(dir, "/tag/") {
		return "", ErrNoRelease
	}
	if !Valid(tag) {
		return "", fmt.Errorf("the latest release is named %q, which is not a version", tag)
	}
	return tag, nil
}

// Fetch downloads the named files of release tag into dir, with the
// release's SHA256SUMS, and checks each against it. A file that does not
// match is removed.
func (c *Client) Fetch(ctx context.Context, tag, dir string, names ...string) error {
	if !Valid(tag) {
		return fmt.Errorf("%q is not a release version", tag)
	}
	for _, name := range append([]string{SumsAsset}, names...) {
		if err := c.download(ctx, c.Base+"/download/"+tag+"/"+name, filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return Verify(dir, names...)
}

func (c *Client) download(ctx context.Context, from, to string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, from, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", from, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", from, resp.Status)
	}
	f, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// a release is tens of megabytes; anything far past that is not one
	if _, err := io.Copy(f, io.LimitReader(resp.Body, maxAsset)); err != nil {
		f.Close()
		os.Remove(to)
		return fmt.Errorf("download %s: %w", from, err)
	}
	return f.Close()
}

const maxAsset = 512 << 20

// Sums reads a SHA256SUMS file: name → hex digest.
func Sums(file string) (map[string]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sums := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || len(fields[0]) != 64 {
			continue
		}
		sums[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
	}
	return sums, sc.Err()
}

// FileSum is a file's SHA-256 in hex.
func FileSum(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Verify checks the named files in dir against dir's SHA256SUMS. Every
// name must be listed there and match; one that does not is removed, so
// nothing later can use it by mistake.
func Verify(dir string, names ...string) error {
	sums, err := Sums(filepath.Join(dir, SumsAsset))
	if err != nil {
		return err
	}
	for _, name := range names {
		want, ok := sums[name]
		if !ok {
			return fmt.Errorf("%s is not listed in %s", name, SumsAsset)
		}
		got, err := FileSum(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if got != want {
			os.Remove(filepath.Join(dir, name))
			return fmt.Errorf("%s does not match its checksum (got %s, the release says %s)", name, got, want)
		}
	}
	return nil
}

// Extract unpacks a .tar.gz into dir: regular files and directories only,
// and none that would land outside dir.
func Extract(tgz, dir string) error {
	f, err := os.Open(tgz)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(tgz), err)
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(tgz), err)
		}
		if !filepath.IsLocal(hdr.Name) {
			return fmt.Errorf("%s holds %q, which is not a path inside it", filepath.Base(tgz), hdr.Name)
		}
		to := filepath.Join(dir, hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(to, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if hdr.Mode&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, io.LimitReader(tr, maxAsset)); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			// links and devices have no business in a release
			return fmt.Errorf("%s holds %q, which is neither a file nor a folder", filepath.Base(tgz), hdr.Name)
		}
	}
}
