# Releases

exe is released for Linux, x86-64 and ARM64, as static binaries. A release
is what the one-line installer (`curl -fsSL https://exe.v2core.com/install.sh | sh`)
and `exe update` fetch. macOS and Windows are built from a checkout.

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

Five files, with the same names in every release, so
`…/releases/latest/download/<name>` always works:

| File | Holds |
|---|---|
| `exe-linux-amd64.tar.gz`, `exe-linux-arm64.tar.gz` | `exe` and `exe-net-helper` |
| `exe-apps.tar.gz` | the bundles of `/www/exe-apps` at its HEAD |
| `install.sh` | the installer, as the daemon serves it at `/install.sh` |
| `SHA256SUMS` | the checksums of the four |

Neither the installer nor `exe update` calls `api.github.com` (60 requests
an hour for an address). The latest version is read from where
`/releases/latest` redirects to, and files come from
`/releases/download/<tag>/<name>`.

## Build

```sh
deploy/release.sh build              # today's version, from HEAD
deploy/release.sh build 2026.10.09.2 # a version by name
```

This writes `dist/release/<version>/` and touches nothing else. It builds
a clean export of the commit, so uncommitted files in the working tree are
not in it; the tarballs are the same bytes whenever they are built from
the same commits with the same Go.

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
step. Never run a released build's `exe setup` on spark: it would install
beside the checkout's own daemon.

## Publish

```sh
deploy/release.sh publish 2026.10.09 [notes.md]
```

It publishes the files `build` made, and refuses if they changed, if the
commit is not on `main`, or if the tag exists. It tags the commit, pushes
`main` and the tag, uploads the five files to a draft, publishes the draft
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
(`cmd/exe/install_linux.go`) asks the questions and does the work, so it
is tested in Go and can be run again as `exe setup`.

- Asks, in order: where to listen (this machine, all interfaces, or
  Tailscale when the machine has it — the answer goes for the desk, the
  proxy and the SSH gate alike); whether to require an API token (not
  asked for all interfaces, where one is always generated); whether to
  install the extra apps; whether to set the machine up for VMs with sudo
  (only where `/dev/kvm` exists, and it lists its commands first).
- Asks nothing with nobody at a keyboard, and takes its answers from
  `EXE_INSTALL_LISTEN`, `EXE_INSTALL_TOKEN`, `EXE_INSTALL_APPS`,
  `EXE_INSTALL_VMS` and `EXE_API_TOKEN`.
- Writes nothing until the last answer; keeps a `config.json` that is
  already there; never touches a systemd unit it did not write.
- Puts the binary in `~/.local/bin`, the staged helper and its own records
  in `~/.exe/release/`, the apps in `~/.exe/apps`, and a user unit in
  `~/.config/systemd/user/exe.service`.

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
