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

func TestCountsUseRepositoryObjectsWithoutStatusLookups(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	otherRun := auth.Owner{RepositoryID: "123", RunID: "789", RunAttempt: "1"}
	otherAttempt := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "2"}
	otherRepo := auth.Owner{RepositoryID: "999", RunID: "456", RunAttempt: "1"}
	vm := func(name string, labels map[string]string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
			"metadata": map[string]any{"name": name, "namespace": "ci", "labels": stringMap(labels)},
		}}
	}
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{vmGVR: "VirtualMachineList"},
		vm(testResourceID(owner, 1), ownerLabels(owner, "vm")),
		vm(testResourceID(owner, 2), ownerLabels(owner, "vm")),
		vm(testResourceID(otherRun, 1), ownerLabels(otherRun, "vm")),
		vm(testResourceID(otherAttempt, 1), ownerLabels(otherAttempt, "vm")),
		vm(testResourceID(otherRepo, 1), ownerLabels(otherRepo, "vm")),
		vm(testResourceID(owner, 3), ownerLabels(owner, "other")),
		vm(testResourceID(owner, 4), map[string]string{repoLabel: owner.RepositoryID, kindLabel: "vm"}),
	)
	pvc := func(name string, labels map[string]string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "ci", Labels: labels,
		}}
	}
	kube := kubefake.NewClientset(
		pvc(testResourceID(owner, 1), ownerLabels(owner, "volume")),
		pvc(testResourceID(otherRun, 1), ownerLabels(otherRun, "volume")),
		pvc(testResourceID(otherAttempt, 1), ownerLabels(otherAttempt, "volume")),
		pvc(testResourceID(owner, 2)+"-root", ownerLabels(owner, "vm-root")),
		pvc(testResourceID(otherRepo, 1), ownerLabels(otherRepo, "volume")),
		pvc(testResourceID(owner, 3), map[string]string{repoLabel: owner.RepositoryID, kindLabel: "volume"}),
	)
	backend := &Backend{dynamic: dynamicClient, kube: kube}
	policy := config.RepositoryPolicy{RepositoryID: "123", Namespace: "ci"}

	vms, err := backend.CountVMs(context.Background(), policy)
	if err != nil || vms != 4 {
		t.Fatalf("VM count = %d, error = %v; want 4", vms, err)
	}
	volumes, err := backend.CountVolumes(context.Background(), policy)
	if err != nil || volumes != 3 {
		t.Fatalf("volume count = %d, error = %v; want 3", volumes, err)
	}
	if actions := dynamicClient.Actions(); len(actions) != 1 || actions[0].GetVerb() != "list" || actions[0].GetResource().Resource != "virtualmachines" {
		t.Fatalf("unexpected dynamic client actions: %v", actions)
	}
	if actions := kube.Actions(); len(actions) != 1 || actions[0].GetVerb() != "list" || actions[0].GetResource().Resource != "persistentvolumeclaims" {
		t.Fatalf("unexpected Kubernetes client actions: %v", actions)
	}
}
