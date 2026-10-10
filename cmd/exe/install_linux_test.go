//go:build linux

package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"exe/internal/release"
)

// Every test here installs into a folder of its own: the layout is handed
// in, never read from HOME or EXE_HOME, and the host is a stand-in that
// runs nothing — no systemctl, no sudo, no network.

type fakeHost struct {
	*host
	out    bytes.Buffer
	ran    []string // commands run with the terminal attached, as typed
	quiet  []string
	env    map[string]string
	busy   map[string]bool // addresses something else listens on
	tools  map[string]bool
	groups map[string][2]bool // name → exists, member
	helper struct {
		sum    string
		rooted bool
	}
	noSystemd bool
	kvmOpens  bool
	fetched   int
}

func newFake(t *testing.T, input string) (*fakeHost, layout) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home", "ada")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeHost{
		env:    map[string]string{"PATH": "/usr/bin:/bin"},
		busy:   map[string]bool{},
		tools:  map[string]bool{"ip": true, "iptables": true, "debugfs": true, "resize2fs": true, "setcap": true, "apt-get": true, "setfacl": true},
		groups: map[string][2]bool{},
	}
	f.host = &host{
		User: "ada", Group: "ada", Home: home, Tty: true,
		Env:      func(k string) string { return f.env[k] },
		In:       bufio.NewReader(strings.NewReader(input)),
		Out:      &f.out,
		PortFree: func(addr string) bool { return !f.busy[addr] },
		LookTool: func(name string) string {
			if f.tools[name] {
				return "/usr/bin/" + name
			}
			return ""
		},
		Run:   func(argv ...string) error { f.ran = append(f.ran, strings.Join(argv, " ")); return nil },
		Reach: func(string) bool { return true },
		GroupInfo: func(name string) (bool, bool) {
			g := f.groups[name]
			return g[0], g[1]
		},
		OpenKVM: func() bool { return f.kvmOpens },
		Helper:  func(string) (string, bool) { return f.helper.sum, f.helper.rooted },
		Firecracker: func(dir string) (string, error) {
			f.fetched++
			p := filepath.Join(dir, "firecracker")
			return p, os.WriteFile(p, []byte("firecracker"), 0o755)
		},
	}
	f.host.Quiet = func(argv ...string) error {
		f.quiet = append(f.quiet, strings.Join(argv, " "))
		if f.noSystemd && argv[0] == "systemctl" {
			return errors.New("Failed to connect to bus")
		}
		return nil
	}
	l := layout{
		Bin:   filepath.Join(home, ".local", "bin", "exe"),
		State: filepath.Join(home, ".exe"),
		Unit:  filepath.Join(home, ".config", "systemd", "user", "exe.service"),
	}
	return f, l
}

func setVersion(t *testing.T, v string) {
	t.Helper()
	old := release.Version
	release.Version = v
	t.Cleanup(func() { release.Version = old })
}

func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		mode := int64(0o644)
		if !strings.Contains(name, "/") {
			mode = 0o755
		}
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func appsTgz(t *testing.T, body string, names ...string) []byte {
	files := map[string]string{"README.md": "not an app"}
	for _, n := range names {
		files[n+"/app.json"] = `{"title":"` + n + `"}`
		files[n+"/index.html"] = body
	}
	return tgz(t, files)
}

// unpacked is a release as install.sh leaves it for `exe setup -from`.
func unpacked(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "exe"), []byte("the released exe"), 0o755)
	os.WriteFile(filepath.Join(dir, "exe-net-helper"), []byte("the helper"), 0o755)
	os.WriteFile(filepath.Join(dir, release.AppsAsset), appsTgz(t, "one", "Notes", "Todo", "World Clock"), 0o644)
	return dir
}

