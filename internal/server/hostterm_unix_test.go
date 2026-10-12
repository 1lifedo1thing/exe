//go:build !windows

package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
