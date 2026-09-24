# Harvester Runner Gateway

## Goal

Build a standalone Go HTTPS service that lets approved GitHub Actions jobs manage
short-lived Harvester VMs and volumes. Target Harvester v1.7.3. The service owns
the Harvester kubeconfig; jobs never receive cluster credentials.

## API and authorization

- Authenticate each resource request with a GitHub OIDC token for a dedicated
  audience. Verify signature, issuer, audience, time claims, repository ID,
  self-hosted runner environment, and administrator-allowed workflow refs and
  event types. No GitHub PAT is needed.
- Map each approved repository ID to one namespace. Repository ID, run ID, and
  run attempt form the owner; jobs in that run attempt share resources and quota.
- Offer create, list, get, and delete for VMs and volumes; VM power on/off and
  reboot; and volume attach/detach. Generate resource IDs on the server. Creates
  return immediately and clients poll status.
- Constrain VM requests to an approved image and network, CPU, memory, root
  disk size, SSH public keys, cloud-config user data, and TTL. Volumes accept
  size and TTL. No raw Kubernetes manifests are accepted.

## Quota and lifecycle

- Each repository policy has quota.maxActiveVMs (N) and quota.maxActiveVolumes
  (M). The quota owner is one workflow run attempt. Pending and deleting
  resources count until their Kubernetes objects disappear. Deletion frees a
  slot. VM boot disks are covered by N; M counts only volume API resources.
- Keep quota as a separate policy structure to add dimensions later. Return
  current usage and limits from GET /v1/quota. Serialize quota checks and
  creates in a single v1 gateway instance.
- Default TTL is six hours and maximum TTL is 24 hours. Label and annotate all
  managed objects with owner and expiry. A reconciler removes expired VMs,
  root disks, cloud-init Secrets, and standalone volumes. Kubernetes objects
  are the durable state; no separate database is needed.

## Implementation and acceptance

- Configure TLS, kubeconfig, repository policies, namespace, approved images
  and networks, storage class, request size limits, and N/M quota in YAML.
- Use Kubernetes/KubeVirt APIs for VM/PVC creation and VM actions. Clone the
  boot disk from an approved Harvester image. Use SCSI for live volume hotplug.
- Ship OpenAPI, an example config, a container image, and a workflow example
  with id-token: write and a gateway-specific audience.
- Test OIDC rejection, ownership isolation, concurrent quota requests, quota
  release, partial failures, status/actions, and expiry cleanup. Gate a live
  smoke test against a dedicated Harvester v1.7.3 namespace.

GitHub OIDC cannot prove a job ran on this exact scale set. The accepted v1
boundary is an approved repository/workflow on a self-hosted runner.
