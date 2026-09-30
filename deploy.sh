#!/usr/bin/env bash
set -euo pipefail

REMOTE_HOST=${REMOTE_HOST:-ubuntu@remote-host}
PROFILE=${PROFILE:-hal-harvester-ci}
GATEWAY_HOST=${GATEWAY_HOST:-runner-gateway.hal.internal}
GATEWAY_HOST_PORT=${GATEWAY_HOST_PORT:-8443}

TOP_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
DEPLOY_DIR="$TOP_DIR/deploy"
REMOTE_WORK_DIR=runner-gateway
DEVELOPER_IDS=()

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
    'umask 077; mkdir -p runner-gateway/tls/developers runner-gateway/harvester runner-gateway/data; chmod 700 runner-gateway runner-gateway/tls runner-gateway/tls/developers runner-gateway/harvester runner-gateway/data'
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

ensure_private_token() {
  local token_file=$1
  local label=$2
  local token_size token

  if [[ -e $token_file || -L $token_file ]]; then
    log "Reusing $label token in $token_file"
  else
    log "Generating $label token in $token_file"
    TOKEN_TEMP_FILE=$(mktemp "${token_file}.XXXXXX")
    trap 'rm -f -- "$TOKEN_TEMP_FILE"' EXIT
    openssl rand -hex 32 > "$TOKEN_TEMP_FILE"
    # Publish without replacing a token another deployment may have created.
    if ! ln -- "$TOKEN_TEMP_FILE" "$token_file" 2>/dev/null; then
      if [[ ! -e $token_file && ! -L $token_file ]]; then
        printf 'Cannot create token file: %s\n' "$token_file" >&2
        exit 1
      fi
    fi
    rm -f -- "$TOKEN_TEMP_FILE"
    trap - EXIT
  fi

  if [[ ! -f $token_file || -L $token_file ]]; then
    printf 'Invalid %s token file: %s\n' "$label" "$token_file" >&2
    exit 1
  fi
  chmod 600 "$token_file"
  token_size=$(wc -c < "$token_file")
  token=$(<"$token_file")
  if [[ ! $token =~ ^[[:xdigit:]]{64}$ ]] ||
     [[ $token_size != 64 && $token_size != 65 ]]; then
    printf '%s token must contain 64 hex characters: %s\n' "$label" "$token_file" >&2
    exit 1
  fi
}

ensure_local_smoke_token() {
  ensure_private_token "$DEPLOY_DIR/local-smoke-token" "shared local smoke"
}

read_developers() {
  local tool developer_ids id
  for tool in yq jq; do
    if ! command -v "$tool" >/dev/null 2>&1; then
      printf '%s is required to read deployment configuration\n' "$tool" >&2
      exit 1
    fi
  done

  developer_ids=$(yq -o=json '.' "$DEPLOY_DIR/$PROFILE.config.yaml" |
    jq -r '
      . as $config
      | (if .developers == null then [] else .developers end) as $developers
      | if ($developers | type) != "array"
        then error("developers must be an array") else . end
      | if all($developers[];
          if type != "object" then false
          elif (.id | type) != "string" then false
          else (.id | length <= 59 and test("^[a-z0-9]+(-[a-z0-9]+)*$")) end)
        then . else error("developer IDs must be lowercase DNS-safe names of at most 59 characters") end
      | if ($developers | map(.id) | unique | length) != ($developers | length)
        then error("developer IDs must be unique") else . end
      | if all($developers[];
          .tokenFile == ("/run/secrets/gateway/developers/" + .id + ".token"))
        then . else error("deploy.sh requires developer tokenFile /run/secrets/gateway/developers/<id>.token") end
      | if all($developers[];
          .repositoryID as $repository_id
          | (($repository_id | type) == "string") and
            ([$config.repositories[] | select((.repositoryID | tostring) == $repository_id)] | length == 1))
        then . else error("each developer repositoryID must match one repository policy") end
      | $developers[].id')

  DEVELOPER_IDS=()
  while IFS= read -r id; do
    if [[ -n $id ]]; then
      DEVELOPER_IDS+=("$id")
    fi
  done <<< "$developer_ids"
}

ensure_local_developer_tokens() {
  local token_dir="$DEPLOY_DIR/developers"
  local id previous token previous_token
  local checked_tokens=("$DEPLOY_DIR/local-smoke-token")

  if (( ${#DEVELOPER_IDS[@]} == 0 )); then
    return
  fi
  if [[ -L $token_dir || ( -e $token_dir && ! -d $token_dir ) ]]; then
    printf 'Invalid developer token directory: %s\n' "$token_dir" >&2
    exit 1
  fi
  mkdir -p -- "$token_dir"
  chmod 700 "$token_dir"

  for id in "${DEVELOPER_IDS[@]}"; do
    ensure_private_token "$token_dir/$id.token" "developer $id"
    token=$(<"$token_dir/$id.token")
    for previous in "${checked_tokens[@]}"; do
      previous_token=$(<"$previous")
      if [[ $token == "$previous_token" ]]; then
        printf 'Developer %s must have a distinct token\n' "$id" >&2
        exit 1
      fi
    done
    checked_tokens+=("$token_dir/$id.token")
  done
}

sync_developer_tokens() {
  local id
  for id in "${DEVELOPER_IDS[@]}"; do
    log "Uploading developer token for $id"
    rsync -a --chmod=F600 "$DEPLOY_DIR/developers/$id.token" \
      "$REMOTE_HOST:$REMOTE_WORK_DIR/tls/developers/$id.token"
  done
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
  read_developers
  ensure_local_tls
  ensure_local_smoke_token
  ensure_local_developer_tokens
  ensure_local_smoke_config
  create_remote_dirs
  sync_files
  sync_runner_scripts
  sync_tls
  sync_developer_tokens
  log "Deployment files are ready on $REMOTE_HOST:$REMOTE_WORK_DIR"
}

main "$@"
