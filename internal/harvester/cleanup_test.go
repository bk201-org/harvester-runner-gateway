package harvester

import (
	"context"
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

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/config"
)

func TestCleanupExpiredRemovesVMRootAndSecret(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	expires := time.Now().Add(-time.Minute)
	id := "rgw-expired"
	vm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{"name": id, "namespace": "ci", "labels": stringMap(ownerLabels(owner, "vm")),
			"annotations": stringMap(annotations(expires, "hash"))},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"volumes": []any{map[string]any{"name": "rootdisk", "persistentVolumeClaim": map[string]any{"claimName": id + "-root"}}},
		}}},
	}}
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{vmGVR: "VirtualMachineList"}, vm)
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: id + "-root", Namespace: "ci",
		Labels: ownerLabels(owner, "vm-root"), Annotations: annotations(expires, "hash")}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: id + "-init", Namespace: "ci",
		Labels: ownerLabels(owner, "cloud-init"), Annotations: annotations(expires, "hash")}}
	kube := kubefake.NewClientset(pvc, secret)
	backend := &Backend{dynamic: dynamicClient, kube: kube,
		cfg: config.Config{Repositories: []config.RepositoryPolicy{{RepositoryID: "123", Namespace: "ci"}}}}
	if err := backend.CleanupExpired(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := dynamicClient.Resource(vmGVR).Namespace("ci").Get(context.Background(), id, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("VM still exists: %v", err)
	}
	if _, err := kube.CoreV1().PersistentVolumeClaims("ci").Get(context.Background(), id+"-root", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("root PVC still exists: %v", err)
	}
	if _, err := kube.CoreV1().Secrets("ci").Get(context.Background(), id+"-init", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("cloud-init Secret still exists: %v", err)
	}
}
