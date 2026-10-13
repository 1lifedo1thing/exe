<!--
What changed since the last release, as Software Update will show it under
"What changed in <version>". `deploy/release.sh notes` prints and checks it;
docs/release.md, "The notes", says how it is written. What every release
before it said is kept in CHANGELOG.md.
-->

- On Windows, `exe ssh` reaches a VM again on a PC where Codex has run: the VM key is kept to its user.
- Every release's notes are one page now: [What Changed](https://exe.v2core.com/docs/changelog).
- **Mac OS X** has an **Agent** button: a Chat that works inside Tiger and uploads Workspace files.
- **Get Info** on a Claude Code session: summary, turns, tokens, models, dates; **Archive…** beside **OK**.
- On a phone, a picture window zooms: pinch the picture or double-tap it, and drag it about while zoomed.
- **Search** in the Claude Code window finds a session by its name too (`exe-claude-90`); its rows and **Get Info** say the name.
- The green dot on a Claude Code session goes out when its turn was stopped with Escape; it used to stay until the next prompt.
- `exe expose host -dir /www/site`: a folder served as a site by exe itself; a `.password` file guards a folder.
