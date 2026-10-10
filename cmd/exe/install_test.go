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
	"os/exec"
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
	// a Mac's launchd, as far as the installer talks to it
	noGUI  bool // nobody is logged in at the screen
	loaded bool // the agent is loaded
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
		OS: "linux", User: "ada", Group: "ada", UID: "1000", Home: home, Tty: true,
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
		if argv[0] == "launchctl" {
			switch argv[1] {
			case "print":
				if f.noGUI || (strings.Count(argv[2], "/") == 2 && !f.loaded) {
					return errors.New("Could not find service")
				}
			case "bootstrap":
				if f.noGUI {
					return errors.New("Bootstrap failed: 125: Domain does not support specified action")
				}
				f.loaded = true
			case "bootout":
				f.loaded = false
			}
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
		"exe 2026.10.09 for " + platformWords(runtime.GOOS, runtime.GOARCH) + "\n",
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
	for name, b := range map[string][]byte{release.BinaryAsset(runtime.GOOS, runtime.GOARCH): bin, release.AppsAsset: apps} {
		s := sha256.Sum256(b)
		sums += hex.EncodeToString(s[:]) + "  " + name + "\n"
	}
	if tamper {
		bin = tgz(t, map[string]string{"exe": "not what was released", "exe-net-helper": "x"})
	}
	files := map[string][]byte{release.BinaryAsset(runtime.GOOS, runtime.GOARCH): bin, release.AppsAsset: apps, release.SumsAsset: []byte(sums)}
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

// ---- an install or an update that stops partway is run again ----

func readApp(l layout, app string) string {
	b, _ := os.ReadFile(filepath.Join(l.Apps(), app, "index.html"))
	return string(b)
}

// The review's case (hub thread 34b0c377): a step after the binary fails.
// The binary must not be in place yet — or the retry is the new binary,
// which finds itself up to date and repairs nothing — and running the
// update again must finish it.
func TestUpdateIsRunAgainAfterAFailure(t *testing.T) {
	r := rig(t, "2026.10.10", false)
	// the helper cannot be staged: a folder stands where its file goes
	os.Remove(r.l.Helper())
	os.MkdirAll(filepath.Join(r.l.Helper(), "in the way"), 0o755)

	err := r.u.run(context.Background(), false, true)
	if err == nil || !strings.Contains(err.Error(), "exe is still 2026.10.09; run `exe update` again") {
		t.Fatalf("a helper that cannot be staged: %v", err)
	}
	if r.binary() != "the released exe" {
		t.Fatal("the binary was replaced before the update was done: a retry would find nothing to do")
	}
	if _, err := os.Stat(r.l.Bin + ".new"); err == nil {
		t.Error("the copy of the new binary was left beside the old one")
	}
	if r.restarts != 0 {
		t.Error("a failed update restarted the daemon")
	}

	// the cause is removed, and the same update is run again
	os.RemoveAll(r.l.Helper())
	r.out.Reset()
	if err := r.u.run(context.Background(), false, true); err != nil {
		t.Fatalf("the update run again: %v\n%s", err, r.out.String())
	}
	if b, _ := os.ReadFile(r.l.Helper()); string(b) != "helper 2026.10.10" {
		t.Errorf("staged helper = %q", b)
	}
	if readApp(r.l, "Todo") != "two" || readApp(r.l, "Weather") != "two" {
		t.Errorf("apps: Todo %q, Weather %q", readApp(r.l, "Todo"), readApp(r.l, "Weather"))
	}
	if r.binary() != "exe 2026.10.10" || r.restarts != 1 {
		t.Errorf("binary %q, restarts %d", r.binary(), r.restarts)
	}
}

// The same when it is the apps that fail, after one of them was placed.
func TestUpdateIsRunAgainAfterAnAppFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root moves a folder it may not write to")
	}
	r := rig(t, "2026.10.10", false)
	// Todo cannot be moved out of the way (moving a folder to another
	// parent needs write permission on it); Notes, before it, can
	todo := filepath.Join(r.l.Apps(), "Todo")
	os.Chmod(todo, 0o555)
	t.Cleanup(func() { os.Chmod(todo, 0o755) })

	err := r.u.run(context.Background(), false, true)
	if err == nil || !strings.Contains(err.Error(), "Todo") || !strings.Contains(err.Error(), "run `exe update` again") {
		t.Fatalf("an app that cannot be replaced: %v", err)
	}
	if readApp(r.l, "Notes") != "two" || readApp(r.l, "Todo") != "one" {
		t.Fatalf("at the failure: Notes %q, Todo %q", readApp(r.l, "Notes"), readApp(r.l, "Todo"))
	}
	if r.binary() != "the released exe" || r.restarts != 0 {
		t.Fatal("the binary was replaced, or the daemon restarted, by an update that failed")
	}

	os.Chmod(todo, 0o755)
	r.out.Reset()
	if err := r.u.run(context.Background(), false, true); err != nil {
		t.Fatal(err)
	}
	// Notes was placed by the run that failed; it is not "edited"
	if out := r.out.String(); strings.Contains(out, "is left as it is") {
		t.Errorf("the retry took its own work for the owner's:\n%s", out)
	}
	for _, app := range []string{"Notes", "Todo", "Weather", "World Clock"} {
		if readApp(r.l, app) != "two" {
			t.Errorf("%s = %q after the retry", app, readApp(r.l, app))
		}
	}
	m, _ := release.ReadAppsManifest(r.l.AppsManifest())
	if len(m) != 4 {
		t.Errorf("on record: %v", m)
	}
	if _, err := os.Stat(r.l.AppsManifest() + ".pending"); err == nil {
		t.Error("the plan of the failed run was left")
	}
	if r.binary() != "exe 2026.10.10" {
		t.Errorf("binary %q", r.binary())
	}
}

