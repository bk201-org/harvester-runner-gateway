package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/bk201-org/harvester-runner-gateway/client"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// LocalConfig uses the action's provisioning input names, without credentials
// or runner-specific paths. It is saved privately for manual reproduction.
type LocalConfig struct {
	Count              int    `json:"vm-count"`
	Image              string `json:"image"`
	Network            string `json:"network"`
	CPU                int    `json:"cpu"`
	Memory             string `json:"memory"`
	BootDiskSize       string `json:"boot-disk-size"`
	Username           string `json:"username"`
	UserData           string `json:"user-data,omitempty"`
	TTLSeconds         *int   `json:"ttl-seconds,omitempty"`
	WaitTimeoutSeconds int    `json:"wait-timeout-seconds,omitempty"`
}

func (c LocalConfig) options() (options, error) {
	opt := options{Count: c.Count, Username: c.Username, UserData: c.UserData,
		Request: client.VMRequest{Image: c.Image, Network: c.Network, CPU: c.CPU, Memory: c.Memory, BootDiskSize: c.BootDiskSize, TTLSeconds: c.TTLSeconds}}
	if c.Count < 1 || c.CPU < 1 {
		return opt, errors.New("vm-count and cpu must be positive")
	}
	if !guestUser.MatchString(c.Username) || c.Username == "default" {
		return opt, errors.New("username must be a valid guest login name")
	}
	for _, name := range []string{c.Image, c.Network} {
		parts := strings.Split(name, "/")
		if len(parts) != 2 || len(validation.IsDNS1123Label(parts[0])) != 0 || len(validation.IsDNS1123Subdomain(parts[1])) != 0 {
			return opt, errors.New("image and network must be namespace/name")
		}
	}
	for _, value := range []string{c.Memory, c.BootDiskSize} {
		q, err := resource.ParseQuantity(value)
		if err != nil || q.Sign() <= 0 {
			return opt, errors.New("memory and boot-disk-size must be positive Kubernetes quantities")
		}
	}
	if c.TTLSeconds != nil && (*c.TTLSeconds < 1 || *c.TTLSeconds > 86400) {
		return opt, errors.New("ttl-seconds must be between 1 and 86400")
	}
	wait := c.WaitTimeoutSeconds
	if wait == 0 {
		wait = 600
	}
	if wait < 1 || wait > 86400 {
		return opt, errors.New("wait-timeout-seconds must be between 1 and 86400")
	}
	opt.WaitTimeout = time.Duration(wait) * time.Second
	// Validate user-data before creating any local state or remote resources.
	if _, err := cloudConfig(c.UserData, c.Username, "ssh-ed25519 validation-only"); err != nil {
		return opt, err
	}
	return opt, nil
}

type localReport struct {
	StateDir      string            `json:"stateDir"`
	VMs           []client.VMStatus `json:"vms"`
	SSHConfigPath string            `json:"sshConfigPath"`
	SSHCommands   []string          `json:"sshCommands"`
}

const localHelp = `Usage: hvst-runner-gw-client [global flags] cluster COMMAND
  create --config FILE --state-dir DIR
  status --state-dir DIR
  delete --state-dir DIR [--wait-timeout 10m]
Clusters persist until explicit deletion or gateway TTL expiry.
Use the same developer credential for all operations; state contains private SSH files.
`

