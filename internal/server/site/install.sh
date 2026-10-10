#!/bin/sh
# exe installer for Linux and macOS (x86-64 and ARM64 / Apple silicon):
#
#   curl -fsSL https://exe.v2core.com/install.sh | sh
#
# It downloads the latest release from GitHub, checks it against the
# release's SHA256SUMS and hands over to `exe setup`, which asks where exe
# should listen, whether it needs a token, whether to install the extra
# apps and — on Linux — whether to set the machine up for VMs (the one
# part that uses sudo), and only then writes anything. exe goes to
# ~/.local/bin, its data to ~/.exe, and it runs as a service of your own
# user: a systemd user unit on Linux, a launchd agent on a Mac.
#
# With nobody at a keyboard it asks nothing and installs for this machine
# only; the answers can be given ahead of time:
#
#   EXE_INSTALL_LISTEN=local|all|tailscale   EXE_INSTALL_TOKEN=yes|no
#   EXE_INSTALL_APPS=yes|no                  EXE_INSTALL_VMS=yes|no
#   EXE_API_TOKEN=<the token to use>
#
# Docs: https://exe.v2core.com/docs/getting-started
set -eu

# Everything is inside functions, and main is called on the last line: a
# download cut short runs nothing.
main() {
	base=${EXE_RELEASE_URL:-https://github.com/livid/exe/releases}
	base=${base%/}

	case "$(uname -s)" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) fail "this installer is for Linux and macOS. On Windows, build exe from source: https://exe.v2core.com/docs/getting-started" ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) fail "there is no exe release for $(uname -m): x86-64 and ARM64 only" ;;
	esac
	if [ "$os" = darwin ]; then
		# a shell running under Rosetta says x86_64 on an Apple silicon Mac
		[ "$(sysctl -n sysctl.proc_translated 2>/dev/null || true)" = 1 ] && arch=arm64
		# the VMs boot through an EFI loader that came with macOS 13
		macos=$(sw_vers -productVersion)
		[ "${macos%%.*}" -ge 13 ] || fail "exe needs macOS 13 or later; this is macOS $macos"
	fi
	for tool in curl tar gzip mktemp awk; do
		command -v "$tool" >/dev/null 2>&1 || fail "$tool is needed and was not found"
	done
	command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 ||
		fail "sha256sum (or shasum) is needed and was not found"

	# The latest release is where /latest redirects to: .../tag/<version>.
	latest=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$base/latest") ||
		fail "could not reach $base"
	version=${latest##*/}
	case "$latest" in
	*/tag/[0-9]*.[0-9]*.[0-9]*) ;;
	*) fail "no release of exe has been published yet" ;;
	esac

	tmp=$(mktemp -d "${TMPDIR:-/tmp}/exe-install.XXXXXX")
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM

	binary=exe-$os-$arch.tar.gz
	echo "Downloading exe $version…"
	for file in SHA256SUMS "$binary" exe-apps.tar.gz; do
		curl -fsSL --retry 3 -o "$tmp/$file" "$base/download/$version/$file" ||
			fail "could not download $file of exe $version"
	done
	for file in "$binary" exe-apps.tar.gz; do
		want=$(awk -v f="$file" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")
		[ -n "$want" ] || fail "$file is not listed in the release's SHA256SUMS"
		[ "$(sha256_of "$tmp/$file")" = "$want" ] ||
			fail "$file does not match the release's checksum; nothing was installed"
	done
	tar -xzf "$tmp/$binary" -C "$tmp"
	"$tmp/exe" version >/dev/null 2>&1 ||
		fail "the downloaded exe does not run from $tmp (a noexec /tmp? set TMPDIR to a folder that allows it)"

	# Piped into sh, stdin is the script; the questions need the keyboard.
	if [ -t 0 ]; then
		"$tmp/exe" setup -from "$tmp"
	elif (: </dev/tty) 2>/dev/null; then
		"$tmp/exe" setup -from "$tmp" </dev/tty
	else
		"$tmp/exe" setup -from "$tmp" </dev/null
	fi
}

# sha256_of prints a file's SHA-256: sha256sum where there is one, shasum
# on a Mac that has none.
sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{ print $1 }'
	else
		shasum -a 256 "$1" | awk '{ print $1 }'
	fi
}

fail() {
	echo "exe install: $*" >&2
	exit 1
}

main "$@"
