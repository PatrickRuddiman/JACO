#!/usr/bin/env bash
# k3s adapter — STUB. Implemented in the follow-up pass.
# Will: `curl -sfL https://get.k3s.io | sh` on node-1 (server), join node-2/3
# as agents with the node-token, apply ../../k3s/manifests (Traefik ships with
# k3s and fronts 80/443 behind the LB).

adapter_label() { echo "k3s"; }
adapter_deploy()   { die "k3s adapter not implemented yet (see samples/k3s/README.md)"; }
adapter_collect()  { :; }
adapter_teardown() { :; }
