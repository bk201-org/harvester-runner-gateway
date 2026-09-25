package harvester

import (
	"context"
	"testing"

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

func TestCountsUseOwnedClusterObjectsWithoutStatusLookups(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	other := auth.Owner{RepositoryID: "123", RunID: "789", RunAttempt: "1"}
	vm := func(name string, labels map[string]string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
			"metadata": map[string]any{"name": name, "namespace": "ci", "labels": stringMap(labels)},
		}}
	}
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{vmGVR: "VirtualMachineList"},
		vm("owned-1", ownerLabels(owner, "vm")),
		vm("owned-2", ownerLabels(owner, "vm")),
		vm("other-run", ownerLabels(other, "vm")),
		vm("other-kind", ownerLabels(owner, "other")),
	)
	pvc := func(name string, labels map[string]string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ci", Labels: labels,
		}}
	}
	kube := kubefake.NewClientset(
		pvc("owned-volume", ownerLabels(owner, "volume")),
		pvc("root-disk", ownerLabels(owner, "vm-root")),
		pvc("other-volume", ownerLabels(other, "volume")),
	)
	backend := &Backend{dynamic: dynamicClient, kube: kube}
	policy := config.RepositoryPolicy{Namespace: "ci"}

	vms, err := backend.CountVMs(context.Background(), policy, owner)
	if err != nil || vms != 2 {
		t.Fatalf("VM count = %d, error = %v; want 2", vms, err)
	}
	volumes, err := backend.CountVolumes(context.Background(), policy, owner)
	if err != nil || volumes != 1 {
		t.Fatalf("volume count = %d, error = %v; want 1", volumes, err)
	}
	if actions := dynamicClient.Actions(); len(actions) != 1 || actions[0].GetVerb() != "list" || actions[0].GetResource().Resource != "virtualmachines" {
		t.Fatalf("unexpected dynamic client actions: %v", actions)
	}
	if actions := kube.Actions(); len(actions) != 1 || actions[0].GetVerb() != "list" || actions[0].GetResource().Resource != "persistentvolumeclaims" {
		t.Fatalf("unexpected Kubernetes client actions: %v", actions)
	}
}
