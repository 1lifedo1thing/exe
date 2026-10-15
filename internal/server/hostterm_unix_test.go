//go:build !windows

package server

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

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
// sessions. Check the working folder and the exact argv received by each CLI.
func TestAgentIssueLaunch(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	tmuxSocket = fmt.Sprintf("exe-test-issue-%d", os.Getpid())
	defer func() { tmuxCmd("kill-server").Run(); tmuxSocket = "" }()
	for _, app := range []string{"claude", "codex"} {
		t.Run(app, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, app)
			script := "#!/bin/sh\nif [ \"$1\" = --help ]; then exit 0; fi\npwd > cwd\nprintf '%s\\0' \"$@\" > argv\nexec sleep 20\n"
			if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			a := hostAgents[app]
			a.bin = bin
			prompt := "--do not parse as flags\nIt's an issue: $(touch injected); `touch injected`; 中文"
			extra, err := agentTaskArgs(prompt)
			if err != nil {
				t.Fatal(err)
			}
			s := &Server{StateDir: t.TempDir()}
			name := "exe-test-issue-" + app
			if err := s.newAgentSessionIn(a, name, dir, extra...); err != nil {
				t.Fatal(err)
			}
			defer tmuxCmd("kill-session", "-t", "="+name).Run()
			var got []byte
			for i := 0; i < 100; i++ {
				got, _ = os.ReadFile(filepath.Join(dir, "argv"))
				if len(got) > 0 {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			args := strings.Split(strings.TrimSuffix(string(got), "\x00"), "\x00")
			if len(args) < 2 || args[len(args)-2] != "--" || args[len(args)-1] != prompt {
				t.Fatalf("task arguments were changed: %q", args)
			}
			cwd, _ := os.ReadFile(filepath.Join(dir, "cwd"))
			if strings.TrimSpace(string(cwd)) != dir {
				t.Fatalf("cwd = %q", cwd)
			}
			if _, err := os.Stat(filepath.Join(dir, "injected")); !os.IsNotExist(err) {
				t.Fatal("prompt executed as shell code")
			}
		})
	}
	for _, bad := range []string{"a\x00b", strings.Repeat("x", 64*1024+1)} {
		if _, err := agentTaskArgs(bad); err == nil {
			t.Fatal("invalid task accepted")
		}
	}
	if args, err := agentTaskArgs(""); err != nil || len(args) != 0 {
		t.Fatal("empty task changed ordinary New")
	}
	if _, err := agentTaskArgs(strings.Repeat("x", 64*1024)); err != nil {
		t.Fatal(err)
	}
}
