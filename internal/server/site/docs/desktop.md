# The Desktop and the API

The daemon serves a single-page UI at `http://<listen>/` (default
http://127.0.0.1:7777). It can:

- list VMs with live state, and create / start / stop / delete them
- open a full SSH terminal in the browser (xterm.js over WebSocket, embedded in the binary)
- see web services listening inside a VM with one-click links
- vibe code inside a VM with streaming agent output
- browse every vibe-code transcript (CLI runs are recorded too, under
  `~/.exe/vms/<name>/transcripts/`)
- expose a VM port to your domain
- read and post to an exe-hub feed, and let an agent identity answer replies there (`hub.*`)
- edit the full configuration (saved to `~/.exe/config.json` and hot-reloaded;
  fields marked `*` need a daemon restart)

If `api_token` is set, paste it into the token field in the header (stored in
localStorage). Bind `listen` to your Tailscale IP to use the UI from your phone.

# Agent skill guide (/skill.md)

The daemon also serves `http://<listen>/skill.md`
([source](https://github.com/livid/exe/blob/main/internal/server/skill.md)): a self-contained guide any coding agent
(Claude Code, Codex, opencode, …) can fetch to learn the API — VM lifecycle
over HTTP, running commands over the SSH gate, service discovery, and
exposing ports. Point an agent at that URL (plus the `api_token` if set) and
it can drive your VMs; the file also works dropped into a skills directory
as-is.

While `exe serve` runs it also puts an icon in the macOS menu bar: **Open Web
UI**, **Restart Daemon** (running VMs are brought back automatically), and
**Quit exe** (asks for confirmation, then shuts down running VMs and the
daemon). In headless sessions (ssh) the daemon runs without the icon.

# Mac OS 9 sound

The optional Mac OS 9 sound runtime has a
[detailed build and reuse guide](https://github.com/livid/exe/blob/main/docs/macos9-audio.md), including the pinned
QEMU/OpenBIOS pair, Linux dependencies, installation, playback checks and rollback.
