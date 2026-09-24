package harvester

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/gateway"
)

func TestOfflineDetachPreservesRootDisk(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	vm, err := buildVM("ci", "rgw-example", gateway.VMRequest{
		Image: "default/ubuntu", Network: "default/network", CPU: 2,
		Memory: "4Gi", BootDiskSize: "20Gi",
	}, "longhorn", ownerLabels(owner, "vm"), map[string]string{expiresKey: "1000", hashKey: "digest"})
	if err != nil {
		t.Fatal(err)
	}
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	volumes = append(volumes, map[string]any{"name": "rgw-extra", "persistentVolumeClaim": map[string]any{"claimName": "rgw-extra"}})
	if err := unstructured.SetNestedSlice(vm.Object, volumes, "spec", "template", "spec", "volumes"); err != nil {
		t.Fatal(err)
	}
	disks, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "disks")
	disks = append(disks, map[string]any{"name": "rgw-extra", "disk": map[string]any{"bus": "scsi"}})
	if err := unstructured.SetNestedSlice(vm.Object, disks, "spec", "template", "spec", "domain", "devices", "disks"); err != nil {
		t.Fatal(err)
	}
	removed, err := removeOfflineVolume(vm, "rgw-extra")
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	remainingVolumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	remainingDisks, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "disks")
	if len(remainingVolumes) != 2 || len(remainingDisks) != 2 {
		t.Fatalf("unexpected volumes/disks after detach: %v %v", remainingVolumes, remainingDisks)
	}
	if removed, err := removeOfflineVolume(vm, "rgw-extra"); err != nil || removed {
		t.Fatalf("second detach should be idempotent: removed=%v err=%v", removed, err)
	}
}