// The new binary is copied before anything else is touched, so an update
// that cannot write it changes nothing.
func TestUpdateThatCannotWriteTheBinaryChangesNothing(t *testing.T) {
	r := rig(t, "2026.10.10", false)
	os.MkdirAll(filepath.Join(r.l.Bin+".new", "in the way"), 0o755)
	if err := r.u.run(context.Background(), false, true); err == nil {
		t.Fatal("the update went through without a binary")
	}
	if b, _ := os.ReadFile(r.l.Helper()); string(b) != "the helper" {
		t.Errorf("the helper was staged by an update that could not write the binary: %q", b)
	}
	if readApp(r.l, "Todo") != "one" || r.binary() != "the released exe" || r.restarts != 0 {
		t.Errorf("Todo %q, binary %q, restarts %d", readApp(r.l, "Todo"), r.binary(), r.restarts)
	}
}

// An install is the same: nothing is written if the binary cannot be, and
// the binary is in place only once the rest is.
func TestSetupThatCannotWriteTheBinaryChangesNothing(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l := newFake(t, "\n\n\n")
	os.MkdirAll(filepath.Join(l.Bin+".new", "in the way"), 0o755)
	if err := runSetup(l, f.host, unpacked(t)); err == nil {
		t.Fatal("the install went through without a binary")
	}
	for _, p := range []string{l.Config(), l.Apps(), l.Release(), l.Unit, l.Bin} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s was written by an install that could not write the binary", p)
		}
	}
	if len(f.quiet) != 0 {
		t.Errorf("ran %v", f.quiet)
	}
}

// A first install that stopped while placing the apps has no manifest,
// only its plan. Running the installer again finishes it without asking
// about the apps a second time; an uninstall instead takes away what it
// had placed.
func TestSetupIsRunAgainAfterAFailure(t *testing.T) {
	setVersion(t, "2026.10.09")
	from := unpacked(t)
	stopped := func(t *testing.T) (*fakeHost, layout) {
		f, l := newFake(t, "") // the rerun must ask nothing
		// what the first run left: its configuration, the staged helper,
		// Notes in place, the plan for all three bundles — and no binary
		os.MkdirAll(l.State, 0o755)
		os.WriteFile(l.Config(), []byte(`{"listen":"127.0.0.1:7777"}`), 0o600)
		src := t.TempDir()
		if err := release.Extract(filepath.Join(from, release.AppsAsset), src); err != nil {
			t.Fatal(err)
		}
		plan := release.AppsManifest{}
		for _, app := range []string{"Notes", "Todo", "World Clock"} {
			plan[app], _ = release.TreeSum(filepath.Join(src, app))
		}
		if err := plan.Write(l.AppsManifest() + ".pending"); err != nil {
			t.Fatal(err)
		}
		os.MkdirAll(filepath.Join(l.Apps(), "Notes"), 0o755)
		for _, name := range []string{"app.json", "index.html"} {
			b, _ := os.ReadFile(filepath.Join(src, "Notes", name))
			os.WriteFile(filepath.Join(l.Apps(), "Notes", name), b, 0o644)
		}
		return f, l
	}

	f, l := stopped(t)
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	out := f.out.String()
	if strings.Contains(out, "?") {
		t.Errorf("the rerun asked again:\n%s", out)
	}
	if strings.Contains(out, "is left as it is") {
		t.Errorf("the rerun took the first run's Notes for the owner's:\n%s", out)
	}
	m, _ := release.ReadAppsManifest(l.AppsManifest())
	if len(m) != 3 || readApp(l, "Todo") != "one" || readApp(l, "World Clock") != "one" {
		t.Errorf("on record %v; Todo %q, World Clock %q", m, readApp(l, "Todo"), readApp(l, "World Clock"))
	}
	if b, _ := os.ReadFile(l.Bin); string(b) != "the released exe" {
		t.Errorf("binary = %q", b)
	}
	if !has(f.quiet, "systemctl --user restart exe") || !strings.Contains(out, "is running.") {
		t.Errorf("the service was not started:\n%s", out)
	}

	// or, instead of the rerun, an uninstall
	f, l = stopped(t)
	os.MkdirAll(filepath.Dir(l.Bin), 0o755)
	os.WriteFile(l.Bin, []byte("the released exe"), 0o755)
	if err := runUninstall(l, f.host, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(l.Apps(), "Notes")); err == nil {
		t.Error("the app an unfinished install had placed was left behind")
	}
	if !strings.Contains(f.out.String(), "Removed the apps it installed: Notes\n") {
		t.Errorf("said:\n%s", f.out.String())
	}
}

// ---- a Mac: launchd instead of systemd, and no VM step ----

