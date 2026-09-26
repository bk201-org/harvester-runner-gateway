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
	k8stesting "k8s.io/client-go/testing"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/config"
)

func TestFailedVMDeletionKeepsRootAndCloudInit(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	expires := time.Now().Add(-time.Minute)
	id := testResourceID(owner, 1)
	vm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{"name": id, "namespace": "ci", "labels": stringMap(ownerLabels(owner, "vm")),
			"annotations": stringMap(annotations(expires, testMetadata))},
	}}
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{vmGVR: "VirtualMachineList"}, vm)
	dynamicClient.PrependReactor("update", "virtualmachines", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("simulated update failure")
	})
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: id + "-root", Namespace: "ci",
		Labels: ownerLabels(owner, "vm-root"), Annotations: annotations(expires, testMetadata)}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: id + "-init", Namespace: "ci",
		Labels: ownerLabels(owner, "cloud-init"), Annotations: annotations(expires, testMetadata)}}
	kube := kubefake.NewClientset(pvc, secret)
	backend := &Backend{dynamic: dynamicClient, kube: kube,
		cfg: config.Config{Repositories: []config.RepositoryPolicy{{RepositoryID: "123", Namespace: "ci"}}}}
	if err := backend.CleanupExpired(context.Background(), time.Now()); err == nil {
		t.Fatal("expected VM deletion error")
	}
	if _, err := kube.CoreV1().PersistentVolumeClaims("ci").Get(context.Background(), id+"-root", metav1.GetOptions{}); err != nil {
		t.Fatalf("root PVC should remain: %v", err)
	}
	if _, err := kube.CoreV1().Secrets("ci").Get(context.Background(), id+"-init", metav1.GetOptions{}); err != nil {
		t.Fatalf("cloud-init Secret should remain: %v", err)
	}
}
