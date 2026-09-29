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
    binary-url: https://github.com/OWNER/REPO/releases/download/VERSION/hvst-runner-gw-cluster-linux-amd64
    binary-sha256: REPLACE_WITH_SHA256_FROM_SHA256SUMS
- run: kubectl --kubeconfig "$KUBECONFIG_PATH" get nodes
  env:
    KUBECONFIG_PATH: ${{ steps.cluster.outputs.kubeconfig-path }}
```

The action runs the `create-k3s` command of the `hvst-runner-gw-cluster`
executable, so the release binary and its checksum are the same as for
`create-ci-cluster`. See that README for building, publishing, and local
testing. This action has no HA or embedded-etcd support and takes no extra k3s
arguments.
