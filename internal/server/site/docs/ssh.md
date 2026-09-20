# SSH as an Interface (:2222)

Like [exe.dev](https://exe.dev) (`ssh exe.dev`), the daemon speaks SSH on
`:2222`. The SSH **username** picks where you land:

```sh
ssh -p 2222 exe@mac               # the lobby: an interactive REPL for VM lifecycle
ssh -p 2222 exe@mac ls --json     # one-shot commands, JSON for scripts/agents
ssh -p 2222 exe@mac new           # create + boot a VM (invents a name like fuzzy-otter)
ssh -p 2222 exe@mac code demo "add a /healthz endpoint"   # vibecode, streamed

ssh -p 2222 demo@mac              # full SSH *into* the VM (auto-starts it)
scp -P 2222 app.py demo@mac:~/    # scp, sftp, -L/-R port forwarding all pass through
ssh -p 2222 -L 8000:localhost:8000 demo@mac   # tunnel a VM port to your laptop
```

Lobby commands: `help`, `ls`, `new`, `start`, `stop`, `rm`, `ip`,
`code <vm> <prompt>`, `expose <vm> <port> [sub]`, `unexpose`, `routes` —
`--json` where it matters. The lobby is commands-only (no scp/sftp there);
`ssh <vm>@mac` is a transparent bridge to the VM's sshd, so everything works
there. The name `exe` is reserved for the lobby.

Who gets in: the service key (`~/.exe/ssh/id_ed25519`), any of the daemon
user's `~/.ssh/*.pub`, and keys listed in `~/.exe/ssh/authorized_clients`
(authorized_keys format — add your phone's key there; edits apply
immediately). The gate's host key lives at `~/.exe/ssh/host_ed25519`.
Configure the address with `ssh_listen` (`"off"` disables).
