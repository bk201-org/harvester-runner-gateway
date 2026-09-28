# Harvester Runner Gateway

An HTTPS API for GitHub Actions jobs to create and manage short-lived Harvester
VMs and volumes. The gateway is a standalone Go service holding the Harvester
kubeconfig. Jobs authenticate with GitHub OIDC and never receive that kubeconfig
or a GitHub PAT. The first target is Harvester v1.7.3.

## Build and run

Copy `config.example.yaml` to a protected location and set the TLS certificate,
kubeconfig, numeric GitHub repository ID, namespace, workflow policy, Harvester
resource names, size limits, N/M quotas, ID prefixes, and an absolute `database.path`.
Create a persistent directory writable by the gateway user for the SQLite
database. Keep that directory across container or host restarts; the gateway
creates the database file and its WAL files there. Then run:

```sh
make build
./bin/hvst-runner-gw --config /secure/path/config.yaml
```

Docker is required for the build, release, test, vet, and image targets.
`make build` builds Linux server and client executables in `Dockerfile.build`
and exports them to `./bin`.
`make cluster-release` exports release binaries and checksums to `./dist`.
`make test`, `make test-cluster-action`, and `make vet` run in containers too.
`make docker-build` builds the runtime image. The image defaults to a non-root
user. Run exactly one gateway instance in v1; allocation and quota checks are
serialized in that process.

To run the Docker image, keep the YAML configuration and credentials outside
the repository. Use the paths in `config.example.yaml` and create a persistent
data directory writable by the host user running the script. Then run:

```sh
make docker-build
./docker-run.sh /secure/path/config.yaml /secure/path/gateway-tls \
  /secure/path/harvester /secure/path/gateway-data
docker logs harvester-runner-gateway
```

The TLS directory contains `tls.crt` and `tls.key`; the Harvester directory
contains `kubeconfig`. Run `./docker-run.sh --help` for image, container name,
and host port overrides. The script runs the container as the invoking host
user (including when invoked through sudo), so that user must be able to read
the config, TLS key, and kubeconfig and write the SQLite directory. The script
mounts credentials read-only and keeps the SQLite directory persistent.

The gateway needs HTTPS access to GitHub's OIDC discovery and JWKS endpoints and
Kubernetes API access to Harvester. Runners need HTTPS access to the gateway.

The kubeconfig identity needs namespace read, VM and VMI get/list, VM
create/update/delete, PVC get/list/create/delete, Secret get/list/create/
update/delete, image and network get, storage class get, and KubeVirt virtualmachine start/stop/restart/addvolume/
removevolume subresource update permissions. The selected storage class must
support ReadWriteMany block PVCs for live hotplug. Bind these rights only in the
configured namespaces and for the configured image/network namespaces where
possible. The TLS certificate must match the hostname used by workflows.

### Generate the TLS certificate and key

`tls.certFile` and `tls.keyFile` are paths to a PEM-encoded server certificate
and its matching, unencrypted private key. The gateway does not create them.
For production, obtain a certificate for the gateway hostname from a CA trusted
by the runners, and put the certificate and key in a protected location outside
this repository.

For a local test, replace the hostname and directory below, then run this on
the gateway host (OpenSSL 1.1.1 or newer):

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

Set `tls.certFile` to the resulting `tls.crt` path and `tls.keyFile` to the
`tls.key` path, or mount both at the example `/run/secrets/gateway/` paths.
The service user must be able to read them. For an IP address, use
`subjectAltName=IP:<address>` instead of `DNS:<hostname>`. The local test
certificate is self-signed, so each runner must trust it before its HTTPS
client can call the gateway. Do not commit the private key or disable TLS
verification.

## Logging

The server writes structured JSON logs to stdout. Startup, preflight, allocation
recovery, expiry cleanup, resource mutations, shutdown, and every HTTP response
are logged. Request completion entries include the method, URL path, status,
response size, and duration. Client errors use `WARN`; server and Harvester
errors use `ERROR`. Resource events include the namespace, resource ID, and
GitHub repository/run ownership fields.

Authorization headers, request bodies, SSH public keys, and cloud-init data
are never logged. Collect stdout with the service manager or container runtime
used to run the gateway. The CLI and live smoke test emit the same style of
per-call JSON logs to stderr.

## Client CLI

`make build` builds the server and `bin/hvst-runner-gw-client`. The client uses HTTPS and
supports every gateway API operation without
needing `curl` or a Harvester kubeconfig. It can also be installed directly from
this checkout with `go install ./cmd/hvst-runner-gw-client`. Go programs can
import `github.com/bk201-org/harvester-runner-gateway/client` for typed VM,
volume, quota, health, readiness, and lifecycle operations.