// newMac is newFake as a Mac: the agent lives in ~/Library/LaunchAgents,
// and a release holds no network helper.
func newMac(t *testing.T, input string) (*fakeHost, layout, string) {
	t.Helper()
	f, l := newFake(t, input)
	f.OS, f.UID = "darwin", "501"
	l.Unit = filepath.Join(f.Home, "Library", "LaunchAgents", launchdLabel+".plist")
	from := t.TempDir()
	os.WriteFile(filepath.Join(from, "exe"), []byte("the released exe"), 0o755)
	os.WriteFile(filepath.Join(from, release.AppsAsset), appsTgz(t, "one", "Notes", "Todo"), 0o644)
	return f, l, from
}

func launchctl(f *fakeHost) []string {
	var out []string
	for _, q := range f.quiet {
		if strings.HasPrefix(q, "launchctl ") {
			out = append(out, strings.TrimPrefix(q, "launchctl "))
		}
	}
	return out
}

func TestSetupOnAMac(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l, from := newMac(t, "\n\n\n")
	f.KVM = false // there is no /dev/kvm on a Mac, and none is needed
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	out := f.out.String()
	if c := readConfig(t, l); c.Listen != "127.0.0.1:7777" || c.APIToken != "" {
		t.Errorf("config %+v", c)
	}
	if b, _ := os.ReadFile(l.Bin); string(b) != "the released exe" {
		t.Errorf("binary = %q", b)
	}
	// three questions: a Mac has nothing to set up for VMs, and says nothing of /dev/kvm
	for _, want := range []string{"1. Where should exe listen?", "2. Require an API token?", "3. Install the extra desktop apps?", "exe 2026.10.09 is running.",
		"macOS asks to let it reach devices on your local network: its VMs are there. Choose Allow."} {
		if !strings.Contains(out, want) {
			t.Errorf("did not say %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"4.", "/dev/kvm", "run VMs", "sudo", "systemctl", "linger"} {
		if strings.Contains(out, not) {
			t.Errorf("a Mac install said %q:\n%s", not, out)
		}
	}
	if _, err := os.Stat(l.Release()); err == nil {
		if _, err := os.Stat(l.Helper()); err == nil {
			t.Error("a network helper was staged on a Mac")
		}
	}
	plist, _ := os.ReadFile(l.Unit)
	for _, want := range []string{
		"<!-- " + serviceMark,
		"<string>com.v2core.exe</string>",
		"<string>" + l.Bin + "</string>\n\t\t<string>serve</string>",
		"<key>EXE_LAUNCHD</key>\n\t\t<string>gui/501/com.v2core.exe</string>",
		"<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>AssociatedBundleIdentifiers</key>\n\t<array>\n\t\t<string>com.v2core.exe</string>\n\t</array>",
		"<key>AbandonProcessGroup</key>\n\t<true/>",
		"<string>" + filepath.Join(l.State, "launchd.log") + "</string>",
	} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("the agent lacks %q:\n%s", want, plist)
		}
	}
	if strings.Contains(string(plist), "EXE_HOME") {
		t.Error("EXE_HOME was pinned for the default state folder")
	}
	want := []string{"print gui/501", "print gui/501/com.v2core.exe", "bootstrap gui/501 " + l.Unit}
	if got := launchctl(f); strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Errorf("launchctl ran %v, want %v", got, want)
	}
	for _, q := range f.quiet {
		if strings.HasPrefix(q, "systemctl") || strings.HasPrefix(q, "loginctl") {
			t.Errorf("ran %q on a Mac", q)
		}
	}
	if len(f.ran) != 0 {
		t.Errorf("ran %v", f.ran)
	}

	// installing over it: the same agent is only restarted, with the new binary
	f.quiet = nil
	f.In = bufio.NewReader(strings.NewReader(""))
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	want = []string{"print gui/501", "print gui/501/com.v2core.exe", "kickstart -k gui/501/com.v2core.exe"}
	if got := launchctl(f); strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Errorf("over an install, launchctl ran %v, want %v", got, want)
	}
}

// launchd keeps the agent it read: one that changed is unloaded and loaded anew.
func TestSetupOnAMacReloadsAChangedAgent(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l, from := newMac(t, "\n\n\n")
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(l.Unit)
	os.WriteFile(l.Unit, []byte(strings.Replace(string(old), "<integer>75</integer>", "<integer>20</integer>", 1)), 0o644)
	f.quiet = nil
	f.In = bufio.NewReader(strings.NewReader(""))
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(launchctl(f), "; ")
	if !strings.Contains(got, "bootout gui/501/com.v2core.exe") || !strings.HasSuffix(got, "bootstrap gui/501 "+l.Unit) {
		t.Errorf("launchctl ran %s", got)
	}
	if now, _ := os.ReadFile(l.Unit); string(now) != string(old) {
		t.Error("the agent was not rewritten")
	}
}

// Over SSH, with nobody at the screen, there is no login session to load
// the agent into: it waits for the next login.
func TestSetupOnAMacWithNobodyLoggedIn(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l, from := newMac(t, "\n\n\n")
	f.noGUI = true
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.Unit); err != nil {
		t.Error("the agent was not left for the next login")
	}
	out := f.out.String()
	if !strings.Contains(out, "exe 2026.10.09 is installed.") || !strings.Contains(out, "exe starts at the next login. To run it now: exe serve") {
		t.Errorf("said:\n%s", out)
	}
	for _, c := range launchctl(f) {
		if strings.HasPrefix(c, "bootstrap") || strings.HasPrefix(c, "kickstart") {
			t.Errorf("ran launchctl %s with no session to run it in", c)
		}
	}
}

