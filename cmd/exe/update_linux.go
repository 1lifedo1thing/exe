//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"exe/internal/config"
	"exe/internal/release"
)

// exe update: move an installed release to the latest one. The binary is
// replaced in one rename, so the running daemon goes on with the file it
// started from until it restarts — and restarting stops and starts every
// VM, which is why that part is asked.
//
// That rename is the last thing an update does. The old binary does the
// whole update, so as long as it is the one in place, an update that
// failed is simply run again; with the new binary in place first, the
// retry would be the new binary, which finds itself up to date and
// repairs nothing.

func cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	check := fs.Bool("check", false, "only say whether a newer release exists")
	yes := fs.Bool("y", false, "restart the daemon without asking")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if release.Version == "" {
		return errors.New(sourceBuildHint)
	}
	l, err := installLayout()
	if err != nil {
		return err
	}
	// the binary that is running is the one to replace, wherever it was put
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	l.Bin = self

	u := updater{
		Layout: l, Host: realHost(), Client: release.NewClient(),
		Probe: func(bin string) (string, error) {
			out, err := exec.Command(bin, "version").Output()
			return strings.TrimSpace(string(out)), err
		},
	}
	if _, ours := unitOurs(l); ours {
		u.RestartHint = "systemctl --user restart exe"
	}
	// the daemon is asked the way the rest of the CLI asks it
	if cfg, err := config.Load(); err == nil {
		u.Running = func() bool {
			return u.Host.Reach("http://" + displayAddr(cfg.Listen) + "/")
		}
		u.Restart = func() error {
			resp, err := api(cfg, "POST", "/v1/daemon/restart", nil, 30*time.Second)
			if err != nil {
				return err
			}
			return decodeInto(resp, nil)
		}
	}
	return u.run(context.Background(), *check, *yes)
}

type updater struct {
	Layout layout
	Host   *host
	Client *release.Client
	// Probe runs a binary's `version` and returns what it printed.
	Probe   func(bin string) (string, error)
	Running func() bool  // the daemon is up
	Restart func() error // ask it to restart
	// RestartHint is the command that restarts the daemon later, where
	// the installer's own service runs it.
	RestartHint string
}

func (u *updater) run(ctx context.Context, check, yes bool) error {
	h, l := u.Host, u.Layout
	latest, err := u.Client.Latest(ctx)
	if err != nil {
		return err
	}
	if release.Compare(latest, release.Version) <= 0 {
		h.say("exe %s is the latest version.\n", release.Version)
		return nil
	}
	if check {
		h.say("exe %s is available; this is %s. Install it with: exe update\n", latest, release.Version)
		return nil
	}

	scratch, err := os.MkdirTemp("", "exe-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	binary := release.BinaryAsset(runtime.GOARCH)
	names := []string{binary}
	apps := release.AppsTracked(l.AppsManifest())
	if apps { // the apps came with the install, so they follow it
		names = append(names, release.AppsAsset)
	}
	h.say("Downloading exe %s…\n", latest)
	if err := u.Client.Fetch(ctx, latest, scratch, names...); err != nil {
		return err
	}
	if err := release.Extract(filepath.Join(scratch, binary), scratch); err != nil {
		return err
	}
	// before it replaces this one, the new binary has to run here and be
	// the release that was asked for
	fresh := filepath.Join(scratch, "exe")
	said, err := u.Probe(fresh)
	if err != nil {
		return fmt.Errorf("the downloaded exe does not run on this machine: %w", err)
	}
	if !strings.HasPrefix(said, "exe "+latest+" ") {
		return fmt.Errorf("the download is not exe %s: it calls itself %q", latest, said)
	}

	// the copy first, the rename last (prepareBinary), and between them
	// the steps that can be done twice: the helper and the apps
	commit, discard, err := prepareBinary(fresh, l.Bin)
	if err != nil {
		return fmt.Errorf("replace %s: %w", l.Bin, err)
	}
	defer discard()
	unfinished := func(err error) error {
		return fmt.Errorf("%w — exe is still %s; run `exe update` again to finish", err, release.Version)
	}
	if err := stageRelease(l, scratch); err != nil {
		return unfinished(err)
	}
	if apps {
		dir, _, _, err := unpackApps(scratch, scratch)
		if err != nil {
			return unfinished(err)
		}
		if _, err := h.placeApps(l, dir); err != nil {
			return unfinished(err)
		}
	}
	if err := commit(); err != nil {
		return unfinished(fmt.Errorf("replace %s: %w", l.Bin, err))
	}
	h.say("Installed exe %s at %s\n", latest, h.tilde(l.Bin))

	// the helper in root's folder is the one thing an update cannot reach
	if have, _ := h.Helper(helperPath); have != "" {
		if want, _ := release.FileSum(l.Helper()); want != "" && want != have {
			h.say("\nThis release has a new network helper. VMs use the old one until you run:\n  %s\n  %s\n",
				h.shown(step{Argv: []string{"install", "-D", "-o", "root", "-g", h.Group, "-m", "0750", l.Helper(), helperPath}}),
				h.shown(step{Argv: []string{"setcap", "cap_net_admin=ep", helperPath}}))
		}
	}

	if u.Running == nil || !u.Running() {
		h.say("\nexe is not running; it starts as %s.\n", latest)
		return nil
	}
	// once the binary is replaced `exe update` has nothing left to do, so
	// a restart put off is said with the command that does it
	later := "it restarts"
	if u.RestartHint != "" {
		later = "you run: " + u.RestartHint
	}
	if !yes {
		if !h.Tty {
			h.say("\nThe daemon keeps running %s until %s\n", release.Version, later)
			return nil
		}
		h.say("\nRestart exe now? Running VMs stop and start again.\n")
		if !h.yesNo(true) {
			h.say("The daemon keeps running %s until %s\n", release.Version, later)
			return nil
		}
	}
	if err := u.Restart(); err != nil {
		return fmt.Errorf("exe %s is installed, but the daemon did not take the restart: %w", latest, err)
	}
	h.say("exe is restarting into %s.\n", latest)
	return nil
}