// RunLocal implements the persistent developer CLI. It never uses Actions OIDC.
func RunLocal(ctx context.Context, args []string, cfg client.Config, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stderr, localHelp)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	operation := args[0]
	if operation != "create" && operation != "status" && operation != "delete" {
		fmt.Fprint(stderr, localHelp)
		return 2
	}
	fs := flag.NewFlagSet("cluster "+operation, flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("state-dir", "", "required: private local cluster directory")
	var configPath string
	var deleteTimeout time.Duration
	if operation == "create" {
		fs.StringVar(&configPath, "config", "", "required: provisioning YAML")
	}
	if operation == "delete" {
		fs.DurationVar(&deleteTimeout, "wait-timeout", 10*time.Minute, "wait for VMs to disappear")
	}
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *dir == "" || (operation == "create" && configPath == "") || (operation == "delete" && deleteTimeout <= 0) {
		fmt.Fprint(stderr, localHelp)
		return 2
	}
	var conf LocalConfig
	var opt options
	if operation == "create" {
		data, err := os.ReadFile(configPath)
		if err == nil {
			err = yaml.UnmarshalStrict(data, &conf)
		}
		if err == nil {
			opt, err = conf.options()
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	absolute, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	cfg.GitHubActions = false
	if cfg.TokenFile == "" && cfg.Token == "" {
		fmt.Fprintln(stderr, "cluster commands require --token-file, GATEWAY_TOKEN_FILE, or GATEWAY_TOKEN")
		return 2
	}
	api, err := client.NewWithLogger(cfg, slog.New(slog.NewJSONHandler(stderr, nil)))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	defer api.Close()
	err = runLocal(ctx, api, operation, strings.TrimRight(cfg.URL, "/"), absolute, conf, opt, deleteTimeout, stdout)
	if err != nil {
		fmt.Fprintln(stderr, err)
		fmt.Fprintf(stderr, "Cluster state: %s\nUse cluster status/delete --state-dir %s with the same developer credentials.\n", absolute, shellQuote(absolute))
		if operation == "create" {
			fmt.Fprintln(stderr, "Creation is not retried automatically. If a response was lost, use vm list to find unrecorded VMs and vm delete ID to clean them up; TTL remains the fallback.")
		}
		return 1
	}
	return 0
}

// lockState uses an OS lock released even when the client is interrupted or killed.
func lockState(dir string, create bool) (*os.File, error) {
	if create {
		if err := os.Mkdir(dir, 0700); err != nil {
			return nil, fmt.Errorf("create state directory (must not exist): %w", err)
		}
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("state directory must be a private directory, not a symlink (chmod 700)")
	}
	path := filepath.Join(dir, ".lock")
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("cluster state is in use by another command")
	}
	current, err := os.Lstat(path)
	opened, statErr := file.Stat()
	if err != nil || statErr != nil || !os.SameFile(current, opened) {
		file.Close()
		return nil, errors.New("cluster state changed while acquiring lock")
	}
	return file, nil
}

var localVMID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func readLocalState(dir, gateway string) (clusterState, error) {
	var state clusterState
	data, err := os.ReadFile(filepath.Join(dir, "cluster.json"))
	if err != nil {
		return state, err
	}
	if err = json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	if state.Version != 1 || state.Dir != dir || state.Gateway != gateway || state.Config == nil {
		return state, errors.New("state version, directory, or gateway does not match")
	}
	if _, err := state.Config.options(); err != nil {
		return state, fmt.Errorf("invalid saved configuration: %w", err)
	}
	seen := map[string]bool{}
	for _, id := range state.IDs {
		if !localVMID.MatchString(id) || seen[id] {
			return state, errors.New("invalid or duplicate saved VM ID")
		}
		seen[id] = true
	}
	return state, nil
}

func runLocal(ctx context.Context, api vmAPI, operation, gateway, dir string, conf LocalConfig, opt options, deleteTimeout time.Duration, output io.Writer) error {
	lock, err := lockState(dir, operation == "create")
	if err != nil {
		return err
	}
	defer lock.Close()
	if operation == "create" {
		state := clusterState{Version: 1, Dir: dir, Gateway: gateway, Config: &conf, IDs: []string{}}
		path := filepath.Join(dir, "cluster.json")
		if err := writeState(path, state); err != nil {
			return err
		}
		_, request, err := prepareRequest(ctx, dir, opt)
		if err != nil {
			return err
		}
		if err := provision(ctx, api, request, opt.Count, &state, path); err != nil {
			return err
		}
		waitCtx, cancel := context.WithTimeout(ctx, opt.WaitTimeout)
		defer cancel()
		if _, err := waitAll(waitCtx, api, state.IDs, pollInterval); err != nil {
			return err
		}
		return reportLocal(ctx, api, state, output)
	}
	state, err := readLocalState(dir, gateway)
	if err != nil {
		return err
	}
	if operation == "status" {
		return reportLocal(ctx, api, state, output)
	}
	waitCtx, cancel := context.WithTimeout(ctx, deleteTimeout)
	defer cancel()
	if err := deleteLocal(waitCtx, api, state, pollInterval); err != nil {
		return err
	}
	// Keep a tombstone so repeated delete is safe and the lock inode stays stable.
	// Remove only known SSH/state files, never unrelated files in this directory.
	for _, name := range []string{"id_ed25519", "id_ed25519.pub", "ssh_config", "known_hosts"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	state.IDs = []string{}
	state.Config.UserData = ""
	if err := writeState(filepath.Join(dir, "cluster.json"), state); err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(map[string]any{"stateDir": dir, "deleted": true})
}

func notFound(err error) bool {
	var httpErr *client.HTTPError
	return errors.As(err, &httpErr) && httpErr.StatusCode == 404
}

func deleteLocal(ctx context.Context, api vmAPI, state clusterState, interval time.Duration) error {
	var failures []error
	pending := map[string]bool{}
	for _, id := range state.IDs {
		if err := api.DeleteVM(ctx, id); err != nil {
			if !notFound(err) {
				failures = append(failures, fmt.Errorf("delete VM %s: %w", id, err))
			}
		} else {
			pending[id] = true
		}
	}
	for len(pending) > 0 {
		for id := range pending {
			_, err := api.GetVM(ctx, id)
			if notFound(err) {
				delete(pending, id)
			} else if err != nil {
				failures = append(failures, fmt.Errorf("confirm deletion of %s: %w", id, err))
				delete(pending, id)
			}
		}
		if len(pending) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return errors.Join(append(failures, fmt.Errorf("waiting for deletion: %w", ctx.Err()))...)
		case <-time.After(interval):
		}
	}
	return errors.Join(failures...)
}

func reportLocal(ctx context.Context, api vmAPI, state clusterState, output io.Writer) error {
	report := localReport{StateDir: state.Dir, VMs: []client.VMStatus{}, SSHConfigPath: filepath.Join(state.Dir, "ssh_config"), SSHCommands: []string{}}
	var accessible []client.VMStatus
	var failures []error
	for _, id := range state.IDs {
		status, err := api.GetVM(ctx, id)
		if notFound(err) {
			status = client.VMStatus{ID: id, Phase: "Deleted"}
		} else if err != nil {
			failures = append(failures, fmt.Errorf("get VM %s: %w", id, err))
			continue
		}
		if status.ID != id {
			failures = append(failures, fmt.Errorf("gateway returned mismatched VM ID for %s", id))
			continue
		}
		report.VMs = append(report.VMs, status)
		if usableIP(status.IPAddresses) != "" {
			accessible = append(accessible, status)
			report.SSHCommands = append(report.SSHCommands, "ssh -F "+shellQuote(report.SSHConfigPath)+" "+shellQuote(id))
		}
	}
	if err := writeSSHConfig(report.SSHConfigPath, filepath.Join(state.Dir, "known_hosts"), filepath.Join(state.Dir, "id_ed25519"), state.Config.Username, accessible); err != nil {
		failures = append(failures, err)
	}
	if err := json.NewEncoder(output).Encode(report); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}