func TestSetupOnAMacLeavesAForeignAgent(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l, from := newMac(t, "\n\n\n")
	theirs := "<plist><dict><key>Label</key><string>com.v2core.exe</string></dict></plist>"
	os.MkdirAll(filepath.Dir(l.Unit), 0o755)
	os.WriteFile(l.Unit, []byte(theirs), 0o644)
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(l.Unit); string(b) != theirs {
		t.Fatal("an agent the installer did not write was rewritten")
	}
	if len(launchctl(f)) != 0 {
		t.Errorf("launchctl ran %v against an agent that is not the installer's", launchctl(f))
	}
}

func TestLaunchdPlist(t *testing.T) {
	h := &host{OS: "darwin", UID: "502", Home: "/Users/a&b"}
	l := layout{Bin: "/Users/a&b/.local/bin/exe", State: "/Volumes/Big <disk>/exe", Unit: "/Users/a&b/Library/LaunchAgents/com.v2core.exe.plist"}
	p := launchdPlist(l, h)
	for _, want := range []string{
		"<string>/Users/a&amp;b/.local/bin/exe</string>",
		"<key>EXE_HOME</key>\n\t\t<string>/Volumes/Big &lt;disk&gt;/exe</string>",
		"<key>PATH</key>\n\t\t<string>/Users/a&amp;b/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>",
		"<string>gui/502/com.v2core.exe</string>",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the agent lacks %q:\n%s", want, p)
		}
	}
	if !strings.HasPrefix(p, "<?xml ") {
		t.Error("a property list starts with its XML declaration; the installer's mark comes after")
	}
}

func TestUninstallOnAMac(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l, from := newMac(t, "\n\n\n")
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	f.quiet = nil
	if err := runUninstall(l, f.host, true); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{l.Bin, l.Unit, filepath.Join(l.Apps(), "Notes")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s is still there", gone)
		}
	}
	if got := launchctl(f); len(got) != 1 || got[0] != "bootout gui/501/com.v2core.exe" {
		t.Errorf("launchctl ran %v", got)
	}
	if out := f.out.String(); strings.Contains(out, "network helper") {
		t.Errorf("a Mac was told about the network helper:\n%s", out)
	}
	if _, err := os.Stat(l.Config()); err != nil {
		t.Error("the configuration was removed")
	}
}

// An update on a Mac: no helper in the release, and the restart that is
// put off is launchd's to do.
func TestUpdateOnAMac(t *testing.T) {
	setVersion(t, "2026.10.09")
	f, l, from := newMac(t, "\n\n\n")
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	latest := "2026.10.10"
	bin := tgz(t, map[string]string{"exe": "exe " + latest}) // a Mac's tarball holds exe alone
	apps := appsTgz(t, "two", "Notes", "Todo")
	sums := ""
	for name, b := range map[string][]byte{release.BinaryAsset(runtime.GOOS, runtime.GOARCH): bin, release.AppsAsset: apps} {
		sum := sha256.Sum256(b)
		sums += hex.EncodeToString(sum[:]) + "  " + name + "\n"
	}
	files := map[string][]byte{release.BinaryAsset(runtime.GOOS, runtime.GOARCH): bin, release.AppsAsset: apps, release.SumsAsset: []byte(sums)}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/"+latest, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/"+latest+"/", func(w http.ResponseWriter, r *http.Request) {
		if b, ok := files[filepath.Base(r.URL.Path)]; ok {
			w.Write(b)
			return
		}
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	restart, _ := f.serviceWords()
	u := &updater{
		Layout: l, Host: f.host, RestartHint: restart,
		Client:  &release.Client{Base: srv.URL + "/releases", HTTP: srv.Client()},
		Probe:   func(string) (string, error) { return "exe " + latest + " (darwin/arm64)", nil },
		Running: func() bool { return true },
		Restart: func() error { t.Error("restarted without being asked"); return nil },
	}
	f.Tty = false
	if err := u.run(context.Background(), false, false); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if b, _ := os.ReadFile(l.Bin); string(b) != "exe "+latest {
		t.Errorf("binary = %q", b)
	}
	if readApp(l, "Todo") != "two" {
		t.Errorf("Todo = %q", readApp(l, "Todo"))
	}
	if _, err := os.Stat(l.Helper()); err == nil {
		t.Error("a network helper was staged on a Mac")
	}
	if out := f.out.String(); !strings.Contains(out, "until you run: launchctl kickstart -k gui/501/com.v2core.exe\n") || strings.Contains(out, "network helper") {
		t.Errorf("said:\n%s", out)
	}
}

func TestPlatformWords(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"linux", "amd64"}:  "Linux x86-64",
		{"linux", "arm64"}:  "Linux ARM64",
		{"darwin", "amd64"}: "macOS (Intel)",
		{"darwin", "arm64"}: "macOS (Apple silicon)",
	} {
		if got := platformWords(in[0], in[1]); got != want {
			t.Errorf("%v: %q, want %q", in, got, want)
		}
	}
}

// ---- Windows: a registry entry instead of a service, QEMU instead of a
// helper, and a drive to choose ----

