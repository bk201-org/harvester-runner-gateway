package harvester

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/bk201/harvester-runner-gateway/internal/auth"
	"github.com/bk201/harvester-runner-gateway/internal/config"
	"github.com/bk201/harvester-runner-gateway/internal/gateway"
)

func retryOnConflict(operation func() error) error {
	return translate(retry.RetryOnConflict(retry.DefaultRetry, operation))
}

// matchingCreate checks the result of an uncertain Kubernetes write for a newly reserved ID.
func matchingCreate(labels, values map[string]string, owner auth.Owner, kind, expires string) bool {
	return owned(labels, owner, kind) && values[expiresKey] == expires && values[workflowRefKey] == owner.WorkflowRef
}

func (b *Backend) verifyVMDependencies(ctx context.Context, policy config.RepositoryPolicy, id string) error {
	_, err := b.kube.CoreV1().PersistentVolumeClaims(policy.Namespace).Get(ctx, id+"-root", metav1.GetOptions{})
	if err == nil {
		return fmt.Errorf("%w: VM root name is occupied", gateway.ErrConflict)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	_, err = b.dynamic.Resource(vmiGVR).Namespace(policy.Namespace).Get(ctx, id, metav1.GetOptions{})
	if err == nil {
		return fmt.Errorf("%w: VMI name is occupied", gateway.ErrConflict)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
