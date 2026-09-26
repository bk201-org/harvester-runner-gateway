package harvester

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/config"
	"github.com/bk201-org/harvester-runner-gateway/internal/gateway"
)

const (
	managedLabel       = "app.kubernetes.io/managed-by"
	managedValue       = "harvester-runner-gateway"
	repoLabel          = "runner-gw-repository-id"
	runLabel           = "runner-gw-run-id"
	attemptLabel       = "runner-gw-run-attempt"
	kindLabel          = "runner-gw-kind"
	expiresKey         = "runner-gw-expires-at"
	hashKey            = "runner-gw-request-hash"
	legacyRepoLabel    = "rgw-repository-id"
	legacyRunLabel     = "rgw-run-id"
	legacyAttemptLabel = "rgw-run-attempt"
	legacyKindLabel    = "rgw-kind"
	legacyExpiresKey   = "rgw-expires-at"
	legacyHashKey      = "rgw-request-hash"
	imageKey           = "harvesterhci.io/imageId"
	autoDelete         = "terraform-provider-harvester-auto-delete"
	claimKey           = "harvesterhci.io/volumeClaimTemplates"
	removedKey         = "harvesterhci.io/removedPersistentVolumeClaims"
	requestTimeout     = 20 * time.Second
	rollbackTimeout    = 5 * time.Second
)

var (
	vmGVR      = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
	vmiGVR     = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
	imageGVR   = schema.GroupVersionResource{Group: "harvesterhci.io", Version: "v1beta1", Resource: "virtualmachineimages"}
	networkGVR = schema.GroupVersionResource{Group: "k8s.cni.cncf.io", Version: "v1", Resource: "network-attachment-definitions"}
)

type Backend struct {
	dynamic dynamic.Interface
	kube    kubernetes.Interface
	sub     *rest.RESTClient
	cfg     config.Config
	logger  *slog.Logger
}

func New(cfg config.Config, logger *slog.Logger) (*Backend, error) {
	if _, err := os.Stat(cfg.Kubeconfig); err != nil {
		return nil, fmt.Errorf("read kubeconfig: %w", err)
	}
	rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: cfg.Kubeconfig}
	values := &clientcmd.ConfigOverrides{CurrentContext: cfg.KubeContext}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, values).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	if restConfig.Timeout == 0 || restConfig.Timeout > requestTimeout {
		restConfig.Timeout = requestTimeout
	}
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	subConfig := rest.CopyConfig(restConfig)
	subConfig.GroupVersion = &schema.GroupVersion{Group: "subresources.kubevirt.io", Version: "v1"}
	subConfig.APIPath = "/apis"
	subConfig.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	subConfig.ContentType = runtime.ContentTypeJSON
	subClient, err := rest.RESTClientFor(subConfig)
	if err != nil {
		return nil, err
	}
	return &Backend{dynamic: dynamicClient, kube: kubeClient, sub: subClient, cfg: cfg, logger: logger}, nil
}

