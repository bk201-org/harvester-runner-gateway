// Package smoke implements the live gateway lifecycle smoke test.
package smoke

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bk201-org/harvester-runner-gateway/client"
)

const (
	defaultConcurrency  = 3
	defaultPollInterval = 5 * time.Second
	defaultWaitTimeout  = 10 * time.Minute
	cleanupTimeout      = 2 * time.Minute
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
	attempt      string
	concurrency  int
	waitTimeout  time.Duration
	pollInterval time.Duration
}

type localConfig struct {
	GatewayURL  string `json:"gatewayURL"`
	Image       string `json:"image"`
	Network     string `json:"network"`
	TokenFile   string `json:"tokenFile"`
	CACert      string `json:"caCert,omitempty"`
	Concurrency int    `json:"concurrency,omitempty"`
}

type workerState struct {
	index            int
	vmID, volumeID   string
	vmKey, volumeKey string
	vmRequest        client.VMRequest
	volumeRequest    client.VolumeRequest
	vmUncertain      bool
	volumeUncertain  bool
	attached         bool
}

type workerResult struct {
	index int
	err   error
}

// Run loads smoke configuration, creates a client, and runs concurrent resource lifecycles.
func Run(ctx context.Context, getenv func(string) string, logger *slog.Logger) error {
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
	defer api.Close()
	return run(ctx, api, cfg, logger)
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		memory:       valueOr(getenv("GATEWAY_MEMORY"), "2Gi"),
		bootDiskSize: valueOr(getenv("GATEWAY_BOOT_DISK"), "20Gi"),
		volumeSize:   valueOr(getenv("GATEWAY_VOLUME_SIZE"), "1Gi"),
		concurrency:  defaultConcurrency,
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
		runID := required(getenv, "GITHUB_RUN_ID")
		runAttempt := required(getenv, "GITHUB_RUN_ATTEMPT")
		if cfg.client.URL == "" || cfg.image == "" || cfg.network == "" || cfg.client.OIDCRequestURL == "" || cfg.client.OIDCRequestToken == "" || runID == "" || runAttempt == "" {
			return config{}, configError("GitHub Actions smoke configuration is incomplete")
		}
		cfg.attempt = runID + "-" + runAttempt
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
		if local.Concurrency != 0 {
			cfg.concurrency = local.Concurrency
		}
		if err := validateLocalToken(local.TokenFile); err != nil {
			return config{}, err
		}
		cfg.attempt, err = randomAttempt()
		if err != nil {
			return config{}, err
		}
	}
	if value := getenv("GATEWAY_SMOKE_CONCURRENCY"); value != "" {
		cfg.concurrency, err = strconv.Atoi(value)
		if err != nil {
			return config{}, configError("GATEWAY_SMOKE_CONCURRENCY must be a positive integer")
		}
	}
	if cfg.concurrency < 1 {
		return config{}, configError("smoke concurrency must be a positive integer")
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

func randomAttempt() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate local smoke attempt: %w", err)
	}
	return "local-" + hex.EncodeToString(data), nil
}

func run(ctx context.Context, api *client.Client, cfg config, logger *slog.Logger) error {
	quota, err := api.Quota(ctx)
	if err != nil {
		return fmt.Errorf("read quota: %w", err)
	}
	availableVMs := quota.MaxActiveVMs - quota.ActiveVMs
	availableVolumes := quota.MaxActiveVolumes - quota.ActiveVolumes
	if availableVMs < cfg.concurrency || availableVolumes < cfg.concurrency {
		return fmt.Errorf("insufficient quota for %d concurrent workers: available VMs=%d, volumes=%d",
			cfg.concurrency, availableVMs, availableVolumes)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	start := make(chan struct{})
	results := make(chan workerResult, cfg.concurrency)
	var wait sync.WaitGroup
	for index := 1; index <= cfg.concurrency; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			err := runWorker(runCtx, api, cfg, workerState{index: index}, logger, cancel)
			results <- workerResult{index: index, err: err}
		}(index)
	}
	close(start)
	wait.Wait()
	close(results)
	var failures []error
	for result := range results {
		if result.err != nil {
			failures = append(failures, fmt.Errorf("worker %d: %w", result.index, result.err))
		}
	}
	return errors.Join(failures...)
}

