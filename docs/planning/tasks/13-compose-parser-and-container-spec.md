Parent slice: [runtime](../slices/runtime.md)
Depends on: 01

# Task 13 — compose-parser-and-container-spec

_Tick `[x]` on each Tasks item as you finish it, and on each Acceptance item as it passes. The unticked state is what tells the next planning run that this task is still safe to edit in place._

## Goal
Wrap `compose-spec/compose-go` to load a compose file, reject any field outside the closed set from spec §3, and produce a `ContainerSpec` shaped for the moby docker client.

## Tasks
- [x] Add `github.com/compose-spec/compose-go/v2` to `go.mod`.
- [x] Create `internal/runtime/compose/compose.go` with `Load(path) (*types.Project, []byte, error)` (returns raw bytes alongside the parsed project so Validate can do a strict closed-field-set check) and `LoadBytes(body, virtualFilename) (*types.Project, error)` for the in-flight Deploy.Apply path. Both go through compose-go's `loader.LoadWithContext` with `SkipConsistencyCheck=true` (we do our own field-level validation).
- [x] Create `internal/runtime/compose/validate.go` with `Validate(rawYAML []byte) error`. Walks each service's raw YAML keys against the closed set `image, command, entrypoint, environment, env_file, volumes, ports, depends_on, healthcheck, labels, user, working_dir, tmpfs, cap_add, cap_drop, sysctls, ulimits, read_only, networks`. `restart` and `name` are also accepted (parsed-but-ignored, per spec §2 failure mode). Anything else → `ValidationError{Code:"validation_failed", Details:{service, field}}`. Service-level `networks:` references that don't appear in the top-level `networks:` map (the implicit `_default` is always considered declared) → `ValidationError{Code:"unknown_network", Message:"unknown network: <name>; declared: [a, b]"}`.
- [x] Create `internal/runtime/compose/spec.go` with `ToContainerSpec(svc types.ServiceConfig, opts SpecOptions) ContainerSpec`. Maps per runtime slice §4: standard fields, healthcheck → JACO Healthcheck, cap_add/drop/sysctls/ulimits/read_only/tmpfs → struct fields, labels merged with the six JACO-managed labels (JACO labels always win). Env is alphabetically sorted for determinism.
- [x] `restart` is parsed but ignored. `ports` carried into `ContainerSpec.Ports` for audit / docs but never published. `networks` becomes the docker network name list (`jaco_<deployment>_<network>`, default `_default` — compose-go's implicit `default` is translated to `_default` at the network-name boundary).
- [x] Add testdata fixtures: `testdata/valid.yml` exercises every allowed field plus `restart`; `testdata/unknown-field.yml` uses `deploy:` (not allowed); `testdata/unknown-network.yml` references a missing network. A small `.env.web` file lets the valid fixture exercise `env_file`.
- [x] Ten unit tests exercise Load, Validate (valid passes, unknown-field rejected with details, unknown-network rejected with bad name in the message, `_default` always considered declared), ToContainerSpec (six JACO labels stamped, core field mapping including healthcheck/ulimits/caps/tmpfs/env-sorted, networks default to `jaco_<deployment>__default`, networks list uses deployment prefix), and ContainerName.

## Acceptance criteria
- [x] `go test ./internal/runtime/compose/... -race -count=1` exits 0.
- [x] Test asserts `unknown-field.yml` produces `Error.code == "validation_failed"` and `unknown-network.yml` produces `Error.code == "unknown_network"`.
- [x] Test asserts the produced `ContainerSpec.Labels` carries all six JACO labels (`jaco.cluster_id`, `.deployment`, `.service`, `.replica_id`, `.replica_index`, `.raft_index`).

> If a `## Tasks` checkbox can't be completed without changing what the parent slice specifies, stop and update the slice. Do not redesign here.
