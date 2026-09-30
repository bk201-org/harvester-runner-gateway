# Harvester Runner Gateway

An HTTPS API for GitHub Actions jobs and developers to create short-lived Harvester VMs and volumes. The standalone Go gateway holds the Harvester kubeconfig; jobs authenticate with GitHub OIDC and never receive the kubeconfig or a GitHub PAT. The first target is Harvester v1.7.3.

## Build and run

1. Copy `config.example.yaml` to a protected location. Set TLS files, kubeconfig, numeric GitHub repository ID, namespace, workflow policy, Harvester resources, size limits, N/M quotas, ID prefixes, and an absolute `database.path`.
2. Create a persistent directory writable by the gateway user for the SQLite database and its WAL files. Keep it across restarts.
3. Build and start:

```sh
make build
./bin/hvst-runner-gw --config /secure/path/config.yaml
```

- Docker is required for the build, release, test, vet, and image targets. `make build` exports Linux server and client binaries to `./bin`; `make cluster-release` exports binaries and checksums to `./dist`.
- `make image` builds the runtime image with `IMAGE` (default `bk201z/harvester-runner-gateway:dev`); `make push` builds and publishes it. The image runs as a non-root user by default.
- Run exactly one gateway instance in v1. Allocation and quota checks are serialized within that process.

### Remote host

Publish the image to a registry the host can access. Keep config and credentials outside tracked source files at the paths in `config.example.yaml`; create a persistent data directory writable by the host user.

The optional `deploy.sh` copies files to `$HOME/runner-gateway` on the remote host and creates `start.sh`, `stop.sh`, and `restart.sh` there on every deployment. On that host:

```sh
cd "$HOME/runner-gateway"
./start.sh
docker logs harvester-runner-gateway
```

- `start.sh` resolves config, TLS, Harvester, and data paths relative to its own location. `restart.sh` pulls and replaces the container; `stop.sh` stops it. Both honor `GATEWAY_CONTAINER_NAME`.
- `docker-run.sh` pulls `GATEWAY_IMAGE` before removing the existing container, then starts the replacement with `--pull=always` and the same data directory. An initial pull failure leaves the container running; a later pull or start failure can interrupt service.
- The TLS directory contains `tls.crt`, `tls.key`, and optionally `local-smoke-token`. The Harvester directory contains `kubeconfig`. Credentials are mounted read-only.
- The container runs as the invoking host user, including through sudo. That user needs read access to config, TLS key, smoke token, and kubeconfig, plus write access to SQLite storage.
- Run `./docker-run.sh --help` for image, container name, and host port overrides.

### Network and Harvester access

- The gateway needs HTTPS access to GitHub OIDC discovery and JWKS, plus Kubernetes API access to Harvester. Runners need HTTPS access to the gateway.
- The kubeconfig identity needs namespace read; VM and VMI get/list; VM create/update/delete; PVC get/list/create/delete; Secret get/list/create/update/delete; image, network, and storage class get; and KubeVirt virtualmachine start/stop/restart/addvolume/removevolume subresource update.
- Bind rights to the configured namespaces and, where possible, configured image and network namespaces. The storage class must support ReadWriteMany block PVCs for live hotplug.

### TLS certificate

Set `tls.certFile` and `tls.keyFile` to a PEM server certificate and matching unencrypted private key. The gateway does not generate them. For production, use a CA trusted by runners and store the files outside this repository. The certificate must match the hostname used by workflows.

For a local test, replace the hostname and directory below, then run this on the gateway host with OpenSSL 1.1.1 or newer:

```sh
GATEWAY_HOST=gateway.example.com
TLS_DIR=/secure/path/gateway-tls
mkdir -p "$TLS_DIR"
chmod 700 "$TLS_DIR"
openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days 30 \
  -keyout "$TLS_DIR/tls.key" -out "$TLS_DIR/tls.crt" \
  -subj "/CN=$GATEWAY_HOST" \
  -addext "subjectAltName=DNS:$GATEWAY_HOST"
chmod 600 "$TLS_DIR/tls.key"
openssl x509 -in "$TLS_DIR/tls.crt" -noout -dates -ext subjectAltName
```

