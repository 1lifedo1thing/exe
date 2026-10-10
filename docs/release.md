# Releases

exe is released for Linux and macOS, x86-64 and ARM64 each. A release is
what the one-line installer (`curl -fsSL https://exe.v2core.com/install.sh | sh`)
and `exe update` fetch. Windows is built from a checkout.

- **Where.** GitHub Releases of `livid/exe` holds the files and says which
  release is the latest. Nothing is pinned in Kubo; the hub only carries
  the announcement.
- **When.** Only when Livid says "release a new version". Publishing
  pushes `main` and a tag and cannot be taken back: the repository has
  immutable releases on, so a published release's files and tag are
  locked. A bad release is followed by a new one.
- **Version.** The UTC date, `2026.10.09`; a second release that day is
  `2026.10.09.2`. The tag is the version. A binary built from a checkout
  has no version, and `exe update`, `exe setup` and `exe uninstall` refuse
  to act on it.

## Before the first release

The homepage, the README, Getting Started and the manual do not mention
the installer yet: a page that showed the one-liner before a release
existed would show a command that fails. Those edits are written and
checked, and wait on spark in `output/release/first-release-docs.patch`.
Apply and commit them, then build the first release from that commit —
and delete this section.

## What a release is

Seven files, with the same names in every release, so
`…/releases/latest/download/<name>` always works:

| File | Holds |
|---|---|
| `exe-linux-amd64.tar.gz`, `exe-linux-arm64.tar.gz` | `exe` and `exe-net-helper`, static |
| `exe-darwin-amd64.tar.gz`, `exe-darwin-arm64.tar.gz` | `exe`, signed for macOS 13 and later |
| `exe-apps.tar.gz` | the bundles of `/www/exe-apps` at its HEAD |
| `install.sh` | the installer, as the daemon serves it at `/install.sh` |
| `SHA256SUMS` | the checksums of the six |

Neither the installer nor `exe update` calls `api.github.com` (60 requests
an hour for an address). The latest version is read from where
`/releases/latest` redirects to, and files come from
`/releases/download/<tag>/<name>`.

## Build

```sh
deploy/release.sh build              # today's version, from HEAD
deploy/release.sh build 2026.10.09.2 # a version by name
```

This writes `dist/release/<version>/` and touches nothing else here. It
builds a clean export of the commit, so uncommitted files in the working
tree are not in it. The Linux and apps tarballs are the same bytes
whenever they are built from the same commits with the same Go; the macOS
ones carry the time they were signed.

### The macOS binaries

They are built on a Mac, over ssh, because they link Apple's frameworks
through cgo (Virtualization.framework, the menu bar) and because their
signature is made there. The signature is not optional: Apple silicon
runs no unsigned code, and the entitlement that lets exe run VMs
(`vz.entitlements`) rides in it. One Apple silicon Mac builds both
processors.

`deploy/release.env` (not in the repository — it names machines) says
which Mac: `EXE_MAC_BUILDER=user@host`. That account needs:

- Go in `~/sdk/go`, and Xcode for `codesign`, `notarytool` and `clang`.
- `~/.exe-signing/pass`: the password of its login keychain, mode 600. A
  session over ssh has no unlocked keychain, and an unlock lasts only for
  the ssh command it is in.
- A Developer ID Application identity whose private key lets `codesign`
  sign without asking. A key made in Keychain Access or by Xcode asks at
  the screen for every new program, which an ssh session cannot answer
  (`errSecInternalComponent`): approve `codesign` once at the screen with
  Always Allow, or import the identity with `security import … -T
  /usr/bin/codesign`, and run `security set-key-partition-list -S
  apple-tool:,apple:,codesign: -s -k <password> <keychain>` once.
- Where "Developer ID Application" does not pick out one identity:
  `~/.exe-signing/identity` (its SHA-1) and `~/.exe-signing/keychain`
  (the keychain it is in).
- A notarytool profile named `exe-notary`
  (`xcrun notarytool store-credentials exe-notary`).

A build from a commit signs both binaries with the Developer ID, with the
hardened runtime and Apple's timestamp, and has Apple notarize them — one
zip, a few minutes. A bare binary cannot carry the notarization ticket
(there is nothing to staple it to), so a Mac that has to check one asks
Apple. It rarely has to: `curl` leaves no quarantine mark on what it
downloads, so an install through the one-liner is never checked; a tarball
downloaded with a browser is, and passes because of the notarization.

A `--worktree` build gives them an ad-hoc signature instead
(`EXE_MAC_SIGN=adhoc|developer-id` says otherwise). That runs on any Mac
through the installer and is what tests use; `publish` refuses it.
`EXE_MAC_BUILDER=none` leaves macOS out of a test build.

## Test it before it is published

`deploy/release-mirror.py` serves `dist/release/` in GitHub's layout, and
`EXE_RELEASE_URL` points the installer and `exe update` at it:

