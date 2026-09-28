// Package smoke implements the live gateway lifecycle smoke test.
package smoke

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bk201/harvester-runner-gateway/client"
)

const (
	defaultPollInterval = 10 * time.Second
	defaultWaitTimeout  = 10 * time.Minute
	cleanupTimeout      = 2 * time.Minute
	maxConcurrentCases  = 2
)

type ConfigError struct{ Err error }

func (e *ConfigError) Error() string { return e.Err.Error() }
func configError(message string) error {
	return &ConfigError{Err: errors.New(message)}
}

type config struct {
	client       client.Config
	image        string
	network      string
	memory       string
	bootDiskSize string
	volumeSize   string
	waitTimeout  time.Duration
	pollInterval time.Duration
}

type localConfig struct {
	GatewayURL string `json:"gatewayURL"`
	Image      string `json:"image"`
	Network    string `json:"network"`
	TokenFile  string `json:"tokenFile"`
	CACert     string `json:"caCert,omitempty"`
}

type testState struct {
	vmID, volumeID string
	attached       bool
}

type smokeTestCase struct {
	name        string
	needsVolume bool
	run         func(context.Context, *client.Client, config, *testState, *slog.Logger) error
}

var smokeTestCases = []smokeTestCase{
	{name: "VM lifecycle", run: runVMLifecycle},
	{name: "Volume hotplug", needsVolume: true, run: runVolumeHotplug},
	{name: "VM power and reboot", run: runVMPowerAndReboot},
	{name: "VM deletion cascades attached volume", needsVolume: true, run: runVMDeletionCascade},
}

// Run loads smoke configuration, creates a client, and runs focused lifecycle subtests.
func Run(t *testing.T, getenv func(string) string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	cfg, err := loadConfig(getenv)
	if err != nil {
		return err
	}
	api, err := client.NewWithLogger(cfg.client, logger)
	if err != nil {
		return err
	}
	// Parallel subtests can continue after run returns.
	t.Cleanup(api.Close)
	return run(t, api, cfg, logger)
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		memory:       valueOr(getenv("GATEWAY_MEMORY"), "2Gi"),
		bootDiskSize: valueOr(getenv("GATEWAY_BOOT_DISK"), "20Gi"),
		volumeSize:   valueOr(getenv("GATEWAY_VOLUME_SIZE"), "1Gi"),
		waitTimeout:  defaultWaitTimeout,
		pollInterval: defaultPollInterval,
	}
	requestTimeout, err := parseDuration(valueOr(getenv("GATEWAY_TIMEOUT"), "30s"), "GATEWAY_TIMEOUT")
	if err != nil {
		return config{}, err
	}
	cfg.client.Timeout = requestTimeout
	if value := getenv("GATEWAY_VM_WAIT_TIMEOUT"); value != "" {
		cfg.waitTimeout, err = parseDuration(value, "GATEWAY_VM_WAIT_TIMEOUT")
		if err != nil {
			return config{}, err
		}
	}
	if getenv("GITHUB_ACTIONS") == "true" {
		if getenv("GATEWAY_SMOKE") != "1" {
			return config{}, configError("Set GATEWAY_SMOKE=1 to run the live smoke test")
		}
		cfg.client.URL = required(getenv, "GATEWAY_URL")
		cfg.image = required(getenv, "GATEWAY_IMAGE")
		cfg.network = required(getenv, "GATEWAY_NETWORK")
		cfg.client.CACert = getenv("GATEWAY_CA_CERT")
		cfg.client.Audience = valueOr(getenv("GATEWAY_AUDIENCE"), client.DefaultAudience)
		cfg.client.GitHubActions = true
		cfg.client.OIDCRequestURL = required(getenv, "ACTIONS_ID_TOKEN_REQUEST_URL")
		cfg.client.OIDCRequestToken = required(getenv, "ACTIONS_ID_TOKEN_REQUEST_TOKEN")
		if cfg.client.URL == "" || cfg.image == "" || cfg.network == "" || cfg.client.OIDCRequestURL == "" || cfg.client.OIDCRequestToken == "" {
			return config{}, configError("GitHub Actions smoke configuration is incomplete")
		}
	} else {
		path := getenv("GATEWAY_SMOKE_CONFIG")
		if path == "" {
			configHome := getenv("XDG_CONFIG_HOME")
			if configHome == "" {
				home := getenv("HOME")
				if home == "" {
					return config{}, configError("HOME is required when GATEWAY_SMOKE_CONFIG is unset")
				}
				configHome = filepath.Join(home, ".config")
			}
			path = filepath.Join(configHome, "harvester-runner-gateway", "smoke.json")
		}
		local, err := readLocalConfig(path)
		if err != nil {
			return config{}, err
		}
		cfg.client.URL, cfg.image, cfg.network = local.GatewayURL, local.Image, local.Network
		cfg.client.TokenFile, cfg.client.CACert = local.TokenFile, local.CACert
		cfg.client.Audience = client.DefaultAudience
		if err := validateLocalToken(local.TokenFile); err != nil {
			return config{}, err
		}
	}
	if parsed, err := url.Parse(cfg.client.URL); err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return config{}, configError("Gateway URL must use HTTPS")
	}
	return cfg, nil
}

