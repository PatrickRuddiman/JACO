#!/usr/bin/env bash
# Delete the resource group containing the testbed VMs. Destroys VMs, disks,
# NICs, per-VM public IPs, NSG, VNet, and the load balancer.
#
# The persistent LB public IP lives in its OWN resource group ($PUBLIC_IP_RG)
# which this script deliberately does NOT delete — that keeps the jaco.sh A
# record valid across rebuilds. Pass --purge-ip to also delete that RG.
#
# Pass --yes to skip the confirmation prompt (for unattended use).
#
# Config from testbed/.env.local: AZ_SUBSCRIPTION (required), RESOURCE_GROUP.

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
PUBLIC_IP_RG="jaco-net" # persistent IP RG — preserved unless --purge-ip

SKIP_CONFIRM=0
PURGE_IP=0
for arg in "$@"; do
  case "$arg" in
    --yes|-y) SKIP_CONFIRM=1 ;;
    --purge-ip) PURGE_IP=1 ;;
    *) echo "teardown.sh: unknown arg $arg" >&2; exit 1 ;;
  esac
done

az account set --subscription "$AZ_SUBSCRIPTION"

if [[ "$SKIP_CONFIRM" != "1" ]]; then
  echo "[teardown] About to DELETE resource group: $RESOURCE_GROUP"
  echo "[teardown] This destroys every VM + disk + NIC + per-VM PIP + NSG + VNet in it."
  read -r -p "[teardown] Type the resource group name to confirm: " confirm
  if [[ "$confirm" != "$RESOURCE_GROUP" ]]; then
    echo "[teardown] aborted (name did not match)"
    exit 1
  fi
fi

echo "[teardown] deleting $RESOURCE_GROUP (async)"
az group delete --name "$RESOURCE_GROUP" --yes --no-wait

if [[ "$PURGE_IP" == "1" ]]; then
  echo "[teardown] --purge-ip: also deleting persistent IP RG $PUBLIC_IP_RG (async)"
  az group delete --name "$PUBLIC_IP_RG" --yes --no-wait
else
  echo "[teardown] preserving persistent LB public IP in RG $PUBLIC_IP_RG (jaco.sh A record stays valid)"
fi

# Drop the ephemeral bed SSH key — the VMs are gone, so the next build mints a
# fresh keypair (deploy.sh).
if [[ -f "$INFRA_DIR/.ssh/jaco" ]]; then
  rm -f "$INFRA_DIR/.ssh/jaco" "$INFRA_DIR/.ssh/jaco.pub"
  echo "[teardown] removed ephemeral SSH key ($INFRA_DIR/.ssh/jaco)"
fi

echo "[teardown] initiated. Monitor with:"
echo "  az group show --name $RESOURCE_GROUP --query properties.provisioningState -o tsv"
