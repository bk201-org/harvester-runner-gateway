#!/usr/bin/env bash
set -euo pipefail

command -v jq >/dev/null || { echo 'jq is required' >&2; exit 2; }
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
GATEWAY_CLIENT=${GATEWAY_CLIENT:-$script_dir/../bin/hvst-runner-gw-client}
command -v "$GATEWAY_CLIENT" >/dev/null || {
  echo "Gateway client is required: $GATEWAY_CLIENT" >&2
  echo 'Run make build or set GATEWAY_CLIENT to its path.' >&2
  exit 2
}

if [[ "${GITHUB_ACTIONS:-}" == "true" ]]; then
  [[ "${GATEWAY_SMOKE:-}" == "1" ]] || { echo 'Set GATEWAY_SMOKE=1 to run the live smoke test' >&2; exit 2; }
  : "${GATEWAY_URL:?Gateway HTTPS URL is required}"
  : "${GATEWAY_IMAGE:?Approved namespace/name image is required}"
  : "${GATEWAY_NETWORK:?Approved namespace/name network is required}"
  : "${ACTIONS_ID_TOKEN_REQUEST_URL:?GitHub OIDC request URL is required}"
  : "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:?GitHub OIDC request token is required}"
  : "${GITHUB_RUN_ID:?GitHub run ID is required}"
  : "${GITHUB_RUN_ATTEMPT:?GitHub run attempt is required}"
  export GATEWAY_AUDIENCE=${GATEWAY_AUDIENCE:-api://harvester-runner-gateway}
  export GATEWAY_CA_CERT=${GATEWAY_CA_CERT:-}
  unset GATEWAY_TOKEN GATEWAY_TOKEN_FILE
  attempt="$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT"
else
  for dependency in od tr; do
    command -v "$dependency" >/dev/null || { echo "$dependency is required for local smoke runs" >&2; exit 2; }
  done
  config_path=${GATEWAY_SMOKE_CONFIG:-${XDG_CONFIG_HOME:-${HOME:?}/.config}/harvester-runner-gateway/smoke.json}
  [[ -r "$config_path" ]] || { echo "Cannot read local smoke config: $config_path" >&2; exit 2; }

  config_value() {
    jq -er --arg key "$1" '.[$key] | select(type == "string" and length > 0)' "$config_path" || {
      echo "Local smoke config requires $1" >&2
      exit 2
    }
  }

  config_optional_value() {
    jq -er --arg key "$1" \
      'if has($key) then .[$key] | select(type == "string" and length > 0) else "" end' \
      "$config_path" || {
      echo "Local smoke config has invalid $1" >&2
      exit 2
    }
  }

  GATEWAY_URL=$(config_value gatewayURL)
  GATEWAY_IMAGE=$(config_value image)
  GATEWAY_NETWORK=$(config_value network)
  GATEWAY_TOKEN_FILE=$(config_value tokenFile)
  GATEWAY_CA_CERT=$(config_optional_value caCert)
  [[ -r "$GATEWAY_TOKEN_FILE" ]] || { echo "Cannot read local smoke token file: $GATEWAY_TOKEN_FILE" >&2; exit 2; }
  local_token=$(<"$GATEWAY_TOKEN_FILE")
  [[ "$local_token" =~ ^[[:xdigit:]]{64}$ ]] || { echo 'Local smoke token must contain 64 hex characters' >&2; exit 2; }
  unset local_token
  export GATEWAY_TOKEN_FILE GATEWAY_CA_CERT
  unset GATEWAY_TOKEN
  attempt="local-$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')"
fi

[[ "$GATEWAY_URL" == https://* ]] || { echo 'Gateway URL must use HTTPS' >&2; exit 2; }
if [[ -n "$GATEWAY_CA_CERT" ]]; then
  [[ -r "$GATEWAY_CA_CERT" ]] || { echo "Cannot read gateway CA certificate: $GATEWAY_CA_CERT" >&2; exit 2; }
fi
export GATEWAY_URL
GATEWAY_MEMORY=${GATEWAY_MEMORY:-2Gi}
GATEWAY_BOOT_DISK=${GATEWAY_BOOT_DISK:-20Gi}
GATEWAY_VOLUME_SIZE=${GATEWAY_VOLUME_SIZE:-1Gi}
vm_id=''
volume_id=''

cleanup() {
  set +e
  if [[ -n "$vm_id" && -n "$volume_id" ]]; then
    "$GATEWAY_CLIENT" vm detach "$vm_id" "$volume_id" >/dev/null 2>&1
  fi
  if [[ -n "$volume_id" ]]; then "$GATEWAY_CLIENT" volume delete "$volume_id" >/dev/null 2>&1; fi
  if [[ -n "$vm_id" ]]; then "$GATEWAY_CLIENT" vm delete "$vm_id" >/dev/null 2>&1; fi
}
trap cleanup EXIT

wait_for() {
  local kind=$1 id=$2 field=$3 expected=$4 value
  for _ in {1..120}; do
    value=$("$GATEWAY_CLIENT" "$kind" get "$id" | jq -r "$field")
    if [[ "$value" == "$expected" ]]; then return 0; fi
    sleep 5
  done
  echo "Timed out waiting for $kind/$id $field=$expected" >&2
  return 1
}

vm_status=$("$GATEWAY_CLIENT" vm create \
  --image "$GATEWAY_IMAGE" --network "$GATEWAY_NETWORK" \
  --cpu 2 --memory "$GATEWAY_MEMORY" --boot-disk-size "$GATEWAY_BOOT_DISK" \
  --ttl-seconds 3600 --idempotency-key "$attempt-vm")
vm_id=$(jq -er \
  'select(.ready == true and (.ipAddresses | type == "array" and length > 0)) | .id' \
  <<<"$vm_status")
echo "Created VM $vm_id"

volume_id=$("$GATEWAY_CLIENT" volume create \
  --size "$GATEWAY_VOLUME_SIZE" --ttl-seconds 3600 \
  --idempotency-key "$attempt-volume" | jq -er .id)
echo "Created volume $volume_id"
wait_for volume "$volume_id" .phase Bound

"$GATEWAY_CLIENT" vm attach "$vm_id" "$volume_id"
wait_for volume "$volume_id" .attachmentPhase Ready
"$GATEWAY_CLIENT" vm detach "$vm_id" "$volume_id"
wait_for volume "$volume_id" .attachedTo null
"$GATEWAY_CLIENT" volume delete "$volume_id"
volume_id=''

"$GATEWAY_CLIENT" vm power "$vm_id" off
wait_for vm "$vm_id" .powerState off
"$GATEWAY_CLIENT" vm power "$vm_id" on
wait_for vm "$vm_id" .phase Running
"$GATEWAY_CLIENT" vm reboot "$vm_id"
"$GATEWAY_CLIENT" vm delete "$vm_id"
vm_id=''
trap - EXIT
echo 'Gateway smoke test passed'