func required(getenv func(string) string, name string) string { return strings.TrimSpace(getenv(name)) }
func valueOr(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func parseDuration(value, name string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, configError(name + " must be a positive duration")
	}
	return duration, nil
}

func readLocalConfig(path string) (localConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return localConfig{}, configError("Cannot read local smoke config: " + path)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var cfg localConfig
	if err := decoder.Decode(&cfg); err != nil {
		return localConfig{}, configError("Local smoke config is invalid")
	}
	if cfg.GatewayURL == "" || cfg.Image == "" || cfg.Network == "" || cfg.TokenFile == "" {
		return localConfig{}, configError("Local smoke config requires gatewayURL, image, network, and tokenFile")
	}
	return cfg, nil
}

func validateLocalToken(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return configError("Cannot read local smoke token file: " + path)
	}
	token := strings.TrimSpace(string(data))
	if len(token) != 64 {
		return configError("Local smoke token must contain 64 hex characters")
	}
	if _, err := hex.DecodeString(token); err != nil {
		return configError("Local smoke token must contain 64 hex characters")
	}
	return nil
}

func run(t *testing.T, api *client.Client, cfg config, logger *slog.Logger) error {
	quota, err := api.Quota(t.Context())
	if err != nil {
		return fmt.Errorf("read quota: %w", err)
	}
	availableVMs := quota.MaxActiveVMs - quota.ActiveVMs
	availableVolumes := quota.MaxActiveVolumes - quota.ActiveVolumes
	if availableVMs < 1 || availableVolumes < 1 {
		return fmt.Errorf("insufficient quota for focused smoke tests: available VMs=%d, volumes=%d", availableVMs, availableVolumes)
	}

	vmSlots := make(chan struct{}, min(maxConcurrentCases, availableVMs))
	volumeSlots := make(chan struct{}, min(maxConcurrentCases, availableVolumes))
	for _, testCase := range smokeTestCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if testCase.needsVolume {
				volumeSlots <- struct{}{}
				defer func() { <-volumeSlots }()
			}
			vmSlots <- struct{}{}
			defer func() { <-vmSlots }()
			runTestCase(t, api, cfg, logger, testCase)
		})
	}
	return nil
}

func runTestCase(t *testing.T, api *client.Client, cfg config, logger *slog.Logger, testCase smokeTestCase) {
	state := testState{}
	caseLogger := logger.With("test", testCase.name)
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(t.Context()), cleanupTimeout)
		defer cleanupCancel()
		if err := cleanup(cleanupCtx, api, &state, cfg, caseLogger); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	}()
	if err := testCase.run(t.Context(), api, cfg, &state, caseLogger); err != nil {
		t.Fatal(err)
	}
}

func createVM(ctx context.Context, api *client.Client, cfg config, state *testState, logger *slog.Logger) error {
	request := client.VMRequest{
		Image: cfg.image, Network: cfg.network, CPU: 2, Memory: cfg.memory,
		BootDiskSize: cfg.bootDiskSize, TTLSeconds: intPointer(3600),
	}
	vm, err := api.CreateVM(ctx, request)
	if err != nil {
		return fmt.Errorf("create VM: %w", err)
	}
	state.vmID = vm.ID
	logger.InfoContext(ctx, "smoke VM created", "vm_id", state.vmID)
	if vm.Ready {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, cfg.waitTimeout)
	defer cancel()
	if _, err := api.WaitForVMReady(waitCtx, state.vmID, cfg.pollInterval); err != nil {
		return err
	}
	return nil
}

