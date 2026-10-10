//go:build windows

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// These run on a real Windows only (the test binary is cross-built and
// run there): they are the parts a stand-in cannot answer for.

// TestMain lets a copy of this test binary be a program that just runs,
// for the tests that need one.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "sleep-for-a-test" {
		time.Sleep(2 * time.Minute)
		return
	}
	// a program that says where it is, then waits to be told to stop the
	// way the daemon is told: through its console
	if len(os.Args) > 2 && os.Args[1] == "hear-the-console" {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		os.WriteFile(os.Args[2], []byte(strconv.Itoa(os.Getpid())), 0o644)
		select {
		case got := <-sig:
			os.WriteFile(os.Args[2]+".heard", []byte(got.String()), 0o644)
		case <-time.After(2 * time.Minute):
		}
		return
	}
	os.Exit(m.Run())
}

func sleeper(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "sleeper.exe")
	if err := os.WriteFile(p, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// The update's last step on the real thing: a program that is running is
// replaced, and goes on running.
func TestOnRealWindowsReplaceARunningProgram(t *testing.T) {
	bin := sleeper(t)
	c := exec.Command(bin, "sleep-for-a-test")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer c.Process.Kill()
	time.Sleep(300 * time.Millisecond)

	if err := os.WriteFile(bin, []byte("x"), 0o755); err == nil {
		t.Fatal("Windows let a running program be overwritten: this test proves nothing here")
	}
	commit, discard, err := prepareBinary(writeFile(t, "the next release"), bin)
	if err != nil {
		t.Fatal(err)
	}
	defer discard()
	if err := commit(); err != nil {
		t.Fatalf("the running program could not be replaced: %v", err)
	}
	if b, _ := os.ReadFile(bin); string(b) != "the next release" {
		t.Errorf("in place: %q", b)
	}
	if _, err := os.Stat(bin + ".old"); err != nil {
		t.Errorf("the running program was not moved aside: %v", err)
	}
	if c.ProcessState != nil {
		t.Error("the program stopped when its file was replaced")
	}
	// once it has exited, the next update clears the old file away
	c.Process.Kill()
	c.Wait()
	commit, discard, err = prepareBinary(writeFile(t, "the release after"), bin)
	if err != nil {
		t.Fatal(err)
	}
	defer discard()
	if err := commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bin + ".old"); !os.IsNotExist(err) {
		t.Errorf("what the earlier update moved aside is still there: %v", err)
	}
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "new.exe")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOnRealWindowsDrives(t *testing.T) {
	driveBusSeen = func(letter string, bus uint32) { t.Logf("%s is on bus type %d", letter, bus) }
	defer func() { driveBusSeen = nil }()
	drives := winDrives()
	if len(drives) == 0 {
		t.Fatal("no fixed NTFS drive was found")
	}
	homes := 0
	for _, d := range drives {
		t.Logf("%s  %s free of %s  kind %q  network %v  home %v", d.Letter, gigabytes(d.Free), gigabytes(d.Size), d.Kind, d.Network, d.Home)
		if d.Size <= 0 || d.Free < 0 || d.Free > d.Size || len(d.Letter) != 2 {
			t.Errorf("%+v does not add up", d)
		}
		if d.Home {
			homes++
		}
	}
	if homes != 1 {
		t.Errorf("%d drives claim the home folder", homes)
	}
	t.Logf("the default would be %s", drives[pickDrive(drives)].Letter)
}

func TestOnRealWindowsHost(t *testing.T) {
	w := newWinHost()
	t.Logf("elevated %v, desktop session %v, can run VMs %v, hypervisor platform %v, QEMU %q", w.Elevated, w.Session, w.CanVM, w.WHPX(), w.QEMU())
}

