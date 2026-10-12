package sshexec

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

func TestReplaceEdit(t *testing.T) {
	const file = "a\nport = 8000\nb\nport = 8000\nc\n"
	tests := []struct {
		name        string
		file        string
		old, new    string
		all         bool
		want        string
		wantN       int
		errContains string
	}{
		{name: "unique match", file: file, old: "b\nport = 8000", new: "b\nport = 9000",
			want: "a\nport = 8000\nb\nport = 9000\nc\n", wantN: 1},
		{name: "replace all", file: file, old: "port = 8000", new: "port = 9000", all: true,
			want: "a\nport = 9000\nb\nport = 9000\nc\n", wantN: 2},
		{name: "ambiguous", file: file, old: "port = 8000", new: "port = 9000", errContains: "2 times"},
		{name: "not found", file: file, old: "port = 8080", new: "port = 9000", errContains: "not found"},
		{name: "empty old", file: file, old: "", new: "x", errContains: "write_file"},
		{name: "no-op", file: file, old: "a", new: "a", errContains: "identical"},

		// fallback: trailing whitespace in the file that the model dropped
		{name: "fuzzy trailing spaces", file: "a  \nport = 8000\t\nb\n",
			old: "a\nport = 8000", new: "a\nport = 9000",
			want: "a\nport = 9000\nb\n", wantN: 1},
		// fallback: CRLF file, LF old_string; the edit must stay CRLF
		{name: "fuzzy crlf", file: "a\r\nport = 8000\r\nb\r\n",
			old: "a\nport = 8000", new: "a\nport = 9000",
			want: "a\r\nport = 9000\r\nb\r\n", wantN: 1},
		{name: "fuzzy crlf single line", file: "a\r\nb\r\nc\r\n",
			old: "b", new: "B",
			want: "a\r\nB\r\nc\r\n", wantN: 1},
		{name: "fuzzy trailing newline on old", file: "a\nb\nc\n",
			old: "b \n", new: "B\n",
			want: "a\nB\nc\n", wantN: 1},
		{name: "fuzzy ambiguous", file: "x q\na\nx q\nb\n",
			old: "x q ", new: "y", errContains: "2 places"},
		{name: "fuzzy replace all", file: "x\na\nx\nb\n",
			old: "x ", new: "y", all: true,
			want: "y\na\ny\nb\n", wantN: 2},
		{name: "fuzzy last line no terminator", file: "a\nb",
			old: "b ", new: "B",
			want: "a\nB", wantN: 1},
		// an all-whitespace old_string must not fuzzy-match blank lines
		{name: "whitespace-only old", file: "a\n\n\nb\n",
			old: " \n ", new: "x", errContains: "not found"},
		// the reverse slip: CRLF strings from the model, LF file — no stray \r
		{name: "fuzzy lf file crlf strings", file: "a\nfoo\nb\n",
			old: "foo\r", new: "FOO\r",
			want: "a\nFOO\nb\n", wantN: 1},
		{name: "fuzzy lf file crlf pair", file: "a\nfoo\nbar\nb\n",
			old: "foo\r\nbar\r", new: "FOO\r\nBAR\r",
			want: "a\nFOO\nBAR\nb\n", wantN: 1},
		// the phantom empty line after a final newline is not matchable
		{name: "phantom eof line", file: "a\nb\n",
			old: "b\n\n", new: "B\n", errContains: "not found"},
		// whole-line deletion swallows the terminator — no blank line left
		{name: "fuzzy line deletion", file: "a\nb \nc\n",
			old: "b\n", new: "",
			want: "a\nc\n", wantN: 1},
		{name: "fuzzy line deletion crlf", file: "a\r\nb \r\nc\r\n",
			old: "b\n", new: "",
			want: "a\r\nc\r\n", wantN: 1},
		// an interior blank line is a real line and still matches
		{name: "interior blank line", file: "a\nb \n\nc\n",
			old: "b\n\nc", new: "B\nC",
			want: "a\nB\nC\n", wantN: 1},

		// diagnostics
		{name: "over-escaped", file: "a\nb\nc\n",
			old: `a\nb`, new: "a\nB", errContains: "escape sequences"},
		{name: "near miss", file: "a\nfoo(1)\nbar(2)\nc\n",
			old: "foo(1)\nbar(3)", new: "x", errContains: "diverges at line 3"},
		{name: "nowhere", file: "a\nb\nc\n",
			old: "zzz\nqqq", new: "x", errContains: "appears nowhere"},
	}
	for _, tt := range tests {
		got, n, err := ReplaceEdit(tt.file, tt.old, tt.new, tt.all)
		if tt.errContains != "" {
			if err == nil || !strings.Contains(err.Error(), tt.errContains) {
				t.Errorf("%s: err = %v, want containing %q", tt.name, err, tt.errContains)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error: %v", tt.name, err)
			continue
		}
		if got != tt.want || n != tt.wantN {
			t.Errorf("%s: got (%q, %d), want (%q, %d)", tt.name, got, n, tt.want, tt.wantN)
		}
	}
}

func TestEscapeDetectorSkipsLiteralBackslashes(t *testing.T) {
	// old_string quoting source with an escaped backslash (\\n as bytes)
	// must not be accused of over-escaping, even when the decoded form
	// happens to appear in the file
	_, _, err := ReplaceEdit("x\nprintf(\"\\\ny\n", `printf("\\n`, "z", false)
	if err == nil || strings.Contains(err.Error(), "escape sequences") {
		t.Errorf("literal backslash content misdiagnosed as over-escaping: %v", err)
	}
	// genuine over-escaping is still detected
	_, _, err = ReplaceEdit("a\nb\nc\n", `a\nb`, "q", false)
	if err == nil || !strings.Contains(err.Error(), "escape sequences") {
		t.Errorf("over-escaped old_string not diagnosed: %v", err)
	}
}

func TestClipRuneBoundary(t *testing.T) {
	got := clip(strings.Repeat("a", 76) + "世界世界")
	if !utf8.ValidString(strings.TrimSuffix(got, "...")) {
		t.Errorf("clip split a rune: %q", got)
	}
}

func TestNotFoundErrLargeFile(t *testing.T) {
	big := strings.Repeat("filler line\n", 2000) // > readElide bytes
	_, _, err := ReplaceEdit(big, "no such text", "x", false)
	if err == nil || !strings.Contains(err.Error(), "grep -n") {
		t.Errorf("large-file miss should point at grep -n, got: %v", err)
	}
	_, _, err = ReplaceEdit("small\n", "no such text", "x", false)
	if err == nil || !strings.Contains(err.Error(), "read_file to check") {
		t.Errorf("small-file miss should point at read_file, got: %v", err)
	}
}

// A Mac OS X Tiger guest's sshd (OpenSSH 3.8 to 4.5) may offer nothing in
// the library's default lists: the legacy lists carry everything the
// library implements, current algorithms first.
func TestLegacyAlgorithmsOfferTigerSomethingAndTheCurrentOnesFirst(t *testing.T) {
	cfg := &ssh.ClientConfig{}
	legacyAlgorithms(cfg)
	has := func(list []string, want string) bool {
		for _, s := range list {
			if s == want {
				return true
			}
		}
		return false
	}
	for list, want := range map[*[]string]string{
		&cfg.KeyExchanges:      "diffie-hellman-group1-sha1",
		&cfg.HostKeyAlgorithms: "ssh-rsa",
		&cfg.Ciphers:           "aes128-cbc",
		&cfg.MACs:              "hmac-sha1",
	} {
		if !has(*list, want) {
			t.Errorf("legacy lists lack %s: %v", want, *list)
		}
	}
	cur := ssh.SupportedAlgorithms()
	if cfg.KeyExchanges[0] != cur.KeyExchanges[0] || cfg.Ciphers[0] != cur.Ciphers[0] {
		t.Errorf("a current algorithm should lead each list: %v / %v", cfg.KeyExchanges[:2], cfg.Ciphers[:2])
	}
}

// Tiger's userland is BSD's of 2005: a bare mktemp exits 1, head has no -c,
// and dd prints statistics into the merged output. The file commands stay
// clear of all three.
func TestFileCommandsRunOnTigerAsOnLinux(t *testing.T) {
	w := writeCmd("/Users/exe/Desktop/a b.txt")
	if !strings.Contains(w, `mktemp "${TMPDIR:-/tmp}/exe.XXXXXX"`) || strings.Contains(w, "mktemp)") {
		t.Errorf("write: mktemp needs its template: %s", w)
	}
	if !strings.Contains(w, `'/Users/exe/Desktop/a b.txt'`) {
		t.Errorf("write: the path is not quoted: %s", w)
	}
	r := editReadCmd("/etc/hosts")
	for _, bad := range []string{"head -c", "dd "} {
		if strings.Contains(r, bad) {
			t.Errorf("edit read uses %q: %s", bad, r)
		}
	}
	if !strings.Contains(r, "wc -c <") || !strings.Contains(r, fmt.Sprintf("-gt %d", editMax)) || !strings.Contains(r, fmt.Sprintf("exit %d", editTooLarge)) {
		t.Errorf("edit read should measure with wc and answer a large file with exit %d: %s", editTooLarge, r)
	}
}

// The commands run through a local sh the way they run on the far side:
// a file of exactly editMax bytes is read whole, one byte more is refused
// with editTooLarge and left as it was, and a missing file keeps the
// shell's own words about it in the output.
func TestEditReadCmdBoundaryInLocalShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	run := func(cmd string) (string, int) {
		out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), code
	}
	exact := filepath.Join(dir, "exact")
	os.WriteFile(exact, bytes.Repeat([]byte{'x'}, editMax), 0o644)
	if out, code := run(editReadCmd(exact)); code != 0 || len(out) != editMax {
		t.Errorf("exactly editMax bytes: exit %d, %d bytes read", code, len(out))
	}
	over := filepath.Join(dir, "over")
	os.WriteFile(over, bytes.Repeat([]byte{'x'}, editMax+1), 0o644)
	if out, code := run(editReadCmd(over)); code != editTooLarge || out != "" {
		t.Errorf("editMax+1 bytes: exit %d (want %d), %d bytes read (want 0)", code, editTooLarge, len(out))
	}
	if st, err := os.Stat(over); err != nil || st.Size() != editMax+1 {
		t.Errorf("the oversized file changed: %v %v", st, err)
	}
	if out, code := run(editReadCmd(filepath.Join(dir, "missing"))); code != 1 || !strings.Contains(out, "missing") {
		t.Errorf("missing file: exit %d, output %q: the shell's words about it should be kept", code, out)
	}
}
