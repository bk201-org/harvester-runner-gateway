package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bk201-org/harvester-runner-gateway/client"
	"sigs.k8s.io/yaml"
)

const testKey = "ssh-ed25519 AAAATEST generated"

func TestCloudConfigMergesNamedUser(t *testing.T) {
	input := "#cloud-config\npackages: [git]\nusers:\n  - default\n  - name: ci\n    groups: [docker]\n    ssh_authorized_keys: [existing-key]\n"
	got, err := cloudConfig(input, "ci", testKey)
	if err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	converted, err := yaml.YAMLToJSON([]byte(got))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(converted, &config); err != nil {
		t.Fatal(err)
	}
	users := config["users"].([]any)
	user := users[1].(map[string]any)
	if len(users) != 2 || users[0] != "default" || user["name"] != "ci" ||
		!reflect.DeepEqual(user["groups"], []any{"docker"}) ||
		!reflect.DeepEqual(user["ssh_authorized_keys"], []any{"existing-key", testKey}) ||
		!reflect.DeepEqual(config["packages"], []any{"git"}) {
		t.Fatalf("unexpected merged cloud-config: %#v", config)
	}
	if _, err := cloudConfig(got, "ci", testKey); err != nil {
		t.Fatalf("repeated merge should deduplicate the key: %v", err)
	}
}

func TestCloudConfigRejectsConflictingUsers(t *testing.T) {
	for _, input := range []string{
		"#cloud-config\nuser: root\n",
		"#cloud-config\nuser: ci\nusers: [ci]\n",
		"#cloud-config\nusers: [ci, ci]\n",
		"#cloud-config\nusers: invalid\n",
		"#cloud-config\nusers: ['bob,ci']\n",
		"#cloud-config\nusers:\n  - name: ci\n    ssh_authorized_keys: invalid\n",
		"packages: [git]\n",
	} {
		if _, err := cloudConfig(input, "ci", testKey); err == nil {
			t.Fatalf("accepted conflicting user-data %q", input)
		}
	}
}

type fakeVMAPI struct {
	created       []client.VMRequest
	deleted       []string
	statuses      map[string][]client.VMStatus
	createErrorAt int
	deleteErrors  map[string]error
}

func (f *fakeVMAPI) CreateVM(_ context.Context, req client.VMRequest) (client.VMStatus, error) {
	f.created = append(f.created, req)
	if f.createErrorAt == len(f.created) {
		return client.VMStatus{}, errors.New("quota exceeded")
	}
	return client.VMStatus{ID: "vm-" + string(rune('0'+len(f.created)))}, nil
}

func (f *fakeVMAPI) GetVM(_ context.Context, id string) (client.VMStatus, error) {
	items := f.statuses[id]
	status := items[0]
	if len(items) > 1 {
		f.statuses[id] = items[1:]
	}
	return status, nil
}

func (f *fakeVMAPI) DeleteVM(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErrors[id]
}

