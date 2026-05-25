# jaco/bootstrap

Two scripts that take three fresh Debian nodes to a running JACO cluster with
the bench workload deployed. See [`../README.md`](../README.md) for the full
flow and env contract.

- **`bootstrap.sh`** — run on the operator host. Resolves node IPs (env or
  Azure), installs everything, forms the cluster, builds/pushes images on
  node-1, and applies the workload.
- **`install-node.sh`** — shipped to and run on each node by `bootstrap.sh`.
  Installs Docker + jacod and wires the insecure in-cluster registry. Safe to
  run standalone if you want to provision a node by hand.

Both are idempotent — re-running `bootstrap.sh` against an already-formed
cluster re-pushes images and re-applies without tearing anything down.
