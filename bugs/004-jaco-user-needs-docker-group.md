# BUG 004 — jaco user lacks docker group; cannot reach docker.sock

## Symptom

After `jaco apply hello.yaml` against the 3-node Azure cluster,
deployment reaches `ACTIVE` in `jaco status` but the **Replicas:**
section stays empty and `docker ps` on each node shows nothing.

`journalctl -u jaco.service` shows the runtime reconciler erroring on
every lifecycle.Start call:

```
start replica hello-web-0: lifecycle.Start: list containers:
permission denied while trying to connect to the Docker daemon
socket at unix:///var/run/docker.sock
```

## Severity

**Blocking.** No containers spawn → no workloads run → the daemon's
runtime layer is non-functional.

## Root cause

`build/jaco.service` sets `User=jaco / Group=jaco`. The Debian docker
package gates `/var/run/docker.sock` behind the `docker` group
(`srw-rw---- 1 root docker`). The jaco system user is in only its
own group, so `Open(socket)` returns EACCES.

The install.sh.tpl does `useradd -r jaco` but doesn't add the user to
`docker`. On dev hosts (where the operator runs jacod as root for
tests) this never surfaced.

## Fix

`build/install.sh.tpl`: add `usermod -aG docker jaco` after the
`useradd` step (only when the docker group exists; some dev images
don't have docker installed). Also fix the running cluster manually
by `usermod -aG docker jaco && systemctl restart jaco.service` on
each VM.

## Status

**FIXING NOW.** Already corrected on the 3 live VMs; install.sh.tpl
update lands in the same commit as this bug doc.
