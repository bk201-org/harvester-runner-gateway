# Harvester Runner Gateway

## Goal and boundary

The gateway is a standalone Go HTTPS service that lets approved GitHub Actions
jobs manage short-lived Harvester VMs and independent volumes. It targets
Harvester v1.7.3 and owns the Harvester kubeconfig; jobs receive neither cluster
credentials nor a GitHub PAT.

GitHub OIDC proves the approved repository, workflow, event, and self-hosted
runner environment. It cannot prove that a job ran on one particular scale set.
That is the accepted v1 security boundary.

## Authentication, ownership, and policy

- Authenticate resource requests with a GitHub OIDC token for a dedicated
  audience. Verify its signature, issuer, audience, time claims, numeric
  repository ID, numeric run ID, numeric run attempt, self-hosted runner
  environment, workflow ref, and event type.
- Map each configured repository ID to a namespace and policy containing
  approved images and networks, storage class, guest defaults, size limits,
  TTL limits, and active-resource quotas.
- Treat repository ID, run ID, and run attempt as the resource owner. Jobs in
  one run attempt share resources. All runs and attempts in a repository share
  that repository's quota.
- Optionally authenticate local smoke requests with a protected token file.
  These requests use the reserved owner `<repository-id>/local-smoke/1` and
  remain isolated from workflow-run resources while sharing repository quota.
- Authorize every operation from Kubernetes ownership metadata. A resource
  name alone never grants access.

## API and resource model

The API provides:

- VM create, list, get, delete, power on/off, and reboot.
- Independent-volume create, list, get, and delete.
- Live volume attach and detach.
- Repository quota usage and limits.
- Process liveness and Harvester readiness endpoints.

Create requests are asynchronous. The API returns the Kubernetes resource
status immediately, and clients poll GET endpoints. The bundled client waits
for a VM by default until its VMI is `Running` and the primary interface reports
a usable IP address.

VM inputs are limited to an approved image and network, CPU, memory, boot disk
size, SSH public keys, cloud-config user data, and TTL. Independent volumes
accept size and TTL. The API never accepts raw Kubernetes manifests.

## Resource names

VMs and independent volumes use the same public ID shape:

```text
ci-<repository-id>-<run-id>-a<attempt>-<sequence>
ci-123456789-1658821493-a2-001
```

- Use the authenticated repository ID, run ID, and attempt without shortening
  them.
- Maintain independent VM and volume sequences for each
  namespace/repository/run/attempt. Both sequences start at 1, so a VM and an
  independent volume may have the same ID. Their API endpoint and Kubernetes
  kind provide object identity.
- Render sequences with at least three digits: `001` through `999`, then
  `1000` and higher. Never wrap or truncate a sequence. Failed allocations may
  leave gaps.
- Name a VM's boot PVC `<id>-root` and cloud-init Secret `<id>-init`. These
  dependencies use the VM allocation and consume no sequence numbers.
- Local smoke resources use names such as
  `ci-123456789-local-smoke-a1-001`.
- Validate owner components and the complete generated name before reserving a
  sequence. A public ID is at most 58 characters, leaving five characters for
  a dependent suffix within the 63-character DNS label limit. Reject invalid,
  oversized, or exhausted names instead of shortening identifiers.

Only this `ci-` public ID scheme is valid for gateway operations. Attached
volume reporting includes valid independent-volume IDs and excludes a VM's root
PVC.

## Kubernetes metadata

All managed objects carry
`app.kubernetes.io/managed-by=harvester-runner-gateway` and the current owner
and kind labels:

- `runner-gw-repository-id`
- `runner-gw-run-id`
- `runner-gw-run-attempt`
- `runner-gw-kind`

Recovery and lifecycle annotations are:

- `runner-gw-expires-at`
- `runner-gw-request-hash`
- `runner-gw-identity-hash`

The identity hash is the full SHA-256 digest of a canonical encoding of the
owner, resource kind, and idempotency key. The request hash is a separate
SHA-256 digest of the normalized request body. Different idempotency keys may
have identical request bodies, so the request hash cannot identify an
allocation.

Write both hashes and owner/kind/expiry metadata on VMs, independent PVCs, VM
boot PVC templates, and cloud-init Secrets. Put the VM recovery metadata on the
VMI template as well. Boot PVCs, Secrets, and VMIs carry their parent VM's
identity and do not create independent allocations. Never persist raw
idempotency keys, credentials, or request bodies, and never put a hash in the
public name.

## In-memory allocator and operation gate

The allocator keeps:

- A high-water mark for each
  namespace/repository/run/attempt/resource-kind scope.
- A mapping from that scope plus identity hash to its public ID.

Allocation state lives in memory and is reconstructed from Kubernetes objects
at startup. The design uses no ConfigMaps, allocation CRDs, separate database,
or additional RBAC verbs.

One operation gate serializes recovery, allocation, quota checks, creates,
mutating actions, deletes, and expiry cleanup. Exactly one active gateway
instance is required because the in-memory allocator and quota checks do not
coordinate multiple writers.

Counters and identity mappings remain for the process lifetime. Deleting or
expiring resources does not lower a counter or remove its mapping. A same-key
request can therefore recreate its mapped name during that process lifetime
when all Kubernetes objects for it are gone.

## Create and retry behavior

For each create request, the gateway:

1. Authenticates the caller, validates and normalizes the request, computes the
   identity and request hashes, and validates owner/name components.
2. Enters the operation gate and looks up the identity mapping.
3. If mapped, reads the recorded resource. A surviving resource must have the
   expected owner, identity hash, and request hash. A compatible retry returns
   it; a different normalized request returns `409 idempotency_conflict`.
   This lookup precedes quota checks so retries work at full quota.
