package cluster

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bk201-org/harvester-runner-gateway/client"
)

func TestReadRKE2Options(t *testing.T) {
	get := func(env map[string]string) func(string) string { return func(k string) string { return env[k] } }
	opt, err := readInstallOptions(get(map[string]string{}), &rke2Dist)
	if err != nil || opt.Dist != &rke2Dist || opt.Version != "" || opt.SSHTimeout != 300*time.Second || opt.InstallTimeout != 1200*time.Second {
		t.Fatalf("defaults = %#v, %v", opt, err)
	}
	opt, err = readInstallOptions(get(map[string]string{"INPUT_RKE2-VERSION": "v1.35.2+rke2r1", "INPUT_RKE2-TIMEOUT-SECONDS": "1800"}), &rke2Dist)
	if err != nil || opt.Version != "v1.35.2+rke2r1" || opt.InstallTimeout != 30*time.Minute {
		t.Fatalf("custom = %#v, %v", opt, err)
	}
	for _, env := range []map[string]string{
		{"INPUT_RKE2-VERSION": "v1.35.2+k3s1"},
		{"INPUT_RKE2-VERSION": "latest"},
		{"INPUT_RKE2-VERSION": "v1.35.2+rke2r1; rm -rf /"},
		{"INPUT_RKE2-TIMEOUT-SECONDS": "0"},
	} {
		if _, err := readInstallOptions(get(env), &rke2Dist); err == nil {
			t.Fatalf("accepted %v", env)
		}
	}
	if _, err := readInstallOptions(get(map[string]string{"INPUT_RKE2-VERSION": "bad"}), &rke2Dist); err == nil || !strings.Contains(err.Error(), "rke2-version must look like v1.35.2+rke2r1") {
		t.Fatalf("version error = %v", err)
	}
	// k3s inputs do not configure RKE2.
	opt, err = readInstallOptions(get(map[string]string{"INPUT_K3S-VERSION": "v1.35.2+k3s1"}), &rke2Dist)
	if err != nil || opt.Version != "" {
		t.Fatalf("k3s input leaked into RKE2 options: %#v, %v", opt, err)
	}
}

