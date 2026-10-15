package server

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"exe/internal/hostinfo"
)

// What Archive gets back. A session's processes — the pane's shell, the
// CLI in it and whatever the CLI started (MCP servers, tool shells) —
// are read from /proc before tmux ends them: the anonymous memory each
// holds (RssAnon) and what it holds in swap (VmSwap), summed over the
// tree. That is what the machine gets back when they are gone; the
// file-backed pages (the CLI's own binary, mapped by every session) stay
// shared and are not counted. Once the pids are gone — the CLI exits
// within a second of the hang-up — MemAvailable is read, the kernel's
// own figure for what can be allocated without swapping. The window
// toasts both (archiveFigures); DELETE on the session answers with them.
// Where /proc does not tell (macOS, Windows) the processes are not
// measured and only what is available is said.

// archiveFigures is what an archived session gave back.
type archiveFigures struct {
	Freed     uint64 `json:"freed"`     // the anonymous memory its processes held, bytes
	Swap      uint64 `json:"swap"`      // and what they held in swap
	Available uint64 `json:"available"` // MemAvailable once they were gone
	Measured  bool   `json:"measured"`  // the processes were read: false where /proc does not tell
}

// procDir is where the kernel tells about processes; tests point the
// readers at a tree of their own.
const procDir = "/proc"

// archiveSettle is how long the processes get to go before the figures
// are read regardless: a CLI exits in about a second.
const archiveSettle = 5 * time.Second

// sessionProcs is the process tree of a tmux session: the pid of each
// pane's process and every descendant, read before the session is
// killed. Nothing where tmux or /proc does not tell.
func sessionProcs(session string) []int {
	cmd := tmuxCmd("list-panes", "-s", "-t", "="+session, "-F", "#{pane_pid}")
	if cmd == nil {
		return nil
	}
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var roots []int
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil && pid > 0 {
			roots = append(roots, pid)
		}
	}
	return procTree(procDir, roots)
}

// procTree is the pids under root for roots and all their descendants,
// breadth first, each once: a child is listed under the thread that
// started it (task/<tid>/children).
func procTree(root string, roots []int) []int {
	var all []int
	seen := map[int]bool{}
	pids := roots
	for len(pids) > 0 {
		var next []int
		for _, p := range pids {
			if seen[p] {
				continue
			}
			seen[p] = true
			all = append(all, p)
			proc := filepath.Join(root, strconv.Itoa(p))
			tasks, _ := os.ReadDir(filepath.Join(proc, "task"))
			for _, task := range tasks {
				b, _ := os.ReadFile(filepath.Join(proc, "task", task.Name(), "children"))
				for _, f := range strings.Fields(string(b)) {
					if c, err := strconv.Atoi(f); err == nil && !seen[c] {
						next = append(next, c)
					}
				}
			}
		}
		pids = next
	}
	return all
}

// procMemory is what a process holds, from <root>/<pid>/status: its
// anonymous resident memory and its swap, in bytes; zero and false when
// the file does not tell.
func procMemory(root string, pid int) (anon, swap uint64, ok bool) {
	f, err := os.Open(filepath.Join(root, strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, found := strings.Cut(sc.Text(), ":")
		if !found {
			continue
		}
		switch k {
		case "RssAnon", "VmSwap":
			fs := strings.Fields(v)
			if len(fs) == 0 {
				continue
			}
			n, err := strconv.ParseUint(fs[0], 10, 64)
			if err != nil {
				continue
			}
			if len(fs) > 1 && fs[1] == "kB" {
				n *= 1024
			}
			if k == "RssAnon" {
				anon = n
			} else {
				swap = n
			}
			ok = true
		}
	}
	return anon, swap, ok
}

// procMem is what one process of a session's held.
type procMem struct {
	pid        int
	anon, swap uint64
}

// procsMemory reads procMemory over a tree, each pid with its own
// figures; measured says whether any of them could be read.
func procsMemory(root string, pids []int) (procs []procMem, measured bool) {
	for _, p := range pids {
		a, s, ok := procMemory(root, p)
		procs = append(procs, procMem{p, a, s})
		measured = measured || ok
	}
	return procs, measured
}

// procGone says whether a process has ended: no status under root for
// it, or a zombie (State: Z), which holds no memory and only waits to be
// reaped.
func procGone(root string, pid int) bool {
	b, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "status"))
	if err != nil {
		return true
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "State:"); ok {
			return strings.HasPrefix(strings.TrimSpace(v), "Z")
		}
	}
	return false
}

// waitProcsGone polls until every pid is gone or the time is up; it
// says whether they all went.
func waitProcsGone(root string, pids []int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		gone := true
		for _, p := range pids {
			if !procGone(root, p) {
				gone = false
				break
			}
		}
		if gone {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// archiveMeasure reads a session's processes before it is killed: each
// pid, so its end can be waited for, with what it held.
type archiveMeasure struct {
	root     string
	procs    []procMem
	measured bool
}

func measureSession(session string) archiveMeasure {
	return measureIn(procDir, sessionProcs(session))
}

func measureIn(root string, pids []int) archiveMeasure {
	m := archiveMeasure{root: root}
	m.procs, m.measured = procsMemory(root, pids)
	return m
}

// settle waits for the processes to go (archiveSettle at most), then
// reads what the machine has available. Freed is what the processes
// that went held: one that outlives the wait — a tool shell ignoring
// the hang-up — is not counted as given back.
func (m archiveMeasure) settle() archiveFigures {
	return m.settleIn(archiveSettle)
}

func (m archiveMeasure) settleIn(timeout time.Duration) archiveFigures {
	pids := make([]int, len(m.procs))
	for i, p := range m.procs {
		pids[i] = p.pid
	}
	all := waitProcsGone(m.root, pids, timeout)
	f := archiveFigures{Available: hostinfo.Mem().Available, Measured: m.measured}
	for _, p := range m.procs {
		if all || procGone(m.root, p.pid) {
			f.Freed += p.anon
			f.Swap += p.swap
		}
	}
	return f
}
