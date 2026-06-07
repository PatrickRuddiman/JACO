---
sources:
  - cmd/jaco/version.go
  - cmd/jaco/root.go
---

# `jaco version` / `jaco --version`

Print the CLI version string. Both the `version` subcommand and the
`--version` persistent flag emit the same line — the bare version,
no `jaco version ` prefix, no commit / build-date trailer. This
matches `jacod --version`'s output so scripted version detection can
treat the two binaries identically.

## Synopsis

```
jaco --version
jaco version
```

## Examples

```
$ jaco --version
v0.3.5

$ jaco version
v0.3.5

$ jacod --version
v0.3.5
```

## How the version string is set

The package-level `version` var in `cmd/jaco/root.go` (and
`cmd/jacod/main.go`) defaults to `dev` and is overridden at build
time via `-ldflags '-X main.version=<tag>'`. The release pipeline
([`.github/workflows/release.yml`](../../.github/workflows/release.yml))
injects the git tag for both binaries; an ad-hoc `go build ./cmd/jaco`
with no ldflag prints `dev`.

If you need both the version string AND the commit hash, build with
the same flags the release pipeline uses or check `jaco self-upgrade`
output for the embedded build metadata.

## Auth

None — purely client-side, no daemon dial.

## Exit codes

- `0` — success.

## See also

- [`jaco self-upgrade`](self-upgrade.md) — atomic binary swap
- [Release and packaging](../contributing/release-and-packaging.md)
