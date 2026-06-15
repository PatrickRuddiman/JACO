---
sources:
  - nfpm.yaml
  - Makefile
  - .github/workflows/release.yml
  - cmd/jacod/main.go
  - internal/packaging/
  - build/jaco.service
  - build/jaco.socket
  - build/release.sh
  - build/install.sh.tpl
---

# Installation

Install the `jaco` CLI and `jacod` daemon on each host that will be a
cluster member. Releases ship `.deb`, `.rpm`, `.apk`, and a generic
`.tar.gz` for `linux/amd64` and `linux/arm64`, plus a `SHA256SUMS`
manifest.

All artifacts are published at
<https://github.com/PatrickRuddiman/JACO/releases/latest>. Swap `<arch>`
for `amd64` or `arm64` in the snippets below.

## Debian / Ubuntu

```sh
curl -fsSL -O https://github.com/PatrickRuddiman/JACO/releases/latest/download/jaco_<arch>.deb
sudo dpkg -i jaco_<arch>.deb
sudo systemctl enable --now jaco
```

The package depends on `docker.io | docker-ce | docker-engine`; any of
the three satisfies it.

## RHEL / Fedora / CentOS

```sh
curl -fsSL -O https://github.com/PatrickRuddiman/JACO/releases/latest/download/jaco_<arch>.rpm
sudo rpm -i jaco_<arch>.rpm        # or `sudo dnf install ./jaco_<arch>.rpm`
sudo systemctl enable --now jaco
```

The rpm depends on `/usr/bin/docker` (whatever package provides it).

## Alpine

```sh
curl -fsSL -O https://github.com/PatrickRuddiman/JACO/releases/latest/download/jaco_<arch>.apk
sudo apk add --allow-untrusted ./jaco_<arch>.apk
```

Alpine ships OpenRC, not systemd. The package installs the binaries and
config but does not register a service unit; bring `jacod` up under your
service manager of choice.

## Generic tarball

For hosts without `.deb` / `.rpm` / `.apk` support:

```sh
curl -fsSL -O https://github.com/PatrickRuddiman/JACO/releases/latest/download/jaco-vX.Y.Z-linux-<arch>.tar.gz
tar xf jaco-vX.Y.Z-linux-<arch>.tar.gz
cd jaco-vX.Y.Z-linux-<arch>
sudo install -m 0755 jaco  /usr/local/bin/jaco
sudo install -m 0755 jacod /usr/local/bin/jacod
sudo install -d -m 0755 /etc/jaco
sudo install -m 0644 jacod.yaml /etc/jaco/jacod.yaml
sudo install -m 0644 jaco.service /lib/systemd/system/jaco.service
sudo install -m 0644 jaco.socket  /lib/systemd/system/jaco.socket
sudo systemctl daemon-reload
sudo systemctl enable --now jaco
```

## Verify the download

`SHA256SUMS` lists every artifact in the release. The filename pattern
matches what `nfpm` / the release workflow emits — for example
`jaco_0.1.0_amd64.deb`, `jaco-0.1.0-1.x86_64.rpm`,
`jaco_0.1.0_x86_64.apk`, `jaco-v0.1.0-linux-amd64.tar.gz`.

```sh
curl -fsSL -O https://github.com/PatrickRuddiman/JACO/releases/latest/download/SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
```

For self-upgrades JACO additionally verifies a minisign signature over
`SHA256SUMS` against an embedded public key — see
[`operations/upgrades.md`](operations/upgrades.md) and
[`contributing/release-and-packaging.md`](contributing/release-and-packaging.md).

## On-disk layout

After a successful install you will find:

- `/usr/local/bin/jaco` — the operator CLI.
- `/usr/local/bin/jacod` — the long-running daemon.
- `/etc/jaco/jacod.yaml` — daemon config. Edit
  [`listen_addr` / `cluster_addr` / `data_dir`](configuration.md) before
  the first start if the defaults don't fit.
- `/lib/systemd/system/jaco.service` — systemd unit. `daemon-reload`
  runs automatically on package install.
- `/lib/systemd/system/jaco.socket` — systemd socket unit. systemd (PID 1)
  creates and binds the local control socket in the host namespace and hands
  the daemon the file descriptor, so the socket is always reachable by the
  `jaco` group regardless of the daemon's filesystem sandbox (issue #167).
  Pulled in automatically by `jaco.service` via `Requires=`.
- `/var/lib/jaco/` — created by the daemon on first boot. Holds raft
  store, snapshots, the node certificate, and the WireGuard private key.
- `/var/run/jaco/jaco.sock` — the local control socket. Mode `0660`,
  group `jaco`; anyone in the `jaco` group can drive the local daemon
  without a bearer token (see [Auth and tokens](concepts/auth-and-tokens.md)).

## Post-install state

The daemon comes up in the **uninitialized** state. Every RPC except
`Cluster.{Init, Join, Status}` returns `cluster_uninitialized` until
either of those two transitions runs. From here, either:

- bootstrap a new cluster with `sudo jaco cluster init`, or
- join an existing cluster with `sudo jaco node join --peer … --token …`.

Both of those commands are the point at which this node commits to a cluster,
so they also run `systemctl enable jaco` for you — the daemon will come back up
on reboot without a manual `systemctl enable`. (The package itself ships the
unit **disabled** on purpose so a half-configured node never auto-starts; the
enable only happens once you've committed to a cluster shape.) Pass
`--no-systemd-enable` to either command to opt out; on non-systemd hosts (the
Alpine/apk path) the step is a no-op and you bring `jacod` up under your own
service manager.

Both flows are walked end-to-end in [Getting started](getting-started.md).

## See also

- [Getting started](getting-started.md)
- [Configuration](configuration.md)
- [Upgrades](operations/upgrades.md)
- [Release and packaging](contributing/release-and-packaging.md)
