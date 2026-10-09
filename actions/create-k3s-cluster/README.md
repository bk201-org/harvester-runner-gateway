# Create CI k3s cluster

This action builds on [create-ci-cluster](../create-ci-cluster/README.md). It
creates the VMs through harvester-runner-gateway, waits for SSH and cloud-init on
every VM, and installs k3s with the official `get.k3s.io` script over SSH. The
first VM becomes the k3s server. Every other VM joins it as an agent. All VMs
are deleted at the end of the job, including after a failed install.

The runner needs the same setup as `create-ci-cluster` (Node 24 action support,
`ssh-keygen`, the OpenSSH client, HTTPS access to the gateway, and
`id-token: write`). The VM image must provide `curl`, `bash`, SSH access for
`username`, and passwordless `sudo`. The VMs need outbound HTTPS access to
`get.k3s.io` and the k3s release downloads. The runner must reach every VM on
SSH (port 22), and reach the server VM on port 6443 to use the kubeconfig. The
VMs must reach each other on port 6443 and the usual k3s ports.

Inputs are the same as `create-ci-cluster`, plus:

- `k3s-version`: optional release such as `v1.35.2+k3s1`. The stable channel is
  used when empty. Pin a version for reproducible jobs.
- `ssh-timeout-seconds`: shared timeout for SSH and cloud-init on all VMs
  (default 300).
- `k3s-timeout-seconds`: shared timeout for installing k3s and for every node to
  report Ready (default 900).
- `registry-mirrors`: optional list of registry mirrors (for example,
  pull-through caches), in YAML or JSON. Each entry has a `registry` (a host,
  `host:port`, or `*` for every registry) and an `endpoint` (an `http` or
  `https` URL without credentials). Entries for the same registry are combined
  in order. The list is written as
  [`registries.yaml`](https://docs.k3s.io/installation/private-registry) to `/etc/rancher/k3s/registries.yaml` on every node before k3s is installed.
  Registry credentials and TLS settings are not supported.

  ```yaml
  registry-mirrors: |
    - registry: docker.io
      endpoint: http://10.0.10.1:5000
  ```
- `system-default-registry`: optional registry host or `host:port` (no scheme
  or path) that prefixes the k3s system images, for example
  `registry.example.com:5000`. It is written as `system-default-registry` to
  `/etc/rancher/k3s/config.yaml` on every node before k3s is installed.
- `artifact-url`: optional `http` or `https` base URL (no credentials, query, or
  fragment) that replaces `https://github.com/k3s-io/k3s/releases/download`.
  The installer downloads `<artifact-url>/<version>/...` (binaries or tarballs
  and `sha256sum-<arch>.txt`), so the mirror must use the GitHub releases
  layout. It sets `INSTALL_K3S_ARTIFACT_URL` for the installer script, which
  is still downloaded from `get.k3s.io`; the stable channel lookup is unchanged.

The action generates a random join token for each run. It is passed to the VMs
on SSH standard input and is neither logged nor exposed as an output. Each node
is named after its VM ID.

Outputs are `vm-ids` (JSON array, server first), `server-vm-id`,
`kubeconfig-path`, `ssh-config-path`, and `private-key-path`. The kubeconfig is
copied from `/etc/rancher/k3s/k3s.yaml` on the server VM. Its server address is
changed from `127.0.0.1` to the server VM IP. The file has mode 0600 and lives
with the SSH files, so it is deleted in the post-job step. Copy it elsewhere,
for example to an artifact, if it must outlive the job. It contains cluster
admin credentials for a cluster that no longer exists after the job.

```yaml
- id: cluster
  uses: OWNER/REPO/actions/create-k3s-cluster@VERSION
  with:
    gateway-url: https://gateway.example.internal:8443
    vm-count: '3'
    image: default/ubuntu-24-04
    network: default/vm-network
    cpu: '2'
    memory: 4Gi
    boot-disk-size: 20Gi
    username: ubuntu
    k3s-version: v1.35.2+k3s1
    binary-url: https://github.com/OWNER/REPO/releases/download/VERSION/hvst-runner-gw-client-linux-amd64
    binary-sha256: REPLACE_WITH_SHA256_FROM_SHA256SUMS
- run: kubectl --kubeconfig "$KUBECONFIG_PATH" get nodes
  env:
    KUBECONFIG_PATH: ${{ steps.cluster.outputs.kubeconfig-path }}
```

The action runs the `action create-k3s` command of the `hvst-runner-gw-client`
executable, so the release binary and its checksum are the same as for
`create-ci-cluster`. Without `binary-url` or `binary-path`, it downloads the
latest release binary as described there. See that README for building, publishing, and local
testing. This action has no HA or embedded-etcd support and takes no extra k3s
arguments.

## Test locally without GitHub Actions

`scripts/k3s-smoke.sh` runs the `action create-k3s` command from your machine with the
same [smoke configuration](../../README.md#local-shell) as
`scripts/cluster-smoke.sh`. It creates 2 VMs by default (`GATEWAY_VM_COUNT`),
installs k3s, checks that every node is Ready and that the saved kubeconfig
points at the server VM, then deletes the VMs. Set `K3S_VERSION` to pin a
release. Add `artifactURL` or `systemDefaultRegistry` strings or a `registryMirrors` array (entries
with `registry` and `endpoint`)
to the smoke configuration to use registry mirrors. Use `--no-cleanup` to keep the cluster; the script prints the kubeconfig
path, SSH commands, and a `--cleanup TEMP_DIR` command. The VM settings and
`CLUSTER_SMOKE_BINARY` work as in the `create-ci-cluster` script. The VMs need
outbound access to `get.k3s.io`, and this machine must reach them over SSH.
