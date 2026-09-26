package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
	"github.com/bk201-org/harvester-runner-gateway/internal/config"
)

type fakeVerifier struct{}

func (fakeVerifier) Verify(_ context.Context, token string, cfg config.Config) (auth.Owner, config.RepositoryPolicy, error) {
	policy := cfg.Repositories[0]
	if token == "run-one-attempt-two" {
		return auth.Owner{RepositoryID: policy.RepositoryID, RunID: "run-one", RunAttempt: "2"}, policy, nil
	}
	if token != "run-one" && token != "run-two" {
		return auth.Owner{}, policy, auth.ErrUnauthorized
	}
	return auth.Owner{RepositoryID: policy.RepositoryID, RunID: token, RunAttempt: "1"}, policy, nil
}

type fakeVM struct {
	owner auth.Owner
	item  VMStatus
}

type fakeVolume struct {
	owner auth.Owner
	item  VolumeStatus
}

type fakeBackend struct {
	mu      sync.Mutex
	vms     map[string]fakeVM
	volumes map[string]fakeVolume
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{vms: map[string]fakeVM{}, volumes: map[string]fakeVolume{}}
}

func (b *fakeBackend) Ping(context.Context) error { return nil }

func (b *fakeBackend) CountVMs(_ context.Context, policy config.RepositoryPolicy) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	count := 0
	for _, record := range b.vms {
		if record.owner.RepositoryID == policy.RepositoryID {
			count++
		}
	}
	return count, nil
}

func (b *fakeBackend) ListVMs(_ context.Context, _ config.RepositoryPolicy, owner auth.Owner) ([]VMStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	items := []VMStatus{}
	for _, record := range b.vms {
		if record.owner == owner {
			items = append(items, record.item)
		}
	}
	return items, nil
}

func (b *fakeBackend) GetVM(_ context.Context, _ config.RepositoryPolicy, owner auth.Owner, id string) (VMStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.vms[id]
	if !ok || record.owner != owner {
		return VMStatus{}, ErrNotFound
	}
	return record.item, nil
}

func (b *fakeBackend) CreateVM(_ context.Context, _ config.RepositoryPolicy, owner auth.Owner, id string, _ VMRequest, expires time.Time, hash string) (VMStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	item := VMStatus{ID: id, Phase: "Provisioning", PowerState: "off", IPAddresses: []string{}, AttachedVolumeIDs: []string{}, ExpiresAt: expires, RequestHash: hash}
	b.vms[id] = fakeVM{owner: owner, item: item}
	return item, nil
}

func (b *fakeBackend) DeleteVM(_ context.Context, _ config.RepositoryPolicy, owner auth.Owner, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.vms[id]
	if !ok || record.owner != owner {
		return ErrNotFound
	}
	delete(b.vms, id)
	return nil
}

func (*fakeBackend) PowerVM(context.Context, config.RepositoryPolicy, auth.Owner, string, string) error {
	return nil
}
func (*fakeBackend) RebootVM(context.Context, config.RepositoryPolicy, auth.Owner, string) error {
	return nil
}

func (b *fakeBackend) CountVolumes(_ context.Context, policy config.RepositoryPolicy) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	count := 0
	for _, record := range b.volumes {
		if record.owner.RepositoryID == policy.RepositoryID {
			count++
		}
	}
	return count, nil
}

func (b *fakeBackend) ListVolumes(_ context.Context, _ config.RepositoryPolicy, owner auth.Owner) ([]VolumeStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	items := []VolumeStatus{}
	for _, record := range b.volumes {
		if record.owner == owner {
			items = append(items, record.item)
		}
	}
	return items, nil
}

func (b *fakeBackend) GetVolume(_ context.Context, _ config.RepositoryPolicy, owner auth.Owner, id string) (VolumeStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.volumes[id]
	if !ok || record.owner != owner {
		return VolumeStatus{}, ErrNotFound
	}
	return record.item, nil
}

func (b *fakeBackend) CreateVolume(_ context.Context, _ config.RepositoryPolicy, owner auth.Owner, id string, req VolumeRequest, expires time.Time, hash string) (VolumeStatus, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	item := VolumeStatus{ID: id, Phase: "Pending", Size: req.Size, ExpiresAt: expires, RequestHash: hash}
	b.volumes[id] = fakeVolume{owner: owner, item: item}
	return item, nil
}

func (b *fakeBackend) DeleteVolume(_ context.Context, _ config.RepositoryPolicy, owner auth.Owner, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	record, ok := b.volumes[id]
	if !ok || record.owner != owner {
		return ErrNotFound
	}
	delete(b.volumes, id)
	return nil
}

