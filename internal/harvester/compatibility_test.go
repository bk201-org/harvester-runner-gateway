package harvester

import (
	"context"
	"strconv"
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
)

func TestLegacyAndCurrentResourcesRemainReadable(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	other := auth.Owner{RepositoryID: "123", RunID: "789", RunAttempt: "1"}
	expires := time.Unix(12345, 0).UTC()
	legacyAnnotations := map[string]string{legacyExpiresKey: strconv.FormatInt(expires.Unix(), 10), legacyHashKey: "old-hash"}
	vm := func(name string, labels, values map[string]string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
			"metadata": map[string]any{"name": name, "namespace": "ci", "labels": stringMap(labels), "annotations": stringMap(values)},
		}}
	}
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{vmGVR: "VirtualMachineList", vmiGVR: "VirtualMachineInstanceList"},
		vm("hrgw-old", legacyOwnerLabels(owner, "vm"), legacyAnnotations),
		vm("runner-gw-new", ownerLabels(owner, "vm"), annotations(expires, "new-hash")),
		vm("hrgw-other", legacyOwnerLabels(other, "vm"), legacyAnnotations),
	)
	pvc := func(name string, labels, values map[string]string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ci", Labels: labels, Annotations: values,
		}}
	}
	backend := &Backend{dynamic: dynamicClient, kube: kubefake.NewClientset(
		pvc("rgw-old", legacyOwnerLabels(owner, "volume"), legacyAnnotations),
		pvc("runner-gw-new", ownerLabels(owner, "volume"), annotations(expires, "new-hash")),
		pvc("rgw-other", legacyOwnerLabels(other, "volume"), legacyAnnotations),
	)}
	policy := config.RepositoryPolicy{RepositoryID: "123", Namespace: "ci"}
	ctx := context.Background()

	vms, err := backend.ListVMs(ctx, policy, owner)
	if err != nil || len(vms) != 2 {
		t.Fatalf("listed VMs = %v, error = %v", vms, err)
	}
	volumes, err := backend.ListVolumes(ctx, policy, owner)
	if err != nil || len(volumes) != 2 {
		t.Fatalf("listed volumes = %v, error = %v", volumes, err)
	}
	oldVM, err := backend.GetVM(ctx, policy, owner, "hrgw-old")
	if err != nil || oldVM.RequestHash != "old-hash" || !oldVM.ExpiresAt.Equal(expires) {
		t.Fatalf("legacy VM = %+v, error = %v", oldVM, err)
	}
	oldVolume, err := backend.GetVolume(ctx, policy, owner, "rgw-old")
	if err != nil || oldVolume.RequestHash != "old-hash" || !oldVolume.ExpiresAt.Equal(expires) {
		t.Fatalf("legacy volume = %+v, error = %v", oldVolume, err)
	}
	if _, err := backend.GetVM(ctx, policy, owner, "hrgw-other"); err == nil {
		t.Fatal("read another owner's legacy VM")
	}
	if _, err := backend.GetVolume(ctx, policy, owner, "rgw-other"); err == nil {
		t.Fatal("read another owner's legacy volume")
	}
}
