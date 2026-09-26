package clientcli

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bk201-org/harvester-runner-gateway/internal/client"
)

func writeTestFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func testCA(t *testing.T, s *httptest.Server) string {
	t.Helper()
	return writeTestFile(t, "ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}))
}
func invoke(args []string, env map[string]string) (int, string, string) {
	var out, err bytes.Buffer
	code := Run(context.Background(), args, func(key string) string { return env[key] }, &out, &err)
	return code, out.String(), err.String()
}

func TestEveryOperation(t *testing.T) {
	keyFile := writeTestFile(t, "key.pub", []byte("ssh-ed25519 YWJj test\n"))
	userData := "#cloud-config\npackages: [curl]\n"
	userFile := writeTestFile(t, "cloud.yaml", []byte(userData))
	vmBody := map[string]any{"image": "default/ubuntu", "network": "default/net", "cpu": float64(2), "memory": "4Gi", "bootDiskSize": "20Gi", "sshPublicKeys": []any{"ssh-ed25519 YWJj test", "ssh-ed25519 YWJj test"}, "userData": userData, "ttlSeconds": float64(600)}
	cases := []struct {
		args         []string
		method, path string
		status       int
		body         any
		key          string
	}{
		{[]string{"health"}, "GET", "/healthz", 204, nil, ""},
		{[]string{"ready"}, "GET", "/readyz", 204, nil, ""},
		{[]string{"quota"}, "GET", "/v1/quota", 200, nil, ""},
		{[]string{"vm", "create", "--image", "default/ubuntu", "--network", "default/net", "--cpu", "2", "--memory", "4Gi", "--boot-disk-size", "20Gi", "--idempotency-key", "vm-key", "--ssh-public-key-file", keyFile, "--ssh-public-key-file", keyFile, "--user-data-file", userFile, "--ttl-seconds", "600"}, "POST", "/v1/vms", 201, vmBody, "vm-key"},
		{[]string{"vm", "list"}, "GET", "/v1/vms", 200, nil, ""},
		{[]string{"vm", "get", "vm1"}, "GET", "/v1/vms/vm1", 200, nil, ""},
		{[]string{"vm", "delete", "vm1"}, "DELETE", "/v1/vms/vm1", 204, nil, ""},
		{[]string{"vm", "power", "vm1", "off"}, "PUT", "/v1/vms/vm1/power", 202, map[string]any{"state": "off"}, ""},
		{[]string{"vm", "reboot", "vm1"}, "POST", "/v1/vms/vm1/reboot", 202, nil, ""},
		{[]string{"vm", "attach", "vm1", "vol1"}, "PUT", "/v1/vms/vm1/volumes/vol1", 202, nil, ""},
		{[]string{"vm", "detach", "vm1", "vol1"}, "DELETE", "/v1/vms/vm1/volumes/vol1", 202, nil, ""},
		{[]string{"volume", "create", "--size", "10Gi", "--idempotency-key", "vol-key"}, "POST", "/v1/volumes", 201, map[string]any{"size": "10Gi"}, "vol-key"},
		{[]string{"volume", "list"}, "GET", "/v1/volumes", 200, nil, ""},
		{[]string{"volume", "get", "vol1"}, "GET", "/v1/volumes/vol1", 200, nil, ""},
		{[]string{"volume", "delete", "vol1"}, "DELETE", "/v1/volumes/vol1", 204, nil, ""},
		// The second accepted power state uses the same API operation.
		{[]string{"vm", "power", "vm1", "on"}, "PUT", "/v1/vms/vm1/power", 202, map[string]any{"state": "on"}, ""},
		{[]string{"volume", "create", "--size", "10Gi", "--idempotency-key", "vol-key", "--ttl-seconds", "1"}, "POST", "/v1/volumes", 200, map[string]any{"size": "10Gi", "ttlSeconds": float64(1)}, "vol-key"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args[:min(2, len(tc.args))], " ")+fmt.Sprint(tc.status), func(t *testing.T) {
			response := `{"id":"resource"}`
			if tc.args[0] == "vm" && tc.args[1] == "create" {
				response = `{"id":"resource","phase":"Running","ready":true,"ipAddresses":["10.0.0.10"]}`
			}
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != tc.method || r.URL.Path != "/gateway"+tc.path {
					t.Errorf("got %s %s", r.Method, r.URL.Path)
				}
				expectedAuth := "Bearer secret"
				if tc.args[0] == "health" || tc.args[0] == "ready" {
					expectedAuth = ""
				}
				if r.Header.Get("Authorization") != expectedAuth {
					t.Error("incorrect authentication")
				}
				if r.Header.Get("Idempotency-Key") != tc.key {
					t.Error("incorrect idempotency key")
				}
				data, _ := io.ReadAll(r.Body)
				var body any
				if len(data) > 0 {
					if err := json.Unmarshal(data, &body); err != nil {
						t.Error(err)
					}
				}
				if !reflect.DeepEqual(body, tc.body) {
					t.Errorf("body = %#v, want %#v", body, tc.body)
				}
				if tc.body != nil && r.Header.Get("Content-Type") != "application/json" {
					t.Error("missing content type")
				}
				w.WriteHeader(tc.status)
				if tc.status == 200 || tc.status == 201 {
					fmt.Fprint(w, response)
				}
			}))
			defer server.Close()
			env := map[string]string{"GATEWAY_URL": server.URL + "/gateway/", "GATEWAY_CA_CERT": testCA(t, server), "GATEWAY_TOKEN": "secret"}
			code, out, err := invoke(tc.args, env)
			if code != 0 || err != "" || calls != 1 {
				t.Fatalf("code=%d stderr=%q calls=%d", code, err, calls)
			}
			expectedOut := ""
			if tc.status == 200 || tc.status == 201 {
				expectedOut = response + "\n"
			}
			if out != expectedOut {
				t.Errorf("stdout=%q", out)
			}
		})
	}
}

