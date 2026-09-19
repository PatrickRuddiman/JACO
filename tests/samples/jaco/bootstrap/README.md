# jaco/bootstrap

Two scripts that take three fresh Debian nodes to a running JACO cluster with
the bench workload deployed. See [`../README.md`](../README.md) for the full
flow and env contract.

- **`bootstrap.sh`** — run on the operator host. Resolves node IPs (env or
  Azure), installs everything, forms the cluster, builds/pushes images on
  node-1, and applies the workload. Requires independently verified SSH
  host keys already in the operator's `known_hosts`; unknown or changed
  keys fail closed.
- **`install-node.sh`** — shipped to and run on each node by `bootstrap.sh`.
  Installs Docker + jacod and wires the insecure in-cluster registry. Safe to
  run standalone if you want to provision a node by hand. Its optional
  fourth argument, `node-private-ip` (after `vnet-cidr`), pins gRPC
  `listen_addr` and Raft `cluster_addr` to that address on ports 7000/7001.
  Bootstrap passes each node's resolved private IPv4 so advertised hosts
  match token approvals.

For each join, bootstrap reads the daemon's OS hostname over verified SSH,
issues a separate token with `--node-name <hostname> --san <private-ip>`,
and transfers node 1's public `/var/lib/jaco/node/ca.crt` over the same
verified channel to `/etc/jaco/cluster-ca.crt` on the joining host. The join
passes that independently provisioned file via `--ca-cert`; it does not
learn trust from a join response. Only the hostname and private IP are
approved; add explicit SAN approvals before using other gRPC dial aliases.
This sample assumes the package's default data directory and OS hostname.
Do not disable SSH host-key or TLS verification to make a run pass.

Both are idempotent — re-running `bootstrap.sh` against an already-formed
cluster re-pushes images and re-applies without tearing anything down.
