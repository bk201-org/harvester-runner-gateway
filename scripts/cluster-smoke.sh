#!/usr/bin/env bash
set -euo pipefail

repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
if [[ -n "${GATEWAY_SMOKE_CONFIG:-}" ]]; then
  smoke_config=$GATEWAY_SMOKE_CONFIG
else
  config_home=${XDG_CONFIG_HOME:-${HOME:?HOME is required when GATEWAY_SMOKE_CONFIG is unset}/.config}
  smoke_config="$config_home/harvester-runner-gateway/smoke.json"
fi
if [[ ! -r "$smoke_config" ]]; then
  echo "Cannot read smoke config: $smoke_config" >&2
  exit 2
fi

gateway_url=$(jq -er '.gatewayURL | strings | select(length > 0)' "$smoke_config")
token_file=$(jq -er '.tokenFile | strings | select(length > 0)' "$smoke_config")
image=$(jq -er '.image | strings | select(length > 0)' "$smoke_config")
network=$(jq -er '.network | strings | select(length > 0)' "$smoke_config")
ca_cert=$(jq -er '.caCert // "" | strings' "$smoke_config")
if [[ ! -r "$token_file" || ( -n "$ca_cert" && ! -r "$ca_cert" ) ]]; then
  echo "Cannot read smoke token or CA certificate file" >&2
  exit 2
fi

temp_dir=$(mktemp -d)
chmod 700 "$temp_dir"
state_file="$temp_dir/github_state"
output_file="$temp_dir/github_output"
binary="$temp_dir/hvst-runner-gw-cluster"
: > "$state_file"
: > "$output_file"
chmod 600 "$state_file" "$output_file"

common_env=(
  "GITHUB_ACTIONS=false"
  "GATEWAY_TOKEN_FILE=$token_file"
  "RUNNER_TEMP=$temp_dir"
  "INPUT_GATEWAY-URL=$gateway_url"
  "INPUT_AUDIENCE=${GATEWAY_AUDIENCE:-api://harvester-runner-gateway}"
  "INPUT_CA-CERT-PATH=$ca_cert"
)

cleanup() {
  local result=$?
  trap - EXIT
  local cluster_state
  cluster_state=$(sed -n 's/^cluster_state=//p' "$state_file" | tail -n 1)
  if [[ -n "$cluster_state" ]]; then
    if ! env "${common_env[@]}" "STATE_cluster_state=$cluster_state" "$binary" cleanup; then
      echo "Cluster cleanup failed" >&2
      result=1
    fi
  fi
  rm -rf -- "$temp_dir"
  if [[ "$result" -eq 0 && -n "${vm_ids:-}" ]]; then
    echo "Cluster command smoke test passed for VM IDs: $vm_ids"
  fi
  exit "$result"
}
trap cleanup EXIT

if [[ -n "${CLUSTER_SMOKE_BINARY:-}" ]]; then
  cp -- "$CLUSTER_SMOKE_BINARY" "$binary"
  chmod 700 "$binary"
else
  (cd "$repo_dir" && CGO_ENABLED=0 go build -trimpath -o "$binary" ./cmd/hvst-runner-gw-cluster)
fi

user_data=""
if [[ -n "${GATEWAY_USER_DATA_FILE:-}" ]]; then
  user_data=$(cat -- "$GATEWAY_USER_DATA_FILE")
fi

vm_count=${GATEWAY_VM_COUNT:-1}
env "${common_env[@]}" \
  "GITHUB_STATE=$state_file" \
  "GITHUB_OUTPUT=$output_file" \
  "INPUT_VM-COUNT=$vm_count" \
  "INPUT_IMAGE=$image" \
  "INPUT_NETWORK=$network" \
  "INPUT_CPU=${GATEWAY_CPU:-2}" \
  "INPUT_MEMORY=${GATEWAY_MEMORY:-4Gi}" \
  "INPUT_BOOT-DISK-SIZE=${GATEWAY_BOOT_DISK_SIZE:-20Gi}" \
  "INPUT_USERNAME=${GATEWAY_USERNAME:-ubuntu}" \
  "INPUT_TTL-SECONDS=${GATEWAY_TTL_SECONDS:-1800}" \
  "INPUT_WAIT-TIMEOUT-SECONDS=${GATEWAY_WAIT_TIMEOUT_SECONDS:-600}" \
  "INPUT_USER-DATA=$user_data" \
  "$binary" create

cluster_state=$(sed -n 's/^cluster_state=//p' "$state_file" | tail -n 1)
[[ -n "$cluster_state" && -f "$cluster_state" ]]
vm_ids=$(sed -n 's/^vm-ids=//p' "$output_file" | tail -n 1)
ssh_config=$(sed -n 's/^ssh-config-path=//p' "$output_file" | tail -n 1)
private_key=$(sed -n 's/^private-key-path=//p' "$output_file" | tail -n 1)
jq -e --argjson expected "$vm_count" \
  'type == "array" and length == $expected and all(.[]; type == "string")' \
  <<< "$vm_ids" >/dev/null
[[ -f "$ssh_config" && -f "$private_key" ]]
ssh-keygen -y -f "$private_key" >/dev/null
while IFS= read -r vm_id; do
  ssh -G -F "$ssh_config" "$vm_id" >/dev/null
done < <(jq -r '.[]' <<< "$vm_ids")
