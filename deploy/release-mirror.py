#!/usr/bin/env python3
"""Serve dist/release/ the way GitHub serves releases, to test one before
it is published (docs/release.md).

    deploy/release-mirror.py [--bind ADDR] [--port N] [--latest VERSION]

Then, on the machine under test:

    export EXE_RELEASE_URL=http://<this machine>:<port>/releases
    curl -fsSL http://<this machine>:<port>/install.sh | sh
    exe update

The layout is GitHub's: /releases/latest redirects to /releases/tag/<v>,
/releases/latest/download/<file> to /releases/download/<v>/<file>, which
is dist/release/<v>/<file>. The latest version is the newest folder, or
the one --latest names — or whatever the file dist/release/LATEST says, so
a test can publish "the next release" without restarting this.
"""
import argparse
import http.server
import os
import re

ROOT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "dist", "release")
VERSION = re.compile(r"^20\d\d\.\d\d\.\d\d(\.\d{1,3})?$")
FILE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")


def key(v):
    return [int(n) for n in v.split(".")]


def latest(pinned):
    try:
        with open(os.path.join(ROOT, "LATEST")) as f:
            said = f.read().strip()
        if VERSION.match(said):
            return said
    except OSError:
        pass
    if pinned:
        return pinned
    have = [d for d in os.listdir(ROOT) if VERSION.match(d) and os.path.isdir(os.path.join(ROOT, d))]
    return max(have, key=key) if have else None


class Handler(http.server.BaseHTTPRequestHandler):
    pinned = None

    def redirect(self, to):
        self.send_response(302)
        self.send_header("Location", to)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def file(self, version, name):
        path = os.path.join(ROOT, version, name)
        if not (VERSION.match(version) and FILE.match(name) and os.path.isfile(path)):
            return self.send_error(404)
        with open(path, "rb") as f:
            body = f.read()
        self.send_response(200)
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def do_GET(self):
        path = self.path.split("?", 1)[0]
        now = latest(self.pinned)
        if path == "/releases/latest":
            # with nothing published GitHub sends the reader to the list
            return self.redirect("/releases/tag/" + now if now else "/releases")
        if path.startswith("/releases/latest/download/") and now:
            return self.redirect("/releases/download/%s/%s" % (now, path.rsplit("/", 1)[1]))
        m = re.match(r"^/releases/download/([^/]+)/([^/]+)$", path)
        if m:
            return self.file(m.group(1), m.group(2))
        if path in ("/install.sh", "/install.ps1") and now:
            return self.file(now, path[1:])
        if path.startswith("/releases"):
            body = b"releases\n"
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            if self.command != "HEAD":
                self.wfile.write(body)
            return
        self.send_error(404)

    do_HEAD = do_GET


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--bind", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=7796)
    ap.add_argument("--latest")
    args = ap.parse_args()
    Handler.pinned = args.latest
    print("serving %s on http://%s:%d — latest is %s" % (os.path.normpath(ROOT), args.bind, args.port, latest(args.latest)))
    http.server.ThreadingHTTPServer((args.bind, args.port), Handler).serve_forever()