func TestInvalidInputsNeverReachNetwork(t *testing.T) {
	badKey := writeTestFile(t, "bad.pub", []byte("not a key"))
	badUser := writeTestFile(t, "bad.yaml", []byte("password: hidden"))
	base := []string{"vm", "create", "--image", "default/ubuntu", "--network", "default/net", "--cpu", "2", "--memory", "4Gi", "--boot-disk-size", "20Gi", "--idempotency-key", "key"}
	cases := [][]string{
		{}, {"unknown"}, {"vm"}, {"vm", "bad"}, {"volume", "reboot", "x"}, {"quota", "extra"}, {"vm", "get"}, {"vm", "get", "../x"}, {"vm", "get", "a/b"}, {"vm", "get", ".."},
		{"vm", "list", "--unknown"}, {"vm", "get", "x", "--unknown"}, {"vm", "power", "x", "bad"}, {"vm", "attach", "x"},
		{"volume", "create", "--size", "0", "--idempotency-key", "key"}, {"volume", "create", "--size", "1Gi"}, {"volume", "create", "--size", "1Gi", "--idempotency-key", "bad key"},
		{"vm", "create", "--idempotency-key", "key"}, {"--timeout", "bad", "health"}, {"--timeout", "0s", "health"},
	}
	for _, extra := range [][]string{{"--ttl-seconds", "0"}, {"--ttl-seconds", "86401"}, {"--cpu", "0"}, {"--cpu", "invalid"}, {"--memory", "NaN"}, {"--image", "ubuntu"}, {"--user-data-file", badUser}, {"--ssh-public-key-file", badKey}, {"--ssh-public-key-file", "/nonexistent"}, {"--user-data-file", "/nonexistent"}, {"--idempotency-key", strings.Repeat("x", 129)}} {
		cases = append(cases, append(append([]string{}, base...), extra...))
	}
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }))
	defer server.Close()
	env := map[string]string{"GATEWAY_URL": server.URL, "GATEWAY_CA_CERT": testCA(t, server), "GATEWAY_TOKEN": "secret"}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, out, err := invoke(args, env)
			if code != 2 || out != "" || err == "" {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, err)
			}
		})
	}
	if calls != 0 {
		t.Fatalf("invalid inputs made %d requests", calls)
	}
}

