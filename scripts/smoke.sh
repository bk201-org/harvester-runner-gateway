#!/usr/bin/env bash
set -euo pipefail

# Run only from an explicitly opted-in GitHub Actions job with id-token: write.
[[ "${GATEWAY_SMOKE:-}" == "1" ]] || { echo 'Set GATEWAY_SMOKE=1 to run the live smoke test' >&2; exit 2; }
: "${GATEWAY_URL:?Gateway HTTPS URL is required}"
: "${GATEWAY_IMAGE:?Approved namespace/name image is required}"
: "${GATEWAY_NETWORK:?Approved namespace/name network is required}"
: "${ACTIONS_ID_TOKEN_REQUEST_URL:?GitHub OIDC request URL is required}"
: "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:?GitHub OIDC request token is required}"

GATEWAY_URL=${GATEWAY_URL%/}
GATEWAY_AUDIENCE=${GATEWAY_AUDIENCE:-api://harvester-runner-gateway}
GATEWAY_MEMORY=${GATEWAY_MEMORY:-2Gi}
GATEWAY_BOOT_DISK=${GATEWAY_BOOT_DISK:-20Gi}
GATEWAY_VOLUME_SIZE=${GATEWAY_VOLUME_SIZE:-1Gi}
attempt="${GITHUB_RUN_ID:-manual}-${GITHUB_RUN_ATTEMPT:-1}"
vm_id=''
volume_id=''

token() {
  local encoded
  encoded=$(printf '%s' "$GATEWAY_AUDIENCE" | jq -sRr @uri)
  curl --fail --silent --show-error \
    -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
    "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=$encoded" | jq -er .value
}

api() {
  local method=$1 path=$2 body=${3:-} key=${4:-} jwt
  jwt=$(token)
  local args=(--fail-with-body --silent --show-error -X "$method" \
    -H "Authorization: Bearer $jwt")
  if [[ -n "$body" ]]; then args+=(-H 'Content-Type: application/json' --data "$body"); fi
  if [[ -n "$key" ]]; then args+=(-H "Idempotency-Key: $key"); fi
  curl "${args[@]}" "$GATEWAY_URL$path"
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
