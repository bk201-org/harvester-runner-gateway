package clusteraction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bk201/harvester-runner-gateway/client"
)

const pollInterval = 10 * time.Second

type vmAPI interface {
	CreateVM(context.Context, client.VMRequest) (client.VMStatus, error)
	GetVM(context.Context, string) (client.VMStatus, error)
	DeleteVM(context.Context, string) error
}

type options struct {
	URL, Audience, CACert, Username, UserData, TempDir, StateFile, OutputFile string
	Count                                                                     int
	WaitTimeout                                                               time.Duration
	Request                                                                   client.VMRequest
}

type clusterState struct {
	Dir string   `json:"dir"`
	IDs []string `json:"ids"`
}

func Run(ctx context.Context, operation string, getenv func(string) string) error {
	if operation != "create" && operation != "cleanup" {
		return fmt.Errorf("unknown operation %q", operation)
	}
	if operation == "create" {
		opt, err := readOptions(getenv)
		if err != nil {
			return err
		}
		api, err := newClient(opt, getenv)
		if err != nil {
			return err
		}
		defer api.Close()
		return create(ctx, api, opt)
	}
	statePath := getenv("STATE_cluster_state")
	if statePath == "" {
		return nil
	}
	state, err := readState(statePath, getenv("RUNNER_TEMP"))
	if err != nil {
		return err
	}
	opt := options{URL: getenv("INPUT_GATEWAY-URL"), Audience: getenv("INPUT_AUDIENCE"), CACert: getenv("INPUT_CA-CERT-PATH")}
	api, err := newClient(opt, getenv)
	if err != nil {
		_ = os.RemoveAll(state.Dir)
		return err
	}
	defer api.Close()
	return cleanup(ctx, api, state)
}

