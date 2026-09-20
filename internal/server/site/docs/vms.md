# How VMs Work

The default image is Debian 13 `genericcloud`. Each VM gets a persistent sparse
disk, the `dev` user, and the service SSH key through cloud-init NoCloud.

On macOS, VMs use EFI and virtio disk/net/console/entropy through
Virtualization.framework. NAT comes from the shared macOS DHCP (`bootpd`);
IPs are discovered from `/var/db/dhcpd_leases`, matching by MAC or, for
DUID-identifying clients (Debian 13's dhcpcd), by lease name with a pre-boot
snapshot to skip stale entries.

On Linux, exe extracts the ext4 root partition from the default GPT cloud
image, resizes it offline, and uses it as Firecracker's single `/dev/vda` root
drive. It injects persistent systemd-networkd and NoCloud data directly into
that filesystem, then direct-boots the configured kernel and connects a per-VM
TAP to host NAT. Custom images must either be raw ext4 filesystems or GPT images
with an ext4 root partition, and must support the configured kernel. Firecracker
serial output is at `~/.exe/vms/<name>/console.log`.

On Windows, VMs are `qemu-system-x86_64 -accel whpx` child processes booting
the same EFI image as macOS (per-VM EFI variable store, virtio disk/net/rng,
seed.iso, serial to `console.log`, QEMU messages to `qemu.log`). The guest
network is a gvisor netstack inside the daemon: each VM's virtio NIC streams
ethernet frames to the daemon over loopback, a built-in DHCP server hands out
one deterministic address per VM (MACs are derived from the address), and the
daemon NATs guests outbound through the host sockets API. A kill-on-close job
object powers VMs off if the daemon dies. Custom images must be full GPT disk
images with an EFI partition.

VMs are children of `exe serve`. An exclusive state lock prevents two Linux
daemons from managing the same VM directory. Graceful daemon shutdown powers
VMs off; parent-death handling terminates Firecracker if the daemon crashes,
and the next daemon startup removes stale per-VM TAP and firewall state. Disks
persist and `exe start` boots them again.

# Security notes

- VMs have private host-local addresses; the proxy is what exposes their services.
- Set `api_token` before binding the API beyond localhost.
- The agent has passwordless sudo **inside the VM** — that's the sandbox boundary.
- On Linux, the daemon and Firecracker are unprivileged. Only the validated
  network helper has `CAP_NET_ADMIN`; it is root-owned and not daemon-writable.
- The SSH gate accepts only keys it already knows (service key, your
  `~/.ssh/*.pub`, `~/.exe/ssh/authorized_clients`) — there is no
  first-come key adoption, so it's safe to leave on `:2222` on a LAN.