type fakeWin struct {
	login    string
	path     []string
	started  [][]string
	killed   []string
	later    []string
	restarts int
	running  bool // a daemon answers
}

const gb = int64(1) << 30

// worldDrives is the PC the installer was first tried on: a system drive
// that is nearly full, a network disk with the most room, and a second SSD.
func worldDrives() []drive {
	return []drive{
		{Letter: "C:", Free: 14 * gb, Size: 937 * gb, Kind: "SSD", Home: true},
		{Letter: "D:", Free: 1011 * gb, Size: 2038 * gb, Kind: "network disk (iSCSI)", Network: true},
		{Letter: "G:", Free: 192 * gb, Size: 3726 * gb, Kind: "SSD"},
	}
}

func newWin(t *testing.T, input string) (*fakeHost, *fakeWin, layout, string) {
	t.Helper()
	f, l := newFake(t, input)
	w := &fakeWin{}
	f.OS = "windows"
	f.env["SystemRoot"] = `C:\Windows`
	f.tools["winget"] = true
	l.Bin = filepath.Join(f.Home, "AppData", "Local", "Programs", "exe", "exe.exe")
	l.Unit = ""
	f.Win = &winHost{
		Session: true, CanVM: true,
		Drives:   worldDrives,
		WHPX:     func() bool { return true },
		QEMU:     func() string { return "" },
		Login:    func() string { return w.login },
		SetLogin: func(cmd string) error { w.login = cmd; return nil },
		AddPath: func(dir string) (bool, error) {
			if has(w.path, dir) {
				return false, nil
			}
			w.path = append(w.path, dir)
			return true, nil
		},
		DelPath:     func(dir string) error { w.path = nil; return nil },
		Start:       func(argv []string) error { w.started = append(w.started, argv); w.running = true; return nil },
		Kill:        func(bin string) int { w.killed = append(w.killed, bin); w.running = false; return 1 },
		DeleteLater: func(p string) error { w.later = append(w.later, p); return nil },
	}
	f.Reach = func(string) bool { return w.running }
	f.RestartDaemon = func(string, string) error { w.restarts++; return nil }
	from := t.TempDir()
	os.WriteFile(filepath.Join(from, "exe.exe"), []byte("the released exe"), 0o755)
	os.WriteFile(filepath.Join(from, release.AppsAsset), appsTgz(t, "one", "Notes", "Todo"), 0o644)
	return f, w, l, from
}

func vmDirOf(t *testing.T, l layout) string {
	t.Helper()
	return readConfig(t, l).VMDir
}

func TestSetupOnWindows(t *testing.T) {
	setVersion(t, "2026.10.11")
	// Return on the first three, yes to the VM step, Return on the drive
	f, w, l, from := newWin(t, "\n\n\ny\n\n")
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	out := f.out.String()
	for _, want := range []string{
		"1. Where should exe listen?",
		"2. Require an API token?",
		"3. Install the extra desktop apps?",
		"4. Set this machine up to run VMs? Windows asks for an administrator's approval. This would run:\n" +
			"     winget install --id SoftwareFreedomConservancy.QEMU -e --accept-package-agreements --accept-source-agreements   # QEMU\n",
		"5. Which drive should hold the VMs?\n   A VM takes up to its disk size (20 GB by default); the base image takes 3 GB.\n",
		"     1) C:    14 GB free of  937 GB   SSD, your home folder\n",
		"     2) D:  1011 GB free of 2038 GB   network disk (iSCSI)\n",
		"     3) G:   192 GB free of 3726 GB   SSD\n",
		"   Choice [3]: ",
		"exe 2026.10.11 is running.",
		"exe is on your PATH in every terminal you open from now on.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("did not say %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"sudo", "/dev/kvm", "systemctl", "launchctl", "linger", "~", "export PATH"} {
		if strings.Contains(out, not) {
			t.Errorf("a Windows install said %q:\n%s", not, out)
		}
	}
	// the default was the second SSD: the home drive has no room, and a
	// network disk is never chosen for anyone
	if got := vmDirOf(t, l); got != `G:\exe` {
		t.Errorf("vm_dir = %q, want G:\\exe", got)
	}
	if b, _ := os.ReadFile(l.Bin); string(b) != "the released exe" {
		t.Errorf("binary at %s = %q", l.Bin, b)
	}
	// at sign-in Windows runs the starter, hidden; the daemon itself is
	// started with a console of its own
	wantLogin := `C:\Windows\System32\conhost.exe --headless ` + l.Bin + ` daemon start`
	if w.login != wantLogin {
		t.Errorf("sign-in entry = %q, want %q", w.login, wantLogin)
	}
	if len(w.started) != 1 || strings.Join(w.started[0], " ") != l.Bin+" serve" {
		t.Errorf("started %v", w.started)
	}
	if len(f.ran) != 1 || !strings.HasPrefix(f.ran[0], "winget install --id SoftwareFreedomConservancy.QEMU") {
		t.Errorf("the VM step ran %v — winget asks for its own approval and is run as it is", f.ran)
	}
	if len(w.path) != 1 || w.path[0] != filepath.Dir(l.Bin) {
		t.Errorf("PATH got %v", w.path)
	}
	for _, q := range f.quiet {
		t.Errorf("ran %q", q)
	}
	if _, err := os.Stat(l.Helper()); err == nil {
		t.Error("a network helper was staged on Windows")
	}

	// installing over it: the configuration is kept, the drive is not asked
	// about again, and the daemon that is running restarts itself
	f.In = bufio.NewReader(strings.NewReader(""))
	f.out.Reset()
	f.Win.QEMU = func() string { return `C:\Program Files\qemu\qemu-system-x86_64.exe` }
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	if out := f.out.String(); strings.Contains(out, "drive") || strings.Contains(out, "?") {
		t.Errorf("installing over an install asked again:\n%s", out)
	}
	if w.restarts != 1 || len(w.started) != 1 {
		t.Errorf("restarts %d, starts %d — a running daemon is asked to restart, not started twice", w.restarts, len(w.started))
	}
}

