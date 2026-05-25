#!/usr/bin/env bash
# Start every <prefix>-N VM in the resource group. Idempotent — already-running
# VMs are a no-op. Config from testbed/.env.local.

set -euo pipefail

INFRA_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ -f "$INFRA_DIR/.env.local" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "$INFRA_DIR/.env.local"
  set +a
fi

: "${AZ_SUBSCRIPTION:?AZ_SUBSCRIPTION required — set in testbed/.env.local}"
RESOURCE_GROUP="${RESOURCE_GROUP:-JACO}"
VM_PREFIX="${VM_NAME_PREFIX:-jaco}"
VM_COUNT="${VM_COUNT:-3}"

az account set --subscription "$AZ_SUBSCRIPTION"

echo "[startup] starting $VM_COUNT VMs in $RESOURCE_GROUP"
for i in $(seq 1 "$VM_COUNT"); do
  vm="${VM_PREFIX}-${i}"
  echo "[startup] $vm"
  az vm start --resource-group "$RESOURCE_GROUP" --name "$vm" --no-wait
done

echo "[startup] waiting for all VMs to reach 'VM running'"
for i in $(seq 1 "$VM_COUNT"); do
  vm="${VM_PREFIX}-${i}"
  az vm wait \
    --resource-group "$RESOURCE_GROUP" --name "$vm" \
    --custom "instanceView.statuses[?code=='PowerState/running']" \
    --interval 5 --timeout 600
  echo "[startup] $vm running"
done

echo "[startup] done. Each VM keeps its public IP across deallocation (Static SKU)."
