Parent slice: [discovery](../slices/discovery.md)
Depends on: 07, 25

# Task 26 — wireguard-mesh

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`wg-jaco` kernel WireGuard interface managed via `wgctrl`; one peer per remote Node; `allowed_ips = ∪ {Subnet.cidr | hosted on that peer}` recomputed on every placement change.

## Tasks
- [ ] Add `golang.zx2c4.com/wireguard/wgctrl` to `go.mod`.
- [ ] Create `internal/discovery/wireguard/key.go` with `EnsureKey(dataDir string) (privateKey wgtypes.Key, publicKey wgtypes.Key, err error)`. Reads/writes `${dataDir}/wg.key` mode 0600 (generated via `wgtypes.GeneratePrivateKey` on first start).
- [ ] Create `internal/discovery/wireguard/iface.go` with `EnsureInterface(name string, listenPort int, privateKey wgtypes.Key) error`. Brings up `wg-jaco` via netlink (use `vishvananda/netlink` or call `ip link` only on first miss); configures via `wgctrl.ConfigureDevice`. Default port 51820 (configurable via `--wg-port`).
- [ ] Create `internal/discovery/wireguard/peers.go` with `ReconcilePeers(ctx, nodes []Node, subnets []Subnet, self string) error`. For each remote Node: build `wgtypes.PeerConfig{PublicKey: node.WireguardPubkey, Endpoint: <node.Address>:<wg_port>, AllowedIPs: subnets where Subnet hosted on that node, PersistentKeepaliveInterval: 25s, ReplaceAllowedIPs: true}`.
- [ ] On daemon start: `EnsureKey`, then publish `Node{wireguard_pubkey}` via `Cluster.UpdateSelf` RPC if not already present in state. Open watches on Nodes, Subnets, ReplicaDesired; on any event, call `ReconcilePeers`.
- [ ] Compute "subnets hosted on node X": for each Subnet, host nodes are derived by aggregating `ReplicaDesired.host` for replicas attached to (deployment, network) of that subnet.
- [ ] Integration test (build tag `wireguard`; skip if no kernel WG or `CAP_NET_ADMIN`): bring up `wg-jaco` in t.TempDir() namespace; configure 2 peers; assert `wg show` output via wgctrl reports both peers with expected allowed_ips after a ReplicaDesired change.

## Acceptance criteria
- [ ] `go test -tags=wireguard ./internal/discovery/wireguard/... -race -count=1` exits 0 when kernel WG is available; skipped otherwise.
- [ ] Test asserts `allowed_ips` re-converges within 5s of a ReplicaDesired placement change.
- [ ] `stat -c '%a' $JACO_DATA_DIR/wg.key` prints `600` after first start.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
