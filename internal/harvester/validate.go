package harvester

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/bk201/harvester-runner-gateway/internal/config"
	"github.com/bk201/harvester-runner-gateway/internal/gateway"
)

// ValidateVM performs cluster-backed validation before the gateway reserves a
// sequence. CreateVM repeats these reads because cluster state can change, but
// a deterministic validation rejection never consumes a number.
func (b *Backend) ValidateVM(ctx context.Context, policy config.RepositoryPolicy, req gateway.VMRequest) error {
	imageNS, imageName := splitName(req.Image)
	image, err := b.dynamic.Resource(imageGVR).Namespace(imageNS).Get(ctx, imageName, metav1.GetOptions{})
	if err != nil {
		return translate(err)
	}
	if !conditionTrue(image, "Imported") {
		return fmt.Errorf("%w: image is not imported", gateway.ErrInvalid)
	}
	if nestedString(image, "status", "storageClassName") == "" {
		return fmt.Errorf("%w: image has no storage class", gateway.ErrInvalid)
	}
	virtualSize, found, err := unstructured.NestedInt64(image.Object, "status", "virtualSize")
	if err != nil {
		return err
	}
	bootSize := resource.MustParse(req.BootDiskSize)
	if found && virtualSize > 0 && bootSize.Value() < virtualSize {
		return fmt.Errorf("%w: boot disk smaller than image", gateway.ErrInvalid)
	}
	networkNS, networkName := splitName(req.Network)
	if _, err := b.dynamic.Resource(networkGVR).Namespace(networkNS).Get(ctx, networkName, metav1.GetOptions{}); err != nil {
		return translate(err)
	}
	if _, err := renderCloudConfig(req.UserData, req.SSHPublicKeys, policy.DefaultUser); err != nil {
		return fmt.Errorf("%w: %v", gateway.ErrInvalid, err)
	}
	return nil
}
