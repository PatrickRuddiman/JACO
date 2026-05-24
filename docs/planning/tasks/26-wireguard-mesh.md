Parent slice: [discovery](../slices/discovery.md)
Depends on: 07, 25

# Task 26 — wireguard-mesh

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
`wg-jaco` kernel WireGuard interface managed via `wgctrl`; one peer per remote Node; `allowed_ips = ∪ {Subnet.cidr | hosted on that peer}` recomputed on every placement change.

## Tasks
- [x] Add `golang.zx2c4.com/wireguard/wgctrl` (provides `wgtypes` — pure-Go keygen, no kernel needed for the primitives).
- [x] Create `internal/discovery/wgmesh/wgmesh.go` (renamed from `wireguard` to keep the package name distinct from the upstream wireguard module). Exposes `GenerateKeypair()`, `LoadOrGenerateKeypair(dataDir)` (reads `<dataDir>/wg/private.key` mode 0600 or creates it), `SlotIP(hostname)` (deterministic /32 in 10.99.0.0/24 — first byte of sha256(hostname), wrapped into [1,254]), `AllowedIP(hostname)` (formatted `<slot>/32`), and `RenderConfig(state, selfHostname, selfPrivate)` which emits a wg-quick config keyed by `Node.wireguard_pubkey` with Endpoint = `<host of Node.address>:51820`.
- [x] `RenderConfig` omits the self entry, omits peers whose `wireguard_pubkey` is empty (the node hasn't registered yet), sorts peers alphabetically for deterministic output, and emits `PublicKey`, `Endpoint`, `AllowedIPs`, `PersistentKeepalive` per peer.
- [x] Ten unit tests pass with -race: GenerateKeypair produces valid 32-byte keys with priv != pub; LoadOrGenerateKeypair persists across calls and stamps 0600 on the file; SlotIP stays inside MeshNetwork with the third octet fixed and the fourth in [1,254]; SlotIP is deterministic; AllowedIP has /32 suffix; RenderConfig emits Interface + one peer block with all required fields; self + unregistered peers omitted; peer ordering is alphabetical; bad pubkey length rejected; empty hostname rejected.
- [ ] **Deferred**: `EnsureInterface` (bring up `jaco0` via netlink), `ReconcilePeers` (call `wgctrl.ConfigureDevice` with the rendered config), and the daemon-start hook (publish `Node{wireguard_pubkey}` via Cluster.UpdateSelf, then watch Nodes + ReplicaDesired and re-render on each event). All three need kernel WG + `CAP_NET_ADMIN` + a Cluster.UpdateSelf RPC; lands when the daemon entry comes together.
- [ ] **Deferred**: real-engine integration test (`-tags=wireguard`) — requires the kernel module + a runnable daemon.

## Acceptance criteria
- [x] `go test ./internal/discovery/wgmesh/... -race -count=1` exits 0 (10 pure-Go tests pass).
- [x] Test asserts RenderConfig output contains `PublicKey =`, `Endpoint =`, `AllowedIPs =` lines per non-self peer (`TestRenderConfig_EmitsInterfaceAndOneSection`).
- [x] `stat -c '%a'` on the persisted wg key prints `600` after first call (`TestLoadOrGenerateKeypair_PersistsAcrossCalls`).
- [ ] `go test -tags=wireguard ./internal/discovery/wgmesh/... -race -count=1` against a real kernel — deferred.
- [ ] `allowed_ips` re-converges within 5s of a ReplicaDesired placement change — deferred to the daemon-entry wiring.

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
