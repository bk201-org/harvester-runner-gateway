package harvester

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/bk201/harvester-runner-gateway/internal/auth"
	"github.com/bk201/harvester-runner-gateway/internal/config"
	"github.com/bk201/harvester-runner-gateway/internal/gateway"
)

func (b *Backend) getOwnedVolume(ctx context.Context, policy config.RepositoryPolicy, owner auth.Owner, id string) (*corev1.PersistentVolumeClaim, error) {
	if !b.validID(id, "volume") {
		return nil, gateway.ErrNotFound
	}
	pvc, err := b.kube.CoreV1().PersistentVolumeClaims(policy.Namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return nil, translate(err)
	}
	if !owned(pvc.Labels, owner, "volume") {
		return nil, gateway.ErrNotFound
	}
	return pvc, nil
}

func (b *Backend) listManagedVolumes(ctx context.Context, policy config.RepositoryPolicy, owner auth.Owner) (*corev1.PersistentVolumeClaimList, error) {
	return b.kube.CoreV1().PersistentVolumeClaims(policy.Namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector()})
}

func (b *Backend) CountVolumes(ctx context.Context, policy config.RepositoryPolicy) (int, error) {
	list, err := b.kube.CoreV1().PersistentVolumeClaims(policy.Namespace).List(ctx,
		metav1.ListOptions{LabelSelector: managedSelector()})
	if err != nil {
		return 0, err
	}
	count := 0
	for i := range list.Items {
		if b.validID(list.Items[i].Name, "volume") && repositoryOwned(list.Items[i].Labels, policy.RepositoryID, "volume") {
			count++
		}
	}
	return count, nil
}

func (b *Backend) ListVolumes(ctx context.Context, policy config.RepositoryPolicy, owner auth.Owner) ([]gateway.VolumeStatus, error) {
	list, err := b.listManagedVolumes(ctx, policy, owner)
	if err != nil {
		return nil, err
	}
	items := make([]gateway.VolumeStatus, 0, len(list.Items))
	for i := range list.Items {
		if !b.validID(list.Items[i].Name, "volume") || !owned(list.Items[i].Labels, owner, "volume") {
			continue
		}
		status, err := b.volumeStatus(ctx, policy.Namespace, &list.Items[i])
		if err != nil {
			return nil, err
		}
		items = append(items, status)
	}
	return items, nil
}

func (b *Backend) GetVolume(ctx context.Context, policy config.RepositoryPolicy, owner auth.Owner, id string) (gateway.VolumeStatus, error) {
	pvc, err := b.getOwnedVolume(ctx, policy, owner, id)
	if err != nil {
		return gateway.VolumeStatus{}, err
	}
	return b.volumeStatus(ctx, policy.Namespace, pvc)
}

func (b *Backend) volumeStatus(ctx context.Context, namespace string, pvc *corev1.PersistentVolumeClaim) (gateway.VolumeStatus, error) {
	attached, err := b.attachedVM(ctx, namespace, pvc.Name)
	if err != nil {
		return gateway.VolumeStatus{}, err
	}
	status := gateway.VolumeStatus{ID: pvc.Name, Phase: string(pvc.Status.Phase), Size: pvc.Spec.Resources.Requests.Storage().String(),
		ExpiresAt: expiry(pvc.Annotations)}
	if status.Phase == "" {
		status.Phase = "Pending"
	}
	if pvc.DeletionTimestamp != nil {
		status.Phase = "Deleting"
	}
	if attached == "" {
		return status, nil
	}
	vm, err := b.dynamic.Resource(vmGVR).Namespace(namespace).Get(ctx, attached, metav1.GetOptions{})
	if err != nil {
		return gateway.VolumeStatus{}, err
	}
	if !owned(vm.GetLabels(), parseOwner(pvc.Labels), "vm") {
		status.AttachedTo = "external"
		return status, nil
	}
	status.AttachedTo = attached
	status.AttachmentPhase = "Configured"
	vmi, err := b.dynamic.Resource(vmiGVR).Namespace(namespace).Get(ctx, attached, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return status, nil
	}
	if err != nil {
		return gateway.VolumeStatus{}, err
	}
	status.AttachmentPhase = "Attaching"
	volumeStatuses, _, _ := unstructured.NestedSlice(vmi.Object, "status", "volumeStatus")
	for _, raw := range volumeStatuses {
		volume, ok := raw.(map[string]any)
		if !ok || volume["name"] != pvc.Name {
			continue
		}
		if phase, ok := volume["phase"].(string); ok && phase != "" {
			status.AttachmentPhase = phase
		}
		break
	}
	return status, nil
}

