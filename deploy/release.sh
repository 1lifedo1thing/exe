#!/usr/bin/env bash
# Build and publish a release of exe: the Linux x86-64 and ARM64 binaries
# the installer (https://exe.v2core.com/install.sh) and `exe update` fetch.
#
#   deploy/release.sh build [version]            build dist/release/<version>/ from HEAD
#   deploy/release.sh build --worktree <version> the same from the files on disk, to test with
#   deploy/release.sh publish <version> [notes]  tag it, push it, publish it on GitHub
#
# A version is the UTC date, 2026.10.09, and 2026.10.09.2 for a second one
# that day. GitHub Releases is where the files live and what says which
# release is the latest; nothing is pinned anywhere else.
#
# `build` is safe at any time: it writes only under dist/. It builds a
# clean export of the commit, not the working tree, so what two agents have
# lying about uncommitted is not in it. `publish` pushes main and a tag and
# puts the release in front of everyone — run it only when Livid says
# "release a new version", and only for a build that was tested: it
# publishes the very files `build` made, and refuses a --worktree build.
#
# A release cannot be changed once it is out (the repository has immutable
# releases on): the files go up on a draft, and publishing the draft is the
# last step. A bad release is followed by a new one.
#
# Testing a build before it is published: serve dist/release/ in GitHub's
# layout and point the installer and `exe update` at it with
# EXE_RELEASE_URL (docs/release.md has the recipe).
set -euo pipefail

root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
apps=${EXE_APPS:-$root/../exe-apps}
repo=${EXE_RELEASE_REPO:-livid/exe}
export PATH="$PATH:/usr/local/go/bin"

assets=(exe-linux-amd64.tar.gz exe-linux-arm64.tar.gz exe-apps.tar.gz install.sh SHA256SUMS)

die() {
	echo "release: $*" >&2
	exit 1
}

valid() {
	[[ $1 =~ ^20[0-9]{2}\.[0-9]{2}\.[0-9]{2}(\.[0-9]{1,3})?$ ]]
}

# today's version, or today's next one when a tag already has the date
next_version() {
	local day n=2 v
	day=$(date -u +%Y.%m.%d)
	v=$day
	while git -C "$root" rev-parse -q --verify "refs/tags/$v" >/dev/null ||
		git -C "$root" ls-remote --exit-code --tags origin "refs/tags/$v" >/dev/null 2>&1; do
		v=$day.$n
		n=$((n + 1))
	done
	echo "$v"
}

# a tarball that is the same bytes whenever it is made from the same files
pack() { # pack <dir> <out.tar.gz> <mtime> <names...>
	local dir=$1 out=$2 stamp=$3
	shift 3
	# --mode: the builder's umask is not part of a release
	printf '%s\n' "$@" | tar --sort=name --mtime="@$stamp" --owner=0 --group=0 --numeric-owner \
		--mode='u=rwX,go=rX' -C "$dir" -cf - -T - | gzip -9n >"$out"
}