func readConfig(t *testing.T, l layout) firstConfig {
	t.Helper()
	b, err := os.ReadFile(l.Config())
	if err != nil {
		t.Fatal(err)
	}
	var c firstConfig
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// Return on every question: this machine only, no token, the apps, no sudo.
func TestSetupReturnOnEverything(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l := newFake(t, "\n\n\n\n")
	f.KVM = true
	f.groups["kvm"] = [2]bool{true, false}
	if err := runSetup(l, f.host, unpacked(t)); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	out := f.out.String()
	c := readConfig(t, l)
	if c.Listen != "127.0.0.1:7777" || c.ProxyListen != "127.0.0.1:8090" || c.SSHListen != "127.0.0.1:2222" {
		t.Errorf("listeners %+v — this machine only means all three on loopback", c)
	}
	if c.APIToken != "" || c.AdvertiseHost != "" {
		t.Errorf("config %+v", c)
	}
	if st, err := os.Stat(l.Config()); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("config mode %v, %v", st.Mode(), err)
	}
	if b, _ := os.ReadFile(l.Bin); string(b) != "the released exe" {
		t.Errorf("binary at %s = %q", l.Bin, b)
	}
	if st, _ := os.Stat(l.Bin); st == nil || st.Mode().Perm()&0o100 == 0 {
		t.Error("the binary is not runnable")
	}
	if b, _ := os.ReadFile(l.Helper()); string(b) != "the helper" {
		t.Errorf("staged helper = %q", b)
	}
	for _, app := range []string{"Notes", "Todo", "World Clock"} {
		if _, err := os.Stat(filepath.Join(l.Apps(), app, "index.html")); err != nil {
			t.Errorf("%s was not installed: %v", app, err)
		}
	}
	if _, err := os.Stat(filepath.Join(l.Apps(), "README.md")); err == nil {
		t.Error("a file that is no app was put in the apps folder")
	}
	if len(f.ran) != 0 {
		t.Errorf("Return on the VM question ran %v", f.ran)
	}
	for _, want := range []string{"systemctl --user daemon-reload", "systemctl --user enable exe", "systemctl --user restart exe", "loginctl enable-linger"} {
		if !has(f.quiet, want) {
			t.Errorf("%q was not run; ran %v", want, f.quiet)
		}
	}
	unit, _ := os.ReadFile(l.Unit)
	if !strings.HasPrefix(string(unit), unitMark) || !strings.Contains(string(unit), "ExecStart="+l.Bin+" serve\n") {
		t.Errorf("unit:\n%s", unit)
	}
	for _, want := range []string{
		"exe 2026.10.09 for Linux",
		"1. Where should exe listen?",
		"2. Require an API token?",
		"3. Install the extra desktop apps?\n   Notes, Todo and World Clock (0.0 MB, into ~/.exe/apps)",
		"4. Set this machine up to run VMs? This needs sudo, and would run:",
		"     sudo usermod -aG kvm ada\n",
		"exe 2026.10.09 is running.",
		"  Desk     http://127.0.0.1:7777\n",
		"~/.local/bin is not on your PATH",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the installer did not say %q; it said:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Tailscale") {
		t.Errorf("Tailscale was offered on a machine without it:\n%s", out)
	}
	if strings.Contains(out, "Token ") {
		t.Errorf("a token was shown though none was set:\n%s", out)
	}
}

// Beyond this machine there is no asking: a token is made and shown.
func TestSetupAllInterfacesAlwaysHasAToken(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l := newFake(t, "2\n\n")
	f.LANIP = "192.168.1.20"
	if err := runSetup(l, f.host, unpacked(t)); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	c := readConfig(t, l)
	if c.Listen != ":7777" || c.ProxyListen != ":8090" || c.SSHListen != ":2222" {
		t.Errorf("listeners %+v", c)
	}
	if len(c.APIToken) != 32 {
		t.Fatalf("api_token = %q, want one that was generated", c.APIToken)
	}
	if strings.Contains(out, "Require an API token?") {
		t.Error("the token was asked about though it is not a choice here")
	}
	for _, want := range []string{"2. API token: required when exe listens on every interface", "  Token    " + c.APIToken, "  Desk     http://192.168.1.20:7777\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("did not say %q:\n%s", want, out)
		}
	}
}

