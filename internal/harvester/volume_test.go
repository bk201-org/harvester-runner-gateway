package harvester

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/config"
	"github.com/bk201-org/harvester-runner-gateway/internal/gateway"
)

func TestPendingAndLiveAttachmentsPreventVolumeDeletion(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	policy := config.RepositoryPolicy{RepositoryID: owner.RepositoryID, Namespace: "ci"}
	vmID := testResourceID(owner, 1)
	volumeID := testResourceID(owner, 1)
	newVolume := func() *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: volumeID, Namespace: "ci", Labels: ownerLabels(owner, "volume"),
			Annotations: annotations(time.Now().Add(time.Hour)),
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
				"metadata": map[string]any{"name": vmID, "namespace": "ci"},
				"status": map[string]any{"volumeRequests": []any{map[string]any{
					"addVolumeOptions": map[string]any{
						"name": volumeID,
						"volumeSource": map[string]any{"persistentVolumeClaim": map[string]any{
							"claimName": volumeID,
						}},
					},
				}}},
			}},
		},
		{
			name: "live VMI attachment",
			object: &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance",
				"metadata": map[string]any{"name": vmID, "namespace": "ci"},
				"spec": map[string]any{"volumes": []any{map[string]any{
					"name": volumeID, "persistentVolumeClaim": map[string]any{
						"claimName": volumeID,
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
			err := backend.DeleteVolume(context.Background(), policy, owner, volumeID)
			if !errors.Is(err, gateway.ErrConflict) {
				t.Fatalf("attached volume deletion returned %v, want conflict", err)
			}
		})
	}
}

func TestOfflineDetachPreservesRootDisk(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	vmID := testResourceID(owner, 1)
	volumeID := testResourceID(owner, 2)
	vm, err := buildVM("ci", vmID, gateway.VMRequest{
		Image: "default/ubuntu", Network: "default/network", CPU: 2,
		Memory: "4Gi", BootDiskSize: "20Gi",
	}, "longhorn", ownerLabels(owner, "vm"), map[string]string{expiresKey: "1000"})
	if err != nil {
		t.Fatal(err)
	}
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	volumes = append(volumes, map[string]any{"name": volumeID, "persistentVolumeClaim": map[string]any{"claimName": volumeID}})
	if err := unstructured.SetNestedSlice(vm.Object, volumes, "spec", "template", "spec", "volumes"); err != nil {
		t.Fatal(err)
	}
	disks, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "disks")
	disks = append(disks, map[string]any{"name": volumeID, "disk": map[string]any{"bus": "scsi"}})
	if err := unstructured.SetNestedSlice(vm.Object, disks, "spec", "template", "spec", "domain", "devices", "disks"); err != nil {
		t.Fatal(err)
	}
	removed, err := removeOfflineVolume(vm, volumeID)
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	remainingVolumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	remainingDisks, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "disks")
	if len(remainingVolumes) != 2 || len(remainingDisks) != 2 {
		t.Fatalf("unexpected volumes/disks after detach: %v %v", remainingVolumes, remainingDisks)
	}
	if removed, err := removeOfflineVolume(vm, volumeID); err != nil || removed {
		t.Fatalf("second detach should be idempotent: removed=%v err=%v", removed, err)
	}
}

func TestOfflineDetachRetriesVMConflict(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	policy := config.RepositoryPolicy{RepositoryID: owner.RepositoryID, Namespace: "ci"}
	vmID := testResourceID(owner, 1)
	volumeID := testResourceID(owner, 2)
	vm, err := buildVM("ci", vmID, gateway.VMRequest{
		Image: "default/ubuntu", Network: "default/network", CPU: 2,
		Memory: "4Gi", BootDiskSize: "20Gi",
	}, "longhorn", ownerLabels(owner, "vm"), annotations(time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	volumes = append(volumes, map[string]any{"name": volumeID, "persistentVolumeClaim": map[string]any{"claimName": volumeID}})
	if err := unstructured.SetNestedSlice(vm.Object, volumes, "spec", "template", "spec", "volumes"); err != nil {
		t.Fatal(err)
	}
	disks, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "disks")
	disks = append(disks, map[string]any{"name": volumeID, "disk": map[string]any{"bus": "scsi"}})
	if err := unstructured.SetNestedSlice(vm.Object, disks, "spec", "template", "spec", "domain", "devices", "disks"); err != nil {
		t.Fatal(err)
	}
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			vmGVR: "VirtualMachineList", vmiGVR: "VirtualMachineInstanceList",
		},
		vm,
	)
	updates := 0
	dynamicClient.PrependReactor("update", "virtualmachines", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: vmGVR.Group, Resource: vmGVR.Resource}, vmID,
				errors.New("the object has been modified"),
			)
		}
		return false, nil, nil
	})
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: volumeID, Namespace: "ci", Labels: ownerLabels(owner, "volume"),
	}}
	backend := &Backend{dynamic: dynamicClient, kube: kubefake.NewClientset(pvc)}

	if err := backend.DetachVolume(context.Background(), policy, owner, vmID, volumeID); err != nil {
		t.Fatal(err)
	}
	if updates != 2 {
		t.Fatalf("VM updates = %d, want 2", updates)
	}
	got, err := dynamicClient.Resource(vmGVR).Namespace("ci").Get(context.Background(), vmID, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, claim := range vmClaimNames(got) {
		if claim == volumeID {
			t.Fatalf("volume %s is still attached", volumeID)
		}
	}
}
