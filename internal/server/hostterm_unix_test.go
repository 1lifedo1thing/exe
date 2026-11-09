//go:build !windows

package server

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A plain Terminal starts as a shell, then runs Codex, then another
// fullscreen program. Its wheel follows the current program without
// recalling prompts in Codex or sending Codex mouse reports to the other
// program. A renamed cat is inert and exposes the bytes that arrive.
func TestTerminalForegroundScrollLive(t *testing.T) {
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("no cat")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	dir := t.TempDir()
	code, err := os.ReadFile(cat)
	if err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(dir, "codex")
	if err := os.WriteFile(fake, code, 0o755); err != nil {
		t.Fatal(err)
	}
	tmuxSocket = fmt.Sprintf("exe-test-terminal-wheel-%d", os.Getpid())
	defer func() { tmuxCmd("kill-server").Run(); tmuxSocket = "" }()
	t.Setenv("HOME", dir)
	t.Setenv("SHELL", "/bin/sh")
	s := &Server{StateDir: t.TempDir()}
	ts, err := s.newTermSession()
	if err != nil {
		t.Fatal(err)
	}
	host, err := startTermClient(ts.Name, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	go io.Copy(io.Discard, host)
	sh := host.(*unixShell)
	pane := func(format string) string {
		out, _ := tmuxCmd("display-message", "-p", "-t", ts.Name, format).Output()
		return strings.TrimSpace(string(out))
	}
	wait := func(what string, ok func() bool) {
		t.Helper()
		for i := 0; i < 100; i++ {
			if ok() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("timed out waiting for " + what)
	}
	wait("the Terminal client", func() bool { return sh.clientTTY() != "" })
	for _, tc := range []struct{ command, name, want string }{
		{shQuote(fake), "codex", "^[[<64;40;8M^[[<64;40;8M^[[<65;40;8M"},
		{shQuote(cat), filepath.Base(cat), "^[[A^[[A^[[A^[[A^[[A^[[A^[[B^[[B^[[B"},
	} {
		sh.Write([]byte("printf '\\033[?1049h\\033[H\\033[2J'; " + tc.command + "\n"))
		wait("foreground "+tc.name, func() bool { return pane("#{alternate_on} #{pane_current_command}") == "1 "+tc.name })
		for _, lines := range []int{6, -3, 0} {
			if err := sh.Scroll(lines); err != nil {
				t.Fatal(err)
			}
		}
		wait(tc.name+" wheel bytes", func() bool {
			out, _ := tmuxCmd("capture-pane", "-p", "-J", "-t", ts.Name).Output()
			return strings.Join(strings.Fields(string(out)), "") == tc.want
		})
		if got := pane("#{pane_in_mode}"); got != "0" {
			t.Fatalf("%s entered tmux copy mode", tc.name)
		}
		sh.Write([]byte{3}) // end only the test's foreground cat
		wait("the shell", func() bool { return pane("#{pane_current_command}") == "sh" })
		sh.Write([]byte("printf '\\033[?1049l'\n"))
		wait("the shell screen", func() bool { return pane("#{alternate_on}") == "0" })
	}
}

// Exercise the installed CLI's help and the shell command used by tmux:
// modern Codex opts out of the shared server, old or failed probes retain
// the old launch, and resume arguments survive shell quoting in each case.
func TestCodexStandaloneLaunch(t *testing.T) {
	for _, tc := range []struct {
		name, help string
		want       bool
	}{
		{"supported", "printf '  --no-daemon  Run without the shared server\\n'", true},
		{"older", "printf '  --no-alt-screen  Use inline mode\\n'", false},
		{"failed", "printf '  --no-daemon\\n'; exit 1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "CLI's bin")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			// The npm shim needs its own directory on PATH, even for help.
			helper := filepath.Join(dir, "exe-test-codex-help")
			if err := os.WriteFile(helper, []byte("#!/bin/sh\n"+tc.help+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(dir, "codex")
			script := "#!/bin/sh\nif [ \"$1\" = --help ]; then exec exe-test-codex-help; fi\nprintf '%s\\n' \"$@\"\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, "session.status.json")
			args, line := agentCommand(hostAgents["codex"], bin, file)
			if got := slices.Contains(args, "--no-daemon"); got != tc.want {
				t.Fatalf("standalone = %v, want %v", got, tc.want)
			}
			if tc.want {
				args = args[1:]
			}
			if want := agentStatusArgs(hostAgents["codex"], file, codexConfigPath()); !slices.Equal(args, want) {
				t.Fatal("standalone mode changed the notification overrides")
			}
			extra := []string{"resume", "55d60159-76bf-4dbd-862a-6c02595708e3", "it's a resumed prompt"}
			for _, arg := range extra {
				line += " " + shQuote(arg)
			}
			out, err := exec.Command("sh", "-c", line).Output()
			if err != nil {
				t.Fatal(err)
			}
			want := append(args, extra...)
			if tc.want {
				want = append([]string{"--no-daemon"}, want...)
			}
			if string(out) != strings.Join(want, "\n")+"\n" {
				t.Fatal("tmux command did not preserve the launch arguments")
			}
		})
	}
}

// A real, isolated tmux launch with inert CLIs: no model call and no live
// sessions. Each CLI gets its first message whole, as one argument after
// "--", in the folder asked for: a whole 64 KiB of it — four times what
// tmux takes on a command line — full of quotes and shell, which runs
// nowhere. The file it came in is gone once the CLI starts, and a launch
// that fails takes its file with it.
func TestAgentIssueLaunch(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	tmuxSocket = fmt.Sprintf("exe-test-issue-%d", os.Getpid())
	defer func() { tmuxCmd("kill-server").Run(); tmuxSocket = "" }()
	piece := "It's an issue: $(touch injected); `touch injected`; \\'\"; \\n 中文\n"
	head := "--do not parse as flags\n"
	var b strings.Builder
	b.WriteString(head)
	for b.Len()+len(piece) <= maxAgentTask {
		b.WriteString(piece)
	}
	task := b.String() + strings.Repeat(".", maxAgentTask-b.Len())
	if err := agentTaskOK(task); err != nil || len(task) != maxAgentTask {
		t.Fatalf("a %d-byte task: %v", len(task), err)
	}
	sid := "55d60159-76bf-4dbd-862a-6c02595708e3"
	for _, c := range []struct {
		app, task string
		req       agentLaunchRequest
		want      []string // the arguments before the task
	}{
		{app: "claude", task: task},
		{app: "codex", task: task},
		{app: "claude"},
		{app: "codex", task: "go on", req: agentLaunchRequest{Resume: sid}, want: []string{"resume", sid}},
	} {
		t.Run(c.app+fmt.Sprint(len(c.task)), func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, c.app)
			script := "#!/bin/sh\nif [ \"$1\" = --help ]; then exit 0; fi\npwd > cwd\nprintf '%s\\0' \"$@\" > argv.part && mv argv.part argv\nexec sleep 20\n"
			if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			a := hostAgents[c.app]
			a.bin = bin
			extra, err := agentLaunchArgs(a, c.req)
			if err != nil {
				t.Fatal(err)
			}
			s := &Server{StateDir: t.TempDir()}
			name := "exe-test-issue-" + c.app
			if err := s.newAgentTaskSession(a, name, dir, c.task, extra...); err != nil {
				t.Fatal(err)
			}
			defer tmuxCmd("kill-session", "-t", "="+name).Run()
			var got []byte
			for i := 0; i < 200 && len(got) == 0; i++ {
				time.Sleep(20 * time.Millisecond)
				got, _ = os.ReadFile(filepath.Join(dir, "argv"))
			}
			args := strings.Split(strings.TrimSuffix(string(got), "\x00"), "\x00")
			want := append([]string{}, c.want...)
			if c.task != "" {
				want = append(want, "--", c.task)
			}
			if len(args) < len(want) || !slices.Equal(args[len(args)-len(want):], want) || c.task == "" && slices.Contains(args, "--") {
				t.Fatalf("the CLI got %d arguments, ending %q", len(args), args[max(0, len(args)-3):])
			}
			if cwd, _ := os.ReadFile(filepath.Join(dir, "cwd")); strings.TrimSpace(string(cwd)) != dir {
				t.Fatalf("cwd = %q", cwd)
			}
			if _, err := os.Stat(filepath.Join(dir, "injected")); !os.IsNotExist(err) {
				t.Fatal("the task ran as shell code")
			}
			left, _ := filepath.Glob(filepath.Join(s.StateDir, "agents", "*.task"))
			if len(left) > 0 {
				t.Fatalf("task files left behind: %q", left)
			}
			// the same name again: tmux refuses it, and the file goes too
			if err := s.newAgentTaskSession(a, name, dir, c.task, extra...); err == nil {
				t.Fatal("a second session under the same name was started")
			}
			if left, _ := filepath.Glob(filepath.Join(s.StateDir, "agents", "*.task")); len(left) > 0 {
				t.Fatalf("a failed launch left %q", left)
			}
		})
	}
	for _, bad := range []string{"a\x00b", strings.Repeat("x", maxAgentTask+1)} {
		if err := agentTaskOK(bad); err == nil {
			t.Fatalf("a %d-byte task with NUL %v was taken", len(bad), strings.ContainsRune(bad, 0))
		}
	}
}
