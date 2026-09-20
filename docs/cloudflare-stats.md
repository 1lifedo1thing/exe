# Live Cloudflare statistics

The Cloudflare module in the Control Strip shows connection counts across
all replicas of the selected tunnel, followed by traffic counters from this
machine's cloudflared connector. **Cloudflare Status…** also shows connector
uptime and the existing tunnel configuration.

Local counters refresh every five seconds while the menu or status window
is open and the page is visible. Cloudflare's tunnel connection information
is cached for thirty seconds. The local request rate is calculated between
samples; opening the widget after a pause or restarting cloudflared starts
a new sample interval. Request and origin-error totals are since the local
connector process started, not daily totals or account-wide analytics.

The local metrics listener defaults to `http://127.0.0.1:20241`. To use
another loopback listener, set `cloudflare.metrics_url` in Configuration to
its base address, without `/metrics`. exe checks the connector identity
against the selected tunnel before displaying its counters. Missing metrics
are shown as unavailable; cached Cloudflare counts are explicitly labelled
when their refresh fails.

## Verified Linux ARM64 build

Source commit: `489843319a29ffa9f7c42845b5c1fea0e5f8c55e`.

The archive contains `exe`, `exe-net-helper`, their SHA-256 checksums,
build metadata and the project README. It was built with `make build`
using Go 1.26.5 on Linux ARM64. The daemon uses cgo and requires glibc;
the network helper is built with cgo disabled. See the README for the
hypervisor and network-helper installation requirements.

Archive: `exe-cloudflare-stats-linux-arm64-4898433.tar.gz`
(10,092,675 bytes).

IPFS CID:

```text
bafybeif7s2w5rkclbrj73klnv5t7qkd4hjhoqjstj6s42g54h7a6lf6cgu
```

SHA-256:

```text
f5152d5180694fc66644619be0e0eda9af586c23b514478881f1ccd9777460a8
```

The recursive pin and a fresh download through the
[local Kubo gateway](http://127.0.0.1:8081/ipfs/bafybeif7s2w5rkclbrj73klnv5t7qkd4hjhoqjstj6s42g54h7a6lf6cgu)
were verified on 2026-09-20, including the byte count and SHA-256. This
verified URL is available on the build machine. Other machines can retrieve
the same archive by CID with an IPFS client:

```sh
ipfs get bafybeif7s2w5rkclbrj73klnv5t7qkd4hjhoqjstj6s42g54h7a6lf6cgu \
  -o exe-cloudflare-stats-linux-arm64-4898433.tar.gz
```

The [public gateway URL](https://ipfs.io/ipfs/bafybeif7s2w5rkclbrj73klnv5t7qkd4hjhoqjstj6s42g54h7a6lf6cgu)
returned HTTP 429 during verification, as did dweb.link; public HTTP
retrieval is therefore not claimed as verified. The archive remains pinned
in Kubo and outside Git.

Validation passed: `go test ./...`; browser checks at device pixel ratios
1, 1.25, 1.5 and 2, plus a 320-pixel phone viewport; stable layouts during
updates and unavailable metrics; and an installed-daemon browser check
after deployment. The deployed tunnel showed two replicas and eight edge
connections, with four local connections and a changing request rate.
The VM's published Hub, monitor and tides APIs remained reachable after
the daemon restart.
