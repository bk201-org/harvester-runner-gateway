package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bk201-org/harvester-runner-gateway/client"
	"sigs.k8s.io/yaml"
)

type localAPI struct {
	requests    []client.VMRequest
	vms         map[string]client.VMStatus
	createErrAt int
	deleteErr   error
	holdDelete  bool
	getErr      error
	notReady    bool
}

func (f *localAPI) CreateVM(ctx context.Context, r client.VMRequest) (client.VMStatus, error) {
	f.requests = append(f.requests, r)
	if len(f.requests) == f.createErrAt {
		return client.VMStatus{}, errors.New("lost create response")
	}
	id := "vm-" + string(rune('0'+len(f.requests)))
	s := client.VMStatus{ID: id, Ready: !f.notReady, Phase: "Running", IPAddresses: []string{"10.0.0.2"}, ExpiresAt: time.Now().Add(time.Hour)}
	f.vms[id] = s
	return s, nil
}
func (f *localAPI) GetVM(ctx context.Context, id string) (client.VMStatus, error) {
	if err := ctx.Err(); err != nil {
		return client.VMStatus{}, err
	}
	if f.getErr != nil {
		return client.VMStatus{}, f.getErr
	}
	if s, ok := f.vms[id]; ok {
		return s, nil
	}
	return client.VMStatus{}, &client.HTTPError{StatusCode: 404, Message: "not_found"}
}
func (f *localAPI) DeleteVM(ctx context.Context, id string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if _, ok := f.vms[id]; !ok {
		return &client.HTTPError{StatusCode: 404, Message: "not_found"}
	}
	if !f.holdDelete {
		delete(f.vms, id)
	}
	return nil
}
func localFixture(t *testing.T) (*localAPI, LocalConfig, options, string) {
	t.Helper()
	c := LocalConfig{Count: 2, Image: "default/ubuntu", Network: "default/net", CPU: 2, Memory: "4Gi", BootDiskSize: "20Gi", Username: "ci", UserData: "#cloud-config\npackages: [curl]\n"}
	o, err := c.options()
	if err != nil {
		t.Fatal(err)
	}
	return &localAPI{vms: map[string]client.VMStatus{}}, c, o, filepath.Join(t.TempDir(), "cluster")
}
func localRun(ctx context.Context, f *localAPI, op, dir string, c LocalConfig, o options, out io.Writer) error {
	return runLocal(ctx, f, op, "https://gateway.example", dir, c, o, time.Second, out)
}

