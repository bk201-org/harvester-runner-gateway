package harvester

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/bk201/harvester-runner-gateway/internal/config"
)

func TestListAllocationsFailsOnIncompleteScan(t *testing.T) {
	dynamicClient := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		vmGVR: "VirtualMachineList", vmiGVR: "VirtualMachineInstanceList",
	})
	dynamicClient.PrependReactor("list", "virtualmachines", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("injected list failure")
	})
	backend := &Backend{dynamic: dynamicClient, kube: kubefake.NewClientset(), cfg: config.Config{Repositories: []config.RepositoryPolicy{{RepositoryID: "123", Namespace: "ci"}}}}
	if observations, err := backend.ListAllocations(context.Background()); err == nil || observations != nil {
		t.Fatalf("failed scan returned observations=%v error=%v", observations, err)
	}
}
