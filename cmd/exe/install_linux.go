//go:build linux

package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"exe/internal/config"
	"exe/internal/release"
)

// The installer. https://exe.v2core.com/install.sh downloads a release,
// checks it and runs `exe setup -from <folder>`: everything a person is
// asked, and everything done with the answers, is here rather than in
// shell — so it can be tested, and `exe setup` can be run again later.
//
// All the questions come first and nothing is written until the last one
// is answered. Return on every question is the careful install: this
// machine only, no sudo.

// layout is where an installed exe keeps its files.
type layout struct {
	Bin   string // ~/.local/bin/exe
	State string // ~/.exe (or $EXE_HOME)
	Unit  string // ~/.config/systemd/user/exe.service
}

func (l layout) Config() string       { return filepath.Join(l.State, "config.json") }
func (l layout) Apps() string         { return filepath.Join(l.State, "apps") }
func (l layout) Release() string      { return filepath.Join(l.State, "release") }
func (l layout) Helper() string       { return filepath.Join(l.Release(), "exe-net-helper") }
func (l layout) AppsManifest() string { return filepath.Join(l.Release(), "apps.json") }

func installLayout() (layout, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return layout{}, err
	}
	return layout{
		Bin:   filepath.Join(home, ".local", "bin", "exe"),
		State: config.Dir(),
		Unit:  filepath.Join(home, ".config", "systemd", "user", "exe.service"),
	}, nil
}

// What the VM step puts in place, as root. The helper's path is the
// daemon's default (firecracker.network_helper); Firecracker's version is
// the one exe is run against, pinned by the checksum of its release
// archive.
const (
	helperPath         = "/usr/local/libexec/exe-net-helper"
	firecrackerPath    = "/usr/local/bin/firecracker"
	firecrackerVersion = "v1.16.1"
)

var firecrackerArchives = map[string]struct{ arch, sum string }{
	"amd64": {"x86_64", "382a02a869e4d6d5cb14c40577f9545e8458021ea8b0b2d3fc10ec14d9c242e6"},
	"arm64": {"aarch64", "8d0e69f6d6f9a1724551f607f18504052c16c1828ee3d4d7b6e6c73380871e0e"},
}

// host is the machine as the installer sees and touches it. The real one
// is realHost; a test hands in its own.
type host struct {
	User, Group string // who exe will run as, and their primary group
	Home        string
	Root        bool
	TailscaleIP string
	LANIP       string
	KVM         bool // /dev/kvm is there
	Tty         bool // someone is at a keyboard to answer
	Env         func(string) string
	In          *bufio.Reader
	Out         io.Writer

	PortFree  func(addr string) bool
	LookTool  func(name string) string   // its path, or ""
	Run       func(argv ...string) error // a command with this terminal attached
	Quiet     func(argv ...string) error // a command whose output is not shown
	Reach     func(url string) bool
	GroupInfo func(name string) (exists, member bool)
	OpenKVM   func() bool
	// Helper describes a network helper on disk: its checksum, and
	// whether it is root's and carries a capability.
	Helper func(path string) (sum string, rooted bool)
	// Firecracker downloads the pinned Firecracker into dir.
	Firecracker func(dir string) (string, error)
}

func realHost() *host {
	h := &host{Env: os.Getenv, In: bufio.NewReader(os.Stdin), Out: os.Stdout}
	h.Tty = term.IsTerminal(int(os.Stdin.Fd()))
	h.Home, _ = os.UserHomeDir()
	if u, err := user.Current(); err == nil {
		h.User, h.Group, h.Root = u.Username, u.Gid, u.Uid == "0"
		if g, err := user.LookupGroupId(u.Gid); err == nil {
			h.Group = g.Name
		}
	}
	h.TailscaleIP, h.LANIP = config.TailscaleIP(), config.LANIP()
	_, err := os.Stat("/dev/kvm")
	h.KVM = err == nil
	h.PortFree = func(addr string) bool {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return false
		}
		ln.Close()
		return true
	}
	h.LookTool = func(name string) string {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
		// Debian keeps the sbin folders off an ordinary user's PATH
		for _, dir := range []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/sbin"} {
			p := filepath.Join(dir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
				return p
			}
		}
		return ""
	}
	h.Run = func(argv ...string) error {
		c := exec.Command(argv[0], argv[1:]...)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		return c.Run()
	}
	h.Quiet = func(argv ...string) error {
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		if err != nil {
			if msg := strings.TrimSpace(string(out)); msg != "" {
				return fmt.Errorf("%w: %s", err, msg)
			}
		}
		return err
	}
	h.Reach = func(url string) bool {
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(url)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return true
	}
	h.GroupInfo = func(name string) (bool, bool) {
		g, err := user.LookupGroup(name)
		if err != nil {
			return false, false
		}
		u, err := user.Current()
		if err != nil {
			return true, false
		}
		ids, _ := u.GroupIds()
		for _, id := range ids {
			if id == g.Gid {
				return true, true
			}
		}
		return true, false
	}
	h.OpenKVM = func() bool {
		f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if err != nil {
			return false
		}
		f.Close()
		return true
	}
	h.Helper = func(path string) (string, bool) {
		st, err := os.Stat(path)
		if err != nil {
			return "", false
		}
		sum, _ := release.FileSum(path)
		sys, ok := st.Sys().(*syscall.Stat_t)
		n, _ := unix.Getxattr(path, "security.capability", make([]byte, 64))
		return sum, ok && sys.Uid == 0 && n > 0
	}
	h.Firecracker = fetchFirecracker
	return h
}