func (*fakeBackend) AttachVolume(context.Context, config.RepositoryPolicy, auth.Owner, string, string) error {
	return nil
}
func (*fakeBackend) DetachVolume(context.Context, config.RepositoryPolicy, auth.Owner, string, string) error {
	return nil
}
func (*fakeBackend) CleanupExpired(context.Context, time.Time) error { return nil }

func testServer(maxVMs, maxVolumes int) *Server {
	policy := config.RepositoryPolicy{RepositoryID: "123", Namespace: "ci",
		Images: []string{"default/ubuntu"}, Networks: []string{"default/network"},
		MaxCPU: 4, MaxMemory: "8Gi", MaxBootDiskSize: "40Gi", MaxVolumeSize: "100Gi",
		Quota: config.QuotaPolicy{MaxActiveVMs: maxVMs, MaxActiveVolumes: maxVolumes}}
	return NewServer(config.Config{Repositories: []config.RepositoryPolicy{policy}}, fakeVerifier{}, newFakeBackend())
}

func doRequest(s *Server, method, path, token, key string, body any) *httptest.ResponseRecorder {
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	r.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	w := httptest.NewRecorder()
	s.Handler.ServeHTTP(w, r)
	return w
}

func vmRequest() VMRequest {
	return VMRequest{Image: "default/ubuntu", Network: "default/network", CPU: 2, Memory: "4Gi", BootDiskSize: "20Gi"}
}

func TestConcurrentCreatesRespectActiveVMQuota(t *testing.T) {
	s := testServer(2, 4)
	var wg sync.WaitGroup
	results := make(chan int, 20)
	tokens := []string{"run-one", "run-two", "run-one-attempt-two"}
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := doRequest(s, http.MethodPost, "/v1/vms", tokens[i%len(tokens)], fmt.Sprintf("vm-%d", i), vmRequest())
			results <- w.Code
		}()
	}
	wg.Wait()
	close(results)
	created, rejected := 0, 0
	for code := range results {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			rejected++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if created != 2 || rejected != 18 {
		t.Fatalf("created=%d rejected=%d, want 2 and 18", created, rejected)
	}
}

func TestIdempotencyOwnershipAndQuotaRelease(t *testing.T) {
	s := testServer(1, 1)
	first := doRequest(s, http.MethodPost, "/v1/vms", "run-one", "vm-key", vmRequest())
	if first.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", first.Code, first.Body.String())
	}
	var vm VMStatus
	if err := json.Unmarshal(first.Body.Bytes(), &vm); err != nil {
		t.Fatal(err)
	}
	if got := doRequest(s, http.MethodPost, "/v1/vms", "run-one", "vm-key", vmRequest()); got.Code != http.StatusOK {
		t.Fatalf("idempotent retry: %d", got.Code)
	}
	changed := vmRequest()
	changed.CPU = 3
	if got := doRequest(s, http.MethodPost, "/v1/vms", "run-one", "vm-key", changed); got.Code != http.StatusConflict {
		t.Fatalf("changed retry: %d", got.Code)
	}
	if got := doRequest(s, http.MethodGet, "/v1/vms/"+vm.ID, "run-two", "", nil); got.Code != http.StatusNotFound {
		t.Fatalf("cross-run read: %d", got.Code)
	}
	if got := doRequest(s, http.MethodPost, "/v1/vms", "run-one", "another", vmRequest()); got.Code != http.StatusConflict {
		t.Fatalf("VM quota: %d", got.Code)
	}
	if got := doRequest(s, http.MethodDelete, "/v1/vms/"+vm.ID, "run-one", "", nil); got.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", got.Code)
	}
	if got := doRequest(s, http.MethodPost, "/v1/vms", "run-one", "another", vmRequest()); got.Code != http.StatusCreated {
		t.Fatalf("quota release: %d %s", got.Code, got.Body.String())
	}
	volume := doRequest(s, http.MethodPost, "/v1/volumes", "run-one", "vol-one", VolumeRequest{Size: "1Gi"})
	if volume.Code != http.StatusCreated {
		t.Fatalf("volume create: %d %s", volume.Code, volume.Body.String())
	}
	if got := doRequest(s, http.MethodPost, "/v1/volumes", "run-one", "vol-two", VolumeRequest{Size: "1Gi"}); got.Code != http.StatusConflict {
		t.Fatalf("volume quota: %d", got.Code)
	}
	quota := doRequest(s, http.MethodGet, "/v1/quota", "run-one", "", nil)
	var usage map[string]int
	if err := json.Unmarshal(quota.Body.Bytes(), &usage); err != nil {
		t.Fatal(err)
	}
	if usage["activeVMs"] != 1 || usage["activeVolumes"] != 1 {
		t.Fatalf("unexpected quota: %#v", usage)
	}
}

// countOnlyBackend catches quota paths that accidentally build full status lists.
type countOnlyBackend struct{ *fakeBackend }

