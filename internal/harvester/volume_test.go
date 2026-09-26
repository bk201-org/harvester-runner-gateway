package harvester

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/config"
	"github.com/bk201-org/harvester-runner-gateway/internal/gateway"
)

func TestPendingAndLiveAttachmentsPreventVolumeDeletion(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	policy := config.RepositoryPolicy{Namespace: "ci"}
	newVolume := func() *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: "runner-gw-volume", Namespace: "ci", Labels: ownerLabels(owner, "volume"),
			Annotations: annotations(time.Now().Add(time.Hour), "hash"),
		}}
	}
	tests := []struct {
		name   string
		object *unstructured.Unstructured
	}{
		{
			name: "pending VM request",
			object: &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
				"metadata": map[string]any{"name": "runner-gw-vm", "namespace": "ci"},
				"status": map[string]any{"volumeRequests": []any{map[string]any{
					"addVolumeOptions": map[string]any{
						"name": "runner-gw-volume",
						"volumeSource": map[string]any{"persistentVolumeClaim": map[string]any{
							"claimName": "runner-gw-volume",
						}},
					},
				}}},
			}},
		},
		{
			name: "live VMI attachment",
			object: &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance",
				"metadata": map[string]any{"name": "runner-gw-vm", "namespace": "ci"},
				"spec": map[string]any{"volumes": []any{map[string]any{
					"name": "runner-gw-volume", "persistentVolumeClaim": map[string]any{
						"claimName": "runner-gw-volume",
					},
				}}},
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
				runtime.NewScheme(),
				map[schema.GroupVersionResource]string{
					vmGVR: "VirtualMachineList", vmiGVR: "VirtualMachineInstanceList",
				},
				test.object,
			)
			kube := kubefake.NewClientset(newVolume())
			backend := &Backend{dynamic: dynamicClient, kube: kube}
			err := backend.DeleteVolume(context.Background(), policy, owner, "runner-gw-volume")
			if !errors.Is(err, gateway.ErrConflict) {
				t.Fatalf("attached volume deletion returned %v, want conflict", err)
			}
		})
	}
}

func TestOfflineDetachPreservesRootDisk(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	vm, err := buildVM("ci", "runner-gw-example", gateway.VMRequest{
		Image: "default/ubuntu", Network: "default/network", CPU: 2,
		Memory: "4Gi", BootDiskSize: "20Gi",
	}, "longhorn", ownerLabels(owner, "vm"), map[string]string{expiresKey: "1000", hashKey: "digest"})
	if err != nil {
		t.Fatal(err)
	}
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	volumes = append(volumes, map[string]any{"name": "runner-gw-extra", "persistentVolumeClaim": map[string]any{"claimName": "runner-gw-extra"}})
	if err := unstructured.SetNestedSlice(vm.Object, volumes, "spec", "template", "spec", "volumes"); err != nil {
		t.Fatal(err)
	}
	disks, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "disks")
	disks = append(disks, map[string]any{"name": "runner-gw-extra", "disk": map[string]any{"bus": "scsi"}})
	if err := unstructured.SetNestedSlice(vm.Object, disks, "spec", "template", "spec", "domain", "devices", "disks"); err != nil {
		t.Fatal(err)
	}
	removed, err := removeOfflineVolume(vm, "runner-gw-extra")
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	remainingVolumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	remainingDisks, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "disks")
	if len(remainingVolumes) != 2 || len(remainingDisks) != 2 {
		t.Fatalf("unexpected volumes/disks after detach: %v %v", remainingVolumes, remainingDisks)
	}
	if removed, err := removeOfflineVolume(vm, "runner-gw-extra"); err != nil || removed {
		t.Fatalf("second detach should be idempotent: removed=%v err=%v", removed, err)
	}
}
