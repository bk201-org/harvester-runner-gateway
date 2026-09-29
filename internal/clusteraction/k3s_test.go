package clusteraction

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bk201-org/harvester-runner-gateway/client"
)

type remoteCall struct {
	host   string
	root   bool
	script string
}

type fakeRunner struct {
	calls        []remoteCall
	sshFailures  map[string]int
	installError map[string]error
	nodesOutput  []string
	kubeconfig   string
}

func (f *fakeRunner) Run(_ context.Context, _, host string, root bool, script string) (string, error) {
	f.calls = append(f.calls, remoteCall{host, root, script})
	switch {
	case script == "true\n":
		if f.sshFailures[host] > 0 {
			f.sshFailures[host]--
			return "", errors.New("connection refused")
		}
	case strings.Contains(script, "get.k3s.io"):
		return "[INFO] installed", f.installError[host]
	case strings.HasPrefix(script, "k3s kubectl get nodes"):
		out := f.nodesOutput[0]
		if len(f.nodesOutput) > 1 {
			f.nodesOutput = f.nodesOutput[1:]
		}
		return out, nil
	case strings.HasPrefix(script, "cat "):
		return f.kubeconfig, nil
	}
	return "", nil
}

func (f *fakeRunner) installs() []remoteCall {
	var out []remoteCall
	for _, call := range f.calls {
		if strings.Contains(call.script, "get.k3s.io") {
			out = append(out, call)
		}
	}
	return out
}

func testCluster(t *testing.T) vmCluster {
	t.Helper()
	return vmCluster{Dir: t.TempDir(), ConfigPath: "ssh_config", Statuses: []client.VMStatus{
		{ID: "ci-vm-00000001", IPAddresses: []string{"10.0.0.1"}},
		{ID: "ci-vm-00000002", IPAddresses: []string{"10.0.0.2"}},
		{ID: "ci-vm-00000003", IPAddresses: []string{"10.0.0.3"}},
	}}
}

func fastPolling(t *testing.T) {
	t.Helper()
	sshPollInterval, k3sPollInterval, logWriter = time.Millisecond, time.Millisecond, io.Discard
	t.Cleanup(func() {
		sshPollInterval, k3sPollInterval, logWriter = 5*time.Second, 5*time.Second, os.Stderr
	})
}

func TestReadK3sOptions(t *testing.T) {
	get := func(env map[string]string) func(string) string { return func(k string) string { return env[k] } }
	opt, err := readK3sOptions(get(map[string]string{}))
	if err != nil || opt.Version != "" || opt.SSHTimeout != 300*time.Second || opt.InstallTimeout != 900*time.Second {
		t.Fatalf("defaults = %#v, %v", opt, err)
	}
	opt, err = readK3sOptions(get(map[string]string{"INPUT_K3S-VERSION": "v1.35.2+k3s1", "INPUT_SSH-TIMEOUT-SECONDS": "60"}))
	if err != nil || opt.Version != "v1.35.2+k3s1" || opt.SSHTimeout != time.Minute {
		t.Fatalf("custom = %#v, %v", opt, err)
	}
	for _, env := range []map[string]string{
		{"INPUT_K3S-VERSION": "latest"},
		{"INPUT_K3S-VERSION": "v1.35.2+k3s1; rm -rf /"},
		{"INPUT_SSH-TIMEOUT-SECONDS": "0"},
		{"INPUT_K3S-TIMEOUT-SECONDS": "abc"},
		{"INPUT_K3S-TIMEOUT-SECONDS": "86401"},
	} {
		if _, err := readK3sOptions(get(env)); err == nil {
			t.Fatalf("accepted %v", env)
		}
	}
}

