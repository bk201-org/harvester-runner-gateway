package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testVMStatus = `{"id":"vm1","phase":"Running","powerState":"on","ready":true,"ipAddresses":["10.0.0.10"],"attachedVolumeIDs":[],"expiresAt":"2026-09-27T00:00:00Z"}`
const testVolumeStatus = `{"id":"vol1","phase":"Bound","size":"1Gi","expiresAt":"2026-09-27T00:00:00Z"}`

func TestTypedOperations(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" && r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing authentication")
		}
		switch {
		case r.URL.Path == "/healthz" || r.URL.Path == "/readyz":
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/v1/quota":
			fmt.Fprint(w, `{"maxActiveVMs":3,"activeVMs":0,"maxActiveVolumes":4,"activeVolumes":0}`)
		case r.URL.Path == "/v1/vms" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, testVMStatus)
		case r.URL.Path == "/v1/vms" && r.Method == http.MethodGet:
			fmt.Fprintf(w, "[%s]", testVMStatus)
		case r.URL.Path == "/v1/vms/vm1" && r.Method == http.MethodGet:
			fmt.Fprint(w, testVMStatus)
		case r.URL.Path == "/v1/vms/vm1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasPrefix(r.URL.Path, "/v1/vms/vm1/"):
			w.WriteHeader(http.StatusAccepted)
		case r.URL.Path == "/v1/volumes" && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, testVolumeStatus)
		case r.URL.Path == "/v1/volumes" && r.Method == http.MethodGet:
			fmt.Fprintf(w, "[%s]", testVolumeStatus)
		case r.URL.Path == "/v1/volumes/vol1" && r.Method == http.MethodGet:
			fmt.Fprint(w, testVolumeStatus)
		case r.URL.Path == "/v1/volumes/vol1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected operation %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	api := newClient(t, configuration(t, server))
	ctx := context.Background()
	if err := api.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if err := api.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if quota, err := api.Quota(ctx); err != nil || quota.MaxActiveVMs != 3 {
		t.Fatalf("quota=%+v error=%v", quota, err)
	}
	if vm, err := api.CreateVM(ctx, VMRequest{Image: "default/image", Network: "default/net", CPU: 2, Memory: "2Gi", BootDiskSize: "20Gi"}); err != nil || vm.ID != "vm1" {
		t.Fatalf("VM=%+v error=%v", vm, err)
	}
	if items, err := api.ListVMs(ctx); err != nil || len(items) != 1 {
		t.Fatalf("VMs=%+v error=%v", items, err)
	}
	if _, err := api.GetVM(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}
	if err := api.SetVMPower(ctx, "vm1", PowerOff); err != nil {
		t.Fatal(err)
	}
	if err := api.RebootVM(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}
	if volume, err := api.CreateVolume(ctx, VolumeRequest{Size: "1Gi"}); err != nil || volume.ID != "vol1" {
		t.Fatalf("volume=%+v error=%v", volume, err)
	}
	if items, err := api.ListVolumes(ctx); err != nil || len(items) != 1 {
		t.Fatalf("volumes=%+v error=%v", items, err)
	}
	if _, err := api.GetVolume(ctx, "vol1"); err != nil {
		t.Fatal(err)
	}
	if err := api.AttachVolume(ctx, "vm1", "vol1"); err != nil {
		t.Fatal(err)
	}
	if err := api.DetachVolume(ctx, "vm1", "vol1"); err != nil {
		t.Fatal(err)
	}
	if err := api.DeleteVolume(ctx, "vol1"); err != nil {
		t.Fatal(err)
	}
	if err := api.DeleteVM(ctx, "vm1"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 15 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestWaitForVMReady(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			fmt.Fprint(w, `{"id":"vm1","phase":"Starting","ready":false,"ipAddresses":[]}`)
			return
		}
		fmt.Fprint(w, testVMStatus)
	}))
	defer server.Close()
	api := newClient(t, configuration(t, server))
	status, err := api.WaitForVMReady(context.Background(), "vm1", time.Millisecond)
	if err != nil || !status.Ready || calls.Load() != 2 {
		t.Fatalf("status=%+v calls=%d error=%v", status, calls.Load(), err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	calls.Store(0)
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"id":"vm1","phase":"Starting","ready":false,"ipAddresses":[]}`)
	})
	_, err = api.WaitForVMReady(ctx, "vm1", time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "remains allocated") {
		t.Fatalf("timeout error=%v", err)
	}
}

func TestClientConcurrentUse(t *testing.T) {
	const workers = 3
	var entered atomic.Int32
	allEntered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if entered.Add(1) == workers {
			close(allEntered)
		}
		<-release
		fmt.Fprint(w, `{"maxActiveVMs":3,"activeVMs":0,"maxActiveVolumes":3,"activeVolumes":0}`)
	}))
	defer server.Close()
	api := newClient(t, configuration(t, server))
	var wait sync.WaitGroup
	errorsCh := make(chan error, workers)
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := api.Quota(context.Background())
			errorsCh <- err
		}()
	}
	select {
	case <-allEntered:
	case <-time.After(time.Second):
		t.Fatal("requests did not overlap")
	}
	close(release)
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestStructuredLoggingAndRedaction(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		fmt.Fprint(w, `{"code":"conflict","message":"secret body-secret"}`)
	}))
	defer server.Close()
	cfg := configuration(t, server)
	api, err := NewWithLogger(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	_, _ = api.CreateVM(context.Background(), VMRequest{Image: "body-secret"})
	logLine := output.String()
	for _, secret := range []string{"secret", "body-secret"} {
		if strings.Contains(logLine, secret) {
			t.Fatalf("log leaked %q: %s", secret, logLine)
		}
	}
	var entry map[string]any
	if json.Unmarshal(bytes.TrimSpace(output.Bytes()), &entry) != nil || entry["level"] != "WARN" || entry["status"] != float64(http.StatusConflict) {
		t.Fatalf("unexpected log: %s", logLine)
	}
}

func TestOIDCLoggingOmitsQueryAndCredentials(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	oidc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"value":"issued-secret"}`)
	}))
	defer oidc.Close()
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer issued-secret" {
			t.Error("incorrect issued token")
		}
		fmt.Fprint(w, `{"maxActiveVMs":3,"activeVMs":0,"maxActiveVolumes":3,"activeVolumes":0}`)
	}))
	defer gateway.Close()
	cfg := configuration(t, gateway)
	cfg.Token = ""
	cfg.GitHubActions = true
	cfg.OIDCRequestURL = oidc.URL + "?query-secret=value"
	cfg.OIDCRequestToken = "request-secret"
	api, err := NewWithLogger(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	api.oidc.Transport = oidc.Client().Transport
	if _, err := api.Quota(context.Background()); err != nil {
		t.Fatal(err)
	}
	logs := output.String()
	for _, secret := range []string{"query-secret", "request-secret", "issued-secret"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("logs leaked %q: %s", secret, logs)
		}
	}
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	if len(lines) != 2 {
		t.Fatalf("log lines=%d: %s", len(lines), logs)
	}
	var oidcEntry map[string]any
	if json.Unmarshal([]byte(lines[0]), &oidcEntry) != nil || oidcEntry["operation"] != "oidc request" {
		t.Fatalf("unexpected OIDC log: %s", lines[0])
	}
	if _, exists := oidcEntry["path"]; exists {
		t.Fatalf("OIDC log exposed path: %s", lines[0])
	}
}