func TestLocalLifecycleAndFailedCleanup(t *testing.T) {
	f, c, o, dir := localFixture(t)
	var out bytes.Buffer
	if err := localRun(context.Background(), f, "create", dir, c, o, &out); err != nil {
		t.Fatal(err)
	}
	var report localReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || len(report.VMs) != 2 || len(report.SSHCommands) != 2 {
		t.Fatalf("report=%s err=%v", out.String(), err)
	}
	state, err := readLocalState(dir, "https://gateway.example")
	if err != nil || len(state.IDs) != 2 {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	for _, name := range []string{"cluster.json", "id_ed25519", "ssh_config"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Mode().Perm()&0077 != 0 {
			t.Fatalf("unsafe or absent %s: %v", name, err)
		}
	}
	if err := localRun(context.Background(), f, "create", dir, c, o, io.Discard); err == nil || len(f.requests) != 2 {
		t.Fatal("existing directory reused")
	}
	// A deleted VM is reported and omitted from the refreshed SSH config.
	delete(f.vms, "vm-2")
	if err := localRun(context.Background(), f, "status", dir, c, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	ssh, _ := os.ReadFile(filepath.Join(dir, "ssh_config"))
	if strings.Contains(string(ssh), "Host vm-2") {
		t.Fatal("stale SSH entry")
	}
	f.deleteErr = errors.New("gateway unavailable")
	if err := localRun(context.Background(), f, "delete", dir, c, o, io.Discard); err == nil {
		t.Fatal("cleanup failure hidden")
	}
	if _, err := os.Stat(filepath.Join(dir, "id_ed25519")); err != nil {
		t.Fatal("key removed after cleanup failure")
	}
	f.deleteErr = nil
	for i := 0; i < 2; i++ {
		if err := localRun(context.Background(), f, "delete", dir, c, o, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "id_ed25519")); !os.IsNotExist(err) {
		t.Fatal("key retained after cleanup")
	}
	state, err = readLocalState(dir, "https://gateway.example")
	if err != nil || len(state.IDs) != 0 {
		t.Fatalf("tombstone: %+v %v", state, err)
	}
}

func TestLocalPartialCreateTimeoutAndInterruption(t *testing.T) {
	for _, scenario := range []string{"partial", "timeout", "interrupt"} {
		t.Run(scenario, func(t *testing.T) {
			f, c, o, dir := localFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "partial":
				f.createErrAt = 2
			case "timeout":
				f.notReady = true
				o.WaitTimeout = time.Millisecond
			case "interrupt":
				cancel()
			}
			if err := localRun(ctx, f, "create", dir, c, o, io.Discard); err == nil {
				t.Fatal("expected failure")
			}
			state, err := readLocalState(dir, "https://gateway.example")
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "partial" && (len(state.IDs) != 1 || len(f.requests) != 2) {
				t.Fatalf("partial IDs=%v calls=%d", state.IDs, len(f.requests))
			}
			if scenario == "timeout" && len(state.IDs) != 2 {
				t.Fatal("timeout lost IDs")
			}
			if err := localRun(context.Background(), f, "delete", dir, c, o, io.Discard); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLocalDeletionRequiresConfirmation(t *testing.T) {
	f, _, _, _ := localFixture(t)
	f.vms["vm-1"] = client.VMStatus{ID: "vm-1"}
	f.holdDelete = true
	state := clusterState{IDs: []string{"vm-1"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if err := deleteLocal(ctx, f, state, time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unconfirmed deletion: %v", err)
	}
	f.getErr = errors.New("connection lost")
	if err := deleteLocal(context.Background(), f, state, time.Millisecond); err == nil {
		t.Fatal("failed confirmation accepted")
	}
}

func TestLocalStateLockAndGatewayBinding(t *testing.T) {
	f, c, o, dir := localFixture(t)
	if err := localRun(context.Background(), f, "create", dir, c, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	lock, err := lockState(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockState(dir, false); err == nil {
		second.Close()
		t.Fatal("concurrent command acquired state")
	}
	lock.Close()
	next, err := lockState(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
	if _, err := readLocalState(dir, "https://wrong-gateway.example"); err == nil {
		t.Fatal("wrong gateway accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if lock, err := lockState(link, false); err == nil {
		lock.Close()
		t.Fatal("symlink state accepted")
	}
}

func TestLocalAndActionProvisioningEquivalent(t *testing.T) {
	_, c, opt, _ := localFixture(t)
	env := map[string]string{"INPUT_GATEWAY-URL": "https://gateway.example", "RUNNER_TEMP": "/tmp", "GITHUB_STATE": "/tmp/state", "GITHUB_OUTPUT": "/tmp/output", "INPUT_VM-COUNT": "2", "INPUT_IMAGE": c.Image, "INPUT_NETWORK": c.Network, "INPUT_CPU": "2", "INPUT_MEMORY": c.Memory, "INPUT_BOOT-DISK-SIZE": c.BootDiskSize, "INPUT_USERNAME": c.Username, "INPUT_USER-DATA": c.UserData}
	action, err := readOptions(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(action.Request, opt.Request) || action.Count != opt.Count || action.WaitTimeout != opt.WaitTimeout {
		t.Fatalf("inputs differ: %+v %+v", action, opt)
	}
	// Generated keys differ, but replacing each with a sentinel must leave the
	// same merged provisioning request, including all user-supplied cloud-config.
	var requests []client.VMRequest
	for _, o := range []options{action, opt} {
		dir := t.TempDir()
		_, request, err := prepareRequest(context.Background(), dir, o)
		if err != nil {
			t.Fatal(err)
		}
		key, err := os.ReadFile(filepath.Join(dir, "id_ed25519.pub"))
		if err != nil {
			t.Fatal(err)
		}
		normalized, err := yaml.YAMLToJSON([]byte(request.UserData))
		if err != nil {
			t.Fatal(err)
		}
		request.UserData = strings.ReplaceAll(string(normalized), strings.TrimSpace(string(key)), "TEST_KEY")
		requests = append(requests, request)
	}
	if !reflect.DeepEqual(requests[0], requests[1]) {
		t.Fatal("action and local VM requests differ")
	}
}
