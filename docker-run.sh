#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage: ./docker-run.sh CONFIG_FILE TLS_DIR HARVESTER_DIR DATA_DIR

All four paths must be absolute. CONFIG_FILE is the gateway YAML config.
TLS_DIR contains tls.crt and tls.key; HARVESTER_DIR contains kubeconfig.
DATA_DIR persists the SQLite database and must be writable by the image's
runtime user. The paths inside the container match config.example.yaml.
Run this script as the non-root host user that owns the mounted files. If you
use sudo for Docker access, the container still runs as the original user.

Build the image first with: make docker-build

Optional environment variables:
  GATEWAY_IMAGE            Image to run (default: harvester-runner-gateway:dev)
  GATEWAY_CONTAINER_NAME   Container name (default: harvester-runner-gateway)
  GATEWAY_HOST_PORT        Host HTTPS port (default: 8443)
EOF
}

if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  usage
  exit 0
fi

if (( $# != 4 )); then
  usage >&2
  exit 2
fi

config_file=$1
tls_dir=$2
harvester_dir=$3
data_dir=$4

for path in "$config_file" "$tls_dir" "$harvester_dir" "$data_dir"; do
  if [[ $path != /* ]]; then
    printf 'Path must be absolute: %s\n' "$path" >&2
    exit 2
  fi
done

if [[ ! -f $config_file ]]; then
  printf 'Config file does not exist: %s\n' "$config_file" >&2
  exit 2
fi
for dir in "$tls_dir" "$harvester_dir" "$data_dir"; do
  if [[ ! -d $dir ]]; then
    printf 'Directory does not exist: %s\n' "$dir" >&2
    exit 2
  fi
done

image=${GATEWAY_IMAGE:-harvester-runner-gateway:dev}
container_name=${GATEWAY_CONTAINER_NAME:-harvester-runner-gateway}
host_port=${GATEWAY_HOST_PORT:-8443}

if (( EUID == 0 )); then
  if [[ ! ${SUDO_UID:-} =~ ^[1-9][0-9]*$ || ! ${SUDO_GID:-} =~ ^[0-9]+$ ]]; then
    printf 'Run as a non-root user, directly or through sudo.\n' >&2
    exit 2
  fi
  run_uid=$SUDO_UID
  run_gid=$SUDO_GID
else
  run_uid=$(id -u)
  run_gid=$(id -g)
fi

exec docker run -d --name "$container_name" \
  --restart unless-stopped \
  --user "$run_uid:$run_gid" \
  --workdir / \
  -p "$host_port:8443" \
  --mount "type=bind,src=$config_file,dst=/etc/gateway/config.yaml,readonly" \
  --mount "type=bind,src=$tls_dir,dst=/run/secrets/gateway,readonly" \
  --mount "type=bind,src=$harvester_dir,dst=/run/secrets/harvester,readonly" \
  --mount "type=bind,src=$data_dir,dst=/var/lib/harvester-runner-gateway" \
  "$image" --config /etc/gateway/config.yaml