func TestSetupOnWindowsChoosingADrive(t *testing.T) {
	setVersion(t, "2026.10.11")
	for input, want := range map[string]string{
		"\n\n\n\n":        `G:\exe`, // nothing to set up, Return on the drive
		"\n\n\n1\n":       ``,       // the home drive: the VMs stay beside everything else
		"\n\n\n2\n":       `D:\exe`, // a network disk may be chosen, it is only never the default
		"\n\n\ng:\n":      `G:\exe`,
		"\n\n\n9\nQ\nd\n": `D:\exe`, // asked again until it is a drive
	} {
		f, _, l, from := newWin(t, input)
		f.Win.QEMU = func() string { return `C:\Program Files\qemu\qemu-system-x86_64.exe` }
		if err := runSetup(l, f.host, from); err != nil {
			t.Fatal(err)
		}
		if got := vmDirOf(t, l); got != want {
			t.Errorf("answers %q: vm_dir = %q, want %q", input, got, want)
		}
		if strings.Contains(f.out.String(), "run VMs?") {
			t.Errorf("a PC with everything for VMs was asked to set them up:\n%s", f.out.String())
		}
	}

	// room on the home drive: it is the default, and no folder of its own
	f, _, l, from := newWin(t, "\n\n\n\n")
	f.Win.QEMU = func() string { return "qemu" }
	f.Win.Drives = func() []drive {
		d := worldDrives()
		d[0].Free = 300 * gb
		return d
	}
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	if got := vmDirOf(t, l); got != "" || !strings.Contains(f.out.String(), "Choice [1]: ") {
		t.Errorf("vm_dir = %q\n%s", got, f.out.String())
	}

	// one drive: there is nothing to ask
	f, _, l, from = newWin(t, "\n\n\n")
	f.Win.QEMU = func() string { return "qemu" }
	f.Win.Drives = func() []drive { return worldDrives()[:1] }
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.out.String(), "drive") {
		t.Errorf("asked about drives on a PC with one:\n%s", f.out.String())
	}

	// the VM step declined: no VMs, so no drive for them
	f, _, l, from = newWin(t, "\n\n\nn\n")
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.out.String(), "Which drive") || vmDirOf(t, l) != "" || len(f.ran) != 0 {
		t.Errorf("after no to VMs: vm_dir %q, ran %v\n%s", vmDirOf(t, l), f.ran, f.out.String())
	}

	// nobody to ask: the same default, or the drive the environment names
	f, _, l, from = newWin(t, "")
	f.Tty = false
	f.Win.QEMU = func() string { return "qemu" }
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	if vmDirOf(t, l) != `G:\exe` || !strings.Contains(f.out.String(), "VMs on G:, 192 GB free") {
		t.Errorf("with nobody to ask: %q\n%s", vmDirOf(t, l), f.out.String())
	}
	f, _, l, from = newWin(t, "")
	f.Tty = false
	f.Win.QEMU = func() string { return "qemu" }
	f.env[envVMDrive] = "d"
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	if vmDirOf(t, l) != `D:\exe` {
		t.Errorf("%s=d: vm_dir = %q", envVMDrive, vmDirOf(t, l))
	}
	f, _, l, from = newWin(t, "")
	f.Tty = false
	f.Win.QEMU = func() string { return "qemu" }
	f.env[envVMDrive] = "Z:"
	if err := runSetup(l, f.host, from); err == nil {
		t.Errorf("%s=Z: passed on a PC without a Z:", envVMDrive)
	}
}

func TestPickDrive(t *testing.T) {
	d := worldDrives()
	if got := pickDrive(d); d[got].Letter != "G:" {
		t.Errorf("picked %s", d[got].Letter)
	}
	d[0].Free = vmRoom
	if got := pickDrive(d); d[got].Letter != "C:" {
		t.Errorf("with room at home, picked %s", d[got].Letter)
	}
	// only a full home drive and a network disk: the network disk is still not it
	d = worldDrives()[:2]
	if got := pickDrive(d); d[got].Letter != "C:" {
		t.Errorf("picked %s over the PC's own drive", d[got].Letter)
	}
}

