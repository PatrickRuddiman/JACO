#cloud-config
# testbed base cloud-init — MINIMAL by design.
#
# This provisions the neutral 3-node Debian substrate that every orchestrator
# in ../samples bootstraps onto. It deliberately installs NO container runtime
# and NO orchestrator: each stack's bootstrap (samples/<stack>/bootstrap) does
# its own full install from this identical baseline, so "ease of setup" and
# "time-to-ready" are measured fairly — nothing here pre-favors any stack.
#
# Tailscale was removed in v0.1.0: each VM has its own public IP and SSH (22)
# is reachable directly (see template.bicep). There are no secrets to render,
# so deploy.sh passes this file through verbatim (no envsubst). $(...) shell
# expressions are evaluated on the VM at first boot.

package_update: true
package_upgrade: false

packages:
  - curl
  - ca-certificates
  - gnupg
  - jq
  - chrony

# Enable IPv4/IPv6 forwarding up front — every overlay/CNI (JACO WireGuard,
# flannel, swarm VXLAN) needs it, and enabling it here keeps it out of the
# per-stack TTL measurement (it's substrate, not orchestrator setup).
write_files:
  - path: /etc/sysctl.d/99-forwarding.conf
    permissions: '0644'
    content: |
      net.ipv4.ip_forward=1
      net.ipv6.conf.all.forwarding=1

runcmd:
  - sysctl --system
  # Keep clocks tight so cross-node latency/throughput numbers are trustworthy.
  - systemctl enable --now chrony || systemctl enable --now chronyd || true
  # Marker so the operator (and the bench harness) can confirm cloud-init ran.
  - touch /var/lib/cloud/instance/testbed-base-done