func (b *Backend) CreateVolume(ctx context.Context, policy config.RepositoryPolicy, owner auth.Owner, id string, req gateway.VolumeRequest, expires time.Time) (gateway.VolumeStatus, error) {
	size := resource.MustParse(req.Size)
	values := resourceAnnotations(expires, owner)
	mode := corev1.PersistentVolumeBlock
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: policy.Namespace, Labels: ownerLabels(owner, "volume"), Annotations: values},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &policy.StorageClass, VolumeMode: &mode,
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
	created, err := b.kube.CoreV1().PersistentVolumeClaims(policy.Namespace).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil {
		existing, getErr := b.kube.CoreV1().PersistentVolumeClaims(policy.Namespace).Get(ctx, id, metav1.GetOptions{})
		if getErr != nil {
			return gateway.VolumeStatus{}, translate(err)
		}
		if !matchingCreate(existing.Labels, existing.Annotations, owner, "volume", values[expiresKey]) {
			return gateway.VolumeStatus{}, fmt.Errorf("%w: volume name is occupied", gateway.ErrConflict)
		}
		created = existing
	}
	return b.volumeStatus(ctx, policy.Namespace, created)
}

func (b *Backend) DeleteVolume(ctx context.Context, policy config.RepositoryPolicy, owner auth.Owner, id string) error {
	if _, err := b.getOwnedVolume(ctx, policy, owner, id); err != nil {
		return err
	}
	attached, err := b.attachedVM(ctx, policy.Namespace, id)
	if err != nil {
		return err
	}
	if attached != "" {
		return fmt.Errorf("%w: detach volume before deletion", gateway.ErrConflict)
	}
	return translate(b.kube.CoreV1().PersistentVolumeClaims(policy.Namespace).Delete(ctx, id, metav1.DeleteOptions{}))
}

