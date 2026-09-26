#!/usr/bin/env bash
set -euo pipefail

for dependency in curl jq; do
  command -v "$dependency" >/dev/null || { echo "$dependency is required" >&2; exit 2; }
done

if [[ "${GITHUB_ACTIONS:-}" == "true" ]]; then
  [[ "${GATEWAY_SMOKE:-}" == "1" ]] || { echo 'Set GATEWAY_SMOKE=1 to run the live smoke test' >&2; exit 2; }
  : "${GATEWAY_URL:?Gateway HTTPS URL is required}"
  : "${GATEWAY_IMAGE:?Approved namespace/name image is required}"
  : "${GATEWAY_NETWORK:?Approved namespace/name network is required}"
  : "${ACTIONS_ID_TOKEN_REQUEST_URL:?GitHub OIDC request URL is required}"
  : "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:?GitHub OIDC request token is required}"
  : "${GITHUB_RUN_ID:?GitHub run ID is required}"
  : "${GITHUB_RUN_ATTEMPT:?GitHub run attempt is required}"
  GATEWAY_AUDIENCE=${GATEWAY_AUDIENCE:-api://harvester-runner-gateway}
  attempt="$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT"

  token() {
    local encoded
    encoded=$(printf '%s' "$GATEWAY_AUDIENCE" | jq -sRr @uri)
    curl --fail --silent --show-error \
      -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
      "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=$encoded" | jq -er .value
  }
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

  GATEWAY_URL=$(config_value gatewayURL)
  GATEWAY_IMAGE=$(config_value image)
  GATEWAY_NETWORK=$(config_value network)
  token_file=$(config_value tokenFile)
  [[ -r "$token_file" ]] || { echo "Cannot read local smoke token file: $token_file" >&2; exit 2; }
  local_token=$(<"$token_file")
  [[ "$local_token" =~ ^[[:xdigit:]]{64}$ ]] || { echo 'Local smoke token must contain 64 hex characters' >&2; exit 2; }
  attempt="local-$(od -An -tx1 -N16 /dev/urandom | tr -d ' \n')"

  token() { printf '%s\n' "$local_token"; }
fi

[[ "$GATEWAY_URL" == https://* ]] || { echo 'Gateway URL must use HTTPS' >&2; exit 2; }
GATEWAY_URL=${GATEWAY_URL%/}
GATEWAY_MEMORY=${GATEWAY_MEMORY:-2Gi}
GATEWAY_BOOT_DISK=${GATEWAY_BOOT_DISK:-20Gi}
GATEWAY_VOLUME_SIZE=${GATEWAY_VOLUME_SIZE:-1Gi}
vm_id=''
volume_id=''

api() {
  local method=$1 path=$2 body=${3:-} key=${4:-} jwt
  jwt=$(token)
  local args=(--fail-with-body --silent --show-error -X "$method")
  if [[ -n "$body" ]]; then args+=(-H 'Content-Type: application/json' --data "$body"); fi
  if [[ -n "$key" ]]; then args+=(-H "Idempotency-Key: $key"); fi
  # Passing the header on stdin keeps the long-lived local token out of curl's argv.
  printf 'header = "Authorization: Bearer %s"\n' "$jwt" |
    curl --config - "${args[@]}" "$GATEWAY_URL$path"
}

cleanup() {
  set +e
  if [[ -n "$vm_id" && -n "$volume_id" ]]; then
    api DELETE "/v1/vms/$vm_id/volumes/$volume_id" >/dev/null 2>&1
  fi
  if [[ -n "$volume_id" ]]; then api DELETE "/v1/volumes/$volume_id" >/dev/null 2>&1; fi
  if [[ -n "$vm_id" ]]; then api DELETE "/v1/vms/$vm_id" >/dev/null 2>&1; fi
}
trap cleanup EXIT

wait_for() {
  local kind=$1 id=$2 field=$3 expected=$4 value
  for _ in {1..120}; do
    value=$(api GET "/v1/$kind/$id" | jq -r "$field")
    if [[ "$value" == "$expected" ]]; then return 0; fi
    sleep 5
  done
  echo "Timed out waiting for $kind/$id $field=$expected" >&2
  return 1
}

vm_request=$(jq -n --arg image "$GATEWAY_IMAGE" --arg network "$GATEWAY_NETWORK" \
  --arg memory "$GATEWAY_MEMORY" --arg disk "$GATEWAY_BOOT_DISK" \
  '{image:$image,network:$network,cpu:2,memory:$memory,bootDiskSize:$disk,ttlSeconds:3600}')
vm_id=$(api POST /v1/vms "$vm_request" "$attempt-vm" | jq -er .id)
echo "Created VM $vm_id"
wait_for vms "$vm_id" .phase Running

volume_request=$(jq -n --arg size "$GATEWAY_VOLUME_SIZE" '{size:$size,ttlSeconds:3600}')
volume_id=$(api POST /v1/volumes "$volume_request" "$attempt-volume" | jq -er .id)
echo "Created volume $volume_id"
wait_for volumes "$volume_id" .phase Bound

api PUT "/v1/vms/$vm_id/volumes/$volume_id" >/dev/null
wait_for volumes "$volume_id" .attachmentPhase Ready
api DELETE "/v1/vms/$vm_id/volumes/$volume_id" >/dev/null
wait_for volumes "$volume_id" .attachedTo null
api DELETE "/v1/volumes/$volume_id" >/dev/null
volume_id=''

api PUT "/v1/vms/$vm_id/power" '{"state":"off"}' >/dev/null
wait_for vms "$vm_id" .powerState off
api PUT "/v1/vms/$vm_id/power" '{"state":"on"}' >/dev/null
wait_for vms "$vm_id" .phase Running
api POST "/v1/vms/$vm_id/reboot" >/dev/null
api DELETE "/v1/vms/$vm_id" >/dev/null
vm_id=''
trap - EXIT
echo 'Gateway smoke test passed'
