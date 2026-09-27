package smoke

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bk201-org/harvester-runner-gateway/client"
)

type fakeGateway struct {
	mu            sync.Mutex
	createTarget  int
	createStarted int
	createGate    chan struct{}
	maxVMs        int
	maxVolumes    int
	failAttach    bool
	attachFailed  bool
	nextVM        int
	nextVolume    int
	power         map[string]string
	volumes       map[string]bool
	attached      map[string]string
	calls         map[string]int
}

func newFakeGateway(createTarget int) *fakeGateway {
	return &fakeGateway{
		createTarget: createTarget, createGate: make(chan struct{}), maxVMs: 3, maxVolumes: 3,
		power: make(map[string]string), volumes: make(map[string]bool),
		attached: make(map[string]string), calls: make(map[string]int),
	}
}

func (f *fakeGateway) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls[r.Method+" "+r.URL.Path]++
	f.mu.Unlock()
	if r.URL.Path == "/v1/quota" {
		f.mu.Lock()
		response := client.Quota{MaxActiveVMs: f.maxVMs, MaxActiveVolumes: f.maxVolumes}
		f.mu.Unlock()
		writeTestJSON(w, response)
		return
	}
	if r.URL.Path == "/v1/vms" && r.Method == http.MethodPost {
		f.mu.Lock()
		f.nextVM++
		id := fmt.Sprintf("vm%d", f.nextVM)
		f.power[id] = "on"
		f.createStarted++
		if f.createStarted == f.createTarget {
			close(f.createGate)
		}
		gate := f.createGate
		f.mu.Unlock()
		<-gate
		w.WriteHeader(http.StatusCreated)
		writeTestJSON(w, client.VMStatus{ID: id, Phase: "Running", PowerState: "on", Ready: true, IPAddresses: []string{"10.0.0.10"}})
		return
	}
	if r.URL.Path == "/v1/vms" && r.Method == http.MethodGet {
		f.mu.Lock()
		items := make([]client.VMStatus, 0, len(f.power))
		for id, power := range f.power {
			items = append(items, client.VMStatus{ID: id, Phase: "Running", PowerState: power})
		}
		f.mu.Unlock()
		writeTestJSON(w, items)
		return
	}
	if r.URL.Path == "/v1/volumes" && r.Method == http.MethodPost {
		f.mu.Lock()
		f.nextVolume++
		id := fmt.Sprintf("vol%d", f.nextVolume)
		f.volumes[id] = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		writeTestJSON(w, client.VolumeStatus{ID: id, Phase: "Bound", Size: "1Gi"})
		return
	}
	if r.URL.Path == "/v1/volumes" && r.Method == http.MethodGet {
		f.mu.Lock()
		items := make([]client.VolumeStatus, 0, len(f.volumes))
		for id := range f.volumes {
			items = append(items, client.VolumeStatus{ID: id, Phase: "Bound", Size: "1Gi"})
		}
		f.mu.Unlock()
		writeTestJSON(w, items)
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 3 && parts[0] == "v1" && parts[1] == "vms" {
		id := parts[2]
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			power := f.power[id]
			f.mu.Unlock()
			writeTestJSON(w, client.VMStatus{ID: id, Phase: "Running", PowerState: power, Ready: power == "on", IPAddresses: []string{"10.0.0.10"}})
		case http.MethodDelete:
			f.mu.Lock()
			delete(f.power, id)
			for volumeID, vmID := range f.attached {
				if vmID == id {
					delete(f.attached, volumeID)
					delete(f.volumes, volumeID)
				}
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}
	if len(parts) == 4 && parts[0] == "v1" && parts[1] == "vms" && parts[3] == "power" {
		var body struct {
			State string `json:"state"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.power[parts[2]] = body.State
		f.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if len(parts) == 4 && parts[0] == "v1" && parts[1] == "vms" && parts[3] == "reboot" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if len(parts) == 5 && parts[0] == "v1" && parts[1] == "vms" && parts[3] == "volumes" {
		vmID, volumeID := parts[2], parts[4]
		if r.Method == http.MethodPut {
			f.mu.Lock()
			if f.failAttach && !f.attachFailed {
				f.attachFailed = true
				f.mu.Unlock()
				w.WriteHeader(http.StatusUnprocessableEntity)
				writeTestJSON(w, map[string]string{"code": "attach_failed", "message": "simulated failure"})
				return
			}
			f.attached[volumeID] = vmID
			f.mu.Unlock()
		} else {
			f.mu.Lock()
			delete(f.attached, volumeID)
			f.mu.Unlock()
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if len(parts) == 3 && parts[0] == "v1" && parts[1] == "volumes" {
		id := parts[2]
		switch r.Method {
		case http.MethodGet:
			f.mu.Lock()
			vmID := f.attached[id]
			exists := f.volumes[id]
			f.mu.Unlock()
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				writeTestJSON(w, map[string]string{"code": "not_found", "message": "volume not found"})
				return
			}
			phase := ""
			if vmID != "" {
				phase = "Ready"
			}
			writeTestJSON(w, client.VolumeStatus{ID: id, Phase: "Bound", Size: "1Gi", AttachedTo: vmID, AttachmentPhase: phase})
		case http.MethodDelete:
			f.mu.Lock()
			delete(f.attached, id)
			delete(f.volumes, id)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}
	w.WriteHeader(http.StatusNotFound)
	writeTestJSON(w, map[string]string{"code": "not_found", "message": "unexpected test request"})
}

func writeTestJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}

func smokeClient(t *testing.T, server *httptest.Server) *client.Client {
	t.Helper()
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, cert, 0600); err != nil {
		t.Fatal(err)
	}
	api, err := client.New(client.Config{URL: server.URL, CACert: path, Token: "secret", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(api.Close)
	return api
}

func testConfig(concurrency int) config {
	return config{
		image: "default/image", network: "default/network", memory: "2Gi", bootDiskSize: "20Gi",
		volumeSize: "1Gi", concurrency: concurrency,
		waitTimeout: time.Second, pollInterval: time.Millisecond,
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestConcurrentFullLifecycles(t *testing.T) {
	fake := newFakeGateway(3)
	server := httptest.NewTLSServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	if err := run(context.Background(), smokeClient(t, server), testConfig(3), discardLogger()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.createStarted != 3 || fake.nextVM != 3 || fake.nextVolume != 6 {
		t.Fatalf("creates=%d VMs=%d volumes=%d", fake.createStarted, fake.nextVM, fake.nextVolume)
	}
	if len(fake.power) != 0 || len(fake.volumes) != 0 || len(fake.attached) != 0 {
		t.Fatalf("resources remain: VMs=%v volumes=%v attachments=%v", fake.power, fake.volumes, fake.attached)
	}
	for _, operation := range []string{
		"POST /v1/vms", "POST /v1/volumes", "PUT /v1/vms/vm1/power", "POST /v1/vms/vm1/reboot",
		"GET /v1/vms", "GET /v1/volumes",
	} {
		if fake.calls[operation] == 0 {
			t.Errorf("operation not exercised: %s", operation)
		}
	}
	volumeDeletes := 0
	for operation, count := range fake.calls {
		if strings.HasPrefix(operation, "DELETE /v1/volumes/") {
			volumeDeletes += count
		}
	}
	if volumeDeletes != 3 {
		t.Fatalf("explicit volume deletes = %d, want 3; attached volumes should be removed with their VMs", volumeDeletes)
	}
}

func TestQuotaFailureCreatesNothing(t *testing.T) {
	fake := newFakeGateway(1)
	fake.maxVMs = 2
	server := httptest.NewTLSServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	err := run(context.Background(), smokeClient(t, server), testConfig(3), discardLogger())
	if err == nil || !strings.Contains(err.Error(), "insufficient quota") {
		t.Fatalf("error=%v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.createStarted != 0 {
		t.Fatalf("created %d VMs", fake.createStarted)
	}
}

func TestFailureCleansUpCreatedResources(t *testing.T) {
	fake := newFakeGateway(3)
	fake.failAttach = true
	server := httptest.NewTLSServer(http.HandlerFunc(fake.handler))
	defer server.Close()
	err := run(context.Background(), smokeClient(t, server), testConfig(3), discardLogger())
	if err == nil || !strings.Contains(err.Error(), "simulated failure") {
		t.Fatalf("error=%v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	vmDeletes, volumeDeletes := 0, 0
	for operation, count := range fake.calls {
		if strings.HasPrefix(operation, "DELETE /v1/vms/") && !strings.Contains(operation, "/volumes/") {
			vmDeletes += count
		}
		if strings.HasPrefix(operation, "DELETE /v1/volumes/") {
			volumeDeletes += count
		}
	}
	if vmDeletes != 3 || volumeDeletes != 3 || len(fake.power) != 0 || len(fake.volumes) != 0 || len(fake.attached) != 0 {
		t.Fatalf("VM deletes=%d volume deletes=%d resources=%v/%v/%v calls=%v",
			vmDeletes, volumeDeletes, fake.power, fake.volumes, fake.attached, fake.calls)
	}
}

func TestActionsOptInAndConcurrencyOverride(t *testing.T) {
	env := map[string]string{
		"GITHUB_ACTIONS": "true", "GATEWAY_URL": "https://gateway.example", "GATEWAY_IMAGE": "default/image",
		"GATEWAY_NETWORK": "default/network", "ACTIONS_ID_TOKEN_REQUEST_URL": "https://oidc.example/token",
		"ACTIONS_ID_TOKEN_REQUEST_TOKEN": "request-token", "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "2",
	}
	getenv := func(key string) string { return env[key] }
	if _, err := loadConfig(getenv); err == nil {
		t.Fatal("Actions smoke ran without opt-in")
	}
	env["GATEWAY_SMOKE"] = "1"
	env["GATEWAY_SMOKE_CONCURRENCY"] = "4"
	cfg, err := loadConfig(getenv)
	if err != nil || cfg.concurrency != 4 {
		t.Fatalf("config=%+v error=%v", cfg, err)
	}
}

func TestLocalConfigConcurrencyAndEnvironmentOverride(t *testing.T) {
	directory := t.TempDir()
	tokenPath := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenPath, []byte(strings.Repeat("ab", 32)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "smoke.json")
	data := fmt.Sprintf(`{"gatewayURL":"https://gateway.example","image":"default/image","network":"default/network","tokenFile":%q,"concurrency":5}`, tokenPath)
	if err := os.WriteFile(configPath, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"GATEWAY_SMOKE_CONFIG": configPath}
	getenv := func(key string) string { return env[key] }
	cfg, err := loadConfig(getenv)
	if err != nil || cfg.concurrency != 5 {
		t.Fatalf("config=%+v error=%v", cfg, err)
	}
	env["GATEWAY_SMOKE_CONCURRENCY"] = "2"
	cfg, err = loadConfig(getenv)
	if err != nil || cfg.concurrency != 2 {
		t.Fatalf("overridden config=%+v error=%v", cfg, err)
	}
}