func (b *Backend) attachedVM(ctx context.Context, namespace, volumeID string) (string, error) {
	list, err := b.dynamic.Resource(vmGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for i := range list.Items {
		for _, claim := range vmVolumeClaims(&list.Items[i]) {
			if claim == volumeID {
				return list.Items[i].GetName(), nil
			}
		}
	}
	vmiList, err := b.dynamic.Resource(vmiGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for i := range vmiList.Items {
		volumes, _, _ := unstructured.NestedSlice(vmiList.Items[i].Object, "spec", "volumes")
		if hasVolumeClaim(volumes, volumeID) {
			return vmiList.Items[i].GetName(), nil
		}
	}
	return "", nil
}

func vmVolumeClaims(vm *unstructured.Unstructured) []string {
	claims := vmClaimNames(vm)
	requests, _, _ := unstructured.NestedSlice(vm.Object, "status", "volumeRequests")
	for _, raw := range requests {
		request, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if add, ok := request["addVolumeOptions"].(map[string]any); ok {
			if source, ok := add["volumeSource"].(map[string]any); ok {
				if pvc, ok := source["persistentVolumeClaim"].(map[string]any); ok {
					if name, ok := pvc["claimName"].(string); ok {
						claims = append(claims, name)
					}
				}
			}
		}
		if remove, ok := request["removeVolumeOptions"].(map[string]any); ok {
			if name, ok := remove["name"].(string); ok {
				claims = append(claims, name)
			}
		}
	}
	return claims
}

func hasVolumeClaim(volumes []any, volumeID string) bool {
	for _, raw := range volumes {
		volume, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		pvc, ok := volume["persistentVolumeClaim"].(map[string]any)
		if !ok {
			continue
		}
		if name, ok := pvc["claimName"].(string); ok && name == volumeID {
			return true
		}
	}
	return false
}

func (b *Backend) AttachVolume(ctx context.Context, policy config.RepositoryPolicy, owner auth.Owner, vmID, volumeID string) error {
	vm, err := b.getOwnedVM(ctx, policy, owner, vmID)
	if err != nil {
		return err
	}
	if vm.GetDeletionTimestamp() != nil || !time.Now().Before(expiry(vm.GetAnnotations())) {
		return fmt.Errorf("%w: VM is deleting or expired", gateway.ErrConflict)
	}
	pvc, err := b.getOwnedVolume(ctx, policy, owner, volumeID)
	if err != nil {
		return err
	}
	if pvc.DeletionTimestamp != nil || !time.Now().Before(expiry(pvc.Annotations)) {
		return fmt.Errorf("%w: volume is deleting or expired", gateway.ErrConflict)
	}
	if pvc.Status.Phase != corev1.ClaimBound {
		return fmt.Errorf("%w: volume is not Bound", gateway.ErrConflict)
	}
	attached, err := b.attachedVM(ctx, policy.Namespace, volumeID)
	if err != nil {
		return err
	}
	if attached == vmID {
		return nil
	}
	if attached != "" {
		return fmt.Errorf("%w: volume is attached to another VM", gateway.ErrConflict)
	}
	if _, err := b.dynamic.Resource(vmiGVR).Namespace(policy.Namespace).Get(ctx, vmID, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: VM must be running for live attach", gateway.ErrConflict)
		}
		return err
	}
	return b.subresource(ctx, policy.Namespace, vmID, "addvolume", map[string]any{
		"name": volumeID, "disk": map[string]any{"disk": map[string]any{"bus": "scsi"}},
		"volumeSource": map[string]any{"persistentVolumeClaim": map[string]any{"claimName": volumeID, "hotpluggable": true}},
	})
}

func (b *Backend) DetachVolume(ctx context.Context, policy config.RepositoryPolicy, owner auth.Owner, vmID, volumeID string) error {
	if _, err := b.getOwnedVM(ctx, policy, owner, vmID); err != nil {
		return err
	}
	if _, err := b.getOwnedVolume(ctx, policy, owner, volumeID); err != nil {
		return err
	}
	attached, err := b.attachedVM(ctx, policy.Namespace, volumeID)
	if err != nil {
		return err
	}
	if attached == "" {
		return nil
	}
	if attached != vmID {
		return gateway.ErrNotFound
	}
	_, err = b.dynamic.Resource(vmiGVR).Namespace(policy.Namespace).Get(ctx, vmID, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return retryOnConflict(func() error {
			vm, err := b.getOwnedVM(ctx, policy, owner, vmID)
			if err != nil {
				return err
			}
			copy := vm.DeepCopy()
			changed, err := removeOfflineVolume(copy, volumeID)
			if err != nil || !changed {
				return err
			}
			_, err = b.dynamic.Resource(vmGVR).Namespace(policy.Namespace).Update(ctx, copy, metav1.UpdateOptions{})
			return err
		})
	}
	if err != nil {
		return err
	}
	return b.subresource(ctx, policy.Namespace, vmID, "removevolume", map[string]any{"name": volumeID})
}

func removeOfflineVolume(vm *unstructured.Unstructured, volumeID string) (bool, error) {
	volumes, found, err := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	if err != nil || !found {
		return false, err
	}
	keptVolumes := make([]any, 0, len(volumes))
	removed := false
	for _, raw := range volumes {
		volume, ok := raw.(map[string]any)
		if !ok || volume["name"] != volumeID {
			keptVolumes = append(keptVolumes, raw)
			continue
		}
		pvc, ok := volume["persistentVolumeClaim"].(map[string]any)
		if !ok || pvc["claimName"] != volumeID {
			keptVolumes = append(keptVolumes, raw)
			continue
		}
		removed = true
	}
	if !removed {
		return false, nil
	}
	disks, _, err := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "domain", "devices", "disks")
	if err != nil {
		return false, err
	}
	keptDisks := make([]any, 0, len(disks))
	for _, raw := range disks {
		disk, ok := raw.(map[string]any)
		if ok && disk["name"] == volumeID {
			continue
		}
		keptDisks = append(keptDisks, raw)
	}
	if err := unstructured.SetNestedSlice(vm.Object, keptVolumes, "spec", "template", "spec", "volumes"); err != nil {
		return false, err
	}
	if err := unstructured.SetNestedSlice(vm.Object, keptDisks, "spec", "template", "spec", "domain", "devices", "disks"); err != nil {
		return false, err
	}
	return true, nil
}

