#!/usr/bin/env bash
set -euo pipefail

REMOTE_HOST=${REMOTE_HOST:-ubuntu@remote-host}
PROFILE=${PROFILE:-hal-harvester-ci}
GATEWAY_HOST=${GATEWAY_HOST:-runner-gateway.hal.internal}
GATEWAY_HOST_PORT=${GATEWAY_HOST_PORT:-8443}

TOP_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
DEPLOY_DIR="$TOP_DIR/deploy"
REMOTE_WORK_DIR=runner-gateway

log() {
  printf '[deploy] %s\n' "$*" >&2
}

check_inputs() {
  local file
  for file in \
    "$TOP_DIR/docker-run.sh" \
    "$DEPLOY_DIR/$PROFILE.config.yaml" \
    "$DEPLOY_DIR/$PROFILE.kubeconfig"; do
    if [[ ! -f $file ]]; then
      printf 'Missing deployment file: %s\n' "$file" >&2
      exit 1
    fi
  done
}

create_remote_dirs() {
  log "Creating remote directories under $REMOTE_HOST:$REMOTE_WORK_DIR"
  ssh "$REMOTE_HOST" \
    'umask 077; mkdir -p runner-gateway/tls runner-gateway/harvester runner-gateway/data; chmod 700 runner-gateway runner-gateway/tls runner-gateway/harvester runner-gateway/data'
}

sync_runner_scripts() {
  RUNNER_SCRIPTS_DIR=$(mktemp -d)
  trap 'rm -rf -- "$RUNNER_SCRIPTS_DIR"' EXIT

  cat > "$RUNNER_SCRIPTS_DIR/start.sh" <<'DEPLOY_START_SH'
#!/usr/bin/env bash
set -euo pipefail

WORK_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

exec "$WORK_DIR/docker-run.sh" \
  "$WORK_DIR/config.yaml" \
  "$WORK_DIR/tls" \
  "$WORK_DIR/harvester" \
  "$WORK_DIR/data"
DEPLOY_START_SH

  cat > "$RUNNER_SCRIPTS_DIR/stop.sh" <<'DEPLOY_STOP_SH'
#!/usr/bin/env bash
set -euo pipefail

container_name=${GATEWAY_CONTAINER_NAME:-harvester-runner-gateway}
running_names=$(docker container ls --format '{{.Names}}')

while IFS= read -r running_name; do
  if [[ $running_name == "$container_name" ]]; then
    printf '[stop] Stopping %s\n' "$container_name" >&2
    exec docker container stop "$container_name"
  fi
done <<< "$running_names"

printf '[stop] %s is not running\n' "$container_name" >&2
DEPLOY_STOP_SH

  cat > "$RUNNER_SCRIPTS_DIR/restart.sh" <<'DEPLOY_RESTART_SH'
#!/usr/bin/env bash
set -euo pipefail

WORK_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

printf '[restart] Pulling and replacing the gateway container\n' >&2
exec "$WORK_DIR/start.sh"
DEPLOY_RESTART_SH

  chmod 755 "$RUNNER_SCRIPTS_DIR/"*.sh
  for script in start.sh stop.sh restart.sh; do
    log "Uploading $script"
    rsync -a --chmod=F755 "$RUNNER_SCRIPTS_DIR/$script" \
      "$REMOTE_HOST:$REMOTE_WORK_DIR/"
  done

  rm -rf -- "$RUNNER_SCRIPTS_DIR"
  trap - EXIT
}

sync_files() {
  log "Uploading docker-run.sh"
  rsync -a "$TOP_DIR/docker-run.sh" "$REMOTE_HOST:$REMOTE_WORK_DIR/"
  log "Uploading $PROFILE.config.yaml as config.yaml"
  rsync -a --chmod=F600 "$DEPLOY_DIR/$PROFILE.config.yaml" \
    "$REMOTE_HOST:$REMOTE_WORK_DIR/config.yaml"
  log "Uploading $PROFILE.kubeconfig as harvester/kubeconfig"
  rsync -a --chmod=F600 "$DEPLOY_DIR/$PROFILE.kubeconfig" \
    "$REMOTE_HOST:$REMOTE_WORK_DIR/harvester/kubeconfig"
}

ensure_local_tls() {
  local tls_dir="$DEPLOY_DIR/$PROFILE.tls"

  if [[ -e $tls_dir ]]; then
    if [[ ! -d $tls_dir || ! -s $tls_dir/tls.crt || ! -s $tls_dir/tls.key ]]; then
      printf 'Incomplete local TLS pair: %s\n' "$tls_dir" >&2
      exit 1
    fi
    log "Reusing local TLS pair in $tls_dir"
  else
    log "Generating local TLS pair for $GATEWAY_HOST in $tls_dir"
    TLS_TEMP_DIR=$(mktemp -d "$DEPLOY_DIR/.$PROFILE.tls.XXXXXX")
    trap 'rm -rf -- "$TLS_TEMP_DIR"' EXIT

    openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days 30 \
      -keyout "$TLS_TEMP_DIR/tls.key" -out "$TLS_TEMP_DIR/tls.crt" \
      -subj "/CN=$GATEWAY_HOST" \
      -addext "subjectAltName=DNS:$GATEWAY_HOST"

    mv -- "$TLS_TEMP_DIR" "$tls_dir"
    trap - EXIT
  fi

  chmod 700 "$tls_dir"
  chmod 600 "$tls_dir/tls.crt" "$tls_dir/tls.key"
}

