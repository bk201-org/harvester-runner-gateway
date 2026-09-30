package cluster

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	k3sInstallURL  = "https://get.k3s.io"
	k3sKubeconfig  = "/etc/rancher/k3s/k3s.yaml"
	k3sAPIPort     = "6443"
	maxRemoteError = 2000
)

var (
	k3sVersionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?\+k3s[0-9]+$`)
	nodeNamePattern   = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

	sshPollInterval           = 5 * time.Second
	k3sPollInterval           = 5 * time.Second
	logWriter       io.Writer = os.Stderr
)

type k3sOptions struct {
	Version        string
	SSHTimeout     time.Duration
	InstallTimeout time.Duration
}

// remoteRunner runs a bash script on a VM, as root when root is true, and
// returns its standard output. The script is passed on standard input so that
// secrets never appear in process arguments. When live is not nil, standard
// output and error are also copied to it line by line as they arrive; only
// pass it for scripts whose output is safe to log.
type remoteRunner interface {
	Run(ctx context.Context, sshConfig, host string, root bool, script string, live io.Writer) (string, error)
}

type sshRunner struct{}

func (sshRunner) Run(ctx context.Context, sshConfig, host string, root bool, script string, live io.Writer) (string, error) {
	remote := "bash -s"
	if root {
		remote = "sudo -n bash -s"
	}
	cmd := exec.CommandContext(ctx, "ssh", "-F", sshConfig, "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host, remote)
	cmd.Stdin = strings.NewReader(script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if live != nil {
		lines := &linePrefixWriter{out: live, prefix: "[" + host + "] "}
		defer lines.Flush()
		cmd.Stdout = io.MultiWriter(&stdout, lines)
		cmd.Stderr = io.MultiWriter(&stderr, lines)
	}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > maxRemoteError {
			msg = "..." + msg[len(msg)-maxRemoteError:]
		}
		if msg != "" {
			return stdout.String(), fmt.Errorf("%w: %s", err, msg)
		}
		return stdout.String(), err
	}
	return stdout.String(), nil
}

// linePrefixWriter writes complete lines to out with a prefix. It is safe for
// concurrent use because ssh stdout and stderr are copied by separate goroutines.
type linePrefixWriter struct {
	mu     sync.Mutex
	out    io.Writer
	prefix string
	buf    []byte
}

func (w *linePrefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		fmt.Fprintf(w.out, "%s%s\n", w.prefix, strings.TrimRight(string(w.buf[:i]), "\r"))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *linePrefixWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		fmt.Fprintf(w.out, "%s%s\n", w.prefix, strings.TrimRight(string(w.buf), "\r"))
		w.buf = nil
	}
}

func readK3sOptions(getenv func(string) string) (k3sOptions, error) {
	opt := k3sOptions{Version: getenv("INPUT_K3S-VERSION")}
	if opt.Version != "" && !k3sVersionPattern.MatchString(opt.Version) {
		return opt, fmt.Errorf("k3s-version must look like v1.35.2+k3s1")
	}
	for _, field := range []struct {
		name, fallback string
		target         *time.Duration
	}{
		{"INPUT_SSH-TIMEOUT-SECONDS", "300", &opt.SSHTimeout},
		{"INPUT_K3S-TIMEOUT-SECONDS", "900", &opt.InstallTimeout},
	} {
		raw := getenv(field.name)
		if raw == "" {
			raw = field.fallback
		}
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 1 || seconds > 86400 {
			return opt, fmt.Errorf("%s must be between 1 and 86400",
				strings.ToLower(strings.TrimPrefix(field.name, "INPUT_")))
		}
		*field.target = time.Duration(seconds) * time.Second
	}
	return opt, nil
}

// createK3s creates the VMs, then installs k3s over SSH: the first VM becomes
// the server and every other VM joins it as an agent.
func createK3s(ctx context.Context, api vmAPI, opt options, k3s k3sOptions, runner remoteRunner) error {
	cluster, err := createVMs(ctx, api, opt)
	if err != nil {
		return err
	}
	kubeconfig, err := setupK3s(ctx, runner, cluster, k3s)
	if err != nil {
		return err
	}
	for _, output := range [][2]string{{"kubeconfig-path", kubeconfig}, {"server-vm-id", cluster.Statuses[0].ID}} {
		if err := appendGitHubFile(opt.OutputFile, output[0], output[1]); err != nil {
			return err
		}
	}
	return nil
}

func setupK3s(ctx context.Context, runner remoteRunner, cluster vmCluster, opt k3sOptions) (string, error) {
	if len(cluster.Statuses) == 0 {
		return "", errors.New("no VMs to configure")
	}
	for _, status := range cluster.Statuses {
		if !nodeNamePattern.MatchString(status.ID) {
			return "", fmt.Errorf("VM ID %q cannot be used as a Kubernetes node name", status.ID)
		}
	}
	serverIP := usableIP(cluster.Statuses[0].IPAddresses)
	if serverIP == "" {
		return "", fmt.Errorf("VM %s has no usable IP address", cluster.Statuses[0].ID)
	}

	sshCtx, cancel := context.WithTimeout(ctx, opt.SSHTimeout)
	defer cancel()
	for _, status := range cluster.Statuses {
		logf("waiting for SSH on %s", status.ID)
		if err := waitSSH(sshCtx, runner, cluster.ConfigPath, status.ID); err != nil {
			return "", err
		}
	}

	token, err := newToken()
	if err != nil {
		return "", err
	}
	installCtx, cancelInstall := context.WithTimeout(ctx, opt.InstallTimeout)
	defer cancelInstall()
	serverURL := "https://" + net.JoinHostPort(serverIP, k3sAPIPort)
	for i, status := range cluster.Statuses {
		role, url := "agent", serverURL
		if i == 0 {
			role, url = "server", ""
		}
		logf("installing k3s %s on %s (version %s, server %s)", role, status.ID, versionLabel(opt.Version), serverURL)
		started := time.Now()
		_, err := runner.Run(installCtx, cluster.ConfigPath, status.ID, true,
			installScript(role, opt.Version, token, status.ID, url), logWriter)
		if err != nil {
			logf("k3s %s install on %s failed after %s", role, status.ID, time.Since(started).Round(time.Second))
			diagnose(runner, cluster.ConfigPath, status.ID, role)
			return "", fmt.Errorf("install k3s %s on %s: %w", role, status.ID, err)
		}
		logf("k3s %s installed on %s in %s", role, status.ID, time.Since(started).Round(time.Second))
	}
	logf("waiting for %d node(s) to become Ready", len(cluster.Statuses))

	if err := waitNodesReady(installCtx, runner, cluster); err != nil {
		return "", err
	}
	logf("reading kubeconfig from %s:%s", cluster.Statuses[0].ID, k3sKubeconfig)
	out, err := runner.Run(installCtx, cluster.ConfigPath, cluster.Statuses[0].ID, true, "cat "+shellQuote(k3sKubeconfig)+"\n", nil)
	if err != nil {
		return "", fmt.Errorf("read kubeconfig from %s: %w", cluster.Statuses[0].ID, err)
	}
	const local = "https://127.0.0.1:" + k3sAPIPort
	if !strings.Contains(out, local) {
		return "", fmt.Errorf("kubeconfig from %s does not reference %s", cluster.Statuses[0].ID, local)
	}
	out = strings.ReplaceAll(out, local, serverURL)
	path := filepath.Join(cluster.Dir, "kubeconfig")
	if err := os.WriteFile(path, []byte(out), 0600); err != nil {
		return "", fmt.Errorf("save kubeconfig: %w", err)
	}
	return path, nil
}

func waitSSH(ctx context.Context, runner remoteRunner, sshConfig, id string) error {
	var last error
	started := time.Now()
	for attempt := 1; ; attempt++ {
		if _, last = runner.Run(ctx, sshConfig, id, false, "true\n", nil); last == nil {
			logf("SSH to %s is ready after %d attempt(s), %s", id, attempt, time.Since(started).Round(time.Second))
			break
		}
		logf("SSH to %s not ready (attempt %d): %v", id, attempt, last)
		select {
		case <-ctx.Done():
			return fmt.Errorf("SSH to %s did not become ready: %w (last error: %v)", id, ctx.Err(), last)
		case <-time.After(sshPollInterval):
		}
	}
	logf("waiting for cloud-init on %s", id)
	const settle = "if command -v cloud-init >/dev/null 2>&1; then cloud-init status --wait --long 2>&1 || echo \"cloud-init status exited with $?\"; else echo 'cloud-init is not installed'; fi\n"
	if _, err := runner.Run(ctx, sshConfig, id, true, settle, logWriter); err != nil {
		return fmt.Errorf("prepare %s (passwordless sudo is required): %w", id, err)
	}
	return nil
}

func waitNodesReady(ctx context.Context, runner remoteRunner, cluster vmCluster) error {
	server := cluster.Statuses[0].ID
	var last error
	var missing []string
	var lastTable string
	for {
		out, err := runner.Run(ctx, cluster.ConfigPath, server, true, "k3s kubectl get nodes --no-headers\n", nil)
		last = err
		if err != nil {
			logf("k3s is not answering on %s yet: %v", server, err)
		} else {
			if table := strings.TrimSpace(out); table != lastTable {
				lastTable = table
				logf("nodes:\n%s", table)
			}
			ready := readyNodes(out)
			missing = missing[:0]
			for _, status := range cluster.Statuses {
				if !ready[status.ID] {
					missing = append(missing, status.ID)
				}
			}
			if len(missing) == 0 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			for i, status := range cluster.Statuses {
				role := "agent"
				if i == 0 {
					role = "server"
				}
				diagnose(runner, cluster.ConfigPath, status.ID, role)
			}
			return fmt.Errorf("k3s nodes did not become Ready (waiting for %s): %w (last error: %v)",
				strings.Join(missing, ", "), ctx.Err(), last)
		case <-time.After(k3sPollInterval):
		}
	}
}

func readyNodes(output string) map[string]bool {
	ready := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && (fields[1] == "Ready" || strings.HasPrefix(fields[1], "Ready,")) {
			ready[fields[0]] = true
		}
	}
	return ready
}

// diagnose prints the recent service log of a node. It is best effort and uses
// its own deadline because the caller's context may already have expired.
func diagnose(runner remoteRunner, sshConfig, id, role string) {
	unit := "k3s"
	if role == "agent" {
		unit = "k3s-agent"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := runner.Run(ctx, sshConfig, id, true, "journalctl -u "+unit+" -n 50 --no-pager\n", nil)
	if err != nil {
		logf("could not read %s logs from %s: %v", unit, id, err)
		return
	}
	logf("last %s log lines from %s:\n%s", unit, id, strings.TrimSpace(out))
}

func installScript(role, version, token, nodeName, serverURL string) string {
	var script strings.Builder
	script.WriteString("set -euo pipefail\n")
	fmt.Fprintf(&script, "export K3S_TOKEN=%s\nexport K3S_NODE_NAME=%s\n", shellQuote(token), shellQuote(nodeName))
	if version != "" {
		fmt.Fprintf(&script, "export INSTALL_K3S_VERSION=%s\n", shellQuote(version))
	}
	if serverURL != "" {
		fmt.Fprintf(&script, "export K3S_URL=%s\n", shellQuote(serverURL))
	}
	// Redirect stdin so no command consumes the rest of this script. Nothing
	// below prints the token, so the output is safe to stream to the job log.
	script.WriteString(`echo "== host =="
hostname || true
date -u || true
uname -r || true
(. /etc/os-release && echo "$PRETTY_NAME") || true
echo "== network =="
ip -br addr 2>&1 || true
ip route 2>&1 || true
grep -v '^#' /etc/resolv.conf 2>&1 || true
env | grep -i '^\(http\|https\|no\)_proxy=' || echo "no proxy environment variables"
getent hosts get.k3s.io || echo "DNS lookup for get.k3s.io failed"
echo "== download =="
installer=$(mktemp)
headers=$(mktemp)
trap 'rm -f "$installer" "$headers"' EXIT
downloaded=false
for attempt in 1 2 3 4 5; do
  echo "downloading ` + k3sInstallURL + ` (attempt $attempt/5)"
  code=$(curl -sS -L --connect-timeout 15 --max-time 120 -D "$headers" -o "$installer" -w '%{http_code}' ` + shellQuote(k3sInstallURL) + ` </dev/null) || code=000
  if [ "$code" = 200 ]; then
    downloaded=true
    break
  fi
  echo "attempt $attempt failed: HTTP status $code"
  echo "-- response headers --"
  cat "$headers" || true
  echo "-- response body (first 500 bytes) --"
  head -c 500 "$installer" || true
  echo
  if [ "$attempt" -lt 5 ]; then
    echo "retrying in $((attempt * 5))s"
    sleep $((attempt * 5))
  fi
done
if [ "$downloaded" != true ]; then
  echo "could not download the k3s installer" >&2
  exit 1
fi
echo "downloaded installer: $(wc -c < "$installer") bytes"
echo "== install k3s ` + role + ` =="
`)
	fmt.Fprintf(&script, "sh \"$installer\" %s </dev/null\n", role)
	return script.String()
}

func versionLabel(version string) string {
	if version == "" {
		return "stable channel"
	}
	return version
}

func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate k3s token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func logf(format string, args ...any) {
	fmt.Fprintf(logWriter, format+"\n", args...)
}