func TestHelpDoesNotRequireConfiguration(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"help"}, {"help", "vm", "create"}, {"vm", "--help"}, {"vm", "create", "--help"}, {"vm", "get", "x", "--help"}, {"ready", "--help"}, {"volume", "create", "--help"}} {
		code, out, err := invoke(args, map[string]string{"GATEWAY_TIMEOUT": "invalid"})
		if code != 0 || out != "" || !strings.Contains(err, "Usage:") {
			t.Fatalf("%v: %d %q %q", args, code, out, err)
		}
	}
}

func TestFlagPrecedenceAndExitCodes(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer from-file" {
			t.Error("token file did not override environment")
		}
		w.WriteHeader(409)
		fmt.Fprint(w, `{"code":"quota_exceeded","message":"active VM quota reached"}`)
	}))
	defer server.Close()
	token := writeTestFile(t, "token", []byte("from-file\n"))
	args := []string{"--url", server.URL, "--ca-cert", testCA(t, server), "--token-file", token, "--timeout", "1s", "quota"}
	code, out, err := invoke(args, map[string]string{"GATEWAY_URL": "http://invalid", "GATEWAY_CA_CERT": "/missing", "GATEWAY_TOKEN_FILE": "/missing", "GATEWAY_TOKEN": "env-secret", "GATEWAY_TIMEOUT": "invalid"})
	if code != 1 || out != "" || !strings.Contains(err, "HTTP 409: quota_exceeded") {
		t.Fatalf("%d %q %q", code, out, err)
	}
	for _, env := range []map[string]string{
		{"GATEWAY_URL": "http://invalid"},
		{"GATEWAY_URL": server.URL, "GATEWAY_CA_CERT": "/missing"},
		{"GATEWAY_URL": server.URL, "GATEWAY_CA_CERT": testCA(t, server), "GATEWAY_TOKEN_FILE": "/missing", "GATEWAY_TOKEN": "fallback"},
	} {
		code, _, _ := invoke([]string{"quota"}, env)
		if code != 2 {
			t.Errorf("expected configuration exit code, got %d", code)
		}
	}
	code, _, _ = invoke([]string{"quota"}, map[string]string{"GATEWAY_URL": server.URL})
	if code != 1 {
		t.Errorf("missing authentication: exit %d", code)
	}
}

func newReadinessTestClient(t *testing.T, server *httptest.Server) *client.Client {
	t.Helper()
	api, err := client.New(client.Config{
		URL: server.URL, CACert: testCA(t, server), Token: "secret", Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(api.Close)
	return api
}

func TestWaitForVMReadyPollsUntilReady(t *testing.T) {
	calls := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/vms/hrgw-example" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		if calls == 1 {
			fmt.Fprint(w, `{"id":"hrgw-example","phase":"Running","ready":false,"ipAddresses":[]}`)
			return
		}
		fmt.Fprint(w, `{"id":"hrgw-example","phase":"Running","ready":true,"ipAddresses":["10.0.0.10"]}`)
	}))
	defer server.Close()

	initial := []byte(`{"id":"hrgw-example","phase":"Provisioning","ready":false,"ipAddresses":[]}`)
	data, err := waitForVMReady(context.Background(), newReadinessTestClient(t, server), initial, time.Second, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	var status client.VMStatus
	if json.Unmarshal(data, &status) != nil || !status.Ready || len(status.IPAddresses) != 1 || calls != 2 {
		t.Fatalf("status=%+v calls=%d", status, calls)
	}
}

func TestWaitForVMReadyFailuresKeepVM(t *testing.T) {
	initial := []byte(`{"id":"hrgw-example","phase":"Provisioning","ready":false,"ipAddresses":[]}`)
	t.Run("timeout", func(t *testing.T) {
		var getCalls, deleteCalls int
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				getCalls++
				fmt.Fprint(w, `{"id":"hrgw-example","phase":"Starting","ready":false,"ipAddresses":[]}`)
			case http.MethodDelete:
				deleteCalls++
				w.WriteHeader(http.StatusNoContent)
			}
		}))
		defer server.Close()
		_, err := waitForVMReady(context.Background(), newReadinessTestClient(t, server), initial, 10*time.Millisecond, time.Millisecond)
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "hrgw-example") ||
			!strings.Contains(err.Error(), "last phase=Starting") || !strings.Contains(err.Error(), "remains allocated") {
			t.Fatalf("unexpected timeout error: %v", err)
		}
		if getCalls == 0 || deleteCalls != 0 {
			t.Fatalf("GET calls=%d DELETE calls=%d", getCalls, deleteCalls)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Error("canceled wait made a request")
		}))
		defer server.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := waitForVMReady(ctx, newReadinessTestClient(t, server), initial, time.Minute, time.Hour)
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "hrgw-example") || !strings.Contains(err.Error(), "remains allocated") {
			t.Fatalf("unexpected cancellation error: %v", err)
		}
	})

	t.Run("polling error", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer server.Close()
		_, err := waitForVMReady(context.Background(), newReadinessTestClient(t, server), initial, time.Second, time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "hrgw-example") || !strings.Contains(err.Error(), "last phase=Provisioning") {
			t.Fatalf("unexpected polling error: %v", err)
		}
	})
}

