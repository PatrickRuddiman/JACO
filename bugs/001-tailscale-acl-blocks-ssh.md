# BUG 001 — Tailscale ACL blocks SSH to jaco-{1,2,3}

## Symptom

```
$ ssh -i ~/.ssh/jaco azureuser@jaco-1
tailscale: tailnet policy does not permit you to SSH to this node
Connection closed by 100.96.111.6 port 22
```

Same for `jaco-2` (100.127.112.15) and `jaco-3` (100.77.36.44).

## Severity

Blocking for SSH-based provisioning, but **not blocking** for the
overall run — `az vm run-command invoke` is a working substitute that
goes through Azure's Linux Guest Agent and bypasses SSH entirely.

## Root cause

The tailnet's ACL grant for `tag:jaco` (or the host identity assigned
to the operator's machine) doesn't include this user as a permitted
SSH client. Tailscale-SSH is opt-in per ACL block; the cloud-init that
runs `tailscale up --ssh` only enables the SSH **server**, not the
permission rule.

## Workaround (in use)

All provisioning commands run via:
```
az vm run-command invoke -g JACO -n jaco-N \
  --command-id RunShellScript \
  --scripts "<inline shell>"
```

Slower (~20-40s per call) than SSH, but unblocks every step.

## Fix (operator action)

In the Tailscale admin console (https://login.tailscale.com/admin/acls),
add a rule along the lines of:

```
{
  "action": "accept",
  "src":    ["autogroup:admin"],
  "dst":    ["tag:jaco"],
  "users":  ["root", "azureuser"]
}
```

(or the specific user identity that needs SSH access). Tailscale-SSH
honors this rule and the `ssh -i ~/.ssh/jaco azureuser@jaco-1` flow
will work afterwards.

## Status

Open. Workaround in place; operator decision when to resolve.