func TestSetupRKE2InstallsServerThenAgents(t *testing.T) {
	fastPolling(t)
	cluster := testCluster(t)
	runner := &fakeRunner{
		nodesOutput: []string{
			"ci-vm-00000001   Ready   control-plane,etcd,master   1m   v1.35.2+rke2r1\n",
			"ci-vm-00000001   Ready   control-plane,etcd,master   1m   v1.35.2+rke2r1\nci-vm-00000002   Ready   <none>   5s   v1.35.2+rke2r1\nci-vm-00000003   Ready   <none>   5s   v1.35.2+rke2r1\n",
		},
		kubeconfig: "server: https://127.0.0.1:6443\n",
	}
	path, err := setupKube(context.Background(), runner, cluster, installOptions{Dist: &rke2Dist,
		Version: "v1.35.2+rke2r1", SSHTimeout: time.Second, InstallTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	installs := runner.installs()
	if len(installs) != 3 || installs[0].host != "ci-vm-00000001" || installs[1].host != "ci-vm-00000002" || installs[2].host != "ci-vm-00000003" {
		t.Fatalf("install calls = %#v", installs)
	}
	token := regexp.MustCompile(`token: '([0-9a-f]{64})'`).FindStringSubmatch(installs[0].script)
	if token == nil {
		t.Fatalf("server script lacks a generated token: %s", installs[0].script)
	}
	for i, call := range installs {
		role := "agent"
		if i == 0 {
			role = "server"
		}
		want := []string{"https://get.rke2.io", "token: '" + token[1] + "'", "node-name: '" + call.host + "'",
			"INSTALL_RKE2_TYPE='" + role + "' INSTALL_RKE2_VERSION='v1.35.2+rke2r1' sh \"$installer\" </dev/null",
			"unit='rke2-" + role + ".service'", `systemctl enable --now --no-block "$unit" </dev/null`}
		for _, w := range want {
			if !call.root || !strings.Contains(call.script, w) {
				t.Fatalf("script %d lacks %q: %s", i, w, call.script)
			}
		}
		hasServer := strings.Contains(call.script, "server: 'https://10.0.0.1:9345'")
		if (i == 0) == hasServer {
			t.Fatalf("script %d join URL mismatch: %s", i, call.script)
		}
		if strings.Contains(call.script, "K3S_") {
			t.Fatalf("script %d has k3s settings: %s", i, call.script)
		}
	}
	var nodes, kubeconfig bool
	for _, call := range runner.calls {
		nodes = nodes || call.script == "/var/lib/rancher/rke2/bin/kubectl --kubeconfig /etc/rancher/rke2/rke2.yaml get nodes --no-headers\n"
		kubeconfig = kubeconfig || call.script == "cat '/etc/rancher/rke2/rke2.yaml'\n"
	}
	if !nodes || !kubeconfig {
		t.Fatalf("RKE2 nodes command used = %v, kubeconfig read = %v", nodes, kubeconfig)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "server: https://10.0.0.1:6443\n" {
		t.Fatalf("kubeconfig = %q, %v", data, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("kubeconfig mode = %v, %v", info, err)
	}
}

func TestSetupRKE2ReportsInstallFailure(t *testing.T) {
	fastPolling(t)
	runner := &fakeRunner{installError: map[string]error{"ci-vm-00000002": errors.New("exit status 1")}}
	_, err := setupKube(context.Background(), runner, testCluster(t), installOptions{Dist: &rke2Dist, SSHTimeout: time.Second, InstallTimeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "install rke2 agent on ci-vm-00000002") {
		t.Fatalf("error = %v", err)
	}
	var logged bool
	for _, call := range runner.calls {
		logged = logged || call.script == "journalctl -u rke2-agent -n 50 --no-pager\n"
	}
	if !logged {
		t.Fatal("failed agent logs were not requested")
	}
}

func TestCreateRKE2WritesOutputs(t *testing.T) {
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
		Request: client.VMRequest{Image: "default/ubuntu", Network: "default/net", CPU: 2, Memory: "8Gi", BootDiskSize: "40Gi"}}
	if err := createKube(context.Background(), api, opt, installOptions{Dist: &rke2Dist, SSHTimeout: time.Second, InstallTimeout: time.Second}, runner); err != nil {
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

func TestRKE2InstallScriptKeepsTokenPrivate(t *testing.T) {
	script := mustInstallScript(t, &rke2Dist, "agent", "", "s3cret-token", "vm-1", "https://10.0.0.1:9345")
	for _, want := range []string{"getent hosts get.rke2.io", "== install rke2 agent ==", "umask 077", "<<'RKE2_CONFIG'"} {
		if !strings.Contains(script, want) {
			t.Fatalf("install script lacks %q: %s", want, script)
		}
	}
	if strings.Count(script, "s3cret-token") != 1 || strings.Contains(script, "set -x") || strings.Contains(script, "INSTALL_RKE2_VERSION") {
		t.Fatalf("unexpected install script: %s", script)
	}
}

func TestRKE2InstallScriptReportsDownloadFailure(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available")
	}
	dir := t.TempDir()
	tools := map[string]string{
		"curl": "#!/bin/sh\nwhile [ $# -gt 0 ]; do case \"$1\" in -D) h=$2; shift;; -o) o=$2; shift;; esac; shift; done\n" +
			"printf 'HTTP/2 503\\r\\n' > \"$h\"; echo 'unavailable' > \"$o\"; printf 503\n",
		"sleep":     "#!/bin/sh\nexit 0\n",
		"systemctl": "#!/bin/sh\necho systemctl must not run >&2\nexit 1\n",
	}
	for name, body := range tools {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(bash, "-s")
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cmd.Stdin = strings.NewReader(mustInstallScript(t, &rke2Dist, "server", "", "tok", "vm-1", ""))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("script succeeded despite HTTP 503: %s", out)
	}
	for _, want := range []string{"downloading https://get.rke2.io", "attempt 5 failed: HTTP status 503", "could not download the rke2 installer"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "systemctl must not run") || strings.Contains(string(out), "tok\n") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

func TestYAMLQuote(t *testing.T) {
	if got := yamlQuote("a'b"); got != "'a''b'" {
		t.Fatalf("yamlQuote = %s", got)
	}
}

func TestStartServiceScriptWaitsAndReportsProgress(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available")
	}
	run := func(t *testing.T, states string) (string, error) {
		t.Helper()
		dir := t.TempDir()
		// is-active prints one state per call from the states file.
		tools := map[string]string{
			"systemctl": "#!/bin/sh\necho \"$*\" >> \"$TOOL_DIR/calls\"\n[ \"$1\" = is-active ] || exit 0\n" +
				"state=$(head -n 1 \"$TOOL_DIR/states\"); sed -i 1d \"$TOOL_DIR/states\"\necho \"$state\"\n[ \"$state\" = active ]\n",
			"journalctl": "#!/bin/sh\necho 'level=info msg=\"Waiting to retrieve agent configuration\"'\n",
			"sleep":      "#!/bin/sh\nexit 0\n",
		}
		for name, body := range tools {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "states"), []byte(states), 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bash, "-s")
		cmd.Env = append(os.Environ(), "TOOL_DIR="+dir, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		cmd.Stdin = strings.NewReader("set -euo pipefail\n" + renderScript(t, rke2Dist.scripts, "start-service", "rke2-agent.service"))
		out, err := cmd.CombinedOutput()
		calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
		if !strings.HasPrefix(string(calls), "enable --now --no-block rke2-agent.service\n") {
			t.Fatalf("systemctl calls = %q", calls)
		}
		return string(out), err
	}

	t.Run("active", func(t *testing.T) {
		out, err := run(t, strings.Repeat("activating\n", 7)+"active\n")
		if err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
		for _, want := range []string{"starting rke2-agent.service", "waiting for rke2-agent.service: activating after 30s; last log line:",
			"Waiting to retrieve agent configuration", "rke2-agent.service is active after 35s"} {
			if !strings.Contains(out, want) {
				t.Fatalf("output lacks %q:\n%s", want, out)
			}
		}
		if strings.Count(out, "waiting for") != 1 {
			t.Fatalf("progress is reported more than every 30s:\n%s", out)
		}
	})
	t.Run("failed", func(t *testing.T) {
		out, err := run(t, "inactive\nactivating\nfailed\nactive\n")
		if err == nil || !strings.Contains(out, "rke2-agent.service failed after 10s") {
			t.Fatalf("failed unit was not reported: %v\n%s", err, out)
		}
	})
}

// hangingRunner blocks installs until the context ends, like ssh waiting on a
// node that never joins.
type hangingRunner struct{ *fakeRunner }

func (h hangingRunner) Run(ctx context.Context, cfg, host string, root bool, script string, live io.Writer) (string, error) {
	if isInstall(script) {
		h.calls = append(h.calls, remoteCall{host, root, script})
		<-ctx.Done()
		return "", errors.New("signal: killed")
	}
	return h.fakeRunner.Run(ctx, cfg, host, root, script, live)
}

func TestSetupRKE2ReportsInstallTimeout(t *testing.T) {
	fastPolling(t)
	runner := hangingRunner{&fakeRunner{}}
	_, err := setupKube(context.Background(), runner, testCluster(t), installOptions{Dist: &rke2Dist, SSHTimeout: time.Second, InstallTimeout: 30 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "install rke2 server on ci-vm-00000001: rke2-timeout-seconds (30ms) expired: signal: killed") {
		t.Fatalf("error = %v", err)
	}
	var logged bool
	for _, call := range runner.calls {
		logged = logged || call.script == "journalctl -u rke2-server -n 50 --no-pager\n"
	}
	if !logged {
		t.Fatal("server logs were not requested after the timeout")
	}
}