For local use, configure the gateway's optional `localSmoke` credential as
[described below](#local-shell), then point the client at the same token file:

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

Save the returned `id` for subsequent commands. Successful JSON responses go to
stdout with a trailing newline, so scripts can extract fields with `jq`.
Commands whose API response has no body produce no output on success.

| Command | Purpose |
| --- | --- |
| `health` | Check process health; no authentication |
| `ready` | Check Harvester reachability; no authentication |
| `quota` | Get repository usage and limits |
| `vm create [flags]` | Create a VM and wait for a running VMI with a usable IP |
| `vm list` | List run-owned VMs |
| `vm get ID` | Get VM status |
| `vm delete ID` | Request deletion of the VM and its attached volumes |
| `vm power ID on` / `vm power ID off` | Set desired power state |
| `vm reboot ID` | Request reboot |
| `vm attach ID VOLUME_ID` | Request live volume attachment |
| `vm detach ID VOLUME_ID` | Request volume detachment |
| `volume create --size 10Gi` | Create an independent volume |
| `volume list` | List run-owned independent volumes |
| `volume get ID` | Get volume status |
| `volume delete ID` | Request volume deletion |

VM creation requires `--image`, `--network`, `--cpu`, `--memory`,
`--boot-disk-size`. Optional inputs are repeatable
`--ssh-public-key-file` (up to 10 keys), `--user-data-file` (cloud-config up to
64 KiB), and `--ttl-seconds`. By default, the command polls every ten seconds
for up to five minutes and prints the final status only after `ready` is true.
Use `--wait-timeout` or `GATEWAY_VM_WAIT_TIMEOUT` to change that limit, or
`--no-wait` to return the initial asynchronous API response. The normal
`--timeout` remains the limit for each HTTP request. A readiness timeout leaves
the VM allocated. Use `--no-wait` and save the returned ID when the caller
must be able to inspect or delete the VM after a wait timeout. Volume creation
requires `--size` and also accepts `--ttl-seconds`. An omitted TTL uses the
server's six-hour default; an explicit TTL must be 1–86400 seconds.

Global flags must precede the command, for example:

```sh
"$client" --url https://gateway.example.internal:8443 --timeout 45s vm get "$vm_id"
"$client" vm create --help
```

| Global flag | Environment variable | Default |
| --- | --- | --- |
| `--url` | `GATEWAY_URL` | Required HTTPS URL; optional base-path prefix |
| `--token-file` | `GATEWAY_TOKEN_FILE` | Unset |
| `--ca-cert` | `GATEWAY_CA_CERT` | System trust only |
| `--audience` | `GATEWAY_AUDIENCE` | `api://harvester-runner-gateway` |
| `--timeout` | `GATEWAY_TIMEOUT` | `30s` per HTTP request |

Flags override their corresponding environment variables. Authentication uses
the configured token file first, then `GATEWAY_TOKEN`, then automatic GitHub
Actions OIDC when `GITHUB_ACTIONS=true`. A configured credential that cannot be
read or is malformed fails without falling back. Tokens are never passed as CLI
arguments or printed. The CLI does not save credentials or profiles. The
additional gateway CA does not change trust for GitHub OIDC requests. TLS
verification stays enabled, and redirects are rejected.

In GitHub Actions, grant `id-token: write` and set `GATEWAY_URL`, optionally
`GATEWAY_AUDIENCE` and `GATEWAY_CA_CERT`. The client obtains a fresh OIDC token
for each invocation using `ACTIONS_ID_TOKEN_REQUEST_URL` and
`ACTIONS_ID_TOKEN_REQUEST_TOKEN`. The [example workflow](examples/workflow.yml)
builds the client, creates a VM, reads its status, and requests cleanup.

The client exits with `0` on success, `1` for HTTP, authentication, or transport
failures, and `2` for invalid usage or local configuration. Structured request
logs and errors go to stderr; errors include the HTTP status and gateway error
code/message when available.
Creation and action commands return as soon as the gateway accepts the request;
poll `vm get` or `volume get` to observe completion. Mutations are not
automatically retried. Deleting an absent resource remains an HTTP 404 failure.

## Authentication

Workflow API calls use `Authorization: Bearer <GitHub OIDC JWT>`. Workflow jobs
grant `id-token: write` and request the audience configured under
`oidc.audience`. An optional, separately configured local smoke token is accepted
only for the repository policy named under `localSmoke.repositoryID`.
For OIDC, the gateway verifies the token's issuer, signature, audience, time claims,
repository ID, `self-hosted` runner environment, workflow ref, and event name.
Workflow refs and event names match exactly; no wildcard policy is supported.
Only GitHub.com is the default issuer. Jobs in the same repository/run ID/run
attempt share resources. All runs and attempts of a repository share its quota.
OIDC does not identify an exact runner VM.

See [examples/workflow.yml](examples/workflow.yml) for token retrieval and a
create/status/delete sequence. Keep the token out of logs and artifacts.

## API

The [OpenAPI specification](openapi.yaml) defines all endpoints. Main routes:

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/v1/quota` | Active N/M usage and limits |
| POST, GET | `/v1/vms` | Create or list run-owned VMs |
| GET, DELETE | `/v1/vms/{id}` | Status or delete |
| PUT | `/v1/vms/{id}/power` | Desired `on` or `off` state |
| POST | `/v1/vms/{id}/reboot` | Reboot a running VM |
| PUT, DELETE | `/v1/vms/{id}/volumes/{volumeID}` | Live attach or detach |
| POST, GET | `/v1/volumes` | Create or list run-owned volumes |
| GET, DELETE | `/v1/volumes/{id}` | Status or delete |

Every successful POST create call allocates a new resource. Repeating a create
request can create another resource, so save the returned ID before polling or
deleting. If a create response is lost, list run-owned resources before trying
again; an unknown resource will otherwise remain until expiry cleanup.
VM IDs use `<vmPrefix><hex>` and independent volume IDs use
`<volumePrefix><hex>`. The default prefixes are `ci-vm-` and `ci-vol-`, so
the first IDs are `ci-vm-00000001` and `ci-vol-00000001`. Each kind has one
gateway-wide counter across repositories and namespaces. Numbers are lowercase
hex with at least eight digits. Configure distinct, lowercase DNS-safe prefixes
ending in `-` at the top level of the YAML file. VM dependencies are named
`<id>-root` and `<id>-init`.
Create returns 201 while Kubernetes provisioning is still in progress;
API clients poll GET for status. The bundled CLI performs that polling for VM
creation unless `--no-wait` is set. A VM status is `ready` when its VMI is
Running and `nic-1` reports a usable, non-link-local IPv4 or IPv6 address.
Approved images on the Multus bridge network must run QEMU Guest Agent for
reliable guest IP reporting. Reboot is not automatically retried by the gateway.

VM requests accept approved image/network names, CPU, memory, bootDiskSize,
optional `sshPublicKeys`, optional `userData` in cloud-config format, and
optional `ttlSeconds`. The SSH keys are inserted for the configured default
guest user. Volume requests accept `size` and optional `ttlSeconds`. Volume
attachments require a running VM and a Bound volume; live hotplug uses SCSI.
Only resources created by this gateway for the same run attempt can be managed.
Repository, run, and attempt remain ownership labels. GitHub OIDC resources
also carry the exact verified workflow ref in a `runner-gw-workflow-ref`
annotation; local smoke resources have no workflow ref annotation.
An attached volume must be detached before an explicit delete.

`maxActiveVMs` counts VMs, including their boot disks. `maxActiveVolumes` counts
only independent volumes created through `/v1/volumes`. Provisioning and
deleting objects count until their Kubernetes objects disappear. A 409 response
with code `quota_exceeded` indicates that a limit is reached. Policy also caps
CPU, memory, boot disk size, and individual volume size; those size caps are
separate from the N/M quota structure so quota dimensions can grow later.
N/M limits apply per repository across all its workflow runs and attempts.
Use a Kubernetes ResourceQuota for a cluster-enforced namespace-wide cap.

Allocation counters and an append-only reservation history live in the local
SQLite database at `database.path`. Each number is committed before the
Kubernetes create call, so failed or interrupted creates can leave gaps. A
retained database prevents reuse of reserved numbers across restarts, even
after resources are deleted. The database records IDs, owner, namespace, kind,
sequence, reservation time, and the configured prefixes; it does not store
request bodies or credentials.
A reservation does not prove that resource creation succeeded.

At startup the gateway scans surviving VM, VMI, PVC, and Secret metadata
before cleanup or request serving and raises each kind's allocation floor.
These observations are not imported into reservation history. If the database
is lost, only surviving Kubernetes objects can set the floor, so numbers from
fully deleted resources may be reused. Keep backups if historical IDs must
remain unique. The configured prefixes are bound to the database: changing
either prefix requires stopping the gateway, removing resources with the old
prefixes, and resetting the database. After stopping, remove the database file
and its `-wal` and `-shm` companions to reset it. This ID change requires that
one-time reset; the old database schema and old resource IDs are unsupported.
The database needs no ConfigMap or extra Kubernetes RBAC verbs. Exactly one
active gateway instance is still required for serialized quota checks.

Resources expire after six hours by default, or after the requested TTL up to
24 hours. A reconciler checks every minute, deleting expired VMs, boot disks,
cloud-init Secrets, and independent volumes. An attached volume that expires
first is detached and then deleted. Deleting a VM also deletes its attached
independent volumes; detached independent volumes keep their own TTL. GET status
may show no IP address until the VM reports one; it does not prove SSH or a
guest service is ready.

## Live smoke test

Unit tests and builds run without a cluster. Live creation, actions, hotplug,
and cleanup must be smoke-tested against a dedicated Harvester v1.7.3 namespace
before production use. The [smoke test](internal/smoke/live_test.go)
runs through Go's standard `testing` package as four named subtests: VM
lifecycle, volume hotplug, VM power and reboot, and VM deletion with an attached
volume. Up to two subtests run at once, subject to available VM and volume
quota. Each subtest owns one VM and cleans up independently. The test polls VM
and volume status every ten seconds by default and checks that at least one VM
and volume slot are available before running.
No smoke executable is built. Run it with `./scripts/gateway-smoke.sh` or directly with
`GATEWAY_SMOKE=1 go test ./internal/smoke -run '^TestGatewaySmoke$' -count=1 -parallel=2 -v`.
The selected policy must have at least one available VM and volume slot.

### GitHub Actions

The checked-in [smoke workflow](.github/workflows/smoke.yml) runs only through
`workflow_dispatch` on the self-hosted `harvester-runners` label. Set repository
variables `GATEWAY_URL`, `GATEWAY_IMAGE`, and `GATEWAY_NETWORK`; set
`GATEWAY_AUDIENCE` only if it differs from `api://harvester-runner-gateway`.
If the gateway uses a self-signed certificate, install its PEM certificate on
the runner and set `GATEWAY_CA_CERT` to its absolute path.
The job sets `GATEWAY_SMOKE=1`, requests an OIDC token with `id-token: write`,
and runs the Go smoke test. For a repository at
`bk201-org/harvester-runner-gateway` on
`main`, permit this exact workflow ref and event in the matching gateway
repository policy:

```yaml
allowedWorkflowRefs:
  - bk201-org/harvester-runner-gateway/.github/workflows/smoke.yml@refs/heads/main
allowedEvents:
  - workflow_dispatch
```

Use the actual repository slug and branch if they differ. Once the workflow is
on the GitHub default branch, run it through the Actions tab or
`gh workflow run smoke.yml --ref main`.

### Local shell

Local invocation needs a one-time, opt-in `localSmoke` credential on the
**existing gateway**. Generate 32 random bytes as hex and store them in a
private file readable by the gateway process, outside the repository:

```sh
umask 077
openssl rand -hex 32 > /secure/path/local-smoke-token
```

Point the gateway configuration at that file and an existing repository policy
ID, then restart the gateway. The selected policy still controls namespace,
allowed images and networks, size limits, and quota. Local resources use the
reserved `local-smoke` run identity, so workflow runs cannot access them; local
runs and workflow runs still share that repository's quota.

```yaml
localSmoke:
  repositoryID: "123456789"
  tokenFile: /secure/path/local-smoke-token
```

Copy the same token to a private file on your workstation. Create
`~/.config/harvester-runner-gateway/smoke.json` with these fields, using
absolute paths and your approved image and network:

```json
{
  "gatewayURL": "https://gateway.example.internal:8443",
  "image": "default/ubuntu-24-04",
  "network": "default/vm-network",
  "tokenFile": "/home/you/.config/harvester-runner-gateway/local-smoke-token",
  "caCert": "/home/you/.config/harvester-runner-gateway/gateway.crt"
}
```

`caCert` is optional. Set it to the self-signed gateway certificate or the PEM
CA certificate that signed the gateway certificate. The certificate must match
the hostname in `gatewayURL`; the script keeps TLS verification enabled.

Keep the token file readable only by its owner (`chmod 600`). It is a bearer
credential for smoke resources under the selected policy; never commit it or
put it in GitHub variables or artifacts. Restart the gateway after replacing
its token file to rotate the credential. Then run:

```sh
./scripts/gateway-smoke.sh
```

Set `GATEWAY_SMOKE_CONFIG=/absolute/path/to/smoke.json` to use another local
configuration. The Go test validates configuration and quota before running four
focused subtests with up to two running at once when quota allows. Each
subtest owns one VM and cleans up independently. The test is skipped during
normal `go test ./...` runs; running the script or setting `GATEWAY_SMOKE=1` is
the explicit opt-in. The gateway has
no deployment manifest because
TLS, network reachability, image/network names, and RBAC are site-specific.

## CI VM cluster action

The reusable [cluster action](actions/create-ci-cluster/README.md) creates identical
VMs, writes a job-local SSH config, and deletes recorded VMs in its post-job
step. Run `make cluster-release` to build Linux amd64 and arm64 executables
and checksums for a GitHub release. The action downloads the matching binary
from the release URL supplied by the caller. Run `make test-cluster-action`
for an offline command smoke test. To exercise the command against the
real gateway with the same smoke configuration as `./scripts/gateway-smoke.sh`, run
`GATEWAY_SMOKE_CONFIG=./kf/smoke.json ./scripts/cluster-smoke.sh`.