func TestSetupTailscale(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l := newFake(t, "3\n\n\n")
	f.TailscaleIP = "100.64.0.7"
	if err := runSetup(l, f.host, unpacked(t)); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	c := readConfig(t, l)
	if c.Listen != "100.64.0.7:7777" || c.ProxyListen != "100.64.0.7:8090" || c.SSHListen != "100.64.0.7:2222" || c.AdvertiseHost != "100.64.0.7" {
		t.Errorf("listeners %+v", c)
	}
	if len(c.APIToken) != 32 {
		t.Errorf("Return on the tailnet's token question left api_token = %q; the default there is yes", c.APIToken)
	}
	if !strings.Contains(out, "     3) Tailscale              100.64.0.7\n") || !strings.Contains(out, "Other devices on your tailnet") {
		t.Errorf("questions:\n%s", out)
	}

	// and the token can be declined there
	f, l = newFake(t, "tailscale\nn\n\n")
	f.TailscaleIP = "100.64.0.7"
	if err := runSetup(l, f.host, unpacked(t)); err != nil {
		t.Fatal(err)
	}
	if c := readConfig(t, l); c.APIToken != "" || c.Listen != "100.64.0.7:7777" {
		t.Errorf("config %+v", c)
	}
}

// An answer that is no choice is asked again; 3 is none without Tailscale.
func TestSetupAsksAgain(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l := newFake(t, "3\nelsewhere\n1\nmaybe\ny\nn\n")
	if err := runSetup(l, f.host, unpacked(t)); err != nil {
		t.Fatal(err)
	}
	c := readConfig(t, l)
	if c.Listen != "127.0.0.1:7777" || len(c.APIToken) != 32 {
		t.Errorf("config %+v", c)
	}
	if n := strings.Count(f.out.String(), "Choice [1]:"); n != 3 {
		t.Errorf("the choice was asked %d times, want 3", n)
	}
	if _, err := os.Stat(filepath.Join(l.Apps(), "Notes")); err == nil {
		t.Error("the apps were installed though the answer was no")
	}
	if _, err := os.Stat(l.AppsManifest()); err == nil {
		t.Error("declined apps left a record, so an update would bring them")
	}
}

// A port something else holds is passed over, and the installer says so.
func TestSetupPassesOverABusyPort(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l := newFake(t, "\n\n\n")
	f.busy["127.0.0.1:8090"] = true
	f.busy["127.0.0.1:8091"] = true
	if err := runSetup(l, f.host, unpacked(t)); err != nil {
		t.Fatal(err)
	}
	c := readConfig(t, l)
	if c.Listen != "127.0.0.1:7777" || c.ProxyListen != "127.0.0.1:8092" || c.SSHListen != "127.0.0.1:2222" {
		t.Errorf("listeners %+v", c)
	}
	if !strings.Contains(f.out.String(), "Port 8090 is taken on this machine, so the proxy is on 8092.") {
		t.Errorf("the move was not said:\n%s", f.out.String())
	}

	// the desk on a tailnet address keeps a 127.0.0.1 companion: both must be free
	f, l = newFake(t, "3\nn\n\n")
	f.TailscaleIP = "100.64.0.7"
	f.busy["127.0.0.1:7777"] = true
	if err := runSetup(l, f.host, unpacked(t)); err != nil {
		t.Fatal(err)
	}
	if c := readConfig(t, l); c.Listen != "100.64.0.7:7778" {
		t.Errorf("listen = %s", c.Listen)
	}
}