// The hypervisor platform is two Windows features that need an
// administrator and a restart; without elevation the command is started
// again through the approval dialog.
func TestSetupOnWindowsTurnsTheHypervisorOn(t *testing.T) {
	setVersion(t, "2026.10.11")
	f, _, l, from := newWin(t, "\n\n\ny\n\n")
	f.Win.WHPX = func() bool { return false }
	f.Win.QEMU = func() string { return "qemu" }
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	dism := "dism /online /enable-feature /featurename:HypervisorPlatform /featurename:VirtualMachinePlatform /all /norestart"
	if !strings.Contains(out, "     "+dism+"\n") || !strings.Contains(out, "Restart Windows before creating a VM") {
		t.Errorf("said:\n%s", out)
	}
	if len(f.ran) != 1 || !strings.HasPrefix(f.ran[0], "powershell -NoProfile -Command $p = Start-Process -FilePath 'dism' -ArgumentList '/online','/enable-feature',") ||
		!strings.Contains(f.ran[0], "-Verb RunAs -Wait -PassThru; exit $p.ExitCode") {
		t.Errorf("ran %v", f.ran)
	}

	// already an administrator: the command itself, and no talk of approval
	f, _, l, from = newWin(t, "\n\n\ny\n\n")
	f.Win.WHPX = func() bool { return false }
	f.Win.QEMU = func() string { return "qemu" }
	f.Win.Elevated = true
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	if len(f.ran) != 1 || f.ran[0] != dism || strings.Contains(f.out.String(), "approval") {
		t.Errorf("elevated: ran %v\n%s", f.ran, f.out.String())
	}
}

// dism answers 3010 when it has done its work and Windows must restart.
func TestStepFineExitCodes(t *testing.T) {
	err := exec.Command("sh", "-c", "exit 3").Run()
	if err == nil {
		t.Skip("no sh to exit 3 with")
	}
	if !(step{Fine: []int{3}}).fine(err) || (step{Fine: []int{4}}).fine(err) || (step{}).fine(err) {
		t.Error("exit codes a step counts as success are not read right")
	}
}

// Over ssh the installer is not in the user's desktop session: what it
// started there would end with the connection. A task that runs once, in
// the signed-in user's session, starts the daemon where it stays.
func TestSetupOnWindowsOverSSH(t *testing.T) {
	setVersion(t, "2026.10.11")
	f, w, l, from := newWin(t, "")
	f.Tty = false
	f.Win.Session = false
	f.Win.QEMU = func() string { return "qemu" }
	reach := 0
	f.Reach = func(string) bool { reach++; return reach > 1 } // down until the task has run
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if len(w.started) != 0 {
		t.Errorf("started %v in a session that ends with the connection", w.started)
	}
	// the task runs the starter, not the daemon: a task's own process
	// is ended with the task
	want := []string{
		`schtasks /Create /TN exe daemon start /TR C:\Windows\System32\conhost.exe --headless ` + l.Bin + ` daemon start /SC ONCE /ST 23:59 /IT /F`,
		`schtasks /Run /TN exe daemon start`,
		`schtasks /Delete /TN exe daemon start /F`,
	}
	if strings.Join(f.quiet, "\n") != strings.Join(want, "\n") {
		t.Errorf("ran:\n%s\nwant:\n%s", strings.Join(f.quiet, "\n"), strings.Join(want, "\n"))
	}
	if !strings.Contains(f.out.String(), "is running.") {
		t.Errorf("said:\n%s", f.out.String())
	}

	// nobody signed in: the task cannot run, and the installer says when exe starts
	f, w, l, from = newWin(t, "")
	f.Tty = false
	f.Win.Session = false
	f.Win.QEMU = func() string { return "qemu" }
	f.host.Quiet = func(argv ...string) error {
		if argv[1] == "/Run" {
			return errors.New("ERROR: The operator or administrator has refused the request")
		}
		return nil
	}
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	if out := f.out.String(); !strings.Contains(out, "is installed.") || !strings.Contains(out, "exe starts the next time you sign in to Windows (nobody is signed in at this PC)") {
		t.Errorf("said:\n%s", out)
	}
	if want := `C:\Windows\System32\conhost.exe --headless ` + l.Bin + ` daemon start`; w.login != want {
		t.Errorf("sign-in entry = %q", w.login)
	}
}

func TestSetupOnWindowsLeavesAForeignEntry(t *testing.T) {
	setVersion(t, "2026.10.11")
	f, w, l, from := newWin(t, "\n\n\n\n")
	f.Win.QEMU = func() string { return "qemu" }
	w.login = `"D:\src\exe\exe.exe" serve`
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	if w.login != `"D:\src\exe\exe.exe" serve` || len(w.started) != 0 {
		t.Errorf("sign-in entry %q, started %v", w.login, w.started)
	}
	if out := f.out.String(); !strings.Contains(out, "That entry is left as it is") || !strings.Contains(out, "is installed.") {
		t.Errorf("said:\n%s", out)
	}
}

func TestSetupOnAWindowsPCThatCannotRunVMs(t *testing.T) {
	setVersion(t, "2026.10.11")
	f, _, l, from := newWin(t, "\n\n\n")
	f.Win.CanVM = false
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	if !strings.Contains(out, "exe runs VMs on x86-64 Windows only") || strings.Contains(out, "drive") || strings.Contains(out, "run VMs?") {
		t.Errorf("said:\n%s", out)
	}
}

