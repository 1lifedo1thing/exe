# Start an agent from a Projects issue

Inside the exe desktop, an issue's right-click menu in Projects offers
**Send to Claude Code** and **Send to Codex**. Either starts a fresh
session of that agent in the project's working directory, with the whole
issue and its discussion as the session's first message. The manual's
Claude Code chapter (`internal/server/docs.md`) is how a user meets it;
this note says how it is put together.

## Which agents

Detection is exe's: the `hostAgents` registry, reported as
`/v1/hostinfo`'s `agents`, which the desktop's own agent windows go by.
Projects lists only the agents found there, and only when the entry
says `"tasks": true`. That means this desktop's agent bridge takes a
first message along with the folder. A desktop older than 2026.10.15
would open the session without the message, so Projects leaves the
items off there. Projects keeps the last answer so the menu opens at
once, asks again on each right-click for the next menu, and the desktop
checks again before it starts anything.

An item is dimmed for a project without a working directory, for one
whose folder is not on this machine, and where there is no tmux for a
second session.

## How the message travels

Projects reads the issue (`GET /issues/{address}`), drops its `excerpt`,
and posts `{exe: "agent", app, dir, prompt}` to the desktop. The desktop
queues it until the agent window's terminal link is up and sends
`{new: true, dir, prompt}` over it. The daemon's `agentColumn.openTask`
checks the folder and the message and starts the session with
`newAgentTaskSession`. The agents API's `POST /v1/agents/{app}/sessions`
`prompt` takes the same road.

The message does not go on the tmux command line with the rest of the
launch. tmux takes a command of at most 16 KiB from its client and
answers anything longer with "command too long". Shell quoting makes
each `'` four bytes, and long issues are longer than that. So the daemon
writes the message to a private file beside the session status files
(`agents/<session>-*.task` in the state directory). The session's shell
reads it into one argument after `--`, so a message that starts with a
dash is not taken as an option, and deletes the file before the CLI
starts. A launch that fails deletes its own file.

A message is at most 64 KiB, well under Linux's 128 KiB limit for one
argument, and holds no NUL byte. Projects and the daemon both refuse a
longer one with its size rather than shorten it. The shell's `$(…)`
drops the message's trailing newlines and nothing else. Attachment
entries name files, not their contents.

The agent keeps its own sign-in, folder trust and permission prompts:
nothing here changes how the CLI is started apart from its first
message.