func TestProvisionWaitAndSSHConfig(t *testing.T) {
	dir, err := os.MkdirTemp(t.TempDir(), "hvst-cluster-")
	if err != nil {
		t.Fatal(err)
	}
	state := clusterState{Dir: dir, IDs: []string{}}
	statePath := filepath.Join(dir, "cluster.json")
	api := &fakeVMAPI{statuses: map[string][]client.VMStatus{
		"vm-1": {{ID: "vm-1", Ready: false}, {ID: "vm-1", Ready: true, IPAddresses: []string{"10.0.0.1"}}},
		"vm-2": {{ID: "vm-2", Ready: true, IPAddresses: []string{"10.0.0.2"}}},
	}}
	req := client.VMRequest{Image: "default/ubuntu", Network: "default/net", CPU: 2, Memory: "4Gi", BootDiskSize: "20Gi", UserData: "#cloud-config\n"}
	if err := provision(context.Background(), api, req, 2, &state, statePath); err != nil {
		t.Fatal(err)
	}
	if len(api.created) != 2 || !reflect.DeepEqual(api.created[0], api.created[1]) {
		t.Fatalf("VM requests differ: %#v", api.created)
	}
	stored, err := readState(statePath, filepath.Dir(dir))
	if err != nil || !reflect.DeepEqual(stored.IDs, []string{"vm-1", "vm-2"}) {
		t.Fatalf("recorded IDs = %#v, %v", stored.IDs, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	statuses, err := waitAll(ctx, api, state.IDs, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ssh_config")
	if err := writeSSHConfig(path, filepath.Join(dir, "known_hosts"), filepath.Join(dir, "id_ed25519"), "ci", statuses); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Host vm-1", "HostName 10.0.0.1", "Host vm-2", "HostName 10.0.0.2", "User ci", "IdentitiesOnly yes"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("SSH config lacks %q: %s", want, data)
		}
	}
	if _, err := exec.LookPath("ssh"); err == nil {
		got, err := exec.Command("ssh", "-G", "-F", path, "vm-1").CombinedOutput()
		if err != nil || !strings.Contains(string(got), "hostname 10.0.0.1") || !strings.Contains(string(got), "user ci") {
			t.Fatalf("OpenSSH rejected generated config: %v, %s", err, got)
		}
	}
}

func TestPartialCreateCleanupContinues(t *testing.T) {
	dir, err := os.MkdirTemp(t.TempDir(), "hvst-cluster-")
	if err != nil {
		t.Fatal(err)
	}
	state := clusterState{Dir: dir, IDs: []string{}}
	path := filepath.Join(dir, "cluster.json")
	api := &fakeVMAPI{createErrorAt: 2, deleteErrors: map[string]error{"vm-1": errors.New("temporary delete failure")}}
	if err := provision(context.Background(), api, client.VMRequest{}, 2, &state, path); err == nil {
		t.Fatal("expected second create to fail")
	}
	stored, err := readState(path, filepath.Dir(dir))
	if err != nil || !reflect.DeepEqual(stored.IDs, []string{"vm-1"}) {
		t.Fatalf("partial state = %#v, %v", stored.IDs, err)
	}
	if err := cleanup(context.Background(), api, stored); err == nil || !strings.Contains(err.Error(), "vm-1") {
		t.Fatalf("cleanup error = %v", err)
	}
	if !reflect.DeepEqual(api.deleted, []string{"vm-1"}) {
		t.Fatalf("deleted IDs = %v", api.deleted)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("cluster SSH directory remains: %v", err)
	}
}

func TestWaitAllTimesOut(t *testing.T) {
	api := &fakeVMAPI{statuses: map[string][]client.VMStatus{"vm-1": {{ID: "vm-1", Ready: false}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := waitAll(ctx, api, []string{"vm-1"}, time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v", err)
	}
}

func TestCreateAndCleanupLifecycle(t *testing.T) {
	temp := t.TempDir()
	stateOutput := filepath.Join(temp, "github_state")
	resultOutput := filepath.Join(temp, "github_output")
	for _, path := range []string{stateOutput, resultOutput} {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	api := &fakeVMAPI{statuses: map[string][]client.VMStatus{
		"vm-1": {{ID: "vm-1", Ready: true, IPAddresses: []string{"10.0.0.1"}}},
		"vm-2": {{ID: "vm-2", Ready: true, IPAddresses: []string{"10.0.0.2"}}},
	}}
	opt := options{TempDir: temp, StateFile: stateOutput, OutputFile: resultOutput,
		Username: "ci", Count: 2, WaitTimeout: time.Second,
		Request: client.VMRequest{Image: "default/ubuntu", Network: "default/net", CPU: 2, Memory: "4Gi", BootDiskSize: "20Gi"}}
	if err := create(context.Background(), api, opt); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.created[0], api.created[1]) || !strings.Contains(api.created[0].UserData, "ssh_authorized_keys:") {
		t.Fatalf("VM requests do not share injected cloud-init: %#v", api.created)
	}
	stateLine, err := os.ReadFile(stateOutput)
	if err != nil || !strings.HasPrefix(string(stateLine), "cluster_state=") {
		t.Fatalf("GitHub state = %q, %v", stateLine, err)
	}
	statePath := strings.TrimSpace(strings.TrimPrefix(string(stateLine), "cluster_state="))
	state, err := readState(statePath, temp)
	if err != nil || len(state.IDs) != 2 {
		t.Fatalf("state = %#v, %v", state, err)
	}
	outputs, err := os.ReadFile(resultOutput)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"vm-ids=[\"vm-1\",\"vm-2\"]", "ssh-config-path=" + filepath.Join(state.Dir, "ssh_config"), "private-key-path=" + filepath.Join(state.Dir, "id_ed25519")} {
		if !strings.Contains(string(outputs), want) {
			t.Fatalf("GitHub outputs lack %q: %s", want, outputs)
		}
	}
	if _, err := os.Stat(filepath.Join(state.Dir, "id_ed25519.pub")); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(context.Background(), api, state); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(api.deleted, []string{"vm-2", "vm-1"}) {
		t.Fatalf("deleted IDs = %v", api.deleted)
	}
}

func TestCleanupContinuesAfterDeleteError(t *testing.T) {
	dir, err := os.MkdirTemp(t.TempDir(), "hvst-cluster-")
	if err != nil {
		t.Fatal(err)
	}
	api := &fakeVMAPI{deleteErrors: map[string]error{
		"vm-2": errors.New("gateway HTTP 404: absent"),
		"vm-1": errors.New("gateway HTTP 503: unavailable"),
	}}
	err = cleanup(context.Background(), api, clusterState{Dir: dir, IDs: []string{"vm-1", "vm-2"}})
	if err == nil || !strings.Contains(err.Error(), "vm-1") || strings.Contains(err.Error(), "vm-2") {
		t.Fatalf("cleanup error = %v", err)
	}
	if !reflect.DeepEqual(api.deleted, []string{"vm-2", "vm-1"}) {
		t.Fatalf("cleanup skipped an ID: %v", api.deleted)
	}
}
