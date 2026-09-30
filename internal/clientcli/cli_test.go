package clientcli

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
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

func assertRequestLogs(t *testing.T, output string, want int) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != want {
		t.Fatalf("log lines=%d, want %d: %q", len(lines), want, output)
	}
	for _, line := range lines {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil || entry["msg"] != "client request completed" {
			t.Fatalf("invalid request log: %q", line)
		}
	}
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
	}{
		{[]string{"health"}, "GET", "/healthz", 204, nil},
		{[]string{"ready"}, "GET", "/readyz", 204, nil},
		{[]string{"quota"}, "GET", "/v1/quota", 200, nil},
		{[]string{"vm", "create", "--image", "default/ubuntu", "--network", "default/net", "--cpu", "2", "--memory", "4Gi", "--boot-disk-size", "20Gi", "--ssh-public-key-file", keyFile, "--ssh-public-key-file", keyFile, "--user-data-file", userFile, "--ttl-seconds", "600"}, "POST", "/v1/vms", 201, vmBody},
		{[]string{"vm", "list"}, "GET", "/v1/vms", 200, nil},
		{[]string{"vm", "get", "vm1"}, "GET", "/v1/vms/vm1", 200, nil},
		{[]string{"vm", "delete", "vm1"}, "DELETE", "/v1/vms/vm1", 204, nil},
		{[]string{"vm", "power", "vm1", "off"}, "PUT", "/v1/vms/vm1/power", 202, map[string]any{"state": "off"}},
		{[]string{"vm", "reboot", "vm1"}, "POST", "/v1/vms/vm1/reboot", 202, nil},
		{[]string{"vm", "attach", "vm1", "vol1"}, "PUT", "/v1/vms/vm1/volumes/vol1", 202, nil},
		{[]string{"vm", "detach", "vm1", "vol1"}, "DELETE", "/v1/vms/vm1/volumes/vol1", 202, nil},
		{[]string{"volume", "create", "--size", "10Gi"}, "POST", "/v1/volumes", 201, map[string]any{"size": "10Gi"}},
		{[]string{"volume", "list"}, "GET", "/v1/volumes", 200, nil},
		{[]string{"volume", "get", "vol1"}, "GET", "/v1/volumes/vol1", 200, nil},
		{[]string{"volume", "delete", "vol1"}, "DELETE", "/v1/volumes/vol1", 204, nil},
		// The second accepted power state uses the same API operation.
		{[]string{"vm", "power", "vm1", "on"}, "PUT", "/v1/vms/vm1/power", 202, map[string]any{"state": "on"}},
		{[]string{"volume", "create", "--size", "10Gi", "--ttl-seconds", "1"}, "POST", "/v1/volumes", 201, map[string]any{"size": "10Gi", "ttlSeconds": float64(1)}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args[:min(2, len(tc.args))], " ")+fmt.Sprint(tc.status), func(t *testing.T) {
			vmResponse := `{"id":"resource","phase":"Running","powerState":"on","ready":true,"ipAddresses":["10.0.0.10"],"attachedVolumeIDs":[],"expiresAt":"2026-09-27T00:00:00Z"}`
			volumeResponse := `{"id":"resource","phase":"Bound","size":"10Gi","expiresAt":"2026-09-27T00:00:00Z"}`
			response := vmResponse
			switch {
			case tc.args[0] == "quota":
				response = `{"maxActiveVMs":3,"activeVMs":0,"maxActiveVolumes":4,"activeVolumes":0}`
			case tc.args[0] == "vm" && tc.args[1] == "list":
				response = "[" + vmResponse + "]"
			case tc.args[0] == "volume" && tc.args[1] == "list":
				response = "[" + volumeResponse + "]"
			case tc.args[0] == "volume":
				response = volumeResponse
			}
			if tc.args[0] == "vm" && tc.args[1] == "get" {
				response = strings.Replace(response, `"id":"resource"`, `"id":"vm1"`, 1)
			}
			if tc.args[0] == "volume" && tc.args[1] == "get" {
				response = strings.Replace(response, `"id":"resource"`, `"id":"vol1"`, 1)
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
			if code != 0 || calls != 1 {
				t.Fatalf("code=%d stderr=%q calls=%d", code, err, calls)
			}
			assertRequestLogs(t, err, 1)
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
	base := []string{"vm", "create", "--image", "default/ubuntu", "--network", "default/net", "--cpu", "2", "--memory", "4Gi", "--boot-disk-size", "20Gi"}
	cases := [][]string{
		{}, {"unknown"}, {"vm"}, {"vm", "bad"}, {"volume", "reboot", "x"}, {"quota", "extra"}, {"vm", "get"}, {"vm", "get", "../x"}, {"vm", "get", "a/b"}, {"vm", "get", ".."},
		{"vm", "list", "--unknown"}, {"vm", "get", "x", "--unknown"}, {"vm", "power", "x", "bad"}, {"vm", "attach", "x"},
		{"volume", "create", "--size", "0"},
		{"vm", "create"}, {"--timeout", "bad", "health"}, {"--timeout", "0s", "health"},
	}
	for _, extra := range [][]string{{"--ttl-seconds", "0"}, {"--ttl-seconds", "86401"}, {"--cpu", "0"}, {"--cpu", "invalid"}, {"--memory", "NaN"}, {"--image", "ubuntu"}, {"--user-data-file", badUser}, {"--ssh-public-key-file", badKey}, {"--ssh-public-key-file", "/nonexistent"}, {"--user-data-file", "/nonexistent"}} {
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
		if code != 0 || out != "" || !strings.Contains(err, "Usage: hvst-runner-gw-client") {
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

func TestVMCreateWaitOptions(t *testing.T) {
	base := []string{"vm", "create", "--image", "default/ubuntu", "--network", "default/net", "--cpu", "2", "--memory", "4Gi", "--boot-disk-size", "20Gi"}
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

func TestVMCreateNoWaitAndReady(t *testing.T) {
	base := []string{"vm", "create", "--image", "default/ubuntu", "--network", "default/net", "--cpu", "2", "--memory", "4Gi", "--boot-disk-size", "20Gi"}
	tests := []struct {
		name     string
		args     []string
		status   int
		response string
	}{
		{name: "no wait", args: append(append([]string{}, base...), "--no-wait"), status: http.StatusCreated,
			response: `{"id":"ci-vm-00000001","phase":"Provisioning","powerState":"on","ready":false,"ipAddresses":[],"attachedVolumeIDs":[],"expiresAt":"2026-09-27T00:00:00Z"}`},
		{name: "ready", args: base, status: http.StatusCreated,
			response: `{"id":"ci-vm-00000001","phase":"Running","powerState":"on","ready":true,"ipAddresses":["10.0.0.10"],"attachedVolumeIDs":[],"expiresAt":"2026-09-27T00:00:00Z"}`},
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
			if code != 0 || out != test.response+"\n" || calls != 1 {
				t.Fatalf("code=%d stdout=%q stderr=%q calls=%d", code, out, stderr, calls)
			}
			assertRequestLogs(t, stderr, 1)
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

func TestLocalAndActionCommands(t *testing.T) {
	for _, mode := range []string{"cluster", "action"} {
		t.Run(mode, func(t *testing.T) {
			exists := false
			creates := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer developer-test-token" {
					t.Error("incorrect auth")
					w.WriteHeader(401)
					return
				}
				switch r.Method {
				case "POST":
					exists = true
					creates++
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["cpu"] != float64(2) || body["image"] != "default/ubuntu" {
						t.Errorf("bad provisioning body: %v", body)
					}
					w.WriteHeader(201)
					fmt.Fprint(w, `{"id":"vm-1","ready":false}`)
				case "GET":
					if !exists {
						w.WriteHeader(404)
						fmt.Fprint(w, `{"code":"not_found"}`)
						return
					}
					fmt.Fprint(w, `{"id":"vm-1","ready":true,"ipAddresses":["10.0.0.1"]}`)
				case "DELETE":
					exists = false
					w.WriteHeader(204)
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			state := filepath.Join(dir, "cluster")
			token := writeTestFile(t, "token", []byte("developer-test-token"))
			env := map[string]string{"GATEWAY_URL": server.URL, "GATEWAY_TOKEN_FILE": token, "GATEWAY_CA_CERT": testCA(t, server)}
			create := []string{"cluster", "create", "--state-dir", state, "--config", writeTestFile(t, "cluster.yaml", []byte("vm-count: 1\nimage: default/ubuntu\nnetwork: default/net\ncpu: 2\nmemory: 4Gi\nboot-disk-size: 20Gi\nusername: ci\n"))}
			cleanup := []string{"cluster", "delete", "--state-dir", state}
			if mode == "action" {
				create = []string{"action", "create"}
				cleanup = []string{"action", "cleanup"}
				for key, value := range map[string]string{"INPUT_GATEWAY-URL": server.URL, "INPUT_CA-CERT-PATH": env["GATEWAY_CA_CERT"], "INPUT_VM-COUNT": "1", "INPUT_IMAGE": "default/ubuntu", "INPUT_NETWORK": "default/net", "INPUT_CPU": "2", "INPUT_MEMORY": "4Gi", "INPUT_BOOT-DISK-SIZE": "20Gi", "INPUT_USERNAME": "ci", "RUNNER_TEMP": dir, "GITHUB_STATE": writeTestFile(t, "state", nil), "GITHUB_OUTPUT": writeTestFile(t, "output", nil)} {
					env[key] = value
				}
			} else {
				// A local command must use its explicit credential even in an Actions environment.
				env["GITHUB_ACTIONS"] = "true"
			}
			if code, out, err := invoke(create, env); code != 0 {
				t.Fatalf("create exit=%d out=%s err=%s", code, out, err)
			}
			if mode == "action" {
				data, err := os.ReadFile(env["GITHUB_STATE"])
				if err != nil {
					t.Fatal(err)
				}
				env["STATE_cluster_state"] = strings.TrimSpace(strings.TrimPrefix(string(data), "cluster_state="))
			} else {
				if code, out, err := invoke([]string{"cluster", "status", "--state-dir", state}, env); code != 0 || !strings.Contains(out, "sshCommands") {
					t.Fatalf("status exit=%d out=%s err=%s", code, out, err)
				}
			}
			if code, out, err := invoke(cleanup, env); code != 0 {
				t.Fatalf("cleanup exit=%d out=%s err=%s", code, out, err)
			}
			if exists || creates != 1 {
				t.Fatalf("exists=%v creates=%d", exists, creates)
			}
		})
	}
}

func TestClusterCLIRejectsInvalidInputBeforeProvisioning(t *testing.T) {
	base := "vm-count: 1\nimage: default/ubuntu\nnetwork: default/net\ncpu: 2\nmemory: 4Gi\nboot-disk-size: 20Gi\nusername: ci\n"
	for _, body := range []string{base + "typo: true\n", strings.Replace(base, "memory: 4Gi", "memory: -1Gi", 1), base + "ttl-seconds: 86401\n", strings.Replace(base, "vm-count: 1", "vm-count: 0", 1), base + "user-data: invalid\n"} {
		dir := filepath.Join(t.TempDir(), "cluster")
		path := writeTestFile(t, "config.yaml", []byte(body))
		code, _, _ := invoke([]string{"cluster", "create", "--config", path, "--state-dir", dir}, map[string]string{"GATEWAY_URL": "https://unused.example", "GATEWAY_TOKEN": "test"})
		if code != 2 {
			t.Fatalf("invalid input exit=%d", code)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal("invalid configuration created state")
		}
	}
}
