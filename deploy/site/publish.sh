#!/usr/bin/env bash
# Publish site/, the project homepage, to https://<sub>.<domain>
# (exe.v2core.com on Livid's node). `make site` runs this.
#
#   deploy/site/publish.sh         upload site/, switch to it, check the public URL
#   deploy/site/publish.sh setup   the same, and reinstall nginx's site file first
#                                  (after editing deploy/site/nginx.conf)
#
# How it is served: nginx inside an exe VM, document root
# /var/www/exe-site/current, a symlink to releases/<UTC stamp>. A publish
# unpacks a new release and renames the symlink over the old one, so a
# visitor never sees half a site; the last three releases stay, and a
# rollback is one `ln -sfn releases/<stamp> current` in the VM. The public
# name is an ordinary `exe expose` route to the VM's port 80. The first
# run does all of it: installs nginx-light, writes the site file, makes
# the route (DNS record and tunnel rule included). Everything goes through
# the daemon's SSH gate, so it needs a key the gate knows and nothing else.
#
# Only files git tracks are published, at their working-tree content;
# symlinks are followed (the screenshot and the icons are the repo's own).
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
VM=${EXE_SITE_VM:-test}                # the VM that serves the site
SUB=${EXE_SITE_SUB:-exe}               # <sub>.<cloudflare.domain>
GATE=${EXE_SITE_GATE:-127.0.0.1}       # the daemon's SSH gate
GATE_PORT=${EXE_SITE_GATE_PORT:-2222}
EXE=${EXE:-$ROOT/exe}
DIR=/var/www/exe-site

# UpdateHostKeys=no: the gate bridges to the VM's sshd, whose host-key
# proof is signed for a session the client is not in; ssh says so on
# every call otherwise. The transfer is unaffected either way.
vm() { ssh -p "$GATE_PORT" -o UpdateHostKeys=no "$VM@$GATE" "$@"; }

sum() { if command -v sha256sum >/dev/null; then sha256sum; else shasum -a 256; fi | cut -d' ' -f1; }

route_line() { "$EXE" routes | awk -v s="$SUB." 'index($1, s) == 1 { print $1, $2; exit }'; }

setup() {
	echo "setup: nginx in VM $VM"
	vm 'test -x /usr/sbin/nginx || { sudo apt-get update -q && sudo DEBIAN_FRONTEND=noninteractive apt-get install -y -q nginx-light; }'
	vm "sudo install -d -o \$(id -un) -g \$(id -gn) -m 755 $DIR $DIR/releases"
	vm 'sudo tee /etc/nginx/sites-available/exe-site >/dev/null' < "$ROOT/deploy/site/nginx.conf"
	vm 'sudo ln -sfn ../sites-available/exe-site /etc/nginx/sites-enabled/exe-site &&
	    sudo rm -f /etc/nginx/sites-enabled/default &&
	    sudo /usr/sbin/nginx -t -q && sudo systemctl enable -q --now nginx && sudo systemctl reload nginx'
}

upload() {
	local rel stray
	stray=$(git -C "$ROOT" ls-files --others --exclude-standard site)
	if [ -n "$stray" ]; then
		echo "not published, git does not track them:" >&2
		echo "$stray" | sed 's/^/  /' >&2
	fi
	rel=$(date -u +%Y%m%dT%H%M%SZ)
	(cd "$ROOT/site" && git ls-files -z | tar -ch --null -T - -f -) |
		vm "set -e; d=$DIR/releases/$rel; mkdir \$d; tar -x -C \$d -f -
		    chmod -R u=rwX,go=rX \$d; test -f \$d/index.html
		    ln -sfn releases/$rel $DIR/current.new; mv -T $DIR/current.new $DIR/current
		    cd $DIR/releases; ls -1 | sort | head -n -3 | while read -r old; do rm -rf -- \"\$old\"; done"
	echo "release $rel is current"
}

route() {
	local line
	line=$(route_line)
	if [ -z "$line" ]; then
		"$EXE" expose "$VM" -port 80 -sub "$SUB"
		line=$(route_line)
	fi
	[ -n "$line" ] || { echo "no route for $SUB.* after exe expose" >&2; exit 1; }
	HOST=${line%% *}
	case "${line#* }" in
	*:80) ;;
	*) echo "warning: $HOST routes to ${line#* }, not to port 80 of $VM" >&2 ;;
	esac
}

# the page as the world gets it must be, byte for byte, the one just
# published; a new DNS name can take a little while to answer
check() {
	local want got i
	want=$(sum < "$ROOT/site/index.html")
	for i in $(seq 1 30); do
		got=$(curl -fsS --max-time 10 "https://$HOST/" 2>/dev/null | sum || true)
		if [ "$got" = "$want" ]; then
			echo "live: https://$HOST/ (index.html sha256 $want)"
			return 0
		fi
		sleep 2
	done
	echo "https://$HOST/ does not serve the index.html just published (want $want, got $got)" >&2
	return 1
}

case "${1:-}" in
"") vm "test -x /usr/sbin/nginx && test -e /etc/nginx/sites-enabled/exe-site && test -d $DIR/releases" || setup ;;
setup) setup ;;
*) sed -n '2,7p' "$0" >&2; exit 2 ;;
esac
upload
route
check
