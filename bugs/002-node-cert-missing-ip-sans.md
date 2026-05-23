# BUG 002 — Cluster CA-signed node cert lacks IP SAN

## Symptom

After `jaco cluster init` on jaco-1 (Tailscale IP 100.96.111.6, listen
addr `100.96.111.6:7000`), dialing the gRPC listener by IP with the
cluster CA pinned fails:

```
$ jaco node list --server 100.96.111.6:7000 --ca-cert /var/lib/jaco/node/ca.crt
rpc error: code = Unavailable desc = connection error:
  desc = "transport: authentication handshake failed:
    tls: failed to verify certificate:
    x509: cannot validate certificate for 100.96.111.6
    because it doesn't contain any IP SANs"
```

This blocks `jaco node issue-join-token` when invoked by IP — the join
token never lands in stdout because the dial returns the TLS error.

## Severity

Functionally blocking when operators dial by IP. Workaround: dial by
hostname (`--server jaco-1:7000`) since the cert *does* include the
hostname as a DNS SAN, or drop `--ca-cert` to fall back to
InsecureSkipVerify with the bearer-token gate.

## Root cause

`internal/controlplane/ca.GenerateNodeKeypair(hostname)` builds the
CSR with only `DNSNames: []string{hostname}` — no IP SANs. When the
operator (or a peer) dials by IP, the standard `tls.Config{RootCAs:
pool}` verifier needs an IP match in the cert's SAN; the hostname-only
SAN doesn't satisfy that.

The same applies to the daemon-side `bootstrap.Run` initial cert and
the `NodeJoin` CSR — neither carries `IPAddresses` on the template.

## Fix

Plumb the listen_addr / bind_addr IP into `GenerateNodeKeypair` (and
`bootstrap.Run`'s self-signing path) so the template includes
`IPAddresses: []net.IP{net.ParseIP(listenIP)}`. Operators can then dial
either form (hostname or IP) and the cert validates.

Tracked separately because the v0 plaintext TCP path that earlier
shipped on the cross-host listener didn't surface this; iter-41's TLS
work is what now exposes it. The workaround (dial by hostname) is
adequate for cluster bring-up; the real fix lands in a follow-up
iteration.

## Status

Open. Workaround active. Real fix wanted in a follow-up touching
`internal/controlplane/ca` + `bootstrap`.
