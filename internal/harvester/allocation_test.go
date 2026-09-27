package harvester

import (
	"context"
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

func TestListAllocationsRecoversAllResourceKinds(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	meta := func(name, kind string) map[string]any {
		return map[string]any{"name": name, "namespace": "ci", "labels": stringMap(ownerLabels(owner, kind)),
			"annotations": stringMap(annotations(time.Unix(12345, 0)))}
	}
	vmID := testResourceID(owner, 2)
	vmiID := testResourceID(owner, 4)
	vm := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
		"metadata": meta(vmID, "vm")}}
	vmi := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance",
		"metadata": meta(vmiID, "vm")}}
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		vmGVR: "VirtualMachineList", vmiGVR: "VirtualMachineInstanceList",
	}, vm, vmi)
	volumeID := testResourceID(owner, 3)
	rootID := testResourceID(owner, 5)
	secretID := testResourceID(owner, 6)
	kube := kubefake.NewClientset(
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: volumeID, Namespace: "ci", Labels: ownerLabels(owner, "volume"), Annotations: annotations(time.Unix(12345, 0))}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: rootID + "-root", Namespace: "ci", Labels: ownerLabels(owner, "vm-root"), Annotations: annotations(time.Unix(12345, 0))}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretID + "-init", Namespace: "ci", Labels: ownerLabels(owner, "cloud-init"), Annotations: annotations(time.Unix(12345, 0))}},
	)
	backend := &Backend{dynamic: dynamicClient, kube: kube, cfg: config.Config{Repositories: []config.RepositoryPolicy{
		{RepositoryID: "123", Namespace: "ci"},
		// A shared namespace must still be scanned only once.
		{RepositoryID: "999", Namespace: "ci"},
	}}}
	observations, err := backend.ListAllocations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 5 {
		t.Fatalf("observations = %#v", observations)
	}
	kinds := map[string]string{}
	for _, observation := range observations {
		kinds[observation.ID] = observation.Kind
	}
	for id, kind := range map[string]string{vmID: "vm", vmiID: "vm", volumeID: "volume", rootID: "vm", secretID: "vm"} {
		if kinds[id] != kind {
			t.Fatalf("%s recovered as %q, want %q", id, kinds[id], kind)
		}
	}
}

func TestListAllocationsRejectsInvalidSupportedMetadata(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	id := testResourceID(owner, 1)
	vm := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{"name": id, "namespace": "ci", "labels": stringMap(ownerLabels(owner, "vm")),
			"annotations": stringMap(map[string]string{})}}}
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		vmGVR: "VirtualMachineList", vmiGVR: "VirtualMachineInstanceList",
	}, vm)
	backend := &Backend{dynamic: dynamicClient, kube: kubefake.NewClientset(), cfg: config.Config{Repositories: []config.RepositoryPolicy{{RepositoryID: "123", Namespace: "ci"}}}}
	if _, err := backend.ListAllocations(context.Background()); err == nil {
		t.Fatal("recovery accepted a supported VM without expiry metadata")
	}
}
