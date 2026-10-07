# Create CI RKE2 cluster

This action builds on [create-ci-cluster](../create-ci-cluster/README.md). It
creates the VMs through harvester-runner-gateway, waits for SSH and cloud-init on
every VM, and installs [RKE2](https://docs.rke2.io/) with the official
`get.rke2.io` script over SSH. The first VM becomes the RKE2 server. Every other
VM joins it as an agent. All VMs are deleted at the end of the job, including
after a failed install.

The runner needs the same setup as `create-ci-cluster` (Node 24 action support,
`ssh-keygen`, the OpenSSH client, HTTPS access to the gateway, and
`id-token: write`). The VM image must use systemd and provide `curl`, `bash`,
SSH access for `username`, and passwordless `sudo`. The VMs need outbound HTTPS
access to `get.rke2.io`, the RKE2 release downloads, and the container
registries for the RKE2 system images. The runner must reach every VM on SSH
(port 22), and reach the server VM on port 6443 to use the kubeconfig. The VMs
must reach each other on the
[RKE2 ports](https://docs.rke2.io/install/requirements#inbound-network-rules),
including 9345 and 6443 on the server, 10250, and 8472/udp for the default
Canal CNI.

RKE2 needs more resources than k3s. Use at least 2 CPUs and 4Gi of memory per
VM (8Gi is more comfortable for the server) and a boot disk of at least 40Gi.

Inputs are the same as `create-ci-cluster`, plus:

- `rke2-version`: optional release such as `v1.35.2+rke2r1`. The stable channel
  is used when empty. Pin a version for reproducible jobs.
- `ssh-timeout-seconds`: shared timeout for SSH and cloud-init on all VMs
  (default 300).
- `rke2-timeout-seconds`: shared timeout for installing RKE2 and for every node
  to report Ready (default 1200).

The action generates a random join token for each run. It is written to
`/etc/rancher/rke2/config.yaml` (mode 0600) through SSH standard input and is
neither logged nor exposed as an output. Each node is named after its VM ID.
The installer does not start RKE2, so the action runs
`systemctl enable --now rke2-server.service` (or `rke2-agent.service`), which
waits until the node has started.

Outputs are `vm-ids` (JSON array, server first), `server-vm-id`,
`kubeconfig-path`, `ssh-config-path`, and `private-key-path`. The kubeconfig is
copied from `/etc/rancher/rke2/rke2.yaml` on the server VM. Its server address
is changed from `127.0.0.1` to the server VM IP. The file has mode 0600 and
lives with the SSH files, so it is deleted in the post-job step. Copy it
elsewhere, for example to an artifact, if it must outlive the job. It contains
cluster admin credentials for a cluster that no longer exists after the job.

```yaml
- id: cluster
  uses: OWNER/REPO/actions/create-rke2-cluster@VERSION
  with:
    gateway-url: https://gateway.example.internal:8443
    vm-count: '3'
    image: default/ubuntu-24-04
    network: default/vm-network
    cpu: '2'
    memory: 8Gi
    boot-disk-size: 40Gi
    username: ubuntu
    rke2-version: v1.35.2+rke2r1
    binary-url: https://github.com/OWNER/REPO/releases/download/VERSION/hvst-runner-gw-client-linux-amd64
    binary-sha256: REPLACE_WITH_SHA256_FROM_SHA256SUMS
- run: kubectl --kubeconfig "$KUBECONFIG_PATH" get nodes
  env:
    KUBECONFIG_PATH: ${{ steps.cluster.outputs.kubeconfig-path }}
```

On the VMs, RKE2 ships its own `kubectl` at
`/var/lib/rancher/rke2/bin/kubectl`. Use it with
`--kubeconfig /etc/rancher/rke2/rke2.yaml` when the runner has no `kubectl`.

The action runs the `action create-rke2` command of the `hvst-runner-gw-client`
executable, so the release binary and its checksum are the same as for
`create-ci-cluster`. Without `binary-url` or `binary-path`, it downloads the
latest release binary as described there. See that README for building,
publishing, and local testing. This action has no HA support and takes no extra
RKE2 configuration.

## Test locally without GitHub Actions

`scripts/rke2-smoke.sh` runs the `action create-rke2` command from your machine
with the same [smoke configuration](../../README.md#local-shell) as
`scripts/cluster-smoke.sh`. It creates 2 VMs by default (`GATEWAY_VM_COUNT`)
with 8Gi of memory and a 40Gi boot disk, installs RKE2, checks that every node
is Ready and that the saved kubeconfig points at the server VM, then deletes
the VMs. Set `RKE2_VERSION` to pin a release, and `RKE2_SSH_TIMEOUT_SECONDS` or
`RKE2_TIMEOUT_SECONDS` to change the timeouts. Use `--no-cleanup` to keep the
cluster; the script prints the kubeconfig path, SSH commands, and a
`--cleanup TEMP_DIR` command. The VM settings and `CLUSTER_SMOKE_BINARY` work as
in the `create-ci-cluster` script. The VMs need outbound access to
`get.rke2.io`, and this machine must reach them over SSH.
