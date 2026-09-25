package harvester

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/config"
	"github.com/bk201-org/harvester-runner-gateway/internal/gateway"
)

func (b *Backend) getOwnedVM(ctx context.Context, policy config.RepositoryPolicy, owner auth.Owner, id string) (*unstructured.Unstructured, error) {
	if !validID(id) {
		return nil, gateway.ErrNotFound
	}
	vm, err := b.dynamic.Resource(vmGVR).Namespace(policy.Namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return nil, translate(err)
	}
	if !owned(vm.GetLabels(), owner, "vm") {
		return nil, gateway.ErrNotFound
	}
	return vm, nil
}

func (b *Backend) listOwnedVMs(ctx context.Context, policy config.RepositoryPolicy, owner auth.Owner) (*unstructured.UnstructuredList, error) {
	return b.dynamic.Resource(vmGVR).Namespace(policy.Namespace).List(ctx, metav1.ListOptions{LabelSelector: ownerSelector(owner, "vm")})
}

func (b *Backend) CountVMs(ctx context.Context, policy config.RepositoryPolicy, owner auth.Owner) (int, error) {
	list, err := b.listOwnedVMs(ctx, policy, owner)
	if err != nil {
		return 0, err
	}
	return len(list.Items), nil
}

func (b *Backend) ListVMs(ctx context.Context, policy config.RepositoryPolicy, owner auth.Owner) ([]gateway.VMStatus, error) {
	list, err := b.listOwnedVMs(ctx, policy, owner)
	if err != nil {
		return nil, err
	}
	items := make([]gateway.VMStatus, 0, len(list.Items))
	for i := range list.Items {
		item, err := b.vmStatus(ctx, policy.Namespace, &list.Items[i])
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (b *Backend) GetVM(ctx context.Context, policy config.RepositoryPolicy, owner auth.Owner, id string) (gateway.VMStatus, error) {
	vm, err := b.getOwnedVM(ctx, policy, owner, id)
	if err != nil {
		return gateway.VMStatus{}, err
	}
	return b.vmStatus(ctx, policy.Namespace, vm)
}

func (b *Backend) vmStatus(ctx context.Context, namespace string, vm *unstructured.Unstructured) (gateway.VMStatus, error) {
	status := gateway.VMStatus{ID: vm.GetName(), Phase: nestedString(vm, "status", "printableStatus"),
		PowerState: "off", IPAddresses: []string{}, AttachedVolumeIDs: []string{},
		ExpiresAt: expiry(vm.GetAnnotations()), RequestHash: vm.GetAnnotations()[hashKey]}
	if status.Phase == "" {
		status.Phase = "Provisioning"
	}
	if vm.GetDeletionTimestamp() != nil {
		status.Phase = "Deleting"
	}
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	for _, raw := range volumes {
		volume, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		pvc, ok := volume["persistentVolumeClaim"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := pvc["claimName"].(string)
		if strings.HasPrefix(name, "rgw-") && name != vm.GetName()+"-root" {
			status.AttachedVolumeIDs = append(status.AttachedVolumeIDs, name)
		}
	}
	vmi, err := b.dynamic.Resource(vmiGVR).Namespace(namespace).Get(ctx, vm.GetName(), metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return gateway.VMStatus{}, err
	}
	if err == nil {
		status.PowerState = "on"
		ifaces, _, _ := unstructured.NestedSlice(vmi.Object, "status", "interfaces")
		seen := map[string]bool{}
		for _, raw := range ifaces {
			iface, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if ip, ok := iface["ipAddress"].(string); ok && ip != "" && !seen[ip] {
				status.IPAddresses = append(status.IPAddresses, ip)
				seen[ip] = true
			}
		}
	}
	return status, nil
}

func (b *Backend) CreateVM(ctx context.Context, policy config.RepositoryPolicy, owner auth.Owner, id string, req gateway.VMRequest, expires time.Time, hash string) (gateway.VMStatus, error) {
	imageNS, imageName := splitName(req.Image)
	image, err := b.dynamic.Resource(imageGVR).Namespace(imageNS).Get(ctx, imageName, metav1.GetOptions{})
	if err != nil {
		return gateway.VMStatus{}, translate(err)
	}
	if !conditionTrue(image, "Imported") {
		return gateway.VMStatus{}, fmt.Errorf("%w: image is not imported", gateway.ErrInvalid)
	}
	storageClass := nestedString(image, "status", "storageClassName")
	if storageClass == "" {
		return gateway.VMStatus{}, fmt.Errorf("%w: image has no storage class", gateway.ErrInvalid)
	}
	virtualSize, found, err := unstructured.NestedInt64(image.Object, "status", "virtualSize")
	if err != nil {
		return gateway.VMStatus{}, err
	}
	bootSize := resource.MustParse(req.BootDiskSize)
	if found && virtualSize > 0 && bootSize.Value() < virtualSize {
		return gateway.VMStatus{}, fmt.Errorf("%w: boot disk smaller than image", gateway.ErrInvalid)
	}
	networkNS, networkName := splitName(req.Network)
	if _, err := b.dynamic.Resource(networkGVR).Namespace(networkNS).Get(ctx, networkName, metav1.GetOptions{}); err != nil {
		return gateway.VMStatus{}, translate(err)
	}
	userData, err := renderCloudConfig(req.UserData, req.SSHPublicKeys, policy.DefaultUser)
	if err != nil {
		return gateway.VMStatus{}, fmt.Errorf("%w: %v", gateway.ErrInvalid, err)
	}
	labels := ownerLabels(owner, "vm")
	values := annotations(expires, hash)
	secretName := id + "-init"
	secretLabels := ownerLabels(owner, "cloud-init")
	if _, err := b.kube.CoreV1().Secrets(policy.Namespace).Create(ctx, secret(policy.Namespace, secretName, secretLabels, values, userData), metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return gateway.VMStatus{}, translate(err)
		}
		existing, getErr := b.kube.CoreV1().Secrets(policy.Namespace).Get(ctx, secretName, metav1.GetOptions{})
		if getErr != nil {
			return gateway.VMStatus{}, translate(getErr)
		}
		if !owned(existing.Labels, owner, "cloud-init") || existing.Annotations[hashKey] != hash {
			return gateway.VMStatus{}, fmt.Errorf("%w: cloud-init Secret name is occupied", gateway.ErrConflict)
		}
		copy := existing.DeepCopy()
		copy.Annotations[expiresKey] = values[expiresKey]
		if _, updateErr := b.kube.CoreV1().Secrets(policy.Namespace).Update(ctx, copy, metav1.UpdateOptions{}); updateErr != nil {
			return gateway.VMStatus{}, translate(updateErr)
		}
	}
	vm, err := buildVM(policy.Namespace, id, req, storageClass, labels, values)
	if err != nil {
		_ = b.kube.CoreV1().Secrets(policy.Namespace).Delete(context.WithoutCancel(ctx), secretName, metav1.DeleteOptions{})
		return gateway.VMStatus{}, err
	}
	created, err := b.dynamic.Resource(vmGVR).Namespace(policy.Namespace).Create(ctx, vm, metav1.CreateOptions{})
	if err != nil {
		_ = b.kube.CoreV1().Secrets(policy.Namespace).Delete(context.WithoutCancel(ctx), secretName, metav1.DeleteOptions{})
		return gateway.VMStatus{}, translate(err)
	}
	if err := b.subresource(ctx, policy.Namespace, id, "start", map[string]any{}); err != nil {
		_ = b.DeleteVM(context.WithoutCancel(ctx), policy, owner, id)
		return gateway.VMStatus{}, fmt.Errorf("start VM: %w", err)
	}
	return b.vmStatus(ctx, policy.Namespace, created)
}

func buildVM(namespace, id string, req gateway.VMRequest, storageClass string, labels, values map[string]string) (*unstructured.Unstructured, error) {
	rootName := id + "-root"
	rootAnnotations := map[string]string{imageKey: req.Image, autoDelete: "true", expiresKey: values[expiresKey]}
	claimTemplate := []any{map[string]any{
		"metadata": map[string]any{"name": rootName, "labels": stringMap(ownerLabels(auth.Owner{
			RepositoryID: labels[repoLabel], RunID: labels[runLabel], RunAttempt: labels[attemptLabel]}, "vm-root")), "annotations": stringMap(rootAnnotations)},
		"spec": map[string]any{"accessModes": []any{"ReadWriteMany"}, "volumeMode": "Block",
			"storageClassName": storageClass, "resources": map[string]any{"requests": map[string]any{"storage": req.BootDiskSize}}},
	}}
	encoded, err := json.Marshal(claimTemplate)
	if err != nil {
		return nil, err
	}
	metaAnnotations := map[string]any{claimKey: string(encoded), expiresKey: values[expiresKey], hashKey: values[hashKey]}
	networkRef := req.Network
	if strings.HasPrefix(networkRef, namespace+"/") {
		networkRef = strings.TrimPrefix(networkRef, namespace+"/")
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{"name": id, "namespace": namespace, "labels": stringMap(labels), "annotations": metaAnnotations},
		"spec": map[string]any{"runStrategy": "Manual", "template": map[string]any{
			"metadata": map[string]any{"labels": stringMap(labels)},
			"spec": map[string]any{
				"domain": map[string]any{
					"cpu":       map[string]any{"cores": int64(req.CPU), "sockets": int64(1), "threads": int64(1)},
					"resources": map[string]any{"limits": map[string]any{"cpu": fmt.Sprint(req.CPU), "memory": req.Memory}},
					"devices": map[string]any{"disks": []any{
						map[string]any{"name": "rootdisk", "bootOrder": int64(1), "disk": map[string]any{"bus": "virtio"}},
						map[string]any{"name": "cloudinitdisk", "disk": map[string]any{"bus": "virtio"}}},
						"interfaces": []any{map[string]any{"name": "nic-1", "model": "virtio", "bridge": map[string]any{}}}}},
				"networks": []any{map[string]any{"name": "nic-1", "multus": map[string]any{"networkName": networkRef}}},
				"volumes": []any{
					map[string]any{"name": "rootdisk", "persistentVolumeClaim": map[string]any{"claimName": rootName}},
					map[string]any{"name": "cloudinitdisk", "cloudInitNoCloud": map[string]any{"secretRef": map[string]any{"name": id + "-init"}}}},
			}}},
	}}, nil
}

func renderCloudConfig(input string, keys []string, defaultUser string) (string, error) {
	result := map[string]any{}
	if input != "" {
		data, err := yaml.YAMLToJSON([]byte(input))
		if err != nil {
			return "", fmt.Errorf("parse userData: %w", err)
		}
		if err := json.Unmarshal(data, &result); err != nil || result == nil {
			return "", fmt.Errorf("userData must be a cloud-config mapping")
		}
	}
	if _, exists := result["ssh_authorized_keys"]; exists && len(keys) > 0 {
		return "", fmt.Errorf("userData cannot set ssh_authorized_keys when sshPublicKeys are provided")
	}
	if len(keys) > 0 {
		if existing, ok := result["user"]; ok && existing != defaultUser {
			return "", fmt.Errorf("userData selects a different SSH user")
		}
		result["user"] = defaultUser
		result["ssh_authorized_keys"] = keys
	}
	encoded, err := yaml.Marshal(result)
	if err != nil {
		return "", err
	}
	return "#cloud-config\n" + string(encoded), nil
}

func conditionTrue(obj *unstructured.Unstructured, kind string) bool {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, raw := range conditions {
		condition, ok := raw.(map[string]any)
		if ok && condition["type"] == kind && condition["status"] == "True" {
			return true
		}
	}
	return false
}

func (b *Backend) DeleteVM(ctx context.Context, policy config.RepositoryPolicy, owner auth.Owner, id string) error {
	vm, err := b.getOwnedVM(ctx, policy, owner, id)
	if err != nil {
		return err
	}
	rootName := id + "-root"
	copy := vm.DeepCopy()
	values := copy.GetAnnotations()
	if values == nil {
		values = map[string]string{}
	}
	values[removedKey] = rootName
	copy.SetAnnotations(values)
	if _, err := b.dynamic.Resource(vmGVR).Namespace(policy.Namespace).Update(ctx, copy, metav1.UpdateOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return translate(err)
	}
	foreground := metav1.DeletePropagationForeground
	if err := deleteIgnoringMissing(b.dynamic.Resource(vmGVR).Namespace(policy.Namespace).Delete(ctx, id, metav1.DeleteOptions{PropagationPolicy: &foreground})); err != nil {
		return err
	}
	if err := deleteIgnoringMissing(b.kube.CoreV1().PersistentVolumeClaims(policy.Namespace).Delete(ctx, rootName, metav1.DeleteOptions{})); err != nil {
		return err
	}
	return deleteIgnoringMissing(b.kube.CoreV1().Secrets(policy.Namespace).Delete(ctx, id+"-init", metav1.DeleteOptions{}))
}

func (b *Backend) PowerVM(ctx context.Context, policy config.RepositoryPolicy, owner auth.Owner, id, state string) error {
	if _, err := b.getOwnedVM(ctx, policy, owner, id); err != nil {
		return err
	}
	_, err := b.dynamic.Resource(vmiGVR).Namespace(policy.Namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if (state == "on" && err == nil) || (state == "off" && apierrors.IsNotFound(err)) {
		return nil
	}
	if state == "on" {
		return b.subresource(ctx, policy.Namespace, id, "start", map[string]any{})
	}
	return b.subresource(ctx, policy.Namespace, id, "stop", map[string]any{})
}

func (b *Backend) RebootVM(ctx context.Context, policy config.RepositoryPolicy, owner auth.Owner, id string) error {
	if _, err := b.getOwnedVM(ctx, policy, owner, id); err != nil {
		return err
	}
	if _, err := b.dynamic.Resource(vmiGVR).Namespace(policy.Namespace).Get(ctx, id, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: VM is powered off", gateway.ErrConflict)
		}
		return err
	}
	return b.subresource(ctx, policy.Namespace, id, "restart", map[string]any{})
}

func isMissing(err error) bool {
	return errors.Is(err, gateway.ErrNotFound) || apierrors.IsNotFound(err)
}

func vmClaimNames(vm *unstructured.Unstructured) []string {
	volumes, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	var names []string
	for _, raw := range volumes {
		volume, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		pvc, ok := volume["persistentVolumeClaim"].(map[string]any)
		if !ok {
			continue
		}
		if name, ok := pvc["claimName"].(string); ok {
			names = append(names, name)
		}
	}
	return names
}
