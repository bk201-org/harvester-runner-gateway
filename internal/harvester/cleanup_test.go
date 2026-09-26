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
)

func TestCleanupExpiredRemovesVMRootAndSecret(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	expires := time.Now().Add(-time.Minute)
	id := testResourceID(owner, 1)
	vm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{"name": id, "namespace": "ci", "labels": stringMap(ownerLabels(owner, "vm")),
			"annotations": stringMap(annotations(expires, testMetadata))},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"volumes": []any{map[string]any{"name": "rootdisk", "persistentVolumeClaim": map[string]any{"claimName": id + "-root"}}},
		}}},
	}}
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{vmGVR: "VirtualMachineList"}, vm)
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: id + "-root", Namespace: "ci",
		Labels: ownerLabels(owner, "vm-root"), Annotations: annotations(expires, testMetadata)}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: id + "-init", Namespace: "ci",
		Labels: ownerLabels(owner, "cloud-init"), Annotations: annotations(expires, testMetadata)}}
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

func TestDeleteVMRetriesAnnotationConflict(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	expires := time.Now().Add(time.Hour)
	id := testResourceID(owner, 1)
	vm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{"name": id, "namespace": "ci", "labels": stringMap(ownerLabels(owner, "vm")),
			"annotations": stringMap(annotations(expires, testMetadata))},
	}}
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{vmGVR: "VirtualMachineList"}, vm)
	updates := 0
	dynamicClient.PrependReactor("update", "virtualmachines", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: vmGVR.Group, Resource: vmGVR.Resource}, id,
				errors.New("the object has been modified"),
			)
		}
		return false, nil, nil
	})
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: id + "-root", Namespace: "ci"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: id + "-init", Namespace: "ci"}}
	kube := kubefake.NewClientset(pvc, secret)
	backend := &Backend{dynamic: dynamicClient, kube: kube}
	policy := config.RepositoryPolicy{RepositoryID: owner.RepositoryID, Namespace: "ci"}

	if err := backend.DeleteVM(context.Background(), policy, owner, id); err != nil {
		t.Fatal(err)
	}
	if updates != 2 {
		t.Fatalf("VM annotation updates = %d, want 2", updates)
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

func TestRecoverCloudInitSecretRetriesConflict(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "1"}
	id := testResourceID(owner, 1)
	name := id + "-init"
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "ci", Labels: ownerLabels(owner, "cloud-init"),
		Annotations: annotations(time.Unix(1000, 0), testMetadata),
	}}
	kube := kubefake.NewClientset(secret)
	updates := 0
	kube.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if updates == 1 {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Resource: "secrets"}, name,
				errors.New("the object has been modified"),
			)
		}
		return false, nil, nil
	})
	backend := &Backend{kube: kube}
	policy := config.RepositoryPolicy{RepositoryID: owner.RepositoryID, Namespace: "ci"}
	wantExpires := "2000"
	createErr := apierrors.NewAlreadyExists(schema.GroupResource{Resource: "secrets"}, name)

	if err := backend.recoverCloudInitSecret(context.Background(), policy, owner, name, wantExpires, testMetadata, createErr); err != nil {
		t.Fatal(err)
	}
	if updates != 2 {
		t.Fatalf("Secret updates = %d, want 2", updates)
	}
	got, err := kube.CoreV1().Secrets("ci").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations[expiresKey] != wantExpires {
		t.Fatalf("Secret expiry = %q, want %q", got.Annotations[expiresKey], wantExpires)
	}
}
