#!/usr/bin/env bash
# collect.sh — snapshot in-cluster resource utilization and bundle a run for
# download. Run after ./run.sh (or it auto-picks the most recent run dir).
#
#   ./collect.sh [results/<stack>-<ts>]
#
# Captures `docker stats` from every node (CPU/mem per container) into the run
# dir, then tars the whole dir to results/<name>.tar.gz. Node resource stats
# are kept node-agnostic (same command everywhere) so they don't bias any stack.
set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib/common.sh"

RUNDIR="${1:-}"
if [[ -z "$RUNDIR" ]]; then
  RUNDIR="$(ls -dt "$RESULTS_DIR"/*/ 2>/dev/null | head -1)"
  [[ -n "$RUNDIR" ]] || die "no run dirs under $RESULTS_DIR — run ./run.sh first"
fi
RUNDIR="${RUNDIR%/}"
[[ -d "$RUNDIR" ]] || die "no such run dir: $RUNDIR"

resolve_nodes

log "snapshotting docker stats across ${#PUB[@]} nodes into $RUNDIR"
for i in "${!PUB[@]}"; do
  node="node$((i+1))"
  ssh_node "${PUB[$i]}" \
    "docker stats --no-stream --format '{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}'" \
    >"$RUNDIR/docker-stats-$node.txt" 2>/dev/null \
    || log "WARN: could not collect docker stats from $node (stack may not use docker)"
done

bundle="$RESULTS_DIR/$(basename "$RUNDIR").tar.gz"
tar -czf "$bundle" -C "$RESULTS_DIR" "$(basename "$RUNDIR")"
log "bundled: $bundle"
log "contents:"; tar -tzf "$bundle" | sed 's/^/  /' >&2
