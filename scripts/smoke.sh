#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
smoke_binary=${GATEWAY_SMOKE_BINARY:-$script_dir/../bin/hvst-runner-gw-smoke}
command -v "$smoke_binary" >/dev/null || {
  echo "Gateway smoke runner is required: $smoke_binary" >&2
  echo 'Run make build or set GATEWAY_SMOKE_BINARY to its path.' >&2
  exit 2
}
exec "$smoke_binary" "$@"
