# Start an agent from a Projects issue

Inside the exe desktop, an issue’s right-click menu offers **Send to
Claude Code** and **Send to Codex** for the CLIs detected by exe. Detection
comes from the existing `hostAgents` registry exposed as
`/v1/hostinfo.agents`, also used by the desktop’s agent windows.

The action reads the full issue, including its discussion, and starts a
fresh session in the project’s working directory. The prompt is passed as
one literal CLI argument after `--`; it is never typed into a shell.
Attachment records identify files, not their contents. The normal agent
sign-in, repository trust and approval prompts still apply.

A missing project folder or tmux disables the action. The desktop checks
detection again before opening the agent. NUL-containing prompts and
prompts larger than 64 KiB are rejected rather than truncated.

## Verification

Both repositories passed `go test ./...`. Projects browser checks passed:
131 app checks, 24 draft checks, 120 Chromium phone checks, 109 WebKit
phone checks and 61 offline checks. The desktop bridge was checked for
origin and frame validation, refreshed detection, folder-only launches,
queued task ordering and a missing tmux. An isolated tmux test used inert
CLI stand-ins for both agents to verify the working directory and literal
argument transport, including quotes, shell substitutions and Unicode.
It did not ask a real model to implement a test issue.

The Projects window was inspected at display scales 1, 1.5 and 2. The
screenshot below uses scratch issue data and both detected agents.

## Screenshot artifact

File: `projects-issue-agents-1.5x.png` (78,026 bytes).

CID: `bafkreic35i76jjcfib6562cmcuzce6h3glmtoxh6tcrajlhlvzyocgfhtu`

SHA-256: `5bea3fe4a445407ddf684c15322278fb32d9375cfe98a204acebae70e118a79d`

The Kubo pin and a fresh download from the
[local gateway](http://127.0.0.1:8081/ipfs/bafkreic35i76jjcfib6562cmcuzce6h3glmtoxh6tcrajlhlvzyocgfhtu)
were verified on 2026-10-15, including the byte count and SHA-256. That
verified URL is available on the build machine. An IPFS client can fetch
the same screenshot by CID:

```sh
ipfs get bafkreic35i76jjcfib6562cmcuzce6h3glmtoxh6tcrajlhlvzyocgfhtu -o projects-issue-agents-1.5x.png
```

The [public gateway URL](https://ipfs.io/ipfs/bafkreic35i76jjcfib6562cmcuzce6h3glmtoxh6tcrajlhlvzyocgfhtu)
and dweb.link returned HTTP 403 during verification; public HTTP retrieval
is not claimed as verified. The screenshot remains pinned outside Git.