// The sign-in entry, under a name of the test's own.
func TestOnRealWindowsSignInEntry(t *testing.T) {
	old := runValue
	runValue = "exe-test-entry"
	defer func() { runValue = old }()
	w := newWinHost()
	defer w.SetLogin("")
	if got := w.Login(); got != "" {
		t.Fatalf("a test entry is already there: %q", got)
	}
	cmd := `C:\Windows\System32\conhost.exe --headless "C:\Users\Ada Lovelace\exe.exe" serve`
	if err := w.SetLogin(cmd); err != nil {
		t.Fatal(err)
	}
	if got := w.Login(); got != cmd {
		t.Errorf("read back %q", got)
	}
	if err := w.SetLogin(""); err != nil {
		t.Fatal(err)
	}
	if got := w.Login(); got != "" {
		t.Errorf("after removing it: %q", got)
	}
	if err := w.SetLogin(""); err != nil {
		t.Errorf("removing an entry that is not there: %v", err)
	}
}

// The user's PATH gains the folder once, loses it again, and is otherwise
// left exactly as it was.
func TestOnRealWindowsUserPath(t *testing.T) {
	read := func() (string, uint32) {
		k, err := registry.OpenKey(registry.CURRENT_USER, envKey, registry.QUERY_VALUE)
		if err != nil {
			t.Fatal(err)
		}
		defer k.Close()
		v, typ, _ := k.GetStringValue("Path")
		return v, typ
	}
	before, typ := read()
	dir := `C:\exe-test-` + strings.ReplaceAll(t.Name(), "/", "-")
	w := newWinHost()
	defer w.DelPath(dir)
	added, err := w.AddPath(dir)
	if err != nil || !added {
		t.Fatalf("AddPath: %v, %v", added, err)
	}
	if now, _ := read(); !strings.HasSuffix(now, ";"+dir) && now != dir {
		t.Errorf("PATH is now %q", now)
	}
	if again, err := w.AddPath(dir); err != nil || again {
		t.Errorf("adding it twice: %v, %v", again, err)
	}
	if err := w.DelPath(dir); err != nil {
		t.Fatal(err)
	}
	after, typAfter := read()
	if strings.TrimRight(after, ";") != strings.TrimRight(before, ";") || typ != typAfter {
		t.Errorf("PATH was %q (type %d) and is %q (type %d)", before, typ, after, typAfter)
	}
}

// Start and Kill: a program started for good, found by its file, ended.
func TestOnRealWindowsStartAndKill(t *testing.T) {
	bin := sleeper(t)
	w := newWinHost()
	if err := w.Start([]string{bin, "sleep-for-a-test"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := w.Kill(bin); n != 1 {
		t.Errorf("ended %d processes, want the one", n)
	}
	time.Sleep(300 * time.Millisecond)
	if n := w.Kill(bin); n != 0 {
		t.Errorf("%d still running after being ended", n)
	}
}

// The daemon has to hear Windows say that its user is signing out, and
// Windows says that through a program's console. A program started with no
// console at all (as the daemon once was) is simply ended, and exe never
// writes down which VMs were running. So: what Start starts has a console,
// and an event sent to it arrives.
func TestOnRealWindowsStartedProgramHearsItsConsole(t *testing.T) {
	bin := sleeper(t)
	mark := filepath.Join(t.TempDir(), "pid")
	w := newWinHost()
	if err := w.Start([]string{bin, "hear-the-console", mark}); err != nil {
		t.Fatal(err)
	}
	defer w.Kill(bin)
	pid := 0
	for i := 0; i < 50 && pid == 0; i++ {
		time.Sleep(100 * time.Millisecond)
		if b, err := os.ReadFile(mark); err == nil {
			pid, _ = strconv.Atoi(string(b))
		}
	}
	if pid == 0 {
		t.Fatal("the program never started")
	}

	// step over to its console for a moment, and press Ctrl+Break there —
	// for its process group, which this test is not in
	k := windows.NewLazySystemDLL("kernel32.dll")
	attach, free := k.NewProc("AttachConsole"), k.NewProc("FreeConsole")
	free.Call()
	defer attach.Call(uintptr(^uint32(0))) // back to the console this test came from
	if ok, _, err := attach.Call(uintptr(pid)); ok == 0 {
		t.Fatalf("the program has no console to be told through: %v", err)
	}
	err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid))
	free.Call()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if b, err := os.ReadFile(mark + ".heard"); err == nil {
			if string(b) != "interrupt" {
				t.Errorf("heard %q, want an interrupt", b)
			}
			return
		}
	}
	t.Error("the event never reached the program")
}
