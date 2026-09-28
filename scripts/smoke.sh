#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_dir=$(cd -- "$script_dir/.." && pwd)
cd -- "$repo_dir"
export GATEWAY_SMOKE=1
exec go test ./internal/smoke -run '^TestGatewaySmoke$' -count=1 -parallel=2 -v "$@"
