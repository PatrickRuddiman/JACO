#!/usr/bin/env bash
# Deallocate every <prefix>-N VM in the resource group. Deallocated VMs stop
# being billed for compute (storage still costs). Config from testbed/.env.local.

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

echo "[shutdown] deallocating $VM_COUNT VMs in $RESOURCE_GROUP"
for i in $(seq 1 "$VM_COUNT"); do
  vm="${VM_PREFIX}-${i}"
  echo "[shutdown] $vm"
  az vm deallocate --resource-group "$RESOURCE_GROUP" --name "$vm" --no-wait
done

echo "[shutdown] waiting for all VMs to reach 'VM deallocated'"
for i in $(seq 1 "$VM_COUNT"); do
  vm="${VM_PREFIX}-${i}"
  az vm wait \
    --resource-group "$RESOURCE_GROUP" --name "$vm" \
    --custom "instanceView.statuses[?code=='PowerState/deallocated']" \
    --interval 5 --timeout 600
  echo "[shutdown] $vm deallocated"
done

echo "[shutdown] done."
