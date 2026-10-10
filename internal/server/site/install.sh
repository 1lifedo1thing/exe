#!/bin/sh
# exe installer for Linux (x86-64 and ARM64):
#
#   curl -fsSL https://exe.v2core.com/install.sh | sh
#
# It downloads the latest release from GitHub, checks it against the
# release's SHA256SUMS and hands over to `exe setup`, which asks where exe
# should listen, whether it needs a token, whether to install the extra
# apps and whether to set the machine up for VMs (the one part that uses
# sudo), and only then writes anything. exe goes to ~/.local/bin, its data
# to ~/.exe, and it runs as a systemd user service.
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

# Everything is inside main, called on the last line: a download cut short
# runs nothing.
main() {
	base=${EXE_RELEASE_URL:-https://github.com/livid/exe/releases}
	base=${base%/}

	[ "$(uname -s)" = Linux ] ||
		fail "this installer is for Linux. On macOS and Windows, build exe from source: https://exe.v2core.com/docs/getting-started"
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) fail "there is no exe release for $(uname -m): x86-64 and ARM64 only" ;;
	esac
	for tool in curl tar gzip sha256sum mktemp; do
		command -v "$tool" >/dev/null 2>&1 || fail "$tool is needed and was not found"
	done

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

	binary=exe-linux-$arch.tar.gz
	echo "Downloading exe $version…"
	for file in SHA256SUMS "$binary" exe-apps.tar.gz; do
		curl -fsSL --retry 3 -o "$tmp/$file" "$base/download/$version/$file" ||
			fail "could not download $file of exe $version"
	done
	for file in "$binary" exe-apps.tar.gz; do
		sum=$(awk -v f="$file" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")
		[ -n "$sum" ] || fail "$file is not listed in the release's SHA256SUMS"
		echo "$sum  $tmp/$file" | sha256sum -c - >/dev/null 2>&1 ||
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

fail() {
	echo "exe install: $*" >&2
	exit 1
}

main "$@"
