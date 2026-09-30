# Create CI VM cluster

This action creates a group of identical VMs through harvester-runner-gateway,
waits until each VM reports a running VMI with a usable IP, and deletes the
recorded VMs at the end of the job. It uses one generated Ed25519 keypair for
all VMs. The SSH config and key exist only on the runner during the job.
The action invokes `hvst-runner-gw-client action create` and
`hvst-runner-gw-client action cleanup`.

Use a Linux self-hosted runner with Node 24 action support, `ssh-keygen`, and
HTTPS access to the gateway. Grant the job `id-token: write`. The gateway must
allow the repository and workflow ref in its policy. The action uses GitHub
OIDC for both creation and post-job deletion.

Build release assets with `make cluster-release`. The release workflow uploads
them, with `SHA256SUMS`, to each published GitHub release. Choose the binary
source with one of:

- Nothing (default): the action downloads the latest release of the repository
  that hosts the action for the runner architecture (linux amd64 or arm64) and
  verifies it against the release `SHA256SUMS`. Pin the action to a release tag
  and set `binary-url` if the binary must match the action version.
- `binary-url` with the matching `binary-sha256` for the runner architecture.
- `binary-path` with a local executable, such as one built earlier in the same
  job (`binary-sha256` is optional there and checked when given).

`binary-url` and `binary-path` are mutually exclusive. The
[example workflow](../../examples/cluster-workflow.yml) contains placeholders
for the future GitHub repository, release tag, and digest, and
[action test workflow](../../.github/workflows/test-action-create-ci-cluster.yml)
builds the binary locally and SSHes into the created VMs.

Inputs `gateway-url`, `vm-count`, `image`, `network`, `cpu`, `memory`,
`boot-disk-size`, and `username` are required. Optional inputs are `binary-url`,
`binary-path`, `binary-sha256` (see above), `ttl-seconds` (gateway default six
hours), `user-data`,
`wait-timeout-seconds` (default 600), `audience` (default
`api://harvester-runner-gateway`), and `ca-cert-path`.

The optional `user-data` value must start with `#cloud-config` and be a YAML
mapping. The action preserves compatible settings, adds its public key to the
requested user, and rejects conflicting user definitions. The merged config is
the same for every VM. The action checks readiness through the gateway; guest
cloud-init and SSH may still be starting after it returns.

Outputs are `vm-ids` (JSON array), `ssh-config-path`, and `private-key-path`.
Use `ssh -F "$SSH_CONFIG" "$VM_ID"` in a later step on the same runner. Each VM
ID is a host alias in the config. The config uses `StrictHostKeyChecking
accept-new` with a dedicated known-hosts file.

On a failed create or readiness timeout, the post-job step still attempts to
delete every VM whose ID was returned and recorded. If the runner disappears or
a create response is lost before its ID can be recorded, the gateway's TTL
cleanup is the fallback.

## Authentication failures

A gateway HTTP 401 includes a rejection reason and a configuration hint.
For `repository_not_allowed`, ask the gateway administrator to check that
`repositories[].repositoryID` contains the numeric GitHub ID of the repository
running the workflow (available as `github.repository_id`), rather than the
repository hosting this action. For `workflow_not_allowed`, check the full
workflow ref, including its branch or tag suffix, in `allowedWorkflowRefs`.
For `event_not_allowed`, check `allowedEvents`.

The gateway supplies these diagnostics; deploy the updated gateway to make
them visible in action logs.

## Test locally without GitHub Actions

Run `make test-cluster-action` from the repository root. This exercises the
Go command against an in-process HTTPS gateway with a local token, plus the
cluster and launcher tests. It requires Go, Node.js, and OpenSSH client tools,
but does not contact your gateway or create VMs.

To smoke test the command against your real gateway without a GitHub Actions
job, reuse the [local smoke configuration](../../README.md#local-shell). For
example, run this from the repository root with `kf/smoke.json`:

```sh
GATEWAY_SMOKE_CONFIG=./kf/smoke.json ./scripts/cluster-smoke.sh
```

The script reads `gatewayURL`, `image`, `network`, `tokenFile`, and optional
`caCert` from that file. Without `GATEWAY_SMOKE_CONFIG`, it uses
`${XDG_CONFIG_HOME:-$HOME/.config}/harvester-runner-gateway/smoke.json`, the
same location as `./scripts/gateway-smoke.sh`.

The script builds and invokes `hvst-runner-gw-client action create`, reports
progress and the returned VM IDs, then checks the key and SSH config. It invokes
`action cleanup` on exit, including after a failed create. Run with `--no-cleanup` to
keep the VMs and SSH files; the script prints SSH and manual cleanup commands.
The cleanup command uses the saved state and removes the saved files on success.
Keep the printed temp directory until you run it. The VMs still expire according
to `GATEWAY_TTL_SECONDS` (default 1800 seconds).

It creates one real VM by default. Set `GATEWAY_VM_COUNT`,
`GATEWAY_USERNAME`, `GATEWAY_CPU`, `GATEWAY_MEMORY`, `GATEWAY_BOOT_DISK_SIZE`,
`GATEWAY_TTL_SECONDS`, or `GATEWAY_USER_DATA_FILE` to override the defaults.
Set `CLUSTER_SMOKE_BINARY` to test an existing executable instead of building
from source. The script requires `jq` as well.