func (h *host) say(format string, a ...any) { fmt.Fprintf(h.Out, format, a...) }

// tilde shows a path the way its owner says it.
func (h *host) tilde(p string) string {
	if h.Home != "" && (p == h.Home || strings.HasPrefix(p, h.Home+"/")) {
		return "~" + strings.TrimPrefix(p, h.Home)
	}
	return p
}

// ---- asking ----------------------------------------------------------------

// readLine reads one answer; ok is false when there is no more input.
func (h *host) readLine() (string, bool) {
	s, err := h.In.ReadString('\n')
	if err != nil && s == "" {
		return "", false
	}
	return strings.TrimSpace(s), true
}

// yesNo asks until it has a yes or a no; Return, or no more input, is def.
func (h *host) yesNo(def bool) bool {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	for {
		h.say("   %s: ", hint)
		s, ok := h.readLine()
		if !ok {
			h.say("\n")
			return def
		}
		switch strings.ToLower(s) {
		case "":
			return def
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
	}
}

// envBool reads a yes-or-no answer given ahead of time in the environment.
func (h *host) envBool(name string) (val, set bool, err error) {
	switch strings.ToLower(strings.TrimSpace(h.Env(name))) {
	case "":
		return false, false, nil
	case "y", "yes", "true", "1", "on":
		return true, true, nil
	case "n", "no", "false", "0", "off":
		return false, true, nil
	}
	return false, false, fmt.Errorf("%s=%q: say yes or no", name, h.Env(name))
}

const (
	listenLocal     = "local"
	listenAll       = "all"
	listenTailscale = "tailscale"
)

// The answers given ahead of time, for an install nobody is sitting at.
const (
	envListen = "EXE_INSTALL_LISTEN" // local, all or tailscale
	envToken  = "EXE_INSTALL_TOKEN"  // yes or no
	envApps   = "EXE_INSTALL_APPS"   // yes or no
	envVMs    = "EXE_INSTALL_VMS"    // yes or no
	// envTokenValue is the daemon's own variable: a token given here is the
	// one written to the configuration, instead of a generated one.
	envTokenValue = "EXE_API_TOKEN"
)

type answers struct {
	Listen string // "" when a configuration was already there and is kept
	Token  string // "" for none
	Apps   bool
	VMs    bool
}

// offer is what the installer has to ask about: what it found on the
// machine and in the release.
type offer struct {
	Fresh    bool     // no configuration yet
	Apps     []string // the bundles in the release, when they are a question
	AppsSize int64
	Plan     []step // what the VM step would run; empty when there is nothing to do
}

func (h *host) ask(l layout, o offer) (answers, error) {
	var a answers
	n := 0
	num := func() int { n++; return n }

	if o.Fresh {
		listen, err := h.askListen(num())
		if err != nil {
			return a, err
		}
		a.Listen = listen
		if a.Token, err = h.askToken(num(), listen); err != nil {
			return a, err
		}
	}

	if len(o.Apps) > 0 {
		want, set, err := h.envBool(envApps)
		if err != nil {
			return a, err
		}
		a.Apps = true
		switch {
		case set:
			a.Apps = want
			h.say("\n%d. Extra desktop apps: %s (%s)\n", num(), yesWord(want), envApps)
		case !h.Tty:
			h.say("\n%d. Extra desktop apps: yes\n", num())
		default:
			h.say("\n%d. Install the extra desktop apps?\n   %s (%s, into %s)\n",
				num(), listWords(o.Apps), megabytes(o.AppsSize), h.tilde(l.Apps()))
			a.Apps = h.yesNo(true)
		}
	}

	if len(o.Plan) > 0 {
		want, set, err := h.envBool(envVMs)
		if err != nil {
			return a, err
		}
		switch {
		case set:
			a.VMs = want
			h.say("\n%d. Set up for VMs: %s (%s)\n", num(), yesWord(want), envVMs)
		case !h.Tty:
			h.say("\n%d. Set up for VMs: no — do it later with: exe setup vms\n", num())
		default:
			needs := "This needs sudo, and would run:"
			if h.Root {
				needs = "This would run:"
			}
			h.say("\n%d. Set this machine up to run VMs? %s\n", num(), needs)
			for _, s := range o.Plan {
				h.say("     %s\n", h.shown(s))
			}
			h.say("   You can do this later with: exe setup vms\n")
			a.VMs = h.yesNo(false)
		}
	}
	return a, nil
}

func (h *host) askListen(n int) (string, error) {
	kinds := []string{listenLocal, listenAll}
	if h.TailscaleIP != "" {
		kinds = append(kinds, listenTailscale)
	}
	if v := strings.ToLower(strings.TrimSpace(h.Env(envListen))); v != "" {
		for _, k := range kinds {
			if v == k {
				h.say("\n%d. Listen: %s (%s)\n", n, h.listenWords(k), envListen)
				return k, nil
			}
		}
		if v == listenTailscale {
			return "", fmt.Errorf("%s=tailscale, but this machine has no Tailscale address", envListen)
		}
		return "", fmt.Errorf("%s=%q: say local, all or tailscale", envListen, v)
	}
	if !h.Tty {
		h.say("\n%d. Listen: %s\n", n, h.listenWords(listenLocal))
		return listenLocal, nil
	}
	h.say("\n%d. Where should exe listen?\n", n)
	h.say("     1) This machine only      127.0.0.1\n")
	h.say("     2) All interfaces         0.0.0.0   (plain HTTP: for a network you trust)\n")
	if h.TailscaleIP != "" {
		h.say("     3) Tailscale              %s\n", h.TailscaleIP)
	}
	for {
		h.say("   Choice [1]: ")
		s, ok := h.readLine()
		if !ok {
			h.say("\n")
			return listenLocal, nil
		}
		s = strings.ToLower(s)
		if s == "" {
			return listenLocal, nil
		}
		for i, k := range kinds {
			if s == strconv.Itoa(i+1) || s == k {
				return k, nil
			}
		}
	}
}

func (h *host) listenWords(kind string) string {
	switch kind {
	case listenAll:
		return "all interfaces (0.0.0.0)"
	case listenTailscale:
		return "Tailscale (" + h.TailscaleIP + ")"
	}
	return "this machine only (127.0.0.1)"
}

func (h *host) askToken(n int, listen string) (string, error) {
	if given := strings.TrimSpace(h.Env(envTokenValue)); given != "" {
		h.say("\n%d. API token: the one in %s\n", n, envTokenValue)
		return given, nil
	}
	want, set, err := h.envBool(envToken)
	if err != nil {
		return "", err
	}
	switch {
	case listen == listenAll:
		// anyone who can reach the API has the host's Terminal
		h.say("\n%d. API token: required when exe listens on every interface. One will be generated.\n", n)
		want = true
	case set:
		h.say("\n%d. API token: %s (%s)\n", n, yesWord(want), envToken)
	case !h.Tty:
		want = listen == listenTailscale
		h.say("\n%d. API token: %s\n", n, yesWord(want))
	case listen == listenTailscale:
		h.say("\n%d. Require an API token?\n   Other devices on your tailnet will be able to reach exe.\n", n)
		want = h.yesNo(true)
	default:
		h.say("\n%d. Require an API token?\n   Every API call then needs it; you paste it once into each browser.\n   Nothing outside this machine can reach exe either way.\n", n)
		want = h.yesNo(false)
	}
	if !want {
		return "", nil
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func yesWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// listWords joins names the way a sentence does: "A, B and C".
func listWords(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func megabytes(n int64) string {
	return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
}

// ---- the first configuration -------------------------------------------------

// firstConfig is the configuration a new install starts with: only what
// was answered. Everything else stays at the daemon's defaults, so a
// default that a later release changes reaches this install too.
type firstConfig struct {
	Listen        string `json:"listen"`
	ProxyListen   string `json:"proxy_listen"`
	SSHListen     string `json:"ssh_listen"`
	AdvertiseHost string `json:"advertise_host"`
	APIToken      string `json:"api_token,omitempty"`
}

// firstConfig turns the answers into addresses. The answer to "where
// should exe listen" is the answer for all three listeners — the desk and
// API, the proxy and the SSH gate. A port something else holds is passed
// over for the next free one, and moved says which.
func (h *host) firstConfig(a answers) (cfg firstConfig, moved []string) {
	bind, advertise := "127.0.0.1", ""
	switch a.Listen {
	case listenAll:
		bind, advertise = "", h.TailscaleIP
	case listenTailscale:
		bind, advertise = h.TailscaleIP, h.TailscaleIP
	}
	pick := func(what string, port int, loopbackToo bool) string {
		got := port
		for p := port; p < port+100; p++ {
			if !h.PortFree(net.JoinHostPort(bind, strconv.Itoa(p))) {
				continue
			}
			// the API on a named address keeps a 127.0.0.1 companion
			if loopbackToo && !h.PortFree(net.JoinHostPort("127.0.0.1", strconv.Itoa(p))) {
				continue
			}
			got = p
			break
		}
		if got != port {
			moved = append(moved, fmt.Sprintf("Port %d is taken on this machine, so %s is on %d.", port, what, got))
		}
		return net.JoinHostPort(bind, strconv.Itoa(got))
	}
	cfg.Listen = pick("the desk", 7777, a.Listen == listenTailscale)
	cfg.ProxyListen = pick("the proxy", 8090, false)
	cfg.SSHListen = pick("the SSH gate", 2222, false)
	cfg.AdvertiseHost = advertise
	cfg.APIToken = a.Token
	return cfg, moved
}

func writeFirstConfig(l layout, cfg firstConfig) error {
	if err := os.MkdirAll(l.State, 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	// O_EXCL: a configuration that appeared since the questions is not ours
	f, err := os.OpenFile(l.Config(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// keptConfig reads what the installer needs from a configuration it did
// not write: where the daemon answers.
func keptConfig(l layout) (listen string) {
	var cfg struct {
		Listen string `json:"listen"`
	}
	if b, err := os.ReadFile(l.Config()); err == nil {
		json.Unmarshal(b, &cfg)
	}
	if cfg.Listen == "" {
		cfg.Listen = config.Default().Listen
	}
	return config.NormalizeListen(cfg.Listen)
}

// deskURL is where a browser finds the desk for a listen address.
func (h *host) deskURL(listen string) string {
	hostname, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	if ip := net.ParseIP(hostname); hostname == "" || (ip != nil && ip.IsUnspecified()) {
		hostname = "127.0.0.1"
		if h.LANIP != "" {
			hostname = h.LANIP
		}
	}
	return "http://" + net.JoinHostPort(hostname, port)
}

// ---- the VM step -------------------------------------------------------------

// step is one command of the VM step, run as root.
type step struct {
	Argv  []string
	Note  string       // said after the command where the question lists it
	Prep  func() error // makes what the command needs, just before it runs
	Retry []string     // run once, and the command again, should it fail
}

func (h *host) asRoot(argv []string) []string {
	switch {
	case h.Root:
		return argv
	case h.Tty:
		return append([]string{"sudo"}, argv...)
	}
	return append([]string{"sudo", "-n"}, argv...) // nobody is there to type a password
}

// shown is a step as a person reads it.
func (h *host) shown(s step) string {
	argv := s.Argv
	if !h.Root {
		argv = append([]string{"sudo"}, argv...)
	}
	words := make([]string, len(argv))
	for i, w := range argv {
		w = h.tilde(w)
		if strings.ContainsAny(w, " \t'\"") {
			w = strconv.Quote(w)
		}
		words[i] = w
	}
	line := strings.Join(words, " ")
	if s.Note != "" {
		line += "   # " + s.Note
	}
	return line
}

// planVMs is what stands between this machine and running VMs, as the
// commands that would close the gap, and what no command here can do.
// helper is the network helper this release brought; scratch is a folder
// for what a step has to download.
func (h *host) planVMs(l layout, helper, scratch string) (plan []step, notes []string) {
	if exists, member := h.GroupInfo("kvm"); exists && !member && !h.Root {
		plan = append(plan, step{Argv: []string{"usermod", "-aG", "kvm", h.User}})
		// a new group reaches a session only when it starts; an ACL on
		// the device reaches this one now, and the group takes over at
		// the next boot
		if !h.OpenKVM() {
			if h.LookTool("setfacl") != "" {
				plan = append(plan, step{Argv: []string{"setfacl", "-m", "u:" + h.User + ":rw", "/dev/kvm"},
					Note: "until the kvm group applies, at the next boot"})
			} else {
				notes = append(notes, "The kvm group applies to a new login session: restart this machine before creating a VM.")
			}
		}
	}

	if h.LookTool("firecracker") == "" {
		if _, ok := firecrackerArchives[runtime.GOARCH]; ok {
			bin := filepath.Join(scratch, "firecracker")
			plan = append(plan, step{
				Argv: []string{"install", "-m", "0755", bin, firecrackerPath},
				Note: "Firecracker " + firecrackerVersion + ", downloaded first",
				Prep: func() error {
					h.say("  downloading Firecracker %s…\n", firecrackerVersion)
					got, err := h.Firecracker(scratch)
					if err != nil {
						return err
					}
					if got != bin {
						return os.Rename(got, bin)
					}
					return nil
				},
			})
		}
	}

	want, _ := release.FileSum(helper)
	have, rooted := h.Helper(helperPath)
	needHelper := want != "" && (have != want || !rooted)

	// what the daemon and its helper run: the helper finds ip and
	// iptables only in the system folders
	tools := []struct{ tool, pkg string }{
		{"ip", "iproute2"}, {"iptables", "iptables"}, {"debugfs", "e2fsprogs"}, {"resize2fs", "e2fsprogs"},
	}
	if needHelper {
		tools = append(tools, struct{ tool, pkg string }{"setcap", "libcap2-bin"})
	}
	var pkgs []string
	seen := map[string]bool{}
	for _, t := range tools {
		if h.LookTool(t.tool) == "" && !seen[t.pkg] {
			seen[t.pkg] = true
			pkgs = append(pkgs, t.pkg)
		}
	}
	sort.Strings(pkgs)
	if len(pkgs) > 0 {
		if h.LookTool("apt-get") != "" {
			plan = append(plan, step{
				Argv:  append([]string{"apt-get", "install", "-y"}, pkgs...),
				Retry: []string{"apt-get", "update"}, // a new machine has no package lists yet
			})
		} else {
			notes = append(notes, "VMs also need these, which this installer only installs with apt: "+listWords(pkgs)+".")
		}
	}

	if needHelper {
		plan = append(plan,
			step{Argv: []string{"install", "-D", "-o", "root", "-g", h.Group, "-m", "0750", l.Helper(), helperPath}},
			step{Argv: []string{"setcap", "cap_net_admin=ep", helperPath}},
		)
	}
	return plan, notes
}

// runPlan runs the VM step, each command said as it runs.
func (h *host) runPlan(plan []step) error {
	for _, s := range plan {
		if s.Prep != nil {
			if err := s.Prep(); err != nil {
				return err
			}
		}
		h.say("  $ %s\n", h.shown(step{Argv: s.Argv}))
		err := h.Run(h.asRoot(s.Argv)...)
		if err != nil && s.Retry != nil {
			h.say("  $ %s\n", h.shown(step{Argv: s.Retry}))
			if rerr := h.Run(h.asRoot(s.Retry)...); rerr == nil {
				h.say("  $ %s\n", h.shown(step{Argv: s.Argv}))
				err = h.Run(h.asRoot(s.Argv)...)
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %w", h.shown(step{Argv: s.Argv}), err)
		}
	}
	return nil
}

// fetchFirecracker downloads the pinned Firecracker release, checks the
// archive against the checksum this binary carries and unpacks the one
// program exe runs.
func fetchFirecracker(dir string) (string, error) {
	fc, ok := firecrackerArchives[runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("no Firecracker for linux/%s", runtime.GOARCH)
	}
	name := "firecracker-" + firecrackerVersion + "-" + fc.arch
	from := "https://github.com/firecracker-microvm/firecracker/releases/download/" + firecrackerVersion + "/" + name + ".tgz"
	tgz := filepath.Join(dir, name+".tgz")
	resp, err := (&http.Client{Timeout: 10 * time.Minute}).Get(from)
	if err != nil {
		return "", fmt.Errorf("download Firecracker: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", from, resp.Status)
	}
	f, err := os.Create(tgz)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, io.LimitReader(resp.Body, 256<<20))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("download Firecracker: %w", err)
	}
	if sum, err := release.FileSum(tgz); err != nil || sum != fc.sum {
		return "", fmt.Errorf("the Firecracker %s archive does not match its checksum (got %s, want %s)", firecrackerVersion, sum, fc.sum)
	}
	bin := filepath.Join(dir, "firecracker")
	if err := untarMember(tgz, "release-"+firecrackerVersion+"-"+fc.arch+"/"+name, bin); err != nil {
		return "", err
	}
	return bin, nil
}

// untarMember writes one file of a .tar.gz to a runnable file at to.
func untarMember(tgz, member, to string) error {
	f, err := os.Open(tgz)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s holds no %s", filepath.Base(tgz), member)
		}
		if err != nil {
			return err
		}
		if hdr.Name != member || hdr.Typeflag != tar.TypeReg {
			continue
		}
		out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, io.LimitReader(tr, 256<<20)); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	}
}

// ---- the service -------------------------------------------------------------

// unitMark heads a unit this installer wrote. One without it is somebody
// else's — a checkout's own deployment, say — and is never touched.
const unitMark = "# Written by exe setup."

func unitText(l layout, home string) string {
	quote := func(s string) string {
		if strings.ContainsAny(s, " \t") {
			return strconv.Quote(s)
		}
		return s
	}
	var b strings.Builder
	b.WriteString(unitMark + " `exe uninstall` removes it; an update leaves it as it is.\n")
	b.WriteString(`#
# KillMode=process is deliberate: the daemon starts tmux servers for the
# Terminal, Claude Code and Codex windows and they live in this unit's
# cgroup. Signalling only the main process keeps them up across a restart.
# On the way down the daemon stops its VMs itself and records them, so they
# come back with the next start.
[Unit]
Description=exe daemon

[Service]
`)
	fmt.Fprintf(&b, "ExecStart=%s serve\n", quote(l.Bin))
	if l.State != filepath.Join(home, ".exe") {
		fmt.Fprintf(&b, "Environment=%s\n", quote("EXE_HOME="+l.State))
	}
	// the sbin folders hold what the VM backend runs (debugfs, resize2fs),
	// and Debian leaves them off a user's PATH
	fmt.Fprintf(&b, "Environment=%s\n", quote("PATH="+filepath.Dir(l.Bin)+":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"))
	b.WriteString(`Restart=always
RestartSec=1
TimeoutStopSec=75
KillMode=process
UMask=0027

[Install]
WantedBy=default.target
`)
	return b.String()
}

// unitOurs reports whether a unit file stands at l.Unit, and whether this
// installer wrote it.
func unitOurs(l layout) (present, ours bool) {
	b, err := os.ReadFile(l.Unit)
	if err != nil {
		return false, false
	}
	return true, strings.HasPrefix(string(b), unitMark)
}

// startService puts the unit in place and (re)starts the daemon under the
// user's own systemd. started is false, with the reason in notes, where
// that cannot be done — the binary is installed all the same.
func (h *host) startService(l layout) (started bool, notes []string) {
	if present, ours := unitOurs(l); present && !ours {
		return false, []string{fmt.Sprintf("%s was not written by this installer, so it is left as it is — and so is the daemon it runs.", h.tilde(l.Unit))}
	}
	if h.Quiet("systemctl", "--user", "show-environment") != nil {
		return false, []string{"There is no systemd user session here, so nothing starts exe for you. Start it with: exe serve"}
	}
	if err := os.MkdirAll(filepath.Dir(l.Unit), 0o755); err != nil {
		return false, []string{err.Error()}
	}
	if err := os.WriteFile(l.Unit, []byte(unitText(l, h.Home)), 0o644); err != nil {
		return false, []string{err.Error()}
	}
	for _, argv := range [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", "exe"},
		{"systemctl", "--user", "restart", "exe"},
	} {
		if err := h.Quiet(argv...); err != nil {
			return false, []string{fmt.Sprintf("%s: %v", strings.Join(argv, " "), err)}
		}
	}
	// lingering keeps a user's services running with nobody logged in,
	// and starts them at boot
	if h.Quiet("loginctl", "enable-linger") != nil && !h.Root {
		notes = append(notes, fmt.Sprintf("exe stops when you log out, and does not start at boot, until you run: sudo loginctl enable-linger %s", h.User))
	}
	return true, notes
}

// ---- putting files in place --------------------------------------------------

// installBinary puts a copy of src at dst, replacing what is there in one
// rename — a running exe keeps the file it started from.
func installBinary(src, dst string) error {
	if a, err := os.Stat(src); err == nil {
		if b, err := os.Stat(dst); err == nil && os.SameFile(a, b) {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".new"
	if err := release.CopyFile(src, tmp, 0o755); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// stageRelease keeps what a release brought beside the binary: the network
// helper, which only root can put where the daemon looks for it.
func stageRelease(l layout, from string) error {
	src := filepath.Join(from, "exe-net-helper")
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("the release holds no exe-net-helper: %w", err)
	}
	return installBinary(src, l.Helper())
}

// unpackApps unpacks the release's apps into scratch and names the
// bundles. A release without them is an empty answer.
func unpackApps(from, scratch string) (dir string, names []string, size int64, err error) {
	tgz := filepath.Join(from, release.AppsAsset)
	if _, serr := os.Stat(tgz); serr != nil {
		return "", nil, 0, nil
	}
	dir = filepath.Join(scratch, "apps")
	if err := release.Extract(tgz, dir); err != nil {
		return "", nil, 0, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, 0, err
	}
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(dir, e.Name(), "app.json")); err == nil && e.IsDir() {
			names = append(names, e.Name())
		}
	}
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			size += info.Size()
		}
		return nil
	})
	return dir, names, size, nil
}

// placeApps brings the unpacked bundles into the apps folder and says
// which it left alone; kept is how many.
func (h *host) placeApps(l layout, dir string) (kept int, err error) {
	prev, err := release.ReadAppsManifest(l.AppsManifest())
	if err != nil {
		return 0, err
	}
	next, changes, err := release.InstallApps(dir, l.Apps(), filepath.Join(l.Release(), "staging"), prev)
	if err != nil {
		return 0, err
	}
	for _, c := range changes {
		if c.Action == "kept" {
			kept++
			h.say("  %s is left as it is: %s.\n", c.Name, c.Why)
		}
	}
	return kept, next.Write(l.AppsManifest())
}

// ---- exe setup ---------------------------------------------------------------

func cmdSetup(args []string) error {
	if release.Version == "" {
		return errors.New("exe setup belongs to a released exe; " + sourceBuildHint)
	}
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	from := fs.String("from", "", "the folder an unpacked release is in (the installer passes it)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	l, err := installLayout()
	if err != nil {
		return err
	}
	h := realHost()
	switch fs.Arg(0) {
	case "":
		return runSetup(l, h, *from)
	case "vms":
		return runSetupVMs(l, h)
	}
	return fmt.Errorf("exe setup %s: there is `exe setup` and `exe setup vms`", fs.Arg(0))
}

func archWords() string {
	if runtime.GOARCH == "amd64" {
		return "x86-64"
	}
	return "ARM64"
}

// runSetup is the installer. from is the folder a release was unpacked
// into; empty when an installed exe runs its setup again.
func runSetup(l layout, h *host, from string) error {
	scratch, err := os.MkdirTemp("", "exe-setup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	h.say("\nexe %s for Linux %s\n", release.Version, archWords())
	h.say("  binary   %s\n  data     %s\n", h.tilde(l.Bin), h.tilde(l.State))

	_, cfgErr := os.Stat(l.Config())
	o := offer{Fresh: os.IsNotExist(cfgErr)}
	if !o.Fresh {
		h.say("\nThe configuration in %s is kept as it is.\n", h.tilde(l.Config()))
	}

	var appsDir string
	_, hadApps := os.Stat(l.AppsManifest())
	if from != "" {
		var names []string
		var size int64
		if appsDir, names, size, err = unpackApps(from, scratch); err != nil {
			return err
		}
		if hadApps != nil { // never installed here: a question
			o.Apps, o.AppsSize = names, size
		}
	}

	helper := l.Helper()
	if from != "" {
		helper = filepath.Join(from, "exe-net-helper")
	}
	var vmNotes []string
	switch {
	case !h.KVM:
		h.say("\nThere is no /dev/kvm on this machine: exe will run the desktop without VMs.\n")
	default:
		o.Plan, vmNotes = h.planVMs(l, helper, scratch)
	}

	a, err := h.ask(l, o)
	if err != nil {
		return err
	}
	h.say("\n")

	// ---- from here on, things are written ----
	if from != "" {
		if err := installBinary(filepath.Join(from, "exe"), l.Bin); err != nil {
			return err
		}
		if err := stageRelease(l, from); err != nil {
			return err
		}
	}
	var notes []string
	listen := keptConfig(l)
	if o.Fresh {
		cfg, moved := h.firstConfig(a)
		if err := writeFirstConfig(l, cfg); err != nil {
			return err
		}
		listen = cfg.Listen
		notes = append(notes, moved...)
	}
	spoke := false // something was said since the questions
	if appsDir != "" && (a.Apps || hadApps == nil) {
		kept, err := h.placeApps(l, appsDir)
		if err != nil {
			return err
		}
		spoke = kept > 0
	}
	if a.VMs {
		spoke = true
		if err := h.runPlan(o.Plan); err != nil {
			// the rest of the install does not need VMs
			notes = append(notes, fmt.Sprintf("The VM step stopped (%v). Run it again with: exe setup vms", err))
		} else {
			notes = append(notes, vmNotes...)
		}
	}

	started, svcNotes := h.startService(l)
	notes = append(notes, svcNotes...)
	url := h.deskURL(listen)
	if spoke {
		h.say("\n")
	}
	if started {
		up := false
		for i := 0; i < 60 && !up; i++ {
			if up = h.Reach("http://" + displayAddr(listen) + "/"); !up {
				time.Sleep(500 * time.Millisecond)
			}
		}
		if !up {
			return fmt.Errorf("exe %s is installed and was started, but it is not answering at %s — its log: journalctl --user -u exe", release.Version, url)
		}
		h.say("exe %s is running.\n\n", release.Version)
	} else {
		h.say("exe %s is installed.\n\n", release.Version)
	}
	h.say("  Desk     %s\n", url)
	if a.Token != "" {
		h.say("  Token    %s\n           paste it into Special → Set API Token… in each browser\n", a.Token)
	}
	h.say("  Update   exe update\n  Remove   exe uninstall\n")

	if !onPath(h.Env("PATH"), filepath.Dir(l.Bin)) {
		notes = append(notes, fmt.Sprintf("%s is not on your PATH. For this shell:\n  export PATH=\"%s:$PATH\"",
			h.tilde(filepath.Dir(l.Bin)), strings.Replace(filepath.Dir(l.Bin), h.Home, "$HOME", 1)))
	}
	for _, n := range notes {
		h.say("\n%s\n", n)
	}
	return nil
}

func onPath(path, dir string) bool {
	for _, p := range filepath.SplitList(path) {
		if filepath.Clean(p) == dir {
			return true
		}
	}
	return false
}

// runSetupVMs is the VM step by itself, for a machine that was installed
// without it.
func runSetupVMs(l layout, h *host) error {
	if !h.KVM {
		return errors.New("there is no /dev/kvm on this machine, so it cannot run VMs; exe runs the desktop without them")
	}
	if _, err := os.Stat(l.Helper()); err != nil {
		return fmt.Errorf("%s is missing — install exe with https://exe.v2core.com/install.sh first", h.tilde(l.Helper()))
	}
	scratch, err := os.MkdirTemp("", "exe-setup-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	plan, notes := h.planVMs(l, l.Helper(), scratch)
	if len(plan) == 0 {
		h.say("This machine is already set up to run VMs.\n")
		for _, n := range notes {
			h.say("\n%s\n", n)
		}
		return nil
	}
	a, err := h.ask(l, offer{Plan: plan})
	if err != nil {
		return err
	}
	if !a.VMs {
		return nil
	}
	h.say("\n")
	if err := h.runPlan(plan); err != nil {
		return err
	}
	// the daemon looks for its hypervisor when it starts
	if _, ours := unitOurs(l); ours && h.Quiet("systemctl", "--user", "restart", "exe") == nil {
		h.say("\nexe was restarted and can run VMs now.\n")
	} else {
		h.say("\nRestart exe for it to run VMs.\n")
	}
	for _, n := range notes {
		h.say("\n%s\n", n)
	}
	return nil
}

// ---- exe uninstall -----------------------------------------------------------

func cmdUninstall(args []string) error {
	if release.Version == "" {
		return errors.New("exe uninstall removes an installed release; this exe was built from source and is removed by deleting its checkout")
	}
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	yes := fs.Bool("y", false, "do not ask")
	if err := fs.Parse(args); err != nil {
		return err
	}
	l, err := installLayout()
	if err != nil {
		return err
	}
	if self, err := os.Executable(); err == nil {
		if real, err := filepath.EvalSymlinks(self); err == nil {
			l.Bin = real
		}
	}
	return runUninstall(l, realHost(), *yes)
}

// runUninstall takes away what the installer put in the user's own
// folders. The data in the state folder stays: VMs, Workspace, app data
// and the configuration are not the installer's to delete.
func runUninstall(l layout, h *host, yes bool) error {
	if !yes {
		if !h.Tty {
			return errors.New("exe uninstall asks before it removes anything; say `exe uninstall -y` where nobody can answer")
		}
		h.say("Remove exe from this machine? Its VMs stop. Your data in %s is kept.\n", h.tilde(l.State))
		if !h.yesNo(false) {
			return nil
		}
	}
	if _, ours := unitOurs(l); ours {
		h.Quiet("systemctl", "--user", "disable", "--now", "exe")
		if err := os.Remove(l.Unit); err != nil {
			return err
		}
		h.Quiet("systemctl", "--user", "daemon-reload")
		h.say("Stopped exe and removed %s\n", h.tilde(l.Unit))
	} else if present, _ := unitOurs(l); present {
		h.say("%s was not written by the installer and is left as it is.\n", h.tilde(l.Unit))
	}
	if err := os.Remove(l.Bin); err != nil && !os.IsNotExist(err) {
		return err
	}
	h.say("Removed %s\n", h.tilde(l.Bin))
	// the apps the installer brought go with it; one that was edited since
	// is its owner's
	if m, err := release.ReadAppsManifest(l.AppsManifest()); err == nil && len(m) > 0 {
		removed, kept, err := release.RemoveApps(l.Apps(), m)
		if err != nil {
			return err
		}
		if len(removed) > 0 {
			h.say("Removed the apps it installed: %s\n", listWords(removed))
		}
		if len(kept) > 0 {
			h.say("Kept, because you edited %s: %s\n", map[bool]string{true: "it", false: "them"}[len(kept) == 1], listWords(kept))
		}
	}
	if err := os.RemoveAll(l.Release()); err != nil {
		return err
	}
	if sum, _ := h.Helper(helperPath); sum != "" {
		h.say("\nThe network helper is root's. Remove it with:\n  sudo rm %s\n", helperPath)
	}
	h.say("\nYour data is still in %s.\n", h.tilde(l.State))
	return nil
}