```sh
deploy/release-mirror.py --bind 0.0.0.0 --port 7796   # on this machine

# on the machine under test
export EXE_RELEASE_URL=http://<this machine>:7796/releases
curl -fsSL http://<this machine>:7796/install.sh | sh
exe update
```

To test an update, build two versions and put the older one's name in
`dist/release/LATEST` before installing; change the file to move "the
latest release" on. `deploy/release.sh build --worktree <version>` builds
the files on disk rather than a commit, for testing something not yet
committed; `publish` refuses such a build.

The machines: `lab` (an exe VM: Debian 13, ARM64, no `/dev/kvm`) for the
installer, the service, `exe update` and `exe uninstall`; `precision`
(Ubuntu 24.04, x86-64, KVM, Tailscale) for the x86-64 build and the VM
step; `birdie` (a Mac, Apple silicon, someone logged in at its screen) for
macOS — the launchd agent, the menu-bar item and a VM. The Intel Mac build
runs on `birdie` under Rosetta, which shows its installer and desk but not
its VMs: those need a real Intel Mac, and none has tried them. Never run a
released build's `exe setup` on spark: it would install beside the
checkout's own daemon.

## Publish

```sh
deploy/release.sh publish 2026.10.09 [notes.md]
```

It publishes the files `build` made, and refuses if they changed, if the
commit is not on `main`, if the tag exists, or if the macOS binaries are
not Developer ID signed and notarized. It tags the commit, pushes
`main` and the tag, uploads the seven files to a draft, publishes the draft
as the latest release, and checks that GitHub's `latest` now serves the
checksums that were built. Without a notes file the notes are the install
lines and the commit subjects since the last release.

Afterwards: rebuild and restart the daemon on spark (it serves
`/install.sh` and the homepage), run the real one-liner once on `lab`, and
post the release on the hub.

Rolling back is moving GitHub's "latest" mark to the release before
(`gh release edit <older> --latest`); whether that is allowed with
immutable releases on has not been tried — publishing the next version is
the sure way.

## What the installer does

`install.sh` only downloads, checks and unpacks; `exe setup -from <dir>`
(`cmd/exe/install_unix.go`) asks the questions and does the work, so it
is tested in Go and can be run again as `exe setup`. The one source serves
both systems: what differs is chosen by the host's `OS` field, so the
Mac's path is tested on Linux as well.

- Asks, in order: where to listen (this machine, all interfaces, or
  Tailscale when the machine has it — the answer goes for the desk, the
  proxy and the SSH gate alike); whether to require an API token (not
  asked for all interfaces, where one is always generated); whether to
  install the extra apps; whether to set the machine up for VMs with sudo
  (Linux only, where `/dev/kvm` exists, and it lists its commands first —
  a Mac needs no setup for VMs and is never asked).
- Asks nothing with nobody at a keyboard, and takes its answers from
  `EXE_INSTALL_LISTEN`, `EXE_INSTALL_TOKEN`, `EXE_INSTALL_APPS`,
  `EXE_INSTALL_VMS` and `EXE_API_TOKEN`.
- Writes nothing until the last answer; keeps a `config.json` that is
  already there; never touches a service it did not write.
- Puts the binary in `~/.local/bin`, the staged helper and its own records
  in `~/.exe/release/`, the apps in `~/.exe/apps`, and the service where
  the system keeps a user's own: a systemd user unit,
  `~/.config/systemd/user/exe.service`, or on a Mac a launchd agent,
  `~/Library/LaunchAgents/com.v2core.exe.plist`.

On a Mac the agent runs in the user's login session — that is where the
menu bar is — so exe starts at login, not at boot, and an install over ssh
with nobody at the screen only leaves the agent for the next login. The
agent comes back by itself only after a failure, so that Quit in the menu
bar stays quit; a restart is `launchctl kickstart -k
gui/<uid>/com.v2core.exe`, which the daemon runs on itself when the agent
started it (`EXE_LAUNCHD`, `server.RestartDaemon`).

`exe update` checks the download and runs its `version`, copies the new
binary beside the old one, stages the helper, refreshes the apps it
installed (not one that was edited or removed), and only then renames the
binary into place. That rename is the commit: the old binary does the
whole update, so an update that fails before it is simply run again. Were
the binary replaced first, the retry would be the new binary, which finds
itself up to date and repairs nothing. The installer keeps the same order.
`exe update` says so when the root-owned network helper has changed, and
restarts the daemon only on a yes or `-y`.

The apps' record is `~/.exe/release/apps.json`, saved at the end of a run.
What a run is about to place is written first, to `apps.json.pending`, and
removed once the record is saved, so a run that stops halfway — an error,
or a killed process — is recognised by the next one: a bundle that stands
there with exactly the contents the plan named is the installer's, and
anything else without a record is its owner's (`internal/release/apps.go`).