ensure_local_smoke_token() {
  local token_file="$DEPLOY_DIR/local-smoke-token"
  local token_size token

  if [[ -e $token_file || -L $token_file ]]; then
    if [[ ! -f $token_file || -L $token_file ]]; then
      printf 'Invalid local smoke token file: %s\n' "$token_file" >&2
      exit 1
    fi
    log "Reusing shared local smoke token in $token_file"
  else
    log "Generating shared local smoke token in $token_file"
    TOKEN_TEMP_FILE=$(mktemp "$DEPLOY_DIR/.local-smoke-token.XXXXXX")
    trap 'rm -f -- "$TOKEN_TEMP_FILE"' EXIT
    openssl rand -hex 32 > "$TOKEN_TEMP_FILE"
    mv -- "$TOKEN_TEMP_FILE" "$token_file"
    trap - EXIT
  fi

  chmod 600 "$token_file"
  token_size=$(wc -c < "$token_file")
  token=$(<"$token_file")
  if [[ ! $token =~ ^[[:xdigit:]]{64}$ ]] ||
     [[ $token_size != 64 && $token_size != 65 ]]; then
    printf 'Local smoke token must contain 64 hex characters: %s\n' "$token_file" >&2
    exit 1
  fi
}

ensure_local_smoke_config() {
  local smoke_file="$DEPLOY_DIR/$PROFILE.smoke.json"
  local tool

  if [[ -e $smoke_file || -L $smoke_file ]]; then
    if [[ ! -f $smoke_file || -L $smoke_file ]]; then
      printf 'Invalid local smoke config file: %s\n' "$smoke_file" >&2
      exit 1
    fi
    log "Reusing local smoke config in $smoke_file"
  else
    for tool in yq jq; do
      if ! command -v "$tool" >/dev/null 2>&1; then
        printf '%s is required to generate smoke JSON\n' "$tool" >&2
        exit 1
      fi
    done
    if [[ ! $GATEWAY_HOST_PORT =~ ^[0-9]+$ ]] ||
       (( 10#$GATEWAY_HOST_PORT < 1 || 10#$GATEWAY_HOST_PORT > 65535 )); then
      printf 'Invalid GATEWAY_HOST_PORT: %s\n' "$GATEWAY_HOST_PORT" >&2
      exit 1
    fi

    log "Generating local smoke config in $smoke_file"
    SMOKE_TEMP_FILE=$(mktemp "$DEPLOY_DIR/.$PROFILE.smoke.XXXXXX")
    trap 'rm -f -- "$SMOKE_TEMP_FILE"' EXIT

    yq -o=json '.' "$DEPLOY_DIR/$PROFILE.config.yaml" |
      jq -e \
        --arg url "https://$GATEWAY_HOST:$GATEWAY_HOST_PORT" \
        --arg token "$DEPLOY_DIR/local-smoke-token" \
        --arg ca "$DEPLOY_DIR/$PROFILE.tls/tls.crt" \
        '. as $config
         | ($config.localSmoke.repositoryID
            // error("profile config requires localSmoke.repositoryID")) as $repository_id
         | [($config.repositories // [])[]
            | select((.repositoryID | tostring) == ($repository_id | tostring))] as $policies
         | if ($policies | length) != 1
           then error("localSmoke.repositoryID must match one repository policy")
           else $policies[0] end
         | if ((.images[0] | type) != "string" or
               (.networks[0] | type) != "string")
           then error("localSmoke repository policy needs an image and network")
           else {
             gatewayURL: $url,
             image: .images[0],
             network: .networks[0],
             tokenFile: $token,
             caCert: $ca
           } end' > "$SMOKE_TEMP_FILE"

    mv -- "$SMOKE_TEMP_FILE" "$smoke_file"
    trap - EXIT
  fi

  chmod 600 "$smoke_file"
}

sync_tls() {
  local tls_dir="$DEPLOY_DIR/$PROFILE.tls"

  log "Uploading TLS certificate"
  rsync -a --chmod=F600 "$tls_dir/tls.crt" \
    "$REMOTE_HOST:$REMOTE_WORK_DIR/tls/tls.crt"
  log "Uploading TLS private key"
  rsync -a --chmod=F600 "$tls_dir/tls.key" \
    "$REMOTE_HOST:$REMOTE_WORK_DIR/tls/tls.key"
  log "Uploading shared smoke token"
  rsync -a --chmod=F600 "$DEPLOY_DIR/local-smoke-token" \
    "$REMOTE_HOST:$REMOTE_WORK_DIR/tls/local-smoke-token"
}

main() {
  log "Deploying profile $PROFILE to $REMOTE_HOST:$REMOTE_WORK_DIR"
  log "Checking local deployment files"
  check_inputs
  ensure_local_tls
  ensure_local_smoke_token
  ensure_local_smoke_config
  create_remote_dirs
  sync_files
  sync_runner_scripts
  sync_tls
  log "Deployment files are ready on $REMOTE_HOST:$REMOTE_WORK_DIR"
}

main "$@"