func (*countOnlyBackend) ListVMs(context.Context, config.RepositoryPolicy, auth.Owner) ([]VMStatus, error) {
	panic("quota path called ListVMs")
}

func (*countOnlyBackend) ListVolumes(context.Context, config.RepositoryPolicy, auth.Owner) ([]VolumeStatus, error) {
	panic("quota path called ListVolumes")
}

func TestQuotaAndCreateUseCountsWithoutStatusLists(t *testing.T) {
	s := testServer(1, 1)
	s.backend = &countOnlyBackend{s.backend.(*fakeBackend)}
	if got := doRequest(s, http.MethodPost, "/v1/vms", "run-one", "vm", vmRequest()); got.Code != http.StatusCreated {
		t.Fatalf("create VM: %d %s", got.Code, got.Body.String())
	}
	if got := doRequest(s, http.MethodPost, "/v1/volumes", "run-one", "volume", VolumeRequest{Size: "1Gi"}); got.Code != http.StatusCreated {
		t.Fatalf("create volume: %d %s", got.Code, got.Body.String())
	}
	got := doRequest(s, http.MethodGet, "/v1/quota", "run-one", "", nil)
	if got.Code != http.StatusOK {
		t.Fatalf("quota: %d %s", got.Code, got.Body.String())
	}
	var usage map[string]int
	if err := json.Unmarshal(got.Body.Bytes(), &usage); err != nil {
		t.Fatal(err)
	}
	if usage["activeVMs"] != 1 || usage["activeVolumes"] != 1 {
		t.Fatalf("unexpected quota: %#v", usage)
	}
}

func TestQuotaSharedAcrossRunsAndAttempts(t *testing.T) {
	s := testServer(1, 1)
	created := doRequest(s, http.MethodPost, "/v1/vms", "run-one", "vm", vmRequest())
	if created.Code != http.StatusCreated {
		t.Fatalf("create VM: %d %s", created.Code, created.Body.String())
	}
	var vm VMStatus
	if err := json.Unmarshal(created.Body.Bytes(), &vm); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"run-two", "run-one-attempt-two"} {
		quota := doRequest(s, http.MethodGet, "/v1/quota", token, "", nil)
		if quota.Code != http.StatusOK {
			t.Fatalf("quota for %s: %d", token, quota.Code)
		}
		var usage map[string]int
		if err := json.Unmarshal(quota.Body.Bytes(), &usage); err != nil {
			t.Fatal(err)
		}
		if usage["activeVMs"] != 1 {
			t.Fatalf("VM quota for %s: %#v", token, usage)
		}
		if got := doRequest(s, http.MethodPost, "/v1/vms", token, "another", vmRequest()); got.Code != http.StatusConflict {
			t.Fatalf("VM create for %s: %d %s", token, got.Code, got.Body.String())
		}
		if got := doRequest(s, http.MethodGet, "/v1/vms/"+vm.ID, token, "", nil); got.Code != http.StatusNotFound {
			t.Fatalf("cross-attempt VM read for %s: %d", token, got.Code)
		}
	}
	volume := doRequest(s, http.MethodPost, "/v1/volumes", "run-two", "volume", VolumeRequest{Size: "1Gi"})
	if volume.Code != http.StatusCreated {
		t.Fatalf("create volume: %d %s", volume.Code, volume.Body.String())
	}
	if got := doRequest(s, http.MethodPost, "/v1/volumes", "run-one", "another-volume", VolumeRequest{Size: "1Gi"}); got.Code != http.StatusConflict {
		t.Fatalf("volume create across runs: %d %s", got.Code, got.Body.String())
	}
	quota := doRequest(s, http.MethodGet, "/v1/quota", "run-one-attempt-two", "", nil)
	var usage map[string]int
	if err := json.Unmarshal(quota.Body.Bytes(), &usage); err != nil {
		t.Fatal(err)
	}
	if usage["activeVMs"] != 1 || usage["activeVolumes"] != 1 {
		t.Fatalf("repository quota: %#v", usage)
	}
	if got := doRequest(s, http.MethodDelete, "/v1/vms/"+vm.ID, "run-one", "", nil); got.Code != http.StatusNoContent {
		t.Fatalf("delete VM: %d", got.Code)
	}
	if got := doRequest(s, http.MethodPost, "/v1/vms", "run-two", "another", vmRequest()); got.Code != http.StatusCreated {
		t.Fatalf("VM quota after delete: %d %s", got.Code, got.Body.String())
	}
}

