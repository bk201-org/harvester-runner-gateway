# Create CI VM cluster

This action creates a group of identical VMs through harvester-runner-gateway,
waits until each VM reports a running VMI with a usable IP, and deletes the
recorded VMs at the end of the job. It uses one generated Ed25519 keypair for
all VMs. The SSH config and key exist only on the runner during the job.

Use a Linux self-hosted runner with Node 24 action support, `ssh-keygen`, and
HTTPS access to the gateway. Grant the job `id-token: write`. The gateway must
allow the repository and workflow ref in its policy. The action uses GitHub
OIDC for both creation and post-job deletion.

Build release assets with `make cluster-release`. Publish both executables and
`SHA256SUMS` under a release tag. Supply the URL and the matching SHA-256 of the
binary for the runner architecture. The [example workflow](../../examples/cluster-workflow.yml)
contains placeholders for the future GitHub repository, release tag, and digest.

Inputs `gateway-url`, `vm-count`, `image`, `network`, `cpu`, `memory`,
`boot-disk-size`, `username`, `binary-url`, and `binary-sha256` are required.
Optional inputs are `ttl-seconds` (gateway default six hours), `user-data`,
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