func createVolume(ctx context.Context, api *client.Client, cfg config, state *testState, logger *slog.Logger) error {
	request := client.VolumeRequest{Size: cfg.volumeSize, TTLSeconds: intPointer(3600)}
	volume, err := api.CreateVolume(ctx, request)
	if err != nil {
		return fmt.Errorf("create volume: %w", err)
	}
	state.volumeID = volume.ID
	logger.InfoContext(ctx, "smoke volume created", "volume_id", state.volumeID)
	if err := waitForVolume(ctx, api, state.volumeID, cfg, func(status client.VolumeStatus) bool {
		return status.Phase == "Bound"
	}); err != nil {
		return fmt.Errorf("wait for volume Bound: %w", err)
	}
	return nil
}

func runVMLifecycle(ctx context.Context, api *client.Client, cfg config, state *testState, logger *slog.Logger) error {
	if err := createVM(ctx, api, cfg, state, logger); err != nil {
		return err
	}
	vmID := state.vmID
	if err := api.DeleteVM(ctx, vmID); err != nil {
		return fmt.Errorf("delete VM: %w", err)
	}
	if err := waitForVMGone(ctx, api, vmID, cfg); err != nil {
		return fmt.Errorf("wait for VM deletion: %w", err)
	}
	state.vmID = ""
	return nil
}

func runVolumeHotplug(ctx context.Context, api *client.Client, cfg config, state *testState, logger *slog.Logger) error {
	if err := createVM(ctx, api, cfg, state, logger); err != nil {
		return err
	}
	if err := createVolume(ctx, api, cfg, state, logger); err != nil {
		return err
	}
	state.attached = true
	if err := api.AttachVolume(ctx, state.vmID, state.volumeID); err != nil {
		return fmt.Errorf("attach volume: %w", err)
	}
	if err := waitForVolume(ctx, api, state.volumeID, cfg, func(status client.VolumeStatus) bool {
		return status.AttachedTo == state.vmID && status.AttachmentPhase == "Ready"
	}); err != nil {
		return fmt.Errorf("wait for volume attachment: %w", err)
	}
	if err := api.DetachVolume(ctx, state.vmID, state.volumeID); err != nil {
		return fmt.Errorf("detach volume: %w", err)
	}
	if err := waitForVolume(ctx, api, state.volumeID, cfg, func(status client.VolumeStatus) bool {
		return status.AttachedTo == ""
	}); err != nil {
		return fmt.Errorf("wait for volume detachment: %w", err)
	}
	state.attached = false
	volumeID := state.volumeID
	if err := api.DeleteVolume(ctx, volumeID); err != nil {
		return fmt.Errorf("delete volume: %w", err)
	}
	if err := waitForVolumeGone(ctx, api, volumeID, cfg); err != nil {
		return fmt.Errorf("wait for explicit volume deletion: %w", err)
	}
	state.volumeID = ""
	return nil
}

func runVMPowerAndReboot(ctx context.Context, api *client.Client, cfg config, state *testState, logger *slog.Logger) error {
	if err := createVM(ctx, api, cfg, state, logger); err != nil {
		return err
	}
	if err := api.SetVMPower(ctx, state.vmID, client.PowerOff); err != nil {
		return fmt.Errorf("power off VM: %w", err)
	}
	if err := waitForVM(ctx, api, state.vmID, cfg, func(status client.VMStatus) bool {
		return status.PowerState == string(client.PowerOff)
	}); err != nil {
		return fmt.Errorf("wait for VM power off: %w", err)
	}
	if err := api.SetVMPower(ctx, state.vmID, client.PowerOn); err != nil {
		return fmt.Errorf("power on VM: %w", err)
	}
	if err := waitForVM(ctx, api, state.vmID, cfg, func(status client.VMStatus) bool {
		return status.Phase == "Running"
	}); err != nil {
		return fmt.Errorf("wait for VM running: %w", err)
	}
	if err := api.RebootVM(ctx, state.vmID); err != nil {
		return fmt.Errorf("reboot VM: %w", err)
	}
	return nil
}