func TestVMCreateWaitOptions(t *testing.T) {
	base := []string{"vm", "create", "--image", "default/ubuntu", "--network", "default/net", "--cpu", "2", "--memory", "4Gi", "--boot-disk-size", "20Gi", "--idempotency-key", "key"}
	tests := []struct {
		name        string
		args        []string
		defaultWait string
		wantWait    bool
		wantTimeout time.Duration
		wantError   bool
	}{
		{name: "environment default", args: base, defaultWait: "7m", wantWait: true, wantTimeout: 7 * time.Minute},
		{name: "flag overrides environment", args: append(append([]string{}, base...), "--wait-timeout", "9m"), defaultWait: "7m", wantWait: true, wantTimeout: 9 * time.Minute},
		{name: "no wait", args: append(append([]string{}, base...), "--no-wait"), defaultWait: "7m", wantTimeout: 7 * time.Minute},
		{name: "invalid environment", args: base, defaultWait: "invalid", wantError: true},
		{name: "invalid flag", args: append(append([]string{}, base...), "--wait-timeout", "0s"), defaultWait: "7m", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			op, err := parseCommand(test.args, io.Discard, test.defaultWait)
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v", err)
			}
			if err == nil && (op.waitForVM != test.wantWait || op.waitTimeout != test.wantTimeout) {
				t.Fatalf("wait=%v timeout=%s", op.waitForVM, op.waitTimeout)
			}
		})
	}
}

func TestVMCreateNoWaitAndIdempotentReady(t *testing.T) {
	base := []string{"vm", "create", "--image", "default/ubuntu", "--network", "default/net", "--cpu", "2", "--memory", "4Gi", "--boot-disk-size", "20Gi", "--idempotency-key", "key"}
	tests := []struct {
		name     string
		args     []string
		status   int
		response string
	}{
		{name: "no wait", args: append(append([]string{}, base...), "--no-wait"), status: http.StatusCreated,
			response: `{"id":"hrgw-example","phase":"Provisioning","ready":false,"ipAddresses":[]}`},
		{name: "idempotent ready", args: base, status: http.StatusOK,
			response: `{"id":"hrgw-example","phase":"Running","ready":true,"ipAddresses":["10.0.0.10"]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost {
					t.Errorf("unexpected polling request: %s", r.Method)
				}
				w.WriteHeader(test.status)
				fmt.Fprint(w, test.response)
			}))
			defer server.Close()
			env := map[string]string{"GATEWAY_URL": server.URL, "GATEWAY_CA_CERT": testCA(t, server), "GATEWAY_TOKEN": "secret"}
			code, out, stderr := invoke(test.args, env)
			if code != 0 || stderr != "" || out != test.response+"\n" || calls != 1 {
				t.Fatalf("code=%d stdout=%q stderr=%q calls=%d", code, out, stderr, calls)
			}
		})
	}
}

func TestQuantityValidation(t *testing.T) {
	for _, value := range []string{"1", "0.5Gi", "1e3", "1E-3", "100m", ".5", "1.", "+2Gi"} {
		if !positiveQuantity(value) {
			t.Errorf("rejected %q", value)
		}
	}
	for _, value := range []string{"", "0", "0Gi", "-1", "-1Gi", "1GB", "NaN", "1K", "1e", "1 Gi"} {
		if positiveQuantity(value) {
			t.Errorf("accepted %q", value)
		}
	}
}
