Parent slice: [runtime](../slices/runtime.md)
Depends on: 01

# Task 13 — compose-parser-and-container-spec

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Wrap `compose-spec/compose-go` to load a compose file, reject any field outside the closed set from spec §3, and produce a `ContainerSpec` shaped for the moby docker client.

## Tasks
- [ ] Add `github.com/compose-spec/compose-go/v2` to `go.mod`.
- [ ] Create `internal/runtime/compose/compose.go` with `Load(path string) (*types.Project, error)` using `cli.NewProjectOptions` + `cli.ProjectFromOptions`.
- [ ] Create `internal/runtime/compose/validate.go` with `Validate(p *types.Project) error`. Closed allowed-fields set per spec §3 In: `image, command, entrypoint, environment, env_file, volumes, ports, depends_on, healthcheck, labels, user, working_dir, tmpfs, cap_add, cap_drop, sysctls, ulimits, read_only, networks`. Any other populated field → `Error{code:"validation_failed", details:{service, field}}`. Service-level `networks:` entries that do not appear in the top-level `networks:` map → `Error{code:"unknown_network", message:"unknown network: <name>; declared: [a, b]"}`.
- [ ] Create `internal/runtime/compose/spec.go` with `ToContainerSpec(svc types.ServiceConfig, deployment, replicaID string, replicaIndex int, raftIndex uint64) ContainerSpec`. Maps per runtime slice §4: standard fields, healthcheck → docker Healthcheck, cap_add/drop/sysctls/ulimits/read_only/tmpfs → HostConfig, labels merged with JACO-managed labels.
- [ ] `restart` is parsed but ignored (logged at debug); `ports` is parsed but not used for ingress; `networks` becomes the bridge attach list (`jaco_<deployment>_<network>`, default `_default`).
- [ ] Add testdata fixtures: `testdata/compose/valid.yml` (uses every allowed field), `testdata/compose/unknown-field.yml` (uses `deploy:` which is not allowed), `testdata/compose/unknown-network.yml` (service references a network that isn't declared at top level).
- [ ] Unit tests: each fixture exercises `Load + Validate + ToContainerSpec` with expected outcomes.

## Acceptance criteria
- [ ] `go test ./internal/runtime/compose/... -race -count=1` exits 0.
- [ ] Test asserts `unknown-field.yml` produces `Error.code == "validation_failed"` and `unknown-network.yml` produces `Error.code == "unknown_network"`.
- [ ] Test asserts the produced `ContainerSpec.Labels` carries all six JACO labels (`jaco.cluster_id`, `.deployment`, `.service`, `.replica_id`, `.replica_index`, `.raft_index`).

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
