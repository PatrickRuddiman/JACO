#!/usr/bin/env bash
# Deploy the benchmark testbed: N identical Debian VMs, each with its own public
# IP (direct SSH), behind a persistent Standard LB for 80/443 ingress.
#
# Config comes from testbed/.env.local (gitignored; see .env.local.example):
#   AZ_SUBSCRIPTION   Azure subscription id            (required)
#   AZ_TENANT         Azure tenant id                  (required for az login)
#   ADMIN_PUBLIC_KEY  SSH public key (OpenSSH one-line) (required)
#   RESOURCE_GROUP    main RG, deleted on teardown     (default: JACO)
#   LOCATION          Azure region                     (default: centralus)
#   SSH_SOURCE_CIDR   restrict SSH source              (default: Internet)
#   SHUTDOWN_NOTIFICATION_EMAIL                         (optional)
#   VM_NAME_PREFIX    override prefix for a parallel bed (optional)
#   DEPLOYMENT_NAME   override deployment name         (default: timestamped)

set -euo pipefail

INFRA_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$INFRA_DIR"

if [[ -f .env.local ]]; then
  set -a
  # shellcheck disable=SC1091
  source ./.env.local
  set +a
fi

: "${AZ_SUBSCRIPTION:?AZ_SUBSCRIPTION required — set in testbed/.env.local (see .env.local.example)}"
: "${ADMIN_PUBLIC_KEY:?ADMIN_PUBLIC_KEY required — set in testbed/.env.local (see .env.local.example)}"

AZ_TENANT="${AZ_TENANT:-}"
RESOURCE_GROUP="${RESOURCE_GROUP:-JACO}"
LOCATION="${LOCATION:-centralus}"
DEPLOYMENT_NAME="${DEPLOYMENT_NAME:-testbed-deploy-$(date -u +%Y%m%dT%H%M%S)}"

echo "[deploy] subscription=$AZ_SUBSCRIPTION resource_group=$RESOURCE_GROUP location=$LOCATION"

if ! az account show >/dev/null 2>&1; then
  echo "[deploy] az not logged in — running az login"
  if [[ -n "$AZ_TENANT" ]]; then az login --tenant "$AZ_TENANT"; else az login; fi
fi
az account set --subscription "$AZ_SUBSCRIPTION"

echo "[deploy] ensuring resource group exists"
az group create --name "$RESOURCE_GROUP" --location "$LOCATION" --output none

# Persistent LB public IP — lives in its OWN resource group so teardown.sh
# (which deletes only $RESOURCE_GROUP) never destroys it. The address survives
# every rebuild and the jaco.sh A record stays valid. Idempotent. These names
# MUST match template.bicep's publicIpResourceGroup / publicIpName defaults.
PUBLIC_IP_RG="jaco-net"
PUBLIC_IP_NAME="jaco-lb-pip"
echo "[deploy] ensuring persistent LB public IP $PUBLIC_IP_NAME in $PUBLIC_IP_RG"
az group create --name "$PUBLIC_IP_RG" --location "$LOCATION" --output none
az network public-ip create \
  --resource-group "$PUBLIC_IP_RG" --name "$PUBLIC_IP_NAME" \
  --sku Standard --allocation-method Static --version IPv4 --output none
PUBLIC_IP_ADDR="$(az network public-ip show -g "$PUBLIC_IP_RG" -n "$PUBLIC_IP_NAME" --query ipAddress -o tsv)"
echo "[deploy] persistent LB public IP: $PUBLIC_IP_ADDR  <-- point the jaco.sh A record here (once)"

# cloud-init carries no secrets now (Tailscale is gone), so just base64 it.
# Exported (not passed on the CLI) so the blob stays out of `ps aux`.
echo "[deploy] rendering cloud-init.yaml.tpl"
export CUSTOM_DATA="$(base64 -w0 < cloud-init.yaml.tpl)"

echo "[deploy] starting deployment: $DEPLOYMENT_NAME"
EXTRA_PARAMS=()
if [[ -n "${VM_NAME_PREFIX:-}" ]]; then
  echo "[deploy] overriding vmNamePrefix=$VM_NAME_PREFIX"
  EXTRA_PARAMS+=(--parameters "vmNamePrefix=$VM_NAME_PREFIX")
fi
az deployment group create \
  --resource-group "$RESOURCE_GROUP" \
  --name "$DEPLOYMENT_NAME" \
  --parameters parameters.bicepparam \
  "${EXTRA_PARAMS[@]}" \
  --output table

unset CUSTOM_DATA

echo
echo "[deploy] complete: $DEPLOYMENT_NAME"
echo "[deploy] per-VM public IPs (SSH directly to each node):"
az deployment group show \
  --resource-group "$RESOURCE_GROUP" --name "$DEPLOYMENT_NAME" \
  --query 'properties.outputs' -o json \
  | jq -r '
      (.vmNames.value) as $n | (.vmPublicIps.value) as $ip
      | range(0; ($n|length)) | "  \($n[.])  ssh azureuser@\($ip[.])"'

echo
echo "[deploy] LB ingress IP (80/443, jaco.sh): $PUBLIC_IP_ADDR"
echo "[deploy] Base cloud-init only installs OS prep — bootstrap a stack with"
echo "[deploy]   ../samples/<stack>/bootstrap, or run the bench: ../samples/bench/run.sh <stack>"
