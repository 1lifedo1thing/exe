package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// a /proc of the test's own: pid 100 (the pane's shell) started 101 and
// 102 (the CLI and an MCP server); 102 started 103, a zombie by now.
func fakeProc(t *testing.T) string {
	root := t.TempDir()
	put := func(pid int, status, children string) {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(filepath.Join(dir, "task", strconv.Itoa(pid)), 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o644)
		os.WriteFile(filepath.Join(dir, "task", strconv.Itoa(pid), "children"), []byte(children), 0o644)
	}
	put(100, "Name:\tbash\nState:\tS (sleeping)\nVmSwap:\t     12 kB\nRssAnon:\t   1000 kB\n", "101 102 ")
	put(101, "Name:\tclaude\nState:\tS (sleeping)\nRssAnon:\t 250000 kB\nVmSwap:\t  50000 kB\n", "")
	put(102, "Name:\tnode\nState:\tR (running)\nRssAnon:\t  30000 kB\nVmSwap:\t      0 kB\n", "103")
	put(103, "Name:\tsh\nState:\tZ (zombie)\nRssAnon:\t      0 kB\n", "")
	return root
}

func TestProcTreeAndMemory(t *testing.T) {
	root := fakeProc(t)
	pids := procTree(root, []int{100})
	if want := []int{100, 101, 102, 103}; len(pids) != 4 || pids[0] != 100 || pids[1] != 101 || pids[2] != 102 || pids[3] != 103 {
		t.Fatalf("tree %v, want %v", pids, want)
	}
	procs, measured := procsMemory(root, pids)
	var anon, swap uint64
	for _, p := range procs {
		anon += p.anon
		swap += p.swap
	}
	if !measured || len(procs) != 4 || anon != (1000+250000+30000)*1024 || swap != (12+50000)*1024 {
		t.Fatalf("anon %d swap %d measured %v over %v", anon, swap, measured, procs)
	}
	m := measureIn(root, pids) // before any of them goes, as archive reads them
	if procGone(root, 101) || !procGone(root, 103) || !procGone(root, 999) {
		t.Fatal("101 lives, the zombie 103 and the missing 999 are gone")
	}
	if waitProcsGone(root, []int{101}, 120*time.Millisecond) {
		t.Fatal("101 did not go")
	}
	os.RemoveAll(filepath.Join(root, "101"))
	if !waitProcsGone(root, []int{101, 103, 999}, time.Second) {
		t.Fatal("101 gone with the zombie and the one that never was")
	}
	if waitProcsGone(root, pids, 50*time.Millisecond) {
		t.Fatal("100 and 102 still live")
	}
	// nothing under a tree that is not there, and not measured
	if _, ok := procsMemory(filepath.Join(root, "none"), []int{1}); ok {
		t.Fatal("measured without a status file")
	}
	// settling while 102 lives on past the wait: only what went is freed
	// — the shell's, the CLI's and the zombie's nothing — and the
	// machine's memory is read regardless
	os.RemoveAll(filepath.Join(root, "100"))
	f := m.settleIn(150 * time.Millisecond)
	if !f.Measured || f.Freed != (1000+250000)*1024 || f.Swap != (12+50000)*1024 || f.Available == 0 {
		t.Fatalf("settled with 102 alive: %+v", f)
	}
	os.RemoveAll(filepath.Join(root, "102"))
	if f = m.settleIn(time.Second); f.Freed != anon || f.Swap != swap {
		t.Fatalf("settled with all gone: %+v", f)
	}
}

// the readers on the real /proc: a shell and the sleep it starts are
// found as a tree, hold memory, and are gone once their group is killed
func TestProcTreeReal(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc is Linux's")
	}
	cmd := exec.Command("sh", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); cmd.Wait() }()
	var pids []int
	for i := 0; i < 50 && len(pids) < 2; i++ {
		time.Sleep(20 * time.Millisecond)
		pids = procTree(procDir, []int{cmd.Process.Pid})
	}
	if len(pids) < 2 || pids[0] != cmd.Process.Pid {
		t.Fatalf("tree %v: want the shell and its sleep", pids)
	}
	procs, measured := procsMemory(procDir, pids)
	if !measured || len(procs) != len(pids) || procs[0].anon == 0 {
		t.Fatalf("measured %v: %v", measured, procs)
	}
	if waitProcsGone(procDir, pids, 50*time.Millisecond) {
		t.Fatal("still running")
	}
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	cmd.Wait() // reaps the shell; the sleep is reaped by init once orphaned
	if !waitProcsGone(procDir, pids, 3*time.Second) {
		t.Fatalf("not gone: %v", pids)
	}
}