func TestTypedInvalidResponse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	api := newClient(t, configuration(t, server))
	if _, err := api.ListVMs(context.Background()); err == nil {
		t.Fatal("accepted an object as a VM list")
	}
}

func TestLogLevelsAndFields(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		wantLevel string
	}{
		{name: "success", status: http.StatusNoContent, wantLevel: "INFO"},
		{name: "client error", status: http.StatusConflict, wantLevel: "WARN"},
		{name: "server error", status: http.StatusServiceUnavailable, wantLevel: "ERROR"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.status)
				if test.status != http.StatusNoContent {
					fmt.Fprint(w, `{"code":"failure","message":"safe"}`)
				}
			}))
			defer server.Close()
			var output bytes.Buffer
			api, err := NewWithLogger(configuration(t, server), slog.New(slog.NewJSONHandler(&output, nil)))
			if err != nil {
				t.Fatal(err)
			}
			defer api.Close()
			_ = api.Health(context.Background())
			var entry map[string]any
			if json.Unmarshal(bytes.TrimSpace(output.Bytes()), &entry) != nil {
				t.Fatalf("invalid log: %s", output.String())
			}
			if entry["level"] != test.wantLevel || entry["method"] != http.MethodGet ||
				entry["path"] != "/healthz" || entry["status"] != float64(test.status) {
				t.Fatalf("unexpected log fields: %v", entry)
			}
			for _, field := range []string{"duration_ms", "response_bytes", "operation"} {
				if _, exists := entry[field]; !exists {
					t.Errorf("missing %s: %v", field, entry)
				}
			}
		})
	}
}