func TestResourceIdentityUsesShortHRGWName(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "456", RunAttempt: "2"}
	id, hash := resourceIdentity(owner, "vm", "example", vmRequest())
	if id != "hrgw-u6bpqc4qo7b6k35d" {
		t.Fatalf("ID = %q", id)
	}
	if old := legacyResourceID(owner, "vm", "example"); old != "rgw-u6bpqc4qo7b6k35diocof63v" {
		t.Fatalf("legacy ID = %q", old)
	}
	changed := vmRequest()
	changed.CPU++
	changedID, changedHash := resourceIdentity(owner, "vm", "example", changed)
	if changedID != id || changedHash == hash {
		t.Fatalf("request change: ID = %q, hash = %q", changedID, changedHash)
	}
}

func TestLegacyResourceIDRetry(t *testing.T) {
	owner := auth.Owner{RepositoryID: "123", RunID: "run-one", RunAttempt: "1"}
	s := testServer(2, 2)
	backend := s.backend.(*fakeBackend)
	vmID := legacyResourceID(owner, "vm", "old-vm")
	vmBody := vmRequest()
	vmBody.TTLSeconds = int(config.DefaultTTL.Seconds())
	_, vmHash := resourceIdentity(owner, "vm", "old-vm", vmBody)
	backend.vms[vmID] = fakeVM{owner: owner, item: VMStatus{ID: vmID, RequestHash: vmHash}}
	volumeID := legacyResourceID(owner, "volume", "old-volume")
	_, volumeHash := resourceIdentity(owner, "volume", "old-volume", VolumeRequest{Size: "1Gi", TTLSeconds: int(config.DefaultTTL.Seconds())})
	backend.volumes[volumeID] = fakeVolume{owner: owner, item: VolumeStatus{ID: volumeID, RequestHash: volumeHash}}

	for _, tc := range []struct {
		path, key, id string
		body          any
	}{
		{"/v1/vms", "old-vm", vmID, vmRequest()},
		{"/v1/volumes", "old-volume", volumeID, VolumeRequest{Size: "1Gi"}},
	} {
		got := doRequest(s, http.MethodPost, tc.path, "run-one", tc.key, tc.body)
		if got.Code != http.StatusOK {
			t.Fatalf("retry %s: %d %s", tc.path, got.Code, got.Body.String())
		}
		var item struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(got.Body.Bytes(), &item); err != nil || item.ID != tc.id {
			t.Fatalf("retry %s: ID %q, error %v", tc.path, item.ID, err)
		}
	}
	changed := vmRequest()
	changed.CPU++
	if got := doRequest(s, http.MethodPost, "/v1/vms", "run-one", "old-vm", changed); got.Code != http.StatusConflict {
		t.Fatalf("changed legacy retry: %d %s", got.Code, got.Body.String())
	}
	if len(backend.vms) != 1 || len(backend.volumes) != 1 {
		t.Fatalf("retry created a duplicate: %d VMs, %d volumes", len(backend.vms), len(backend.volumes))
	}
}

func TestLocalSmokeResourcesStaySeparateFromWorkflowRun(t *testing.T) {
	policy := config.RepositoryPolicy{RepositoryID: "123", Namespace: "ci",
		Images: []string{"default/ubuntu"}, Networks: []string{"default/network"},
		MaxCPU: 4, MaxMemory: "8Gi", MaxBootDiskSize: "40Gi", MaxVolumeSize: "100Gi",
		Quota: config.QuotaPolicy{MaxActiveVMs: 2, MaxActiveVolumes: 2}}
	token := strings.Repeat("ab", 32)
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Repositories: []config.RepositoryPolicy{policy},
		LocalSmoke: config.LocalSmokeConfig{RepositoryID: "123", TokenFile: path}}
	verifier, err := auth.NewLocalSmokeVerifier(fakeVerifier{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg, verifier, newFakeBackend())
	local := doRequest(s, http.MethodPost, "/v1/vms", token, "local-vm", vmRequest())
	workflow := doRequest(s, http.MethodPost, "/v1/vms", "run-one", "workflow-vm", vmRequest())
	if local.Code != http.StatusCreated || workflow.Code != http.StatusCreated {
		t.Fatalf("create local=%d workflow=%d", local.Code, workflow.Code)
	}
	var localVM, workflowVM VMStatus
	if err := json.Unmarshal(local.Body.Bytes(), &localVM); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(workflow.Body.Bytes(), &workflowVM); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, caller string }{
		{localVM.ID, "run-one"}, {workflowVM.ID, token},
	} {
		if got := doRequest(s, http.MethodGet, "/v1/vms/"+tc.id, tc.caller, "", nil); got.Code != http.StatusNotFound {
			t.Fatalf("cross-owner read of %s returned %d", tc.id, got.Code)
		}
	}
	if got := doRequest(s, http.MethodGet, "/v1/vms/"+localVM.ID, token, "", nil); got.Code != http.StatusOK {
		t.Fatalf("local owner cannot read its VM: %d", got.Code)
	}
}
