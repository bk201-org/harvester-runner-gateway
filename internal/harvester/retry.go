package harvester

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/config"
	"github.com/bk201-org/harvester-runner-gateway/internal/gateway"
)

func matchingRecovery(labels, values map[string]string, owner auth.Owner, kind string, metadata gateway.ResourceMetadata) bool {
	return owned(labels, owner, kind) && values[identityKey] == metadata.IdentityHash && values[hashKey] == metadata.RequestHash
}

func (b *Backend) verifyVMDependencies(ctx context.Context, policy config.RepositoryPolicy, owner auth.Owner, id string, metadata gateway.ResourceMetadata) error {
	root, err := b.kube.CoreV1().PersistentVolumeClaims(policy.Namespace).Get(ctx, id+"-root", metav1.GetOptions{})
	if err == nil {
		if root.DeletionTimestamp != nil || !matchingRecovery(root.Labels, root.Annotations, owner, "vm-root", metadata) {
			return fmt.Errorf("%w: VM root name is occupied or deleting", gateway.ErrConflict)
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	vmi, err := b.dynamic.Resource(vmiGVR).Namespace(policy.Namespace).Get(ctx, id, metav1.GetOptions{})
	if err == nil {
		if vmi.GetDeletionTimestamp() != nil || !matchingRecovery(vmi.GetLabels(), vmi.GetAnnotations(), owner, "vm", metadata) {
			return fmt.Errorf("%w: VMI name is occupied or deleting", gateway.ErrConflict)
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
