package harvester

import (
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

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
	vm, err := buildVM("ci", "rgw-example", gateway.VMRequest{
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
	if cloudDisk["secretRef"].(map[string]any)["name"] != "rgw-example-init" {
		t.Fatalf("wrong cloud-init secret: %v", cloudDisk)
	}
	var template []map[string]any
	if err := json.Unmarshal([]byte(vm.GetAnnotations()[claimKey]), &template); err != nil || len(template) != 1 {
		t.Fatalf("claim template: %v %v", template, err)
	}
	metadata := template[0]["metadata"].(map[string]any)
	if metadata["name"] != "rgw-example-root" {
		t.Fatalf("wrong root claim: %v", metadata)
	}
	rootAnnotations := metadata["annotations"].(map[string]any)
	if rootAnnotations[imageKey] != "default/ubuntu" {
		t.Fatalf("wrong source image: %v", rootAnnotations)
	}
}