func runWorker(ctx context.Context, api *client.Client, cfg config, state workerState, logger *slog.Logger, cancelRun context.CancelFunc) (err error) {
	logger = logger.With("worker", state.index)
	defer func() {
		if err != nil {
			cancelRun()
		}
		if err == nil && state.vmID == "" && state.volumeID == "" {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cleanupCancel()
		if cleanupErr := cleanup(cleanupCtx, api, &state, cfg, logger); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("cleanup: %w", cleanupErr))
		}
	}()

	state.vmRequest = client.VMRequest{
		Image: cfg.image, Network: cfg.network, CPU: 2, Memory: cfg.memory,
		BootDiskSize: cfg.bootDiskSize, TTLSeconds: intPointer(3600),
	}
	state.vmKey = fmt.Sprintf("%s-vm-%d", cfg.attempt, state.index)
	state.vmUncertain = true
	vm, err := api.CreateVM(ctx, state.vmRequest, state.vmKey)
	if err != nil {
		return fmt.Errorf("create VM: %w", err)
	}
	state.vmID = vm.ID
	state.vmUncertain = false
	logger.InfoContext(ctx, "smoke VM created", "vm_id", state.vmID)
	if !vm.Ready {
		waitCtx, cancel := context.WithTimeout(ctx, cfg.waitTimeout)
		_, err = api.WaitForVMReady(waitCtx, state.vmID, cfg.pollInterval)
		cancel()
		if err != nil {
			return err
		}
	}

	state.volumeRequest = client.VolumeRequest{Size: cfg.volumeSize, TTLSeconds: intPointer(3600)}
	state.volumeKey = fmt.Sprintf("%s-volume-%d", cfg.attempt, state.index)
	state.volumeUncertain = true
	volume, err := api.CreateVolume(ctx, state.volumeRequest, state.volumeKey)
	if err != nil {
		return fmt.Errorf("create volume: %w", err)
	}
	state.volumeID = volume.ID
	state.volumeUncertain = false
	logger.InfoContext(ctx, "smoke volume created", "volume_id", state.volumeID)
	if err := waitForVolume(ctx, api, state.volumeID, cfg, func(status client.VolumeStatus) bool {
		return status.Phase == "Bound"
	}); err != nil {
		return fmt.Errorf("wait for volume Bound: %w", err)
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
	if err := api.DeleteVolume(ctx, state.volumeID); err != nil {
		return fmt.Errorf("delete volume: %w", err)
	}
	state.volumeID = ""

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
	if err := api.DeleteVM(ctx, state.vmID); err != nil {
		return fmt.Errorf("delete VM: %w", err)
	}
	state.vmID = ""
	logger.InfoContext(ctx, "smoke worker completed")
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

func cleanup(ctx context.Context, api *client.Client, state *workerState, cfg config, logger *slog.Logger) error {
	var failures []error
	if state.vmUncertain {
		vm, err := api.CreateVM(ctx, state.vmRequest, state.vmKey)
		if err != nil {
			failures = append(failures, fmt.Errorf("recover uncertain VM create: %w", err))
		} else {
			state.vmID = vm.ID
			state.vmUncertain = false
		}
	}
	if state.volumeUncertain {
		volume, err := api.CreateVolume(ctx, state.volumeRequest, state.volumeKey)
		if err != nil {
			failures = append(failures, fmt.Errorf("recover uncertain volume create: %w", err))
		} else {
			state.volumeID = volume.ID
			state.volumeUncertain = false
		}
	}
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