// Installing over an install keeps its configuration and asks nothing about it.
func TestSetupKeepsAConfiguration(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l := newFake(t, "\n")
	os.MkdirAll(l.State, 0o755)
	mine := `{"listen":"10.0.0.5:9000","api_token":"mine"}`
	os.WriteFile(l.Config(), []byte(mine), 0o600)
	if err := runSetup(l, f.host, unpacked(t)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(l.Config()); string(b) != mine {
		t.Fatalf("the configuration was rewritten: %s", b)
	}
	out := f.out.String()
	if strings.Contains(out, "Where should exe listen?") || strings.Contains(out, "API token") {
		t.Errorf("a kept configuration was asked about:\n%s", out)
	}
	if !strings.Contains(out, "1. Install the extra desktop apps?") || !strings.Contains(out, "  Desk     http://10.0.0.5:9000\n") {
		t.Errorf("said:\n%s", out)
	}
	if strings.Contains(out, "mine") {
		t.Error("a token the installer did not make was printed")
	}
}

// A unit somebody else wrote — a checkout's own deployment — is not ours
// to rewrite, and neither is the daemon it runs ours to restart.
func TestSetupLeavesAForeignUnit(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l := newFake(t, "\n\n\n")
	theirs := "[Service]\nExecStart=/www/exe/exe serve\n"
	os.MkdirAll(filepath.Dir(l.Unit), 0o755)
	os.WriteFile(l.Unit, []byte(theirs), 0o644)
	if err := runSetup(l, f.host, unpacked(t)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(l.Unit); string(b) != theirs {
		t.Fatalf("the unit was rewritten:\n%s", b)
	}
	for _, q := range f.quiet {
		if strings.HasPrefix(q, "systemctl") {
			t.Errorf("ran %q against a service that is not the installer's", q)
		}
	}
	if out := f.out.String(); !strings.Contains(out, "exe 2026.10.09 is installed.") || !strings.Contains(out, "was not written by this installer") {
		t.Errorf("said:\n%s", out)
	}
}

func TestSetupWithoutSystemd(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l := newFake(t, "\n\n\n")
	f.noSystemd = true
	if err := runSetup(l, f.host, unpacked(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.Unit); err == nil {
		t.Error("a unit was written where no user systemd runs")
	}
	if out := f.out.String(); !strings.Contains(out, "Start it with: exe serve") || !strings.Contains(out, "is installed.") {
		t.Errorf("said:\n%s", out)
	}
}

// The VM step lists what it will run, runs exactly that, and only on a yes.
func TestSetupVMStep(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l := newFake(t, "\n\n\ny\n")
	f.KVM = true
	f.groups["kvm"] = [2]bool{true, false}
	f.tools["iptables"], f.tools["setcap"] = false, false // firecracker is not in tools either
	if err := runSetup(l, f.host, unpacked(t)); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	out := f.out.String()
	want := []string{
		"sudo usermod -aG kvm ada",
		"sudo setfacl -m u:ada:rw /dev/kvm",
		"sudo install -m 0755 ", // Firecracker, from a scratch folder
		"sudo apt-get install -y iptables libcap2-bin",
		"sudo install -D -o root -g ada -m 0750 " + l.Helper() + " /usr/local/libexec/exe-net-helper",
		"sudo setcap cap_net_admin=ep /usr/local/libexec/exe-net-helper",
	}
	if len(f.ran) != len(want) {
		t.Fatalf("ran %d commands:\n%s", len(f.ran), strings.Join(f.ran, "\n"))
	}
	for i, w := range want {
		if !strings.HasPrefix(f.ran[i], w) {
			t.Errorf("command %d = %q, want %q", i+1, f.ran[i], w)
		}
	}
	if !strings.HasSuffix(f.ran[2], "/firecracker /usr/local/bin/firecracker") || f.fetched != 1 {
		t.Errorf("Firecracker: %q, fetched %d times", f.ran[2], f.fetched)
	}
	// each was listed in the question, before the answer
	question := out[:strings.Index(out, "You can do this later with: exe setup vms")]
	for _, w := range []string{
		"     sudo usermod -aG kvm ada\n",
		"     sudo setfacl -m u:ada:rw /dev/kvm   # until the kvm group applies, at the next boot\n",
		"/firecracker /usr/local/bin/firecracker   # Firecracker v1.16.1, downloaded first\n",
		"     sudo apt-get install -y iptables libcap2-bin\n",
		"     sudo install -D -o root -g ada -m 0750 ~/.exe/release/exe-net-helper /usr/local/libexec/exe-net-helper\n",
		"     sudo setcap cap_net_admin=ep /usr/local/libexec/exe-net-helper\n",
	} {
		if !strings.Contains(question, w) {
			t.Errorf("the question did not list %q:\n%s", w, question)
		}
	}
	// the helper is staged before root is asked to install it
	if _, err := os.Stat(l.Helper()); err != nil {
		t.Error(err)
	}
}

// A machine that has it all is asked nothing about VMs.
func TestSetupVMStepWithNothingToDo(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l := newFake(t, "\n\n\n")
	f.KVM = true
	f.groups["kvm"] = [2]bool{true, true}
	f.tools["firecracker"] = true
	from := unpacked(t)
	sum := sha256.Sum256([]byte("the helper"))
	f.helper.sum, f.helper.rooted = hex.EncodeToString(sum[:]), true
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	if out := f.out.String(); strings.Contains(out, "run VMs?") || strings.Contains(out, "/dev/kvm") {
		t.Errorf("asked about VMs with nothing to do:\n%s", out)
	}
}

func TestSetupWithoutKVM(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l := newFake(t, "\n\n\n")
	if err := runSetup(l, f.host, unpacked(t)); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	if strings.Contains(out, "run VMs?") || !strings.Contains(out, "There is no /dev/kvm on this machine: exe will run the desktop without VMs.") {
		t.Errorf("said:\n%s", out)
	}
}

// With nobody at a keyboard nothing is asked: the careful install, or the
// answers the environment gives.
func TestSetupWithNobodyToAsk(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l := newFake(t, "all\ny\n") // input that must not be read
	f.Tty = false
	f.KVM = true
	f.groups["kvm"] = [2]bool{true, false}
	if err := runSetup(l, f.host, unpacked(t)); err != nil {
		t.Fatal(err)
	}
	c := readConfig(t, l)
	if c.Listen != "127.0.0.1:7777" || c.APIToken != "" || len(f.ran) != 0 {
		t.Errorf("config %+v, ran %v", c, f.ran)
	}
	if _, err := os.Stat(filepath.Join(l.Apps(), "Notes")); err != nil {
		t.Error("the apps are part of the default install")
	}
	if out := f.out.String(); strings.Contains(out, "?") {
		t.Errorf("a question was put to nobody:\n%s", out)
	}

	f, l = newFake(t, "")
	f.Tty = false
	f.KVM = true
	f.groups["kvm"] = [2]bool{true, false}
	f.kvmOpens = true
	f.env[envListen], f.env[envApps], f.env[envVMs], f.env[envTokenValue] = "all", "no", "yes", "HelloWorld"
	if err := runSetup(l, f.host, unpacked(t)); err != nil {
		t.Fatal(err)
	}
	c = readConfig(t, l)
	if c.Listen != ":7777" || c.APIToken != "HelloWorld" {
		t.Errorf("config %+v", c)
	}
	if _, err := os.Stat(filepath.Join(l.Apps(), "Notes")); err == nil {
		t.Error("the apps were installed against EXE_INSTALL_APPS=no")
	}
	if len(f.ran) == 0 || !strings.HasPrefix(f.ran[0], "sudo -n usermod -aG kvm ada") {
		t.Errorf("ran %v — with nobody to type a password, sudo must not ask for one", f.ran)
	}

	f, l = newFake(t, "")
	f.Tty = false
	f.env[envListen] = "tailscale"
	if err := runSetup(l, f.host, unpacked(t)); err == nil {
		t.Error("EXE_INSTALL_LISTEN=tailscale passed on a machine without Tailscale")
	}
	if _, err := os.Stat(l.Bin); err == nil {
		t.Error("a refused install left a binary behind")
	}
}

func TestSetupAsRoot(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l := newFake(t, "\n\n\ny\n")
	f.Root, f.User, f.Group = true, "root", "root"
	f.KVM = true
	f.groups["kvm"] = [2]bool{true, false}
	if err := runSetup(l, f.host, unpacked(t)); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.ran {
		if strings.HasPrefix(r, "sudo") || strings.HasPrefix(r, "usermod") {
			t.Errorf("root ran %q", r)
		}
	}
	if out := f.out.String(); strings.Contains(out, "sudo") {
		t.Errorf("root was told about sudo:\n%s", out)
	}
}

func TestUnitText(t *testing.T) {
	home := "/home/ada"
	l := layout{Bin: home + "/.local/bin/exe", State: home + "/.exe", Unit: home + "/.config/systemd/user/exe.service"}
	u := unitText(l, home)
	for _, want := range []string{
		"ExecStart=/home/ada/.local/bin/exe serve\n",
		"Environment=PATH=/home/ada/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin\n",
		"KillMode=process\n", "Restart=always\n", "WantedBy=default.target\n",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit lacks %q:\n%s", want, u)
		}
	}
	if strings.Contains(u, "EXE_HOME") {
		t.Error("EXE_HOME was pinned for the default state folder")
	}
	l.State = "/srv/exe data"
	if u := unitText(l, home); !strings.Contains(u, "Environment=\"EXE_HOME=/srv/exe data\"\n") {
		t.Errorf("unit for a state folder of its own:\n%s", u)
	}
}

func TestUninstall(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l := newFake(t, "\n\n\n")
	if err := runSetup(l, f.host, unpacked(t)); err != nil {
		t.Fatal(err)
	}
	// the owner has edited Todo and written an app and some app data
	os.WriteFile(filepath.Join(l.Apps(), "Todo", "index.html"), []byte("edited"), 0o644)
	os.MkdirAll(filepath.Join(l.Apps(), "Mine"), 0o755)
	os.WriteFile(filepath.Join(l.Apps(), "Mine", "app.json"), []byte("{}"), 0o644)
	os.MkdirAll(filepath.Join(l.State, "appdata", "Notes"), 0o755)
	os.WriteFile(filepath.Join(l.State, "appdata", "Notes", "notes.json"), []byte("[]"), 0o644)
	f.Tty = false
	if err := runUninstall(l, f.host, false); err == nil {
		t.Fatal("uninstall went ahead with nobody to ask and no -y")
	}
	if _, err := os.Stat(l.Bin); err != nil {
		t.Fatal("the refused uninstall removed the binary")
	}
	if err := runUninstall(l, f.host, true); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{l.Bin, l.Unit, l.Release(), filepath.Join(l.Apps(), "Notes"), filepath.Join(l.Apps(), "World Clock")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s is still there (%v)", gone, err)
		}
	}
	for _, kept := range []string{
		l.Config(),
		filepath.Join(l.Apps(), "Todo", "index.html"), // edited: its owner's now
		filepath.Join(l.Apps(), "Mine", "app.json"),
		filepath.Join(l.State, "appdata", "Notes", "notes.json"),
	} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("%s is the owner's and was removed", kept)
		}
	}
	if out := f.out.String(); !strings.Contains(out, "Removed the apps it installed: Notes and World Clock\n") || !strings.Contains(out, "Kept, because you edited it: Todo\n") {
		t.Errorf("said:\n%s", out)
	}
	if !has(f.quiet, "systemctl --user disable --now exe") {
		t.Errorf("the service was not stopped; ran %v", f.quiet)
	}
}