- Point `tls.certFile` and `tls.keyFile` to these files, or mount them at the example `/run/secrets/gateway/` paths. The service user must be able to read them.
- For an IP address, use `subjectAltName=IP:<address>` instead of `DNS:<hostname>`.
- Runners must trust a self-signed certificate. Keep the private key out of version control and leave TLS verification enabled.

## Logging

- The server writes JSON logs to stdout for startup, preflight, recovery, cleanup, mutations, shutdown, and HTTP responses. Response entries include method, path, status, size, and duration.
- Client errors use `WARN`; server and Harvester errors use `ERROR`. Resource events include namespace, resource ID, and GitHub ownership fields.
- Authorization headers, request bodies, SSH public keys, and cloud-init data are not logged. Collect stdout with the service manager or container runtime.
- The CLI and live smoke test write per-call JSON logs to stderr.

## Client CLI

`make build` produces `bin/hvst-runner-gw-client`; `go install ./cmd/hvst-runner-gw-client` installs it from this checkout. It uses HTTPS and needs no Harvester kubeconfig. Go programs can import `github.com/bk201-org/harvester-runner-gateway/client`.

For an operator smoke check, configure [local smoke access](#local-shell) and use the same token file:

```sh
export GATEWAY_URL=https://gateway.example.internal:8443
export GATEWAY_TOKEN_FILE=/secure/path/local-smoke-token
export GATEWAY_CA_CERT=/secure/path/gateway.crt # optional additional trusted CA
client=./bin/hvst-runner-gw-client

"$client" health
"$client" ready
"$client" quota
"$client" vm create \
  --image default/ubuntu-24-04 --network default/vm-network \
  --cpu 2 --memory 4Gi --boot-disk-size 20Gi \
  --ssh-public-key-file "$HOME/.ssh/id_ed25519.pub" \
  --ttl-seconds 3600
"$client" vm list
```

Save the returned `id` for later commands. Successful JSON responses end with a newline for tools such as `jq`; commands with no response body produce no output.

| Command | Purpose |
| --- | --- |
| `health`, `ready` | Check process health or Harvester reachability; no authentication |
| `quota` | Get repository usage and limits |
| `vm create [flags]` | Create a VM and wait for a running VMI with a usable IP |
| `vm list`, `vm get ID` | List caller-owned VMs or get status |
| `vm delete ID` | Request deletion of the VM and its attached volumes |
| `vm power ID on` / `vm power ID off`, `vm reboot ID` | Change power state or request reboot |
| `vm attach ID VOLUME_ID`, `vm detach ID VOLUME_ID` | Attach or detach a volume |
| `volume create --size 10Gi` | Create an independent volume |
| `volume list`, `volume get ID`, `volume delete ID` | List, inspect, or delete owned volumes |

- VM creation requires `--image`, `--network`, `--cpu`, `--memory`, and `--boot-disk-size`. Optional inputs: repeatable `--ssh-public-key-file` (up to 10 keys), `--user-data-file` (cloud-config up to 64 KiB), and `--ttl-seconds`.
- By default, `vm create` polls every ten seconds for up to five minutes and prints status once `ready` is true. Change the wait with `--wait-timeout` or `GATEWAY_VM_WAIT_TIMEOUT`; `--no-wait` returns the initial response. `--timeout` still limits each HTTP request.
- A readiness timeout leaves the VM allocated. Use `--no-wait` and save the ID when you need to inspect or delete it after a timeout.
- Volume creation requires `--size` and accepts `--ttl-seconds`. An omitted TTL uses the six-hour server default; an explicit TTL must be 1–86400 seconds.

Global flags precede the command:

```sh
"$client" --url https://gateway.example.internal:8443 --timeout 45s vm get "$vm_id"
"$client" vm create --help
```

| Flag | Environment variable | Default |
| --- | --- | --- |
| `--url` | `GATEWAY_URL` | Required HTTPS URL; optional base path |
| `--token-file` | `GATEWAY_TOKEN_FILE` | Unset |
| `--ca-cert` | `GATEWAY_CA_CERT` | System trust only |
| `--audience` | `GATEWAY_AUDIENCE` | `api://harvester-runner-gateway` |
| `--timeout` | `GATEWAY_TIMEOUT` | `30s` per HTTP request |

- Flags override environment variables. Authentication checks the token file, then `GATEWAY_TOKEN`, then automatic GitHub Actions OIDC when `GITHUB_ACTIONS=true`. An unreadable or malformed configured credential fails without fallback.
- Tokens are never CLI arguments or printed. The CLI saves no credentials or profiles; cluster commands save private resource state and SSH files. The extra gateway CA does not affect GitHub OIDC trust. TLS verification remains enabled and redirects are rejected.
- In GitHub Actions, grant `id-token: write` and set `GATEWAY_URL`; set `GATEWAY_AUDIENCE` or `GATEWAY_CA_CERT` if needed. See the [example workflow](examples/workflow.yml).
- The client requests a fresh OIDC token for each invocation through `ACTIONS_ID_TOKEN_REQUEST_URL` and `ACTIONS_ID_TOKEN_REQUEST_TOKEN`.
- Exit codes: `0` success, `1` HTTP/authentication/transport failure, `2` invalid usage or local configuration. Errors and request logs go to stderr; errors include HTTP status and gateway code/message when available.
- Creation and action commands return when the gateway accepts a request. Poll `vm get` or `volume get` for completion. Mutations are not automatically retried, and deleting an absent resource returns HTTP 404.

## On-demand developer clusters

Install the Linux or macOS (amd64 or arm64) client release and verify its `SHA256SUMS`. You also need `ssh`, `ssh-keygen`, and network access to the gateway and VM network, such as through a VPN.

### Developer access

- An administrator adds `developers` to the gateway config. Each developer has a stable ID, one repository policy, and a unique token.
- Generate a token with `umask 077` and `openssl rand -hex 32 > /secure/path/alice.token`. Token files must be private regular files (`chmod 600`) with 64 hex characters and an optional trailing newline.

```yaml
developers:
  - id: alice
    repositoryID: "123456789"
    tokenFile: /run/secrets/gateway/developers/alice.token
```

- With `docker-run.sh`, store tokens under the mounted TLS directory's `developers/` subdirectory.
- `deploy.sh` creates missing tokens at `deploy/developers/<id>.token`, reuses them across profiles, and uploads them to `runner-gateway/tls/developers/<id>.token`.
- The helper requires `tokenFile: /run/secrets/gateway/developers/<id>.token`, yq v4, and jq. It validates credentials before connecting and sets private permissions. It does not restart the gateway or distribute tokens.
- For manual setup, distribute each token privately. To rotate a helper-managed token, replace its local file and redeploy. To revoke access, remove the developer entry and redeploy. Restart the gateway after either change.
- Never reassign a former developer's ID: ownership survives token rotation. Developer resources use `dev-<id>` and are isolated from other developers, GitHub runs, and local smoke resources. Developers and CI share their repository policy and quota; revoked developers' resources expire normally.

### Create and manage a cluster

Copy [examples/debug-cluster.yaml](examples/debug-cluster.yaml), fill in the failed job's provisioning inputs, and run:

```sh
export GATEWAY_URL=https://gateway.example.internal:8443
export GATEWAY_TOKEN_FILE="$HOME/.config/harvester-gateway/token"
export GATEWAY_CA_CERT=/secure/path/gateway-ca.pem # optional for a private CA

hvst-runner-gw-client cluster create \
  --config ./debug-cluster.yaml --state-dir ./debug-cluster
hvst-runner-gw-client cluster status --state-dir ./debug-cluster
ssh -F ./debug-cluster/ssh_config <vm-id>
hvst-runner-gw-client cluster delete --state-dir ./debug-cluster
```

- The YAML uses action input names and rejects unknown fields. Required fields: `vm-count`, `image`, `network`, `cpu`, `memory`, `boot-disk-size`, `username`. Optional fields: `user-data` (inline cloud-config), `ttl-seconds`, `wait-timeout-seconds` (default 600).
- Connection and credentials use global flags or environment variables. Local cluster commands never request GitHub OIDC.
- JSON output includes VM status, IPs, expiry, SSH config, and SSH commands. Check out the failed test revision and run its setup and tests manually.
- These are fresh VMs, not snapshots. An IP does not mean SSH or cloud-init is ready. Local k3s installation and automatic test execution are not included.
- The state directory must not exist before creation. It contains private SSH material and saved configuration: exclude it from version control and artifacts. Commands lock it, bind it to the original gateway URL, and save returned VM IDs atomically.
- Use the same developer identity for status and deletion. Another identity cannot see the VMs, and a 404 does not distinguish an inaccessible VM from a deleted one.
- On interruption, run `cluster status` to refresh addresses and SSH config, then retry `cluster delete`. Creation is never retried automatically. If a create response was lost, use `vm list` to find unrecorded resources and `vm delete ID` to remove them; cluster deletion covers recorded IDs only.
- Deletion waits for recorded VMs to disappear (default ten minutes; change with `--wait-timeout`). Failure preserves keys and state.
- Success removes keys and leaves a small tombstone so repeated deletion is safe; you can then remove the directory. Otherwise, clusters expire after six hours by default, up to 24 hours, even while your workstation is offline.

## Authentication

- Workflows send `Authorization: Bearer <GitHub OIDC JWT>`, grant `id-token: write`, and request the configured `oidc.audience`.
- The gateway verifies issuer, signature, audience, time claims, repository ID, `self-hosted` runner environment, workflow ref, and event name. Refs and event names match exactly; wildcard policies are unsupported. GitHub.com is the default issuer.
- Jobs with the same repository, run ID, and attempt share resources. All runs and attempts in a repository share its quota. OIDC does not identify an exact runner VM.
- An optional local smoke token is accepted only for `localSmoke.repositoryID`.
- See [examples/workflow.yml](examples/workflow.yml) for token retrieval and create/status/delete. Keep tokens out of logs and artifacts.

## API

The [OpenAPI specification](openapi.yaml) lists every endpoint. See [ARCHITECTURE.md](ARCHITECTURE.md) for ownership, VM status, allocation, quotas, and cleanup.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/v1/quota` | Repository usage and limits |
| POST, GET | `/v1/vms` | Create or list VMs |
| GET, DELETE | `/v1/vms/{id}` | Status or delete |
| PUT | `/v1/vms/{id}/power` | Set `on` or `off` |
| POST | `/v1/vms/{id}/reboot` | Reboot |
| PUT, DELETE | `/v1/vms/{id}/volumes/{volumeID}` | Attach or detach |
| POST, GET | `/v1/volumes` | Create or list volumes |
| GET, DELETE | `/v1/volumes/{id}` | Status or delete |

- Each successful POST create allocates a new resource. Save the returned ID. If a response is lost, list owned resources before retrying.
- Create returns HTTP 201 while provisioning continues. Poll GET for status; the CLI polls VM creation unless `--no-wait` is set. A VM is `ready` when its VMI is Running and `nic-1` reports a usable IP. An IP does not prove SSH or a guest service is ready.
- VM requests take approved image/network, CPU, memory, `bootDiskSize`, and optional `sshPublicKeys`, cloud-config `userData`, and `ttlSeconds`. Volume requests take `size` and optional `ttlSeconds`.
- Only resources owned by the authenticated run attempt, developer, or smoke identity can be managed. Attachments require a running VM and Bound volume; detach before explicitly deleting a volume.
- `maxActiveVMs` counts VMs; `maxActiveVolumes` counts independent volumes. Provisioning and deleting objects count until removed. Limits return HTTP 409 with `quota_exceeded`. Use a Kubernetes ResourceQuota for a namespace-wide cap.
- Resources expire after six hours by default or the requested TTL, up to 24 hours. Cleanup checks each minute. Deleting a VM also deletes its attached independent volumes.
- Keep the SQLite database across restarts to preserve reserved IDs. If it is lost, IDs from fully deleted resources may be reused. Changing ID prefixes requires stopping the gateway, removing old-prefix resources, and resetting the database and its `-wal` and `-shm` files.

## Live smoke test

Builds and unit tests do not need a cluster. Before production use, run the [live smoke test](internal/smoke/live_test.go) in a dedicated Harvester v1.7.3 namespace.

- It covers VM lifecycle, volume hotplug, power/reboot, and deletion with an attached volume.
- Four subtests run independently, up to two at once when quota permits. Each owns one VM, cleans up independently, and polls every ten seconds by default. At least one VM and volume slot must be available.

There is no smoke executable. Run `./scripts/gateway-smoke.sh` or `GATEWAY_SMOKE=1 go test ./internal/smoke -run '^TestGatewaySmoke$' -count=1 -parallel=2 -v`. Normal `go test ./...` skips it.

### GitHub Actions

- The [smoke workflow](.github/workflows/smoke.yml) runs only through `workflow_dispatch` on the self-hosted `harvester-runners` label.
- Set repository variables `GATEWAY_URL`, `GATEWAY_IMAGE`, and `GATEWAY_NETWORK`. Set `GATEWAY_AUDIENCE` only if it differs from `api://harvester-runner-gateway`. For a self-signed certificate, install its PEM on the runner and set `GATEWAY_CA_CERT` to its absolute path.
- The job sets `GATEWAY_SMOKE=1`, grants `id-token: write`, and runs the Go smoke test. For `bk201-org/harvester-runner-gateway` on `main`, allow this exact ref and event in the matching policy:

```yaml
allowedWorkflowRefs:
  - bk201-org/harvester-runner-gateway/.github/workflows/smoke.yml@refs/heads/main
allowedEvents:
  - workflow_dispatch
```

Use the actual slug and branch if different. Once the workflow is on the default branch, start it from the Actions tab or with `gh workflow run smoke.yml --ref main`.

### Local shell

- Local invocation needs an opt-in `localSmoke` credential on the existing gateway. `deploy.sh` creates a shared token at `deploy/local-smoke-token` across profiles and syncs it to `runner-gateway/tls/local-smoke-token`. Keep it private and reuse it.
- On first deployment of a profile, `deploy.sh` creates `deploy/<PROFILE>.smoke.json` from `GATEWAY_HOST`, `GATEWAY_HOST_PORT` (default 8443), the first allowed image/network, token path, and local TLS certificate path.
- Existing smoke JSON is not changed; edit it to use another approved image or network. Generation requires yq v4 and jq. `deploy/` is Git-ignored.
- For manual setup, create a private 32-byte hex token and configure an existing repository policy:

```sh
umask 077
openssl rand -hex 32 > /secure/path/local-smoke-token
```

```yaml
localSmoke:
  repositoryID: "123456789"
  tokenFile: /secure/path/local-smoke-token
```

Restart the gateway. The policy controls namespace, approved images/networks, size limits, and quota. Local resources use the reserved `local-smoke` run identity, isolated from workflows while sharing their repository quota.

For manual workstation setup, copy the token privately and create `~/.config/harvester-runner-gateway/smoke.json` with absolute paths:

```json
{
  "gatewayURL": "https://gateway.example.internal:8443",
  "image": "default/ubuntu-24-04",
  "network": "default/vm-network",
  "tokenFile": "/home/you/.config/harvester-runner-gateway/local-smoke-token",
  "caCert": "/home/you/.config/harvester-runner-gateway/gateway.crt"
}
```

- `caCert` is optional; use the self-signed gateway certificate or its PEM CA. The certificate must match the hostname in `gatewayURL`; TLS verification stays enabled.
- Keep the token owner-readable only (`chmod 600`), out of version control, GitHub variables, and artifacts. Restart the gateway after replacing the token to rotate it.
- Run `./scripts/gateway-smoke.sh`. Set `GATEWAY_SMOKE_CONFIG=/absolute/path/to/smoke.json` to use another config, including generated `deploy/<PROFILE>.smoke.json`.
- There is no gateway deployment manifest: TLS, networking, image/network names, and RBAC depend on the site.

## CI VM cluster actions

- [Create CI VM cluster](actions/create-ci-cluster/README.md) (`actions/create-ci-cluster`): Create identical VMs, write a job-local SSH config, and delete recorded VMs after the job.
- [Create CI k3s cluster](actions/create-k3s-cluster/README.md) (`actions/create-k3s-cluster`): Create VMs, install k3s over SSH (first VM server, others agents), save a kubeconfig, and delete the VMs after the job.

Both actions use `hvst-runner-gw-client` to manage GitHub inputs and job cleanup. Run `make cluster-release` to build Linux amd64/arm64 binaries and checksums. Each action downloads the caller's release URL, or the latest action-repository release when no URL or local path is given.

Run `make test-cluster-action` for an offline command smoke test. To exercise the command against a gateway with the smoke config, run `GATEWAY_SMOKE_CONFIG=./kf/smoke.json ./scripts/cluster-smoke.sh`.