build() {
	local worktree=
	if [ "${1:-}" = --worktree ]; then
		worktree=1
		shift
	fi
	local version=${1:-}
	[ -n "$version" ] || { [ -z "$worktree" ] && version=$(next_version); }
	[ -n "$version" ] || die "a --worktree build needs a version"
	valid "$version" || die "$version is not a version: 2026.10.09 or 2026.10.09.2"
	[ -d "$apps/.git" ] || die "the apps checkout is not at $apps (set EXE_APPS)"

	local out=$root/dist/release/$version work commit stamp
	work=$(mktemp -d)
	trap "rm -rf '$work'" EXIT # expanded now: work is gone by the time the trap runs
	mkdir -p "$work/src" "$work/apps" "$work/out"

	if [ -n "$worktree" ]; then
		commit=worktree
		stamp=$(date +%s)
		# the files on disk, tracked or not yet, without what git ignores
		(cd "$root" && git ls-files -z --cached --others --exclude-standard |
			tar --null -T - --ignore-failed-read -cf - 2>/dev/null) | tar -x -C "$work/src"
	else
		commit=$(git -C "$root" rev-parse HEAD)
		stamp=$(git -C "$root" log -1 --format=%ct "$commit")
		if [ -n "$(git -C "$root" status --porcelain)" ]; then
			echo "note: this builds HEAD (${commit:0:7}); what is uncommitted in $root is not in it"
		fi
		git -C "$root" archive "$commit" | tar -x -C "$work/src"
	fi

	local arch
	for arch in amd64 arm64; do
		echo "building linux/$arch"
		mkdir -p "$work/$arch"
		(cd "$work/src" &&
			CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -buildvcs=false \
				-ldflags "-s -w -X exe/internal/release.Version=$version" -o "$work/$arch/exe" ./cmd/exe &&
			CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -buildvcs=false \
				-ldflags "-s -w" -o "$work/$arch/exe-net-helper" ./cmd/exe-net-helper)
		pack "$work/$arch" "$work/out/exe-linux-$arch.tar.gz" "$stamp" exe exe-net-helper
	done

	# the binary for this machine has to run and know its version
	local here said
	case $(uname -m) in
	x86_64) here=amd64 ;;
	aarch64 | arm64) here=arm64 ;;
	*) here= ;;
	esac
	if [ -n "$here" ]; then
		said=$("$work/$here/exe" version)
		[ "$said" = "exe $version (linux/$here)" ] || die "the build calls itself \"$said\""
	fi

	# the extra apps: every folder of the apps checkout that is a bundle
	local apps_commit apps_stamp
	apps_commit=$(git -C "$apps" rev-parse HEAD)
	apps_stamp=$(git -C "$apps" log -1 --format=%ct "$apps_commit")
	git -C "$apps" archive "$apps_commit" | tar -x -C "$work/apps"
	local bundles=() d
	for d in "$work/apps"/*/; do
		[ -f "$d/app.json" ] && bundles+=("$(basename "$d")")
	done
	[ ${#bundles[@]} -gt 0 ] || die "no app bundles in $apps"
	pack "$work/apps" "$work/out/exe-apps.tar.gz" "$apps_stamp" "${bundles[@]}"

	cp "$work/src/internal/server/site/install.sh" "$work/out/install.sh"
	(cd "$work/out" && sha256sum exe-linux-amd64.tar.gz exe-linux-arm64.tar.gz exe-apps.tar.gz install.sh >SHA256SUMS)
	printf '%s\n' "$commit" >"$work/out/.commit"
	printf '%s\n' "$apps_commit" >"$work/out/.apps-commit"

	rm -rf "$out"
	mkdir -p "$(dirname "$out")"
	mv "$work/out" "$out"
	echo
	echo "exe $version, from ${commit:0:12} (apps ${apps_commit:0:12}, ${#bundles[@]} bundles):"
	(cd "$out" && ls -l "${assets[@]}" | awk '{printf "  %9d  %s\n", $5, $9}')
	echo "in $out"
}

publish() {
	local version=${1:-} notes=${2:-}
	valid "$version" || die "publish which version? deploy/release.sh publish 2026.10.09"
	local out=$root/dist/release/$version commit prev f
	[ -d "$out" ] || die "$out is not there: build it, and test it, first"
	commit=$(cat "$out/.commit")
	[ "$commit" != worktree ] || die "$version was built from the working tree, which is for testing: build it from a commit"
	git -C "$root" merge-base --is-ancestor "$commit" main || die "${commit:0:12} is not on main"
	(cd "$out" && sha256sum --quiet -c SHA256SUMS) || die "the files in $out no longer match their checksums"
	for f in "${assets[@]}"; do
		[ -f "$out/$f" ] || die "$out/$f is missing"
	done
	command -v gh >/dev/null || die "gh is needed to publish"
	if git -C "$root" rev-parse -q --verify "refs/tags/$version" >/dev/null ||
		git -C "$root" ls-remote --exit-code --tags origin "refs/tags/$version" >/dev/null 2>&1; then
		die "$version is already a tag: a release is never replaced, make the next one"
	fi

	if [ -z "$notes" ]; then
		notes=$(mktemp)
		prev=$(git -C "$root" describe --tags --abbrev=0 "$commit" 2>/dev/null || true)
		{
			echo "Install or update on Linux (x86-64, ARM64):"
			echo
			echo '```sh'
			echo "curl -fsSL https://exe.v2core.com/install.sh | sh    # new machine"
			echo "exe update                                            # an installed exe"
			echo '```'
			if [ -n "$prev" ]; then
				echo
				echo "Since $prev:"
				echo
				git -C "$root" log --no-merges --format='- %s' "$prev..$commit"
			fi
		} >"$notes"
	fi

	git -C "$root" tag "$version" "$commit"
	git -C "$root" push origin main "refs/tags/$version"
	# the files go up on a draft; publishing it is what makes the release,
	# and from then on it cannot be changed
	(cd "$out" && gh release create "$version" --repo "$repo" --draft --verify-tag \
		--title "exe $version" --notes-file "$notes" "${assets[@]}")
	gh release edit "$version" --repo "$repo" --draft=false --latest

	# what the world gets now has to be what was built
	local got
	got=$(curl -fsSL "https://github.com/$repo/releases/latest/download/SHA256SUMS")
	[ "$got" = "$(cat "$out/SHA256SUMS")" ] || die "GitHub's latest release does not serve the checksums that were built — look at https://github.com/$repo/releases"
	echo "exe $version is published: https://github.com/$repo/releases/tag/$version"
}

case ${1:-} in
build)
	shift
	build "$@"
	;;
publish)
	shift
	publish "$@"
	;;
*)
	sed -n '2,8p' "$0" | sed 's/^# \{0,1\}//'
	exit 2
	;;
esac