4. Performs cluster-backed VM validation and checks active repository quota
   before reserving a new number. Validation and quota rejection consume no
   sequence.
5. Reuses a mapped ID or validates and reserves the next ID, then writes the
   workload with recovery metadata in its initial Kubernetes objects.
6. On an ambiguous Kubernetes write, rereads the resource and dependencies and
   verifies their metadata before retrying or rolling back. It retains the
   in-memory reservation if the result cannot be established.

A compatible orphaned Secret, root PVC, or VMI lets a partial VM creation
continue with the recovered ID. A dependency that is deleting or has
incompatible owner, identity, or request metadata causes a conflict. An
unrelated object occupying a reserved name is never adopted or modified.
Rollback never deletes an unrelated occupied object. After an ambiguous write,
it retains dependencies when a VM may have been confirmed despite a lost
response.

## Startup recovery

After Harvester preflight, the gateway constructs the server and allocator,
recovers allocation state, runs initial expiry cleanup through the operation
gate, and only then starts serving requests.

Recovery deduplicates configured namespaces and completes every page while
listing managed VMs, VMIs, PVCs, and cloud-init Secrets. It reads object
metadata directly and does not require guest readiness or per-VM status calls.
Expired and deleting objects remain occupied until Kubernetes removes them.

For each supported object, recovery:

- Validates the `ci-` name, current owner and kind labels, expiry, request hash,
  and identity hash.
- Derives parent VM IDs from `-root` and `-init` names.
- Treats VMs, VMIs, roots, and Secrets as observations of their parent VM
  allocation; only independent-volume PVCs advance volume scopes.
- Recovers every identity-to-ID mapping and sets each scope to its highest
  surviving sequence.
- Allows a VM and independent volume with the same ID because their allocation
  kinds are separate.

Recovery rejects conflicting identities for one scoped ID, multiple IDs for one
scoped identity, inconsistent request hashes, mismatched owner/name fields,
missing metadata, malformed current names, and list failures. Reconstructed
maps are published only after the complete scan succeeds. A later rescan merges
state without lowering an existing high-water mark or discarding in-memory
reservations.

## Restart semantics

Only surviving Kubernetes objects can reconstruct allocation state after a
restart:

- Deleted sequences and memory-only reservations may be reused. If `001` and
  `002` survive after `003` is deleted, a new allocation may receive `003`.
- An empty scope restarts at `001`.
- A retry whose resource and all dependencies are gone may receive a different
  ID. A surviving resource or partial create retains its recovered ID.
- A saved ID may eventually identify a different resource in the same owner
  scope after restart and reuse; IDs are not permanent historical identifiers.
- A deleted resource imposes no permanent request-body lock. Request conflicts
  are checked against surviving resources and partial-create objects.
- Allocator memory grows with distinct identities during one process lifetime.
  Restart naturally rebuilds only surviving allocations; there is no silent
  in-process eviction policy or separate allocation-history cleanup.

## Quota and lifecycle

Each repository policy defines `quota.maxActiveVMs` and
`quota.maxActiveVolumes`. Quota spans every run and attempt for the repository.
Pending and deleting objects count until they disappear. VM boot disks are
covered by VM quota; volume quota counts only independent-volume PVCs. Deletion
frees the active slot but does not reduce the allocator's in-memory high-water
mark.

The default TTL is six hours and the maximum is 24 hours. Every minute, cleanup
removes expired VMs, root PVCs, cloud-init Secrets, and independent volumes.
An attached volume is detached before deletion. Deleting a VM does not delete
its independent volumes. Kubernetes workloads and their recovery metadata are
the durable state; no separate backup of allocation history is required.

## Harvester integration and status

- Use Kubernetes and KubeVirt APIs for VM, VMI, PVC, Secret, and VM action
  operations.
- Clone each boot disk from an approved Harvester image and use SCSI for live
  independent-volume hotplug.
- Resolve status from live Kubernetes objects rather than an in-memory cache.
  VM readiness requires a running VMI and a usable primary-interface address.
- Keep ownership isolation for list, get, quota, attachment, cleanup, and direct
  mutation paths.
- Require namespace read; VM/VMI get and list; VM create, update, and delete;
  PVC get, list, create, and delete; Secret get, list, create, update, and
  delete; image, network, and storage-class reads; and the required KubeVirt VM
  subresources.
  Allocation adds no ConfigMap permission.

## Configuration, delivery, and verification

Configuration is YAML and covers TLS, kubeconfig and context, OIDC settings,
optional local smoke credentials, repository policies, namespaces, approved
images and networks, storage classes, guest defaults, size limits, and quotas.
The project ships its OpenAPI definition, example configuration, container
image, client, workflow example with `id-token: write`, and smoke tooling.

Automated coverage includes authentication rejection, ownership isolation,
sequential naming and width changes, independent scopes and kinds, same-key and
distinct-key concurrency, request conflicts, validation and quota rejection
before reservation, counter lifetime, recovery and conflict detection,
injected list/write failures, current metadata propagation, quota behavior,
attached-volume reporting, status/actions, rollback, and expiry cleanup.

Repository verification runs:

```text
make test
make vet
go test -race ./internal/gateway ./internal/harvester
```

A live test uses a dedicated Harvester namespace and an identity without
ConfigMap permissions. It creates multiple VMs and volumes, verifies independent
numbering and same-string VM/volume IDs, restarts the gateway, retries keys,
exercises attach/detach and deletion, checks TTL cleanup, and confirms actual
boot PVCs and VMIs contain recovery metadata.
