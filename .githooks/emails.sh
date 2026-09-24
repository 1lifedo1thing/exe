# Shared by pre-commit and commit-msg: finds email addresses that may not
# be committed. This repo is public, and a real address (Livid's, a
# visitor's, one copied out of CLI output into a fixture) must never land
# in it. Allowed: the reserved example domains (someone@example.com,
# …@x.example, .test, .invalid, .localhost), no-reply senders (the
# Co-Authored-By trailer, GitHub's users.noreply), git@ remotes, and names
# that only look like addresses (icon@2x.png).
#
# Enable once per checkout: git config core.hooksPath .githooks

EMAIL_RE='[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}'

# email_allowed ADDRESS — succeeds when ADDRESS may be committed
email_allowed() {
	a=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')
	user=${a%@*}
	domain=${a#*@}
	case "$domain" in
	example.com | example.net | example.org | *.example.com | *.example.net | *.example.org) return 0 ;;
	*.example | *.test | *.invalid | *.localhost) return 0 ;;
	users.noreply.github.com | *.noreply.github.com) return 0 ;;
	*.png | *.jpg | *.jpeg | *.gif | *.svg | *.webp | *.ico) return 0 ;; # retina asset names
	esac
	case "$user" in
	noreply | no-reply | git) return 0 ;;
	esac
	return 1
}

# email_scan — reads "where<TAB>text" lines on stdin, prints each
# disallowed address as "where: address", and fails if it printed any
email_scan() {
	found=0
	while IFS="$(printf '\t')" read -r where text; do
		for addr in $(printf '%s\n' "$text" | grep -oE "$EMAIL_RE"); do
			email_allowed "$addr" && continue
			printf '  %s: %s\n' "$where" "$addr"
			found=1
		done
	done
	[ "$found" -eq 0 ]
}

email_refuse() {
	cat >&2 <<EOF

Refused: $1 holds an email address (above). This repo is public.
Use a reserved example instead (someone@example.com), or mask it.
Only with Livid's say-so: git commit --no-verify
EOF
}