func TestSetupK3sInstallsServerThenAgents(t *testing.T) {
	fastPolling(t)
	cluster := testCluster(t)
	runner := &fakeRunner{
		sshFailures: map[string]int{"ci-vm-00000002": 2},
		nodesOutput: []string{
			"ci-vm-00000001   Ready   control-plane,master   1m   v1.35.2+k3s1\n",
			"ci-vm-00000001   Ready   control-plane,master   1m   v1.35.2+k3s1\nci-vm-00000002   Ready   <none>   5s   v1.35.2+k3s1\nci-vm-00000003   NotReady   <none>   5s   v1.35.2+k3s1\n",
			"ci-vm-00000001   Ready   control-plane,master   1m   v1.35.2+k3s1\nci-vm-00000002   Ready   <none>   5s   v1.35.2+k3s1\nci-vm-00000003   Ready   <none>   5s   v1.35.2+k3s1\n",
		},
		kubeconfig: "server: https://127.0.0.1:6443\n",
	}
	path, err := setupK3s(context.Background(), runner, cluster, k3sOptions{
		Version: "v1.35.2+k3s1", SSHTimeout: time.Second, InstallTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	installs := runner.installs()
	if len(installs) != 3 || !installs[0].root {
		t.Fatalf("install calls = %#v", installs)
	}
	if installs[0].host != "ci-vm-00000001" || installs[1].host != "ci-vm-00000002" || installs[2].host != "ci-vm-00000003" {
		t.Fatalf("install order = %#v", installs)
	}
	token := regexp.MustCompile(`K3S_TOKEN='([0-9a-f]{64})'`).FindStringSubmatch(installs[0].script)
	if token == nil {
		t.Fatalf("server script lacks a generated token: %s", installs[0].script)
	}
	for i, call := range installs {
		want := []string{"K3S_TOKEN='" + token[1] + "'", "K3S_NODE_NAME='" + call.host + "'", "INSTALL_K3S_VERSION='v1.35.2+k3s1'", "</dev/null"}
		for _, w := range want {
			if !strings.Contains(call.script, w) {
				t.Fatalf("script %d lacks %q: %s", i, w, call.script)
			}
		}
		hasURL := strings.Contains(call.script, "K3S_URL='https://10.0.0.1:6443'")
		if (i == 0) == hasURL {
			t.Fatalf("script %d K3S_URL mismatch: %s", i, call.script)
		}
		role := "agent"
		if i == 0 {
			role = "server"
		}
		if !strings.Contains(call.script, "\"$installer\" "+role+" </dev/null") {
			t.Fatalf("script %d has the wrong role: %s", i, call.script)
		}
	}
	for _, call := range runner.calls {
		if strings.Contains(call.script, "get.k3s.io") {
			break
		}
		if call.script == "true\n" && call.root {
			t.Fatal("SSH probe must not require sudo")
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "server: https://10.0.0.1:6443\n" || path != filepath.Join(cluster.Dir, "kubeconfig") {
		t.Fatalf("kubeconfig = %q at %s, %v", data, path, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("kubeconfig mode = %v, %v", info, err)
	}
}

func TestSetupK3sReportsInstallFailure(t *testing.T) {
	fastPolling(t)
	runner := &fakeRunner{installError: map[string]error{"ci-vm-00000002": errors.New("exit status 1")}}
	_, err := setupK3s(context.Background(), runner, testCluster(t), k3sOptions{SSHTimeout: time.Second, InstallTimeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "agent on ci-vm-00000002") {
		t.Fatalf("error = %v", err)
	}
	if len(runner.installs()) != 2 {
		t.Fatalf("installs after failure = %d", len(runner.installs()))
	}
	var logged bool
	for _, call := range runner.calls {
		logged = logged || strings.Contains(call.script, "journalctl -u k3s-agent")
	}
	if !logged {
		t.Fatal("failed agent logs were not requested")
	}
}

func TestSetupK3sTimeouts(t *testing.T) {
	fastPolling(t)
	cluster := testCluster(t)
	runner := &fakeRunner{sshFailures: map[string]int{"ci-vm-00000001": 1 << 30}}
	_, err := setupK3s(context.Background(), runner, cluster, k3sOptions{SSHTimeout: 20 * time.Millisecond, InstallTimeout: time.Second})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "SSH to ci-vm-00000001") {
		t.Fatalf("SSH timeout error = %v", err)
	}
	if len(runner.installs()) != 0 {
		t.Fatal("installed before SSH was ready")
	}
	runner = &fakeRunner{nodesOutput: []string{"ci-vm-00000001 Ready <none> 1m v1\n"}}
	_, err = setupK3s(context.Background(), runner, cluster, k3sOptions{SSHTimeout: time.Second, InstallTimeout: 30 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "ci-vm-00000002, ci-vm-00000003") {
		t.Fatalf("Ready timeout error = %v", err)
	}
}

func TestSetupK3sRejectsUnsafeInput(t *testing.T) {
	fastPolling(t)
	cluster := testCluster(t)
	cluster.Statuses[1].ID = "Bad_Name"
	if _, err := setupK3s(context.Background(), &fakeRunner{}, cluster, k3sOptions{SSHTimeout: time.Second, InstallTimeout: time.Second}); err == nil {
		t.Fatal("accepted an invalid node name")
	}
	cluster = testCluster(t)
	runner := &fakeRunner{nodesOutput: []string{"ci-vm-00000001 Ready\nci-vm-00000002 Ready\nci-vm-00000003 Ready\n"}, kubeconfig: "server: https://example:6443\n"}
	if _, err := setupK3s(context.Background(), runner, cluster, k3sOptions{SSHTimeout: time.Second, InstallTimeout: time.Second}); err == nil {
		t.Fatal("accepted a kubeconfig without the loopback server")
	}
	if _, err := os.Stat(filepath.Join(cluster.Dir, "kubeconfig")); !os.IsNotExist(err) {
		t.Fatalf("kubeconfig written despite failure: %v", err)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Fatalf("shellQuote = %s", got)
	}
}

func TestSSHRunnerUsesStdinAndSudo(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$FAKE_SSH_ARGS\"\ncat > \"$FAKE_SSH_STDIN\"\necho done\n"
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_SSH_ARGS", filepath.Join(dir, "args"))
	t.Setenv("FAKE_SSH_STDIN", filepath.Join(dir, "stdin"))
	out, err := sshRunner{}.Run(context.Background(), "cfg", "vm-1", true, "echo secret\n")
	if err != nil || strings.TrimSpace(out) != "done" {
		t.Fatalf("output = %q, %v", out, err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if !strings.Contains(string(args), "-F cfg") || !strings.Contains(string(args), "vm-1 sudo -n bash -s") || strings.Contains(string(args), "secret") {
		t.Fatalf("ssh args = %s", args)
	}
	if string(stdin) != "echo secret\n" {
		t.Fatalf("ssh stdin = %q", stdin)
	}
}

func TestCreateK3sWritesOutputs(t *testing.T) {
	fastPolling(t)
	temp := t.TempDir()
	stateOutput, resultOutput := filepath.Join(temp, "github_state"), filepath.Join(temp, "github_output")
	for _, path := range []string{stateOutput, resultOutput} {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	api := &fakeVMAPI{statuses: map[string][]client.VMStatus{
		"vm-1": {{ID: "vm-1", Ready: true, IPAddresses: []string{"10.0.0.1"}}},
		"vm-2": {{ID: "vm-2", Ready: true, IPAddresses: []string{"10.0.0.2"}}},
	}}
	runner := &fakeRunner{nodesOutput: []string{"vm-1 Ready\nvm-2 Ready\n"}, kubeconfig: "server: https://127.0.0.1:6443\n"}
	opt := options{TempDir: temp, StateFile: stateOutput, OutputFile: resultOutput,
		Username: "ci", Count: 2, WaitTimeout: time.Second,
		Request: client.VMRequest{Image: "default/ubuntu", Network: "default/net", CPU: 2, Memory: "4Gi", BootDiskSize: "20Gi"}}
	if err := createK3s(context.Background(), api, opt, k3sOptions{SSHTimeout: time.Second, InstallTimeout: time.Second}, runner); err != nil {
		t.Fatal(err)
	}
	outputs, err := os.ReadFile(resultOutput)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"vm-ids=[\"vm-1\",\"vm-2\"]", "kubeconfig-path=" + temp, "/kubeconfig\n", "server-vm-id=vm-1\n"} {
		if !strings.Contains(string(outputs), want) {
			t.Fatalf("outputs lack %q: %s", want, outputs)
		}
	}
	if len(runner.installs()) != 2 {
		t.Fatalf("installs = %d", len(runner.installs()))
	}
}