func (b *Backend) Ping(ctx context.Context) error {
	for _, policy := range b.cfg.Repositories {
		if _, err := b.kube.CoreV1().Namespaces().Get(ctx, policy.Namespace, metav1.GetOptions{}); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) Preflight(ctx context.Context) error {
	if err := b.Ping(ctx); err != nil {
		return fmt.Errorf("read allowed namespaces: %w", err)
	}
	for _, policy := range b.cfg.Repositories {
		if _, err := b.kube.StorageV1().StorageClasses().Get(ctx, policy.StorageClass, metav1.GetOptions{}); err != nil {
			return fmt.Errorf("read storage class %s: %w", policy.StorageClass, err)
		}
		for _, name := range policy.Images {
			ns, resourceName := splitName(name)
			if _, err := b.dynamic.Resource(imageGVR).Namespace(ns).Get(ctx, resourceName, metav1.GetOptions{}); err != nil {
				return fmt.Errorf("read image %s: %w", name, err)
			}
		}
		for _, name := range policy.Networks {
			ns, resourceName := splitName(name)
			if _, err := b.dynamic.Resource(networkGVR).Namespace(ns).Get(ctx, resourceName, metav1.GetOptions{}); err != nil {
				return fmt.Errorf("read network %s: %w", name, err)
			}
		}
	}
	return nil
}

func (b *Backend) subresource(ctx context.Context, namespace, name, action string, body any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	path := "/apis/subresources.kubevirt.io/v1/namespaces/" + namespace + "/virtualmachines/" + name + "/" + action
	return translate(b.sub.Put().AbsPath(path).Body(encoded).Do(ctx).Error())
}

func ownerLabels(owner auth.Owner, kind string) map[string]string {
	return map[string]string{managedLabel: managedValue, repoLabel: owner.RepositoryID,
		runLabel: owner.RunID, attemptLabel: owner.RunAttempt, kindLabel: kind}
}

func managedSelector() string {
	return managedLabel + "=" + managedValue
}

func labeledOwner(labels map[string]string) (auth.Owner, string) {
	if labels[kindLabel] != "" {
		return auth.Owner{RepositoryID: labels[repoLabel], RunID: labels[runLabel], RunAttempt: labels[attemptLabel]}, labels[kindLabel]
	}
	return auth.Owner{RepositoryID: labels[legacyRepoLabel], RunID: labels[legacyRunLabel], RunAttempt: labels[legacyAttemptLabel]}, labels[legacyKindLabel]
}

func owned(labels map[string]string, owner auth.Owner, kind string) bool {
	actual, actualKind := labeledOwner(labels)
	return labels[managedLabel] == managedValue && actualKind == kind && actual == owner
}

func repositoryOwned(labels map[string]string, repositoryID, kind string) bool {
	owner, actualKind := labeledOwner(labels)
	return labels[managedLabel] == managedValue && actualKind == kind && owner.RepositoryID == repositoryID
}

func parseOwner(labels map[string]string) auth.Owner {
	owner, _ := labeledOwner(labels)
	return owner
}

func labelKind(labels map[string]string) string {
	_, kind := labeledOwner(labels)
	return kind
}

func annotations(expires time.Time, hash string) map[string]string {
	return map[string]string{expiresKey: strconv.FormatInt(expires.Unix(), 10), hashKey: hash}
}

func annotationValue(values map[string]string, current, legacy string) string {
	if value, ok := values[current]; ok {
		return value
	}
	return values[legacy]
}

func expiry(values map[string]string) time.Time {
	seconds, _ := strconv.ParseInt(annotationValue(values, expiresKey, legacyExpiresKey), 10, 64)
	return time.Unix(seconds, 0).UTC()
}

func validID(id string) bool {
	return (strings.HasPrefix(id, "runner-gw-") || strings.HasPrefix(id, "hrgw-") || strings.HasPrefix(id, "rgw-")) && len(validation.IsDNS1123Label(id)) == 0
}

func splitName(name string) (string, string) {
	parts := strings.SplitN(name, "/", 2)
	return parts[0], parts[1]
}

func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return gateway.ErrNotFound
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
		return fmt.Errorf("%w: %v", gateway.ErrConflict, err)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return fmt.Errorf("%w: %v", gateway.ErrInvalid, err)
	default:
		return err
	}
}

func stringMap(values map[string]string) map[string]any {
	out := make(map[string]any, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func nestedString(obj *unstructured.Unstructured, fields ...string) string {
	value, _, _ := unstructured.NestedString(obj.Object, fields...)
	return value
}

func deleteIgnoringMissing(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func rollbackContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
}

func secret(namespace, name string, labels, values map[string]string, userData string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace,
		Labels: labels, Annotations: values}, StringData: map[string]string{"userdata": userData}}
}

var _ gateway.Backend = (*Backend)(nil)
