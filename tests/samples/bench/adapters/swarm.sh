#!/usr/bin/env bash
# Docker Swarm adapter — STUB. Implemented in the follow-up pass.
# Will: `docker swarm init` on node-1, join node-2/3 as workers with the
# join-token, `docker stack deploy` ../../swarm/stack.yml. Swarm's routing mesh
# already publishes 80/443 on every node behind the LB.

adapter_label() { echo "Docker Swarm"; }
adapter_deploy()   { die "swarm adapter not implemented yet (see samples/swarm/README.md)"; }
adapter_collect()  { :; }
adapter_teardown() { :; }
