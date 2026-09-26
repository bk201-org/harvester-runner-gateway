package harvester

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/gateway"
)

func TestRenderCloudConfigMergesSSHKeys(t *testing.T) {
	input := "#cloud-config\npackage_update: true\n"
	got, err := renderCloudConfig(input, []string{"ssh-ed25519 AAAATEST"}, "ubuntu")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#cloud-config", "package_update: true", "user: ubuntu", "ssh_authorized_keys:", "ssh-ed25519 AAAATEST"} {
		if !strings.Contains(got, want) {
			t.Fatalf("cloud-config is missing %q: %s", want, got)
		}
	}
	if _, err := renderCloudConfig("#cloud-config\nssh_authorized_keys: []\n", []string{"ssh-ed25519 AAAATEST"}, "ubuntu"); err == nil {
		t.Fatal("expected conflicting SSH key field to be rejected")
	}
	if _, err := renderCloudConfig("#cloud-config\nuser: root\n", []string{"ssh-ed25519 AAAATEST"}, "ubuntu"); err == nil {
		t.Fatal("expected conflicting SSH user to be rejected")
	}
}

func TestBuildVMUsesImageCloneAndCloudInitSecret(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	labels := ownerLabels(owner, "vm")
	vm, err := buildVM("ci", "runner-gw-example", gateway.VMRequest{
		Image: "default/ubuntu", Network: "default/vm-network", CPU: 2,
		Memory: "4Gi", BootDiskSize: "20Gi",
	}, "longhorn", labels, map[string]string{expiresKey: "1000", hashKey: "digest"})
	if err != nil {
		t.Fatal(err)
	}
	strategy, found, err := unstructured.NestedString(vm.Object, "spec", "runStrategy")
	if err != nil || !found || strategy != "Manual" {
		t.Fatalf("runStrategy=%q found=%v err=%v", strategy, found, err)
	}
	networks, _, err := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "networks")
	if err != nil || len(networks) != 1 {
		t.Fatalf("networks=%v err=%v", networks, err)
	}
	volumes, _, err := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	if err != nil || len(volumes) != 2 {
		t.Fatalf("volumes=%v err=%v", volumes, err)
	}
	cloudDisk := volumes[1].(map[string]any)["cloudInitNoCloud"].(map[string]any)
	if cloudDisk["secretRef"].(map[string]any)["name"] != "runner-gw-example-init" {
		t.Fatalf("wrong cloud-init secret: %v", cloudDisk)
	}
	var template []map[string]any
	if err := json.Unmarshal([]byte(vm.GetAnnotations()[claimKey]), &template); err != nil || len(template) != 1 {
		t.Fatalf("claim template: %v %v", template, err)
	}
	metadata := template[0]["metadata"].(map[string]any)
	if metadata["name"] != "runner-gw-example-root" {
		t.Fatalf("wrong root claim: %v", metadata)
	}
	rootAnnotations := metadata["annotations"].(map[string]any)
	if rootAnnotations[imageKey] != "default/ubuntu" {
		t.Fatalf("wrong source image: %v", rootAnnotations)
	}
	if vm.GetLabels()[repoLabel] != owner.RepositoryID || vm.GetAnnotations()[expiresKey] != "1000" || vm.GetAnnotations()[hashKey] != "digest" {
		t.Fatalf("wrong gateway metadata: labels=%v annotations=%v", vm.GetLabels(), vm.GetAnnotations())
	}
}

func TestValidIDAcceptsCurrentAndLegacyNames(t *testing.T) {
	for _, id := range []string{"runner-gw-u6bpqc4qo7b6k35d", "hrgw-u6bpqc4qo7b6k35d", "rgw-u6bpqc4qo7b6k35diocof63v"} {
		if !validID(id) {
			t.Fatalf("valid gateway ID rejected: %s", id)
		}
	}
	if validID("other-u6bpqc4qo7b6k35d") {
		t.Fatal("unrelated ID accepted")
	}
}