func newClient(opt options, getenv func(string) string) (*client.Client, error) {
	actions := getenv("GITHUB_ACTIONS") == "true"
	cfg := client.Config{URL: opt.URL, Audience: opt.Audience, CACert: opt.CACert,
		Timeout: 30 * time.Second, GitHubActions: actions,
		OIDCRequestURL:   getenv("ACTIONS_ID_TOKEN_REQUEST_URL"),
		OIDCRequestToken: getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN")}
	if !actions {
		cfg.TokenFile = getenv("GATEWAY_TOKEN_FILE")
		cfg.Token = getenv("GATEWAY_TOKEN")
	}
	return client.New(cfg)
}

func readOptions(getenv func(string) string) (options, error) {
	opt := options{URL: getenv("INPUT_GATEWAY-URL"), Audience: getenv("INPUT_AUDIENCE"),
		CACert: getenv("INPUT_CA-CERT-PATH"), Username: getenv("INPUT_USERNAME"), UserData: getenv("INPUT_USER-DATA"),
		TempDir: getenv("RUNNER_TEMP"), StateFile: getenv("GITHUB_STATE"), OutputFile: getenv("GITHUB_OUTPUT")}
	if opt.URL == "" || opt.TempDir == "" || opt.StateFile == "" || opt.OutputFile == "" {
		return opt, fmt.Errorf("gateway URL and GitHub runner state, output, and temp paths are required")
	}
	if !guestUser.MatchString(opt.Username) || opt.Username == "default" {
		return opt, fmt.Errorf("username must be a valid guest login name")
	}
	var err error
	opt.Count, err = strconv.Atoi(getenv("INPUT_VM-COUNT"))
	if err != nil || opt.Count < 1 {
		return opt, fmt.Errorf("vm-count must be a positive integer")
	}
	opt.Request = client.VMRequest{Image: getenv("INPUT_IMAGE"), Network: getenv("INPUT_NETWORK"),
		Memory: getenv("INPUT_MEMORY"), BootDiskSize: getenv("INPUT_BOOT-DISK-SIZE")}
	opt.Request.CPU, err = strconv.Atoi(getenv("INPUT_CPU"))
	if err != nil || opt.Request.CPU < 1 || opt.Request.Image == "" || opt.Request.Network == "" || opt.Request.Memory == "" || opt.Request.BootDiskSize == "" {
		return opt, fmt.Errorf("image, network, positive CPU, memory, and boot-disk-size are required")
	}
	if raw := getenv("INPUT_TTL-SECONDS"); raw != "" {
		ttl, err := strconv.Atoi(raw)
		if err != nil || ttl < 1 || ttl > 86400 {
			return opt, fmt.Errorf("ttl-seconds must be between 1 and 86400")
		}
		opt.Request.TTLSeconds = &ttl
	}
	waitSeconds := getenv("INPUT_WAIT-TIMEOUT-SECONDS")
	if waitSeconds == "" {
		waitSeconds = "600"
	}
	wait, err := strconv.Atoi(waitSeconds)
	if err != nil || wait < 1 || wait > 86400 {
		return opt, fmt.Errorf("wait-timeout-seconds must be between 1 and 86400")
	}
	opt.WaitTimeout = time.Duration(wait) * time.Second
	return opt, nil
}

func create(ctx context.Context, api vmAPI, opt options) error {
	dir, err := os.MkdirTemp(opt.TempDir, "hvst-cluster-")
	if err != nil {
		return fmt.Errorf("create cluster directory: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	state := clusterState{Dir: dir, IDs: []string{}}
	statePath := filepath.Join(dir, "cluster.json")
	if err := writeState(statePath, state); err != nil {
		return err
	}
	if err := appendGitHubFile(opt.StateFile, "cluster_state", statePath); err != nil {
		return err
	}
	privateKey := filepath.Join(dir, "id_ed25519")
	keygen := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", privateKey)
	if output, err := keygen.CombinedOutput(); err != nil {
		return fmt.Errorf("generate SSH keypair: %w: %s", err, strings.TrimSpace(string(output)))
	}
	publicKey, err := os.ReadFile(privateKey + ".pub")
	if err != nil {
		return fmt.Errorf("read generated SSH public key: %w", err)
	}
	opt.Request.UserData, err = cloudConfig(opt.UserData, opt.Username, strings.TrimSpace(string(publicKey)))
	if err != nil {
		return err
	}
	if err := provision(ctx, api, opt.Request, opt.Count, &state, statePath); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, opt.WaitTimeout)
	defer cancel()
	statuses, err := waitAll(waitCtx, api, state.IDs, pollInterval)
	if err != nil {
		return err
	}
	configPath := filepath.Join(dir, "ssh_config")
	if err := writeSSHConfig(configPath, filepath.Join(dir, "known_hosts"), privateKey, opt.Username, statuses); err != nil {
		return err
	}
	ids, err := json.Marshal(state.IDs)
	if err != nil {
		return err
	}
	for _, output := range [][2]string{{"vm-ids", string(ids)}, {"ssh-config-path", configPath}, {"private-key-path", privateKey}} {
		if err := appendGitHubFile(opt.OutputFile, output[0], output[1]); err != nil {
			return err
		}
	}
	return nil
}

func provision(ctx context.Context, api vmAPI, request client.VMRequest, count int, state *clusterState, statePath string) error {
	for len(state.IDs) < count {
		status, err := api.CreateVM(ctx, request)
		if err != nil {
			return fmt.Errorf("create VM %d of %d: %w", len(state.IDs)+1, count, err)
		}
		if status.ID == "" {
			return fmt.Errorf("gateway returned a VM without an ID")
		}
		state.IDs = append(state.IDs, status.ID)
		if err := writeState(statePath, *state); err != nil {
			return fmt.Errorf("record created VM %s: %w", status.ID, err)
		}
	}
	return nil
}

func waitAll(ctx context.Context, api vmAPI, ids []string, interval time.Duration) ([]client.VMStatus, error) {
	statuses := make([]client.VMStatus, len(ids))
	ready := make([]bool, len(ids))
	for {
		remaining := 0
		for i, id := range ids {
			if ready[i] {
				continue
			}
			status, err := api.GetVM(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("get VM %s: %w", id, err)
			}
			statuses[i] = status
			ready[i] = status.Ready && usableIP(status.IPAddresses) != ""
			if !ready[i] {
				remaining++
			}
		}
		if remaining == 0 {
			return statuses, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%d VMs did not become ready: %w", remaining, ctx.Err())
		case <-time.After(interval):
		}
	}
}

func usableIP(addresses []string) string {
	for _, raw := range addresses {
		if ip, err := netip.ParseAddr(raw); err == nil && ip.IsGlobalUnicast() && !ip.IsLinkLocalUnicast() {
			return ip.String()
		}
	}
	return ""
}

func writeSSHConfig(path, knownHosts, privateKey, username string, statuses []client.VMStatus) error {
	var content strings.Builder
	for _, status := range statuses {
		ip := usableIP(status.IPAddresses)
		if ip == "" || status.ID == "" || strings.ContainsAny(status.ID, " \t\r\n") {
			return fmt.Errorf("VM %s has no usable SSH host", status.ID)
		}
		fmt.Fprintf(&content, "Host %s\n  HostName %s\n  User %s\n  IdentityFile %s\n  IdentitiesOnly yes\n  UserKnownHostsFile %s\n  StrictHostKeyChecking accept-new\n\n",
			status.ID, ip, username, sshQuote(privateKey), sshQuote(knownHosts))
	}
	return os.WriteFile(path, []byte(content.String()), 0600)
}

func sshQuote(path string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(path) + `"`
}

func writeState(path string, state clusterState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readState(path, tempDir string) (clusterState, error) {
	var state clusterState
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	if tempDir == "" || filepath.Dir(state.Dir) != filepath.Clean(tempDir) || !strings.HasPrefix(filepath.Base(state.Dir), "hvst-cluster-") || path != filepath.Join(state.Dir, "cluster.json") {
		return state, fmt.Errorf("invalid cluster state path")
	}
	return state, nil
}

func cleanup(ctx context.Context, api vmAPI, state clusterState) error {
	var failures []error
	for i := len(state.IDs) - 1; i >= 0; i-- {
		id := state.IDs[i]
		if err := api.DeleteVM(ctx, id); err != nil && !strings.HasPrefix(err.Error(), "gateway HTTP 404:") {
			failures = append(failures, fmt.Errorf("delete VM %s: %w", id, err))
		}
	}
	if err := os.RemoveAll(state.Dir); err != nil {
		failures = append(failures, fmt.Errorf("remove local SSH files: %w", err))
	}
	return errors.Join(failures...)
}

func appendGitHubFile(path, key, value string) error {
	if strings.ContainsAny(key, "=\r\n") || strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("invalid GitHub state or output value")
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = fmt.Fprintf(file, "%s=%s\n", key, value)
	return err
}
