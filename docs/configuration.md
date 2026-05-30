---
sources:
  - internal/daemon/config/
  - internal/daemon/netdetect/
  - cmd/jacod/main.go
---

# Configuration

The daemon reads `/etc/jaco/jacod.yaml` at startup. The schema is
**closed**: any unknown key fails the parse with an error pointing at the
offending field. A missing file is equivalent to "all defaults" — the
daemon does not refuse to start when the config is absent.

The path can be overridden with the `JACO_CONFIG` environment variable,
honored by `cmd/jacod`.

## Defaults

```yaml
data_dir:     /var/lib/jaco
listen_addr:  0.0.0.0:7000
cluster_addr: 0.0.0.0:7001
unix_socket:  /var/run/jaco/jaco.sock
wg_port:      51820
log_level:    info
ipam_pool:    10.244.0.0/16
acme_email:   ""
acme_ca:      "https://acme-v02.api.letsencrypt.org/directory"
acme_enabled: true
acme_skip_staging: false
```

All defaults live in `internal/daemon/config/config.go`. The same
constants seed the `jacod.yaml` template shipped in the packages, so a
freshly-installed cluster is functional with zero edits provided the
host has a private-LAN interface JACO can auto-detect.

## Keys

### `data_dir` (string, required)

Filesystem path that holds the raft store, snapshots, the node TLS cert
+ key, and the WireGuard private key. Default `/var/lib/jaco`. The
directory and everything under it MUST be readable + writable by the
user the daemon runs as (the package installer creates the `jaco`
system user). A missing directory is created on first boot.

### `listen_addr` (string, required, `host:port`)

Cross-host gRPC listener — peers and remote CLI dial this address. The
default `0.0.0.0:7000` is **not** a literal bind on every interface:
the daemon resolves an unspecified host against
`internal/daemon/netdetect`, which picks a **private-LAN** IPv4
candidate (RFC 1918, CGNAT, link-local) and binds the listener to
exactly that face. A host whose only routable interface is public
fails fast with guidance — JACO never auto-exposes the control plane
to the public Internet. See `cmd/jacod/main.go::resolveAdvertise` for
the resolution code.

Pin an explicit `host:port` (e.g. `10.0.0.5:7000`) to bypass detection
on multi-NIC hosts, on overlay-only clusters where the daemon should
listen on the overlay interface, or whenever you want bind == advertise
to be an exact value. A pinned value is honored verbatim.

### `cluster_addr` (string, required, `host:port`)

Raft TCP transport. Same resolution semantics as `listen_addr`. MUST
differ from `listen_addr`. Default `0.0.0.0:7001`.

### `unix_socket` (string, required)

Path the daemon binds locally for CLI-to-daemon control. Mode `0660`,
group `jaco`. The socket's filesystem permissions ARE the auth
boundary: any process whose user is in the `jaco` group can drive the
local daemon **without** presenting a bearer token. See
[Auth and tokens](concepts/auth-and-tokens.md). Default
`/var/run/jaco/jaco.sock`.

### `wg_port` (int, required, 1–65535)

UDP port for the per-node WireGuard interface (`wg-jaco`). All peers
must agree; mismatches present as silent traffic loss. Default `51820`.

### `acme_email` (string, optional)

**Cluster-wide default** ACME contact address. Used by every
deployment whose `jaco.yaml` does not declare its own top-level
`acme_email:`; deployments that do set the field get their own ACME
account and own automation policy (see
[Ingress → Per-stack ACME contact email](concepts/ingress.md#per-stack-acme-contact-email)
and [`jaco.yaml` schema](manifests/jaco-yaml.md#acme_email)).

Empty here AND empty per-stack is permitted but recommended against —
ACME providers may not deliver expiry warnings without an address.
No default.

### `acme_ca` (string, optional, https URL)

ACME directory URL the cert issuer targets. Empty (the default) means
Let's Encrypt production
(`https://acme-v02.api.letsencrypt.org/directory`). Pin
`https://acme-staging-v02.api.letsencrypt.org/directory` (or the
`ACMEStagingCA` constant) for a dev/test cluster.

### `acme_enabled` (bool, optional, default `true`)

Cluster-wide ACME kill switch. Set to `false` to opt out entirely: the
daemon does not register the ACME issuer and the rendered Caddy config
carries no `tls.automation` block, which is operator-verifiable without
any outbound ACME call. Useful for clusters fronted by a separate cert
pipeline.

### `acme_skip_staging` (bool, optional, default `false`)

Skip the stage-first dry run for new domains. By default, new domains
issue against Let's Encrypt staging before flipping to the production
URL — staging's much looser rate limits absorb DNS/firewall
misconfigurations cheaply. Already-non-prod `acme_ca` values skip
staging automatically regardless of this setting.

### `log_level` (string, optional)

One of `debug | info | warn | error`. Default `info`. Logs go to the
systemd journal under `SYSLOG_IDENTIFIER=jacod` when the daemon
detects systemd, JSON-on-stderr when the journal socket is unreachable,
and human-readable text otherwise (see
[Observability](concepts/observability.md)). The `JACO_LOG` env
variable overrides this at the process level.

### `ipam_pool` (string, required, IPv4 `/16` CIDR)

Cluster-wide IP pool the leader carves into `/24`s, one per
(deployment, network) pair. Default `10.244.0.0/16` gives 256
allocations before exhaustion. MUST be exactly a `/16` — any other
prefix length is rejected.

## Validation rules (enforced at parse time)

- `data_dir`, `listen_addr`, `cluster_addr`, `unix_socket`,
  `ipam_pool` are required.
- `listen_addr` and `cluster_addr` parse as `host:port` and MUST differ.
- `wg_port` is in `1..65535`.
- `log_level` is one of `debug | info | warn | error`.
- `acme_ca`, when set, MUST be an `https://…` URL.
- `ipam_pool` parses as a CIDR with a `/16` mask exactly.
- Any unknown top-level key fails the parse with the offending field
  name in the error message.

See `internal/daemon/config/config.go::Validate` for the canonical
rule set.

## Reloading

There is no hot reload in v1. Change `jacod.yaml` and
`sudo systemctl restart jacod` (or your service manager equivalent).
A restart is safe: orphaned containers are reclaimed on next boot via
JACO's label-based reconcile, and raft replays cleanly from disk.

## See also

- [Installation](installation.md)
- [Networking](concepts/networking.md)
- [Auth and tokens](concepts/auth-and-tokens.md)
- [Observability](concepts/observability.md)
