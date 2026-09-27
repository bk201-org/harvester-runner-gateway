package harvester

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/gateway"
)

const recoveryPageSize int64 = 500

// ListAllocations scans only object metadata. It deliberately does not depend
// on VM readiness or status subresources, so partial creates remain recoverable.
func (b *Backend) ListAllocations(ctx context.Context) ([]gateway.AllocationObservation, error) {
	repositories := map[string]map[string]bool{}
	for _, policy := range b.cfg.Repositories {
		if repositories[policy.Namespace] == nil {
			repositories[policy.Namespace] = map[string]bool{}
		}
		repositories[policy.Namespace][policy.RepositoryID] = true
	}

	var observations []gateway.AllocationObservation
	for namespace, allowedRepositories := range repositories {
		continuation := ""
		for {
			list, err := b.dynamic.Resource(vmGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: managedSelector(), Limit: recoveryPageSize, Continue: continuation,
			})
			if err != nil {
				return nil, fmt.Errorf("list VMs in %s: %w", namespace, err)
			}
			for i := range list.Items {
				observation, include, err := allocationObservation(namespace, list.Items[i].GetName(), list.Items[i].GetLabels(), list.Items[i].GetAnnotations(), "vm", "")
				if err != nil {
					return nil, err
				}
				if include && allowedRepositories[observation.Owner.RepositoryID] {
					observations = append(observations, observation)
				}
			}
			continuation = list.GetContinue()
			if continuation == "" {
				break
			}
		}

		continuation = ""
		for {
			list, err := b.dynamic.Resource(vmiGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: managedSelector(), Limit: recoveryPageSize, Continue: continuation,
			})
			if err != nil {
				return nil, fmt.Errorf("list VMIs in %s: %w", namespace, err)
			}
			for i := range list.Items {
				observation, include, err := allocationObservation(namespace, list.Items[i].GetName(), list.Items[i].GetLabels(), list.Items[i].GetAnnotations(), "vm", "")
				if err != nil {
					return nil, err
				}
				if include && allowedRepositories[observation.Owner.RepositoryID] {
					observations = append(observations, observation)
				}
			}
			continuation = list.GetContinue()
			if continuation == "" {
				break
			}
		}

		continuation = ""
		for {
			list, err := b.kube.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: managedSelector(), Limit: recoveryPageSize, Continue: continuation,
			})
			if err != nil {
				return nil, fmt.Errorf("list PVCs in %s: %w", namespace, err)
			}
			for i := range list.Items {
				item := &list.Items[i]
				expectedKind, suffix := "volume", ""
				if labelKind(item.Labels) == "vm-root" {
					expectedKind, suffix = "vm-root", "-root"
				}
				observation, include, err := allocationObservation(namespace, item.Name, item.Labels, item.Annotations, expectedKind, suffix)
				if err != nil {
					return nil, err
				}
				if include && allowedRepositories[observation.Owner.RepositoryID] {
					observations = append(observations, observation)
				}
			}
			continuation = list.Continue
			if continuation == "" {
				break
			}
		}

		continuation = ""
		for {
			list, err := b.kube.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: managedSelector(), Limit: recoveryPageSize, Continue: continuation,
			})
			if err != nil {
				return nil, fmt.Errorf("list Secrets in %s: %w", namespace, err)
			}
			for i := range list.Items {
				item := &list.Items[i]
				observation, include, err := allocationObservation(namespace, item.Name, item.Labels, item.Annotations, "cloud-init", "-init")
				if err != nil {
					return nil, err
				}
				if include && allowedRepositories[observation.Owner.RepositoryID] {
					observations = append(observations, observation)
				}
			}
			continuation = list.Continue
			if continuation == "" {
				break
			}
		}
	}
	return observations, nil
}

func allocationObservation(namespace, name string, labels, values map[string]string, expectedKind, suffix string) (gateway.AllocationObservation, bool, error) {
	if suffix != "" {
		if !strings.HasSuffix(name, suffix) {
			if strings.HasPrefix(name, "ci-") {
				return gateway.AllocationObservation{}, false, fmt.Errorf("invalid dependent resource name %s/%s", namespace, name)
			}
			return gateway.AllocationObservation{}, false, nil
		}
		name = strings.TrimSuffix(name, suffix)
	}
	nameOwner, _, supported := gateway.ParseResourceID(name)
	if !supported {
		if strings.HasPrefix(name, "ci-") {
			return gateway.AllocationObservation{}, false, fmt.Errorf("invalid resource name %s/%s", namespace, name)
		}
		// Pre-change objects are outside the recovery contract.
		return gateway.AllocationObservation{}, false, nil
	}
	owner, actualKind := labeledOwner(labels)
	if labels[managedLabel] != managedValue || actualKind != expectedKind || owner != nameOwner {
		return gateway.AllocationObservation{}, false, fmt.Errorf("invalid ownership metadata for %s/%s", namespace, name)
	}
	if _, err := strconv.ParseInt(values[expiresKey], 10, 64); err != nil {
		return gateway.AllocationObservation{}, false, fmt.Errorf("missing or invalid recovery metadata for %s/%s", namespace, name)
	}
	kind := "volume"
	if expectedKind != "volume" {
		kind = "vm"
	}
	return gateway.AllocationObservation{Namespace: namespace, Owner: auth.Owner{
		RepositoryID: owner.RepositoryID, RunID: owner.RunID, RunAttempt: owner.RunAttempt,
	}, Kind: kind, ID: name}, true, nil
}