func (b *Backend) CleanupExpired(ctx context.Context, now time.Time) error {
	var problems []error
	seen := map[string]bool{}
	for _, policy := range b.cfg.Repositories {
		if seen[policy.Namespace] {
			continue
		}
		seen[policy.Namespace] = true
		vms, err := b.dynamic.Resource(vmGVR).Namespace(policy.Namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector()})
		if err != nil {
			problems = append(problems, err)
			continue
		}
		for i := range vms.Items {
			vm := &vms.Items[i]
			if !b.validID(vm.GetName(), "vm") || labelKind(vm.GetLabels()) != "vm" || now.Before(expiry(vm.GetAnnotations())) {
				continue
			}
			owner := parseOwner(vm.GetLabels())
			if err := b.DeleteVM(ctx, policy, owner, vm.GetName()); err != nil && !isMissing(err) {
				problems = append(problems, fmt.Errorf("delete expired VM %s: %w", vm.GetName(), err))
			} else if err == nil && b.logger != nil {
				b.logger.InfoContext(ctx, "expired resource cleanup requested", "resource_type", "vm",
					"resource_id", vm.GetName(), "namespace", policy.Namespace)
			}
		}
		volumes, err := b.kube.CoreV1().PersistentVolumeClaims(policy.Namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector()})
		if err != nil {
			problems = append(problems, err)
			continue
		}
		for i := range volumes.Items {
			pvc := &volumes.Items[i]
			kind := labelKind(pvc.Labels)
			if kind == "volume" && !b.validID(pvc.Name, "volume") {
				continue
			}
			if kind == "vm-root" && (!strings.HasSuffix(pvc.Name, "-root") || !b.validID(strings.TrimSuffix(pvc.Name, "-root"), "vm")) {
				continue
			}
			if (kind != "volume" && kind != "vm-root") || now.Before(expiry(pvc.Annotations)) {
				continue
			}
			if kind == "volume" {
				owner := parseOwner(pvc.Labels)
				attached, err := b.attachedVM(ctx, policy.Namespace, pvc.Name)
				if err != nil {
					problems = append(problems, err)
					continue
				}
				if attached != "" {
					if err := b.DetachVolume(ctx, policy, owner, attached, pvc.Name); err != nil && !isMissing(err) {
						problems = append(problems, err)
					} else if err == nil && b.logger != nil {
						b.logger.InfoContext(ctx, "expired volume detach requested", "resource_id", pvc.Name,
							"vm_id", attached, "namespace", policy.Namespace)
					}
					continue
				}
			}
			if kind == "vm-root" {
				vmID := strings.TrimSuffix(pvc.Name, "-root")
				if vmID == pvc.Name {
					continue
				}
				gone, checkErr := b.vmGone(ctx, policy.Namespace, vmID)
				if checkErr != nil {
					problems = append(problems, checkErr)
					continue
				}
				if !gone {
					continue
				}
			} else if kind != "volume" {
				continue
			}
			if err := deleteIgnoringMissing(b.kube.CoreV1().PersistentVolumeClaims(policy.Namespace).Delete(ctx, pvc.Name, metav1.DeleteOptions{})); err != nil {
				problems = append(problems, err)
			} else if b.logger != nil {
				b.logger.InfoContext(ctx, "expired resource deleted", "resource_type", kind,
					"resource_id", pvc.Name, "namespace", policy.Namespace)
			}
		}
		secrets, err := b.kube.CoreV1().Secrets(policy.Namespace).List(ctx, metav1.ListOptions{LabelSelector: managedSelector()})
		if err != nil {
			problems = append(problems, err)
			continue
		}
		for i := range secrets.Items {
			item := &secrets.Items[i]
			if labelKind(item.Labels) == "cloud-init" && strings.HasSuffix(item.Name, "-init") && b.validID(strings.TrimSuffix(item.Name, "-init"), "vm") && !now.Before(expiry(item.Annotations)) {
				vmID := strings.TrimSuffix(item.Name, "-init")
				if vmID == item.Name {
					continue
				}
				gone, checkErr := b.vmGone(ctx, policy.Namespace, vmID)
				if checkErr != nil {
					problems = append(problems, checkErr)
					continue
				}
				if !gone {
					continue
				}
				if err := deleteIgnoringMissing(b.kube.CoreV1().Secrets(policy.Namespace).Delete(ctx, item.Name, metav1.DeleteOptions{})); err != nil {
					problems = append(problems, err)
				} else if b.logger != nil {
					b.logger.InfoContext(ctx, "expired resource deleted", "resource_type", "cloud-init",
						"resource_id", item.Name, "namespace", policy.Namespace)
				}
			}
		}
	}
	return errors.Join(problems...)
}

func (b *Backend) vmGone(ctx context.Context, namespace, id string) (bool, error) {
	_, err := b.dynamic.Resource(vmGVR).Namespace(namespace).Get(ctx, id, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	return false, err
}
