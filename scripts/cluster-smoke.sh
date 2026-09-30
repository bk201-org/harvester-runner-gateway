#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: cluster-smoke.sh [--no-cleanup | --cleanup TEMP_DIR]

  --no-cleanup       Keep created VMs and print a manual cleanup command.
  --cleanup TEMP_DIR Delete VMs saved by a previous --no-cleanup run.
EOF
}

no_cleanup=false
cleanup_dir=""
if [[ $# -eq 1 && ${1:-} == --no-cleanup ]]; then
  no_cleanup=true
elif [[ $# -eq 2 && ${1:-} == --cleanup && -n $2 ]]; then
  cleanup_dir=$2
elif [[ $# -eq 1 && ( ${1:-} == --help || ${1:-} == -h ) ]]; then
  usage
  exit 0
elif [[ $# -ne 0 ]]; then
  usage >&2
  exit 2
fi

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

if [[ -n "$cleanup_dir" ]]; then
  temp_dir=$(cd -- "$cleanup_dir" && pwd)
else
  temp_dir=$(mktemp -d)
  chmod 700 "$temp_dir"
fi
state_file="$temp_dir/github_state"
output_file="$temp_dir/github_output"
binary="$temp_dir/hvst-runner-gw-client"

common_env=(
  "GITHUB_ACTIONS=false"
  "GATEWAY_TOKEN_FILE=$token_file"
  "RUNNER_TEMP=$temp_dir"
  "INPUT_GATEWAY-URL=$gateway_url"
  "INPUT_AUDIENCE=${GATEWAY_AUDIENCE:-api://harvester-runner-gateway}"
  "INPUT_CA-CERT-PATH=$ca_cert"
)

cluster_state_path() {
  sed -n 's/^cluster_state=//p' "$state_file" | tail -n 1
}

run_cluster_cleanup() {
  local cluster_state=$1
  local backup="$temp_dir/cluster_state_backup"
  if ! cp -- "$cluster_state" "$backup"; then
    return 1
  fi
  if env "${common_env[@]}" "STATE_cluster_state=$cluster_state" "$binary" action cleanup; then
    rm -f -- "$backup"
    return 0
  fi
  if [[ ! -f "$cluster_state" ]]; then
    mkdir -p -- "$(dirname -- "$cluster_state")"
    chmod 700 -- "$(dirname -- "$cluster_state")"
    cp -- "$backup" "$cluster_state"
    chmod 600 -- "$cluster_state"
  fi
  return 1
}

show_manual_cleanup() {
  printf 'Saved cluster files: %s\n' "$temp_dir"
  printf 'To delete the VMs and saved files, run:\n'
  printf '  (cd %q && GATEWAY_SMOKE_CONFIG=%q %q --cleanup %q)\n' \
    "$PWD" "$smoke_config" "$repo_dir/scripts/cluster-smoke.sh" "$temp_dir"
}

if [[ -n "$cleanup_dir" ]]; then
  if [[ ! -f "$state_file" || ! -x "$binary" ]]; then
    echo "Cannot find saved cluster state and executable in $temp_dir" >&2
    exit 2
  fi
  cluster_state=$(cluster_state_path)
  if [[ -z "$cluster_state" ]]; then
    echo "No cluster state recorded in $state_file" >&2
    exit 2
  fi
  printf 'Cleaning up saved cluster in %s\n' "$temp_dir"
  run_cluster_cleanup "$cluster_state"
  rm -rf -- "$temp_dir"
  echo "Cluster cleanup complete"
  exit 0
fi

: > "$state_file"
: > "$output_file"
chmod 600 "$state_file" "$output_file"

cleanup() {
  local result=$?
  trap - EXIT
  local cluster_state
  cluster_state=$(cluster_state_path)
  if [[ -n "$cluster_state" ]]; then
    if [[ "$no_cleanup" == true ]]; then
      echo "Leaving the cluster running (--no-cleanup)."
      show_manual_cleanup
    else
      echo "Cleaning up cluster..."
      if run_cluster_cleanup "$cluster_state"; then
        rm -rf -- "$temp_dir"
        echo "Cluster cleanup complete"
      else
        echo "Cluster cleanup failed; saved files are available for retry." >&2
        show_manual_cleanup
        result=1
      fi
    fi
  else
    rm -rf -- "$temp_dir"
  fi
  if [[ "$result" -eq 0 && -n "${vm_ids:-}" ]]; then
    echo "Cluster command smoke test passed for VM IDs: $vm_ids"
  fi
  exit "$result"
}
trap cleanup EXIT

printf 'Using smoke config: %s\n' "$smoke_config"
if [[ -n "${CLUSTER_SMOKE_BINARY:-}" ]]; then
  printf 'Using client executable: %s\n' "$CLUSTER_SMOKE_BINARY"
  cp -- "$CLUSTER_SMOKE_BINARY" "$binary"
  chmod 700 "$binary"
else
  echo "Building client executable..."
  (cd "$repo_dir" && CGO_ENABLED=0 go build -trimpath -o "$binary" ./cmd/hvst-runner-gw-client)
fi

user_data=""
if [[ -n "${GATEWAY_USER_DATA_FILE:-}" ]]; then
  user_data=$(cat -- "$GATEWAY_USER_DATA_FILE")
fi

vm_count=${GATEWAY_VM_COUNT:-1}
printf 'Creating %s VM(s) and waiting for readiness...\n' "$vm_count"
env "${common_env[@]}" \
  "GITHUB_STATE=$state_file" \
  "GITHUB_OUTPUT=$output_file" \
  "INPUT_VM-COUNT=$vm_count" \
  "INPUT_IMAGE=$image" \
  "INPUT_NETWORK=$network" \
  "INPUT_CPU=${GATEWAY_CPU:-2}" \
  "INPUT_MEMORY=${GATEWAY_MEMORY:-1Gi}" \
  "INPUT_BOOT-DISK-SIZE=${GATEWAY_BOOT_DISK_SIZE:-20Gi}" \
  "INPUT_USERNAME=${GATEWAY_USERNAME:-ubuntu}" \
  "INPUT_TTL-SECONDS=${GATEWAY_TTL_SECONDS:-1800}" \
  "INPUT_WAIT-TIMEOUT-SECONDS=${GATEWAY_WAIT_TIMEOUT_SECONDS:-600}" \
  "INPUT_USER-DATA=$user_data" \
  "$binary" action create

echo "Validating cluster state and command outputs..."
cluster_state=$(cluster_state_path)
[[ -n "$cluster_state" && -f "$cluster_state" ]]
vm_ids=$(sed -n 's/^vm-ids=//p' "$output_file" | tail -n 1)
ssh_config=$(sed -n 's/^ssh-config-path=//p' "$output_file" | tail -n 1)
private_key=$(sed -n 's/^private-key-path=//p' "$output_file" | tail -n 1)
jq -e --argjson expected "$vm_count" \
  'type == "array" and length == $expected and all(.[]; type == "string")' \
  <<< "$vm_ids" >/dev/null
[[ -f "$ssh_config" && -f "$private_key" ]]
ssh-keygen -y -f "$private_key" >/dev/null
printf 'VM IDs: %s\nSSH config: %s\nPrivate key: %s\n' "$vm_ids" "$ssh_config" "$private_key"
while IFS= read -r vm_id; do
  printf 'Checking SSH configuration for %s...\n' "$vm_id"
  ssh -T -G -F "$ssh_config" "$vm_id" >/dev/null
done < <(jq -r '.[]' <<< "$vm_ids")
echo "Cluster state, key, and SSH configuration are valid."
if [[ "$no_cleanup" == true ]]; then
  while IFS= read -r vm_id; do
    printf 'Connect with: ssh -F %q %q\n' "$ssh_config" "$vm_id"
  done < <(jq -r '.[]' <<< "$vm_ids")
fi