// ---- exe update ----

type updateRig struct {
	*fakeHost
	u        *updater
	l        layout
	restarts int
	running  bool
	said     string // what the downloaded binary answers to `version`
}

// rig installs 2026.10.09 into a folder of its own and publishes latest
// on a stand-in for the release address.
func rig(t *testing.T, latest string, tamper bool) *updateRig {
	t.Helper()
	setVersion(t, "2026.10.09")
	f, l := newFake(t, "\n\n\n")
	if err := runSetup(l, f.host, unpacked(t)); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	f.quiet = nil

	bin := tgz(t, map[string]string{"exe": "exe " + latest, "exe-net-helper": "helper " + latest})
	apps := appsTgz(t, "two", "Notes", "Todo", "World Clock", "Weather")
	sums := ""
	for name, b := range map[string][]byte{release.BinaryAsset(runtime.GOARCH): bin, release.AppsAsset: apps} {
		s := sha256.Sum256(b)
		sums += hex.EncodeToString(s[:]) + "  " + name + "\n"
	}
	if tamper {
		bin = tgz(t, map[string]string{"exe": "not what was released", "exe-net-helper": "x"})
	}
	files := map[string][]byte{release.BinaryAsset(runtime.GOARCH): bin, release.AppsAsset: apps, release.SumsAsset: []byte(sums)}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/"+latest, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/"+latest+"/", func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[filepath.Base(r.URL.Path)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	r := &updateRig{fakeHost: f, l: l, running: true, said: "exe " + latest + " (linux/" + runtime.GOARCH + ")"}
	r.u = &updater{
		Layout: l, Host: f.host,
		Client:  &release.Client{Base: srv.URL + "/releases", HTTP: srv.Client()},
		Probe:   func(string) (string, error) { return r.said, nil },
		Running: func() bool { return r.running },
		Restart: func() error { r.restarts++; return nil },
	}
	return r
}

func (r *updateRig) binary() string {
	b, _ := os.ReadFile(r.l.Bin)
	return string(b)
}

func TestUpdateWhenCurrent(t *testing.T) {
	for _, latest := range []string{"2026.10.09", "2026.10.08"} {
		r := rig(t, latest, false)
		if err := r.u.run(context.Background(), false, true); err != nil {
			t.Fatal(err)
		}
		if out := r.out.String(); out != "exe 2026.10.09 is the latest version.\n" {
			t.Errorf("latest %s: said %q", latest, out)
		}
		if r.binary() != "the released exe" || r.restarts != 0 {
			t.Errorf("latest %s: the binary changed or the daemon restarted", latest)
		}
	}
}

func TestUpdateCheckOnlyLooks(t *testing.T) {
	r := rig(t, "2026.10.10", false)
	if err := r.u.run(context.Background(), true, false); err != nil {
		t.Fatal(err)
	}
	if out := r.out.String(); !strings.Contains(out, "exe 2026.10.10 is available; this is 2026.10.09.") {
		t.Errorf("said %q", out)
	}
	if r.binary() != "the released exe" {
		t.Error("-check replaced the binary")
	}
}

func TestUpdate(t *testing.T) {
	r := rig(t, "2026.10.09.2", false)
	// between install and update: Notes was edited, and root has the old helper
	os.WriteFile(filepath.Join(r.l.Apps(), "Notes", "index.html"), []byte("edited"), 0o644)
	old := sha256.Sum256([]byte("the helper"))
	r.helper.sum, r.helper.rooted = hex.EncodeToString(old[:]), true

	if err := r.u.run(context.Background(), false, true); err != nil {
		t.Fatalf("%v\n%s", err, r.out.String())
	}
	out := r.out.String()
	if r.binary() != "exe 2026.10.09.2" {
		t.Errorf("binary = %q", r.binary())
	}
	if st, _ := os.Stat(r.l.Bin); st.Mode().Perm()&0o100 == 0 {
		t.Error("the new binary is not runnable")
	}
	if _, err := os.Stat(r.l.Bin + ".new"); err == nil {
		t.Error("the staging file was left beside the binary")
	}
	if b, _ := os.ReadFile(r.l.Helper()); string(b) != "helper 2026.10.09.2" {
		t.Errorf("staged helper = %q", b)
	}
	read := func(app string) string {
		b, _ := os.ReadFile(filepath.Join(r.l.Apps(), app, "index.html"))
		return string(b)
	}
	if read("Todo") != "two" || read("Weather") != "two" {
		t.Errorf("apps did not follow the release: Todo %q, Weather %q", read("Todo"), read("Weather"))
	}
	if read("Notes") != "edited" || !strings.Contains(out, "Notes is left as it is: you edited it.") {
		t.Errorf("an edited app: %q\n%s", read("Notes"), out)
	}
	if !strings.Contains(out, "This release has a new network helper.") ||
		!strings.Contains(out, "  sudo install -D -o root -g ada -m 0750 ~/.exe/release/exe-net-helper /usr/local/libexec/exe-net-helper\n") {
		t.Errorf("the helper that only root can replace was not mentioned:\n%s", out)
	}
	if r.restarts != 1 || !strings.Contains(out, "exe is restarting into 2026.10.09.2.") {
		t.Errorf("restarts = %d\n%s", r.restarts, out)
	}
	if len(r.ran) != 0 {
		t.Errorf("an update ran %v", r.ran)
	}
}

// A restart stops every VM, so it happens on a yes or a -y, never by itself.
func TestUpdateAsksBeforeRestarting(t *testing.T) {
	r := rig(t, "2026.10.10", false)
	r.In = bufio.NewReader(strings.NewReader("n\n"))
	if err := r.u.run(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if r.restarts != 0 || r.binary() != "exe 2026.10.10" || !strings.Contains(r.out.String(), "Restart exe now? Running VMs stop and start again.") {
		t.Errorf("restarts %d, binary %q\n%s", r.restarts, r.binary(), r.out.String())
	}

	r = rig(t, "2026.10.10", false)
	r.In = bufio.NewReader(strings.NewReader("\n"))
	if err := r.u.run(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if r.restarts != 1 {
		t.Error("Return on the restart question did not restart")
	}

	r = rig(t, "2026.10.10", false)
	r.Tty = false
	if err := r.u.run(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if r.restarts != 0 || !strings.Contains(r.out.String(), "The daemon keeps running 2026.10.09 until it restarts\n") {
		t.Errorf("with nobody to ask: restarts %d\n%s", r.restarts, r.out.String())
	}

	// under the installer's service, the way to restart later is said
	r = rig(t, "2026.10.10", false)
	r.Tty = false
	r.u.RestartHint = "systemctl --user restart exe"
	if err := r.u.run(context.Background(), false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.out.String(), "until you run: systemctl --user restart exe\n") {
		t.Errorf("the restart that was put off:\n%s", r.out.String())
	}

	r = rig(t, "2026.10.10", false)
	r.running = false
	if err := r.u.run(context.Background(), false, true); err != nil {
		t.Fatal(err)
	}
	if r.restarts != 0 || !strings.Contains(r.out.String(), "exe is not running; it starts as 2026.10.10.") {
		t.Errorf("a daemon that is down: restarts %d\n%s", r.restarts, r.out.String())
	}
}

// What does not match the release's checksums never replaces the binary.
func TestUpdateRefusesWhatWasNotReleased(t *testing.T) {
	r := rig(t, "2026.10.10", true)
	err := r.u.run(context.Background(), false, true)
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("a tampered download: %v", err)
	}
	if r.binary() != "the released exe" || r.restarts != 0 {
		t.Error("a tampered download replaced the binary")
	}

	// nor does a binary that runs but is some other version
	r = rig(t, "2026.10.10", false)
	r.said = "exe 2026.10.01 (linux/arm64)"
	if err := r.u.run(context.Background(), false, true); err == nil {
		t.Fatal("a binary that calls itself another version was installed")
	}
	if r.binary() != "the released exe" || r.restarts != 0 {
		t.Error("the wrong version replaced the binary")
	}

	r = rig(t, "2026.10.10", false)
	r.u.Probe = func(string) (string, error) { return "", errors.New("exec format error") }
	if err := r.u.run(context.Background(), false, true); err == nil || r.binary() != "the released exe" {
		t.Fatalf("a binary that does not run here: %v, binary %q", err, r.binary())
	}
}