func TestUninstallOnWindows(t *testing.T) {
	setVersion(t, "2026.10.11")
	f, w, l, from := newWin(t, "\n\n\n\n")
	f.Win.QEMU = func() string { return "qemu" }
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	// an update moved the running binary aside, and the daemon has not
	// restarted since: it is that file which is running
	aside := l.Bin + ".old"
	if err := os.WriteFile(aside, []byte("the release before"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	if err := runUninstall(l, f.host, true); err != nil {
		t.Fatal(err)
	}
	if w.login != "" || strings.Join(w.killed, " ") != l.Bin+" "+aside || len(w.path) != 0 {
		t.Errorf("sign-in entry %q, killed %v, PATH %v", w.login, w.killed, w.path)
	}
	if left, _ := os.ReadDir(filepath.Dir(l.Bin)); len(left) != 0 {
		t.Errorf("left in the program folder: %v", left)
	}
	out := f.out.String()
	if !strings.Contains(out, "Stopped exe; it no longer starts when you sign in.") || !strings.Contains(out, `Your VMs are in G:\exe.`) {
		t.Errorf("said:\n%s", out)
	}
	if _, err := os.Stat(l.Config()); err != nil {
		t.Error("the configuration was removed")
	}
}

func TestWinCommandLine(t *testing.T) {
	got := winCommandLine([]string{`C:\Windows\System32\conhost.exe`, "--headless", `C:\Users\Ada Lovelace\AppData\Local\Programs\exe\exe.exe`, "serve"})
	want := `C:\Windows\System32\conhost.exe --headless "C:\Users\Ada Lovelace\AppData\Local\Programs\exe\exe.exe" serve`
	if got != want {
		t.Errorf("%s", got)
	}
}

// Windows will not let a running program's file be replaced, only renamed:
// the old binary is moved aside, and cleared out by a later update.
func TestReplaceARunningProgram(t *testing.T) {
	dir := t.TempDir()
	dst, tmp := filepath.Join(dir, "exe.exe"), filepath.Join(dir, "exe.exe.new")
	os.WriteFile(dst, []byte("running"), 0o755)
	os.WriteFile(tmp, []byte("new"), 0o755)
	real := renameFile
	t.Cleanup(func() { renameFile = real })
	locked := true // the file at dst is a program that is running
	renameFile = func(from, to string) error {
		if to == dst && locked {
			if _, err := os.Stat(dst); err == nil {
				return errors.New("Access is denied.")
			}
		}
		return real(from, to)
	}
	if err := replaceFile(tmp, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "new" {
		t.Errorf("in place: %q", b)
	}
	if b, _ := os.ReadFile(dst + ".old"); string(b) != "running" {
		t.Errorf("moved aside: %q", b)
	}
	// the next update: the daemon has restarted, the old file is nobody's
	locked = false
	os.WriteFile(tmp, []byte("newer"), 0o755)
	if err := replaceFile(tmp, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst + ".old"); !os.IsNotExist(err) {
		t.Error("what an earlier update moved aside was left for good")
	}
	if b, _ := os.ReadFile(dst); string(b) != "newer" {
		t.Errorf("in place: %q", b)
	}
	// a second file that cannot go: the next one gets a name of its own
	locked = true
	os.WriteFile(dst+".old", []byte("still in use"), 0o755)
	os.WriteFile(tmp, []byte("newest"), 0o755)
	if err := replaceFile(tmp, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "newest" {
		t.Errorf("in place: %q", b)
	}
}

func TestUpdateOnWindows(t *testing.T) {
	setVersion(t, "2026.10.11")
	f, w, l, from := newWin(t, "\n\n\n\n")
	f.Win.QEMU = func() string { return "qemu" }
	if err := runSetup(l, f.host, from); err != nil {
		t.Fatal(err)
	}
	f.out.Reset()
	latest := "2026.10.12"
	bin := tgz(t, map[string]string{"exe.exe": "exe " + latest})
	apps := appsTgz(t, "two", "Notes", "Todo")
	sums := ""
	for name, b := range map[string][]byte{release.BinaryAsset(runtime.GOOS, runtime.GOARCH): bin, release.AppsAsset: apps} {
		sum := sha256.Sum256(b)
		sums += hex.EncodeToString(sum[:]) + "  " + name + "\n"
	}
	files := map[string][]byte{release.BinaryAsset(runtime.GOOS, runtime.GOARCH): bin, release.AppsAsset: apps, release.SumsAsset: []byte(sums)}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, "/releases/tag/"+latest, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/"+latest+"/", func(rw http.ResponseWriter, r *http.Request) {
		if b, ok := files[filepath.Base(r.URL.Path)]; ok {
			rw.Write(b)
			return
		}
		http.NotFound(rw, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	restart, _ := f.serviceWords()
	u := &updater{
		Layout: l, Host: f.host, RestartHint: restart,
		Client:  &release.Client{Base: srv.URL + "/releases", HTTP: srv.Client()},
		Probe:   func(string) (string, error) { return "exe " + latest + " (windows/amd64)", nil },
		Running: func() bool { return w.running },
		Restart: func() error { t.Error("restarted without being asked"); return nil },
	}
	f.Tty = false
	if err := u.run(context.Background(), false, false); err != nil {
		t.Fatalf("%v\n%s", err, f.out.String())
	}
	if b, _ := os.ReadFile(l.Bin); string(b) != "exe "+latest {
		t.Errorf("binary = %q", b)
	}
	if readApp(l, "Todo") != "two" {
		t.Errorf("Todo = %q", readApp(l, "Todo"))
	}
	if out := f.out.String(); !strings.Contains(out, "until you run: exe daemon restart\n") || strings.Contains(out, "network helper") {
		t.Errorf("said:\n%s", out)
	}
}
