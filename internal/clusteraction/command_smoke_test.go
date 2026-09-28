package clusteraction

import (
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCommandSmokeWithLocalToken(t *testing.T) {
	var creates, reads, deletes atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer smoke-token" {
			http.Error(w, "wrong local token", http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/vms":
			creates.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"vm-1","ready":false,"ipAddresses":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/vms/vm-1":
			reads.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"vm-1","ready":true,"ipAddresses":["10.0.0.1"]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/vms/vm-1":
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	temp := t.TempDir()
	tokenPath := filepath.Join(temp, "token")
	caPath := filepath.Join(temp, "ca.pem")
	stateOutput := filepath.Join(temp, "github_state")
	resultOutput := filepath.Join(temp, "github_output")
	for path, data := range map[string][]byte{
		tokenPath:    []byte("smoke-token\n"),
		caPath:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
		stateOutput:  nil,
		resultOutput: nil,
	} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	env := map[string]string{
		"GITHUB_ACTIONS": "false", "GATEWAY_TOKEN_FILE": tokenPath,
		"RUNNER_TEMP": temp, "GITHUB_STATE": stateOutput, "GITHUB_OUTPUT": resultOutput,
		"INPUT_GATEWAY-URL": server.URL, "INPUT_CA-CERT-PATH": caPath,
		"INPUT_VM-COUNT": "1", "INPUT_IMAGE": "default/ubuntu", "INPUT_NETWORK": "default/net",
		"INPUT_CPU": "2", "INPUT_MEMORY": "4Gi", "INPUT_BOOT-DISK-SIZE": "20Gi", "INPUT_USERNAME": "ci",
	}
	getenv := func(name string) string { return env[name] }
	if err := Run(context.Background(), "create", getenv); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(stateOutput)
	if err != nil || !strings.HasPrefix(string(data), "cluster_state=") {
		t.Fatalf("command state = %q, %v", data, err)
	}
	statePath := strings.TrimSpace(strings.TrimPrefix(string(data), "cluster_state="))
	env["STATE_cluster_state"] = statePath
	if err := Run(context.Background(), "cleanup", getenv); err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 1 || reads.Load() != 1 || deletes.Load() != 1 {
		t.Fatalf("gateway calls: create=%d get=%d delete=%d", creates.Load(), reads.Load(), deletes.Load())
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("cluster state remains after cleanup: %v", err)
	}
}
