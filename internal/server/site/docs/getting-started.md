# Getting Started

exe is one Go binary. Build it, open the desktop, and everything else —
a VM, an agent inside it, a public HTTPS address — follows from there.

**1. Open the desktop.** Needs Go 1.25 or newer.

```sh
git clone https://github.com/livid/exe.git
cd exe
make build        # also builds exe-net-helper on Linux
                  # Windows: go build -o exe.exe ./cmd/exe
./exe init        # writes ~/.exe/config.json
./exe serve       # run the daemon; it keeps this terminal
```

Open http://127.0.0.1:7777: the desktop is the first thing that works, and it
needs nothing else. A Linux machine without KVM or Firecracker still gets
it, with the apps, the Terminal and the Hub, and an empty VM list; see
[Running without VMs](#running-without-vms-a-nas-a-container).

**2. A VM, from a second terminal** (`serve` has the first). Linux and Windows
have [requirements](#linux-requirements) of [their own](#windows-requirements);
`exe code` talks to the Ollama named in the [configuration](/docs/config).

```sh
./exe create demo               # clone Debian 13, boot, cloud-init, SSH ready
./exe ssh demo                  # log in (key in ~/.exe/ssh/)
./exe code demo "build me a guestbook app on port 8000"
```

The first `create` downloads the Debian 13 `genericcloud` raw image (~3 GB) once
into `~/.exe/images/`. Linux also downloads the configured direct-boot kernel.

**3. A public URL.** Needs a Cloudflare tunnel and a token, set up once:
[Cloudflare setup](/docs/config#cloudflare-setup-one-time).

```sh
./exe expose demo -port 8000 -sub guestbook   # -> https://guestbook.<domain>
```

# Linux requirements

- An amd64 or arm64 host with hardware virtualization and `/dev/kvm`.
- Firecracker on `PATH`, plus `ip`, `iptables`, `debugfs`, and `resize2fs`.
- The daemon user must belong to the `kvm` group. Restrict the network helper's
  execute permission to root and the daemon user's group.
- The root-owned `exe-net-helper` must have only `CAP_NET_ADMIN`. It
  validates private /30 subnets, deterministic `exe-*` TAP names, the caller
  uid, and outbound interface names. Privileged child tools are resolved only
  from root-owned system directories and run with a minimal environment.

For the systemd deployment in this repository (a user unit, so the daemon
restarts, reports status and shows its journal without sudo):

```sh
sudo usermod -aG kvm livid
make build
sudo install -o root -g livid -m 0750 exe-net-helper /usr/local/libexec/exe-net-helper
sudo setcap cap_net_admin=ep /usr/local/libexec/exe-net-helper
sudo loginctl enable-linger livid
install -m 0644 deploy/systemd/exe.service ~/.config/systemd/user/exe.service
systemctl --user daemon-reload
systemctl --user enable --now exe
```

Install Firecracker from its official release archive before starting the
service. Reapply `setcap` whenever the helper binary is replaced. The
checked-in unit is specific to `/www/exe` and `/home/livid/.exe`.

After `make build`, `systemctl --user restart exe` runs the new binary;
`journalctl --user -u exe` is the log. The daemon stops its VMs on the way
down and starts them again on the way up, and the unit's `KillMode=process`
leaves the tmux servers behind the Terminal, Claude Code and Codex windows
running across the restart. The restart endpoint (`POST
/v1/daemon/restart`, the desktop's Restart item) does the same under
systemd: it exits and lets the manager start the new binary.

At boot the daemon may come up before Tailscale has its address. It waits
up to five minutes for `listen` (likewise `proxy_listen` and `ssh_listen`)
to become bindable, and exits non-zero after that so systemd starts it
again.

# Windows requirements

- An x86-64 host. Enable **Windows Hypervisor Platform** and **Virtual
  Machine Platform** under *Windows features* (available on Windows Home
  too), then reboot.
- QEMU: `winget install SoftwareFreedomConservancy.QEMU`, or set
  `qemu.binary` if it is not on `PATH` or in `C:\Program Files\qemu`.
- No admin rights, drivers or TAP devices: the guest network is a userspace
  TCP/IP stack inside the daemon. VM IPs (default `192.168.127.0/24`) exist
  only inside `exe serve` — the SSH gate, web terminal, agent and reverse
  proxy all reach them, and the Services tab publishes per-port
  `127.0.0.1` forwards for your browser.
- Consider excluding `%USERPROFILE%\.exe` from Microsoft Defender scanning;
  first boots are much faster.

# Running without VMs (a NAS, a container)

The daemon only needs a hypervisor for the VMs. When the Linux backend
cannot start because Firecracker, its network helper, `debugfs` or
`resize2fs` is missing (or the CPU is one Firecracker does not support),
`exe serve` logs why and runs without VMs: the desktop, Workspace, apps,
the Terminal, Claude Code and Codex windows, the Hub, Chat and Mac OS 9 all
work, the VM list is empty, About This Computer says why, and every VM call
answers `503` with the reason. A bad configuration or a busy state
directory still stops the daemon.

`make cross` builds static `dist/exe-linux-amd64` and `dist/exe-linux-arm64`
(plus the network helpers) with cgo off, so the binary runs on any Linux
userland with no Go installed. Point `EXE_HOME` at a writable directory
when the daemon user has no home. Synology DSM is one such target: the
Intel Plus and xs models are amd64, the Realtek models are arm64, and
Container Manager runs a plain Linux image on both; the x86 models that
run Synology's own Virtual Machine Manager have `/dev/kvm`, which a
container would need passed through (plus `/dev/net/tun` and
`CAP_NET_ADMIN`) before Firecracker could work there. That last step is
untested.