func TestVMStatusListsCurrentAndLegacyVolumes(t *testing.T) {
	vm := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "runner-gw-example"},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"volumes": []any{
				map[string]any{"persistentVolumeClaim": map[string]any{"claimName": "runner-gw-example-root"}},
				map[string]any{"persistentVolumeClaim": map[string]any{"claimName": "runner-gw-new-volume"}},
				map[string]any{"persistentVolumeClaim": map[string]any{"claimName": "hrgw-old-volume"}},
				map[string]any{"persistentVolumeClaim": map[string]any{"claimName": "rgw-old-volume"}},
			},
		}}},
	}}
	backend := &Backend{dynamic: fake.NewSimpleDynamicClient(runtime.NewScheme())}
	status, err := backend.vmStatus(context.Background(), "ci", vm)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.AttachedVolumeIDs) != 3 || status.AttachedVolumeIDs[0] != "runner-gw-new-volume" || status.AttachedVolumeIDs[1] != "hrgw-old-volume" || status.AttachedVolumeIDs[2] != "rgw-old-volume" {
		t.Fatalf("attached volumes = %v", status.AttachedVolumeIDs)
	}
	if status.Ready || len(status.IPAddresses) != 0 || status.PowerState != "off" {
		t.Fatalf("missing VMI status = %+v", status)
	}
}

func TestVMStatusReadinessRequiresRunningVMIWithUsableNICAddress(t *testing.T) {
	tests := []struct {
		name       string
		phase      string
		interfaces []any
		wantReady  bool
		wantIPs    []string
	}{
		{
			name:  "running with normalized IPv4 and IPv6",
			phase: "Running",
			interfaces: []any{
				map[string]any{"name": "other", "ipAddress": "10.0.0.99"},
				map[string]any{"name": "nic-1", "ipAddress": "10.0.0.10", "ipAddresses": []any{
					"fe80::1", "2001:db8::10", "10.0.0.10", "bad-address", "127.0.0.1", "224.0.0.1",
				}},
			},
			wantReady: true,
			wantIPs:   []string{"10.0.0.10", "2001:db8::10"},
		},
		{
			name:       "non-running VMI retains usable IP but is not ready",
			phase:      "Pending",
			interfaces: []any{map[string]any{"name": "nic-1", "ipAddress": "10.0.0.10"}},
			wantIPs:    []string{"10.0.0.10"},
		},
		{
			name:       "running without an IP",
			phase:      "Running",
			interfaces: []any{map[string]any{"name": "nic-1"}},
			wantIPs:    []string{},
		},
		{
			name:  "running with only unusable addresses",
			phase: "Running",
			interfaces: []any{map[string]any{"name": "nic-1", "ipAddresses": []any{
				"", "not-an-ip", "0.0.0.0", "::", "169.254.1.1", "fe80::1", "::1", "ff02::1",
			}}},
			wantIPs: []string{},
		},
		{
			name:       "running with an IP only on another interface",
			phase:      "Running",
			interfaces: []any{map[string]any{"name": "other", "ipAddress": "10.0.0.10"}},
			wantIPs:    []string{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			vm := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
				"metadata": map[string]any{"name": "runner-gw-example", "namespace": "ci"},
				"status":   map[string]any{"printableStatus": "Running"},
			}}
			vmi := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance",
				"metadata": map[string]any{"name": "runner-gw-example", "namespace": "ci"},
				"status":   map[string]any{"phase": test.phase, "interfaces": test.interfaces},
			}}
			backend := &Backend{dynamic: fake.NewSimpleDynamicClient(runtime.NewScheme(), vmi)}
			status, err := backend.vmStatus(context.Background(), "ci", vm)
			if err != nil {
				t.Fatal(err)
			}
			if status.Ready != test.wantReady || !reflect.DeepEqual(status.IPAddresses, test.wantIPs) {
				t.Fatalf("ready=%v IPs=%v, want ready=%v IPs=%v", status.Ready, status.IPAddresses, test.wantReady, test.wantIPs)
			}
		})
	}
}