func runVMDeletionCascade(ctx context.Context, api *client.Client, cfg config, state *testState, logger *slog.Logger) error {
	if err := createVM(ctx, api, cfg, state, logger); err != nil {
		return err
	}
	if err := createVolume(ctx, api, cfg, state, logger); err != nil {
		return err
	}
	state.attached = true
	vmID, volumeID := state.vmID, state.volumeID
	if err := api.AttachVolume(ctx, vmID, volumeID); err != nil {
		return fmt.Errorf("attach volume for VM deletion: %w", err)
	}
	if err := waitForVolume(ctx, api, volumeID, cfg, func(status client.VolumeStatus) bool {
		return status.AttachedTo == vmID && status.AttachmentPhase == "Ready"
	}); err != nil {
		return fmt.Errorf("wait for volume attachment before VM deletion: %w", err)
	}
	if err := api.DeleteVM(ctx, vmID); err != nil {
		return fmt.Errorf("delete VM with attached volume: %w", err)
	}
	state.attached = false
	if err := waitForVMGone(ctx, api, vmID, cfg); err != nil {
		return fmt.Errorf("wait for VM deletion: %w", err)
	}
	state.vmID = ""
	if err := waitForVolumeGone(ctx, api, volumeID, cfg); err != nil {
		return fmt.Errorf("wait for attached volume deletion: %w", err)
	}
	state.volumeID = ""
	return nil
}

func waitForVolume(ctx context.Context, api *client.Client, id string, cfg config, ready func(client.VolumeStatus) bool) error {
	waitCtx, cancel := context.WithTimeout(ctx, cfg.waitTimeout)
	defer cancel()
	return poll(waitCtx, cfg.pollInterval, func() (bool, error) {
		status, err := api.GetVolume(waitCtx, id)
		return err == nil && ready(status), err
	})
}

func waitForVM(ctx context.Context, api *client.Client, id string, cfg config, ready func(client.VMStatus) bool) error {
	waitCtx, cancel := context.WithTimeout(ctx, cfg.waitTimeout)
	defer cancel()
	return poll(waitCtx, cfg.pollInterval, func() (bool, error) {
		status, err := api.GetVM(waitCtx, id)
		return err == nil && ready(status), err
	})
}

func waitForVolumeGone(ctx context.Context, api *client.Client, id string, cfg config) error {
	waitCtx, cancel := context.WithTimeout(ctx, cfg.waitTimeout)
	defer cancel()
	return poll(waitCtx, cfg.pollInterval, func() (bool, error) {
		volumes, err := api.ListVolumes(waitCtx)
		if err != nil {
			return false, err
		}
		for _, volume := range volumes {
			if volume.ID == id {
				return false, nil
			}
		}
		return true, nil
	})
}

func waitForVMGone(ctx context.Context, api *client.Client, id string, cfg config) error {
	waitCtx, cancel := context.WithTimeout(ctx, cfg.waitTimeout)
	defer cancel()
	return poll(waitCtx, cfg.pollInterval, func() (bool, error) {
		vms, err := api.ListVMs(waitCtx)
		if err != nil {
			return false, err
		}
		for _, vm := range vms {
			if vm.ID == id {
				return false, nil
			}
		}
		return true, nil
	})
}

func poll(ctx context.Context, interval time.Duration, check func() (bool, error)) error {
	for {
		ready, err := check()
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func cleanup(ctx context.Context, api *client.Client, state *testState, cfg config, logger *slog.Logger) error {
	var failures []error
	if state.attached && state.vmID != "" && state.volumeID != "" {
		if err := api.DetachVolume(ctx, state.vmID, state.volumeID); err != nil {
			failures = append(failures, fmt.Errorf("detach volume %s: %w", state.volumeID, err))
		} else if err := waitForVolume(ctx, api, state.volumeID, cfg, func(status client.VolumeStatus) bool {
			return status.AttachedTo == ""
		}); err != nil {
			failures = append(failures, fmt.Errorf("wait to detach volume %s: %w", state.volumeID, err))
		}
	}
	if state.volumeID != "" {
		if err := api.DeleteVolume(ctx, state.volumeID); err != nil {
			failures = append(failures, fmt.Errorf("delete volume %s: %w", state.volumeID, err))
		} else {
			logger.InfoContext(ctx, "cleaned up smoke volume", "volume_id", state.volumeID)
		}
	}
	if state.vmID != "" {
		if err := api.DeleteVM(ctx, state.vmID); err != nil {
			failures = append(failures, fmt.Errorf("delete VM %s: %w", state.vmID, err))
		} else {
			logger.InfoContext(ctx, "cleaned up smoke VM", "vm_id", state.vmID)
		}
	}
	return errors.Join(failures...)
}

func intPointer(value int) *int { return &value }
