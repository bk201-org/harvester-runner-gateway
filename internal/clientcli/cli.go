// Package clientcli provides the script-oriented gateway command-line interface.
package clientcli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/bk201-org/harvester-runner-gateway/internal/client"
)

const defaultVMWaitTimeout = 5 * time.Minute
const vmPollInterval = 3 * time.Second

type command struct {
	client.Request
	waitForVM   bool
	waitTimeout time.Duration
}

const rootHelp = `Usage: hvst-runner-gw-client [global flags] COMMAND

Commands:
  health                         Check gateway health
  ready                          Check Harvester readiness
  quota                          Get repository usage and limits
  vm create [flags]              Create a VM
  vm list                        List run-owned VMs
  vm get ID                      Get VM status
  vm delete ID                   Request VM deletion
  vm power ID on|off             Set desired power state
  vm reboot ID                   Request reboot
  vm attach ID VOLUME_ID         Attach an independent volume
  vm detach ID VOLUME_ID         Detach an independent volume
  volume create [flags]          Create an independent volume
  volume list                    List run-owned volumes
  volume get ID                  Get volume status
  volume delete ID               Request volume deletion

Global flags must precede the command. Use COMMAND --help for details.
JSON responses go to stdout; accepted operations may still be in progress.
`

func flagSet(name string, output io.Writer, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() { fmt.Fprintln(output, usage); fs.PrintDefaults() }
	return fs
}

// Run returns an exit code, allowing tests and callers to supply their own I/O
// and environment without changing process-wide state.
func Run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cfg := client.Config{
		URL: getenv("GATEWAY_URL"), TokenFile: getenv("GATEWAY_TOKEN_FILE"), Token: getenv("GATEWAY_TOKEN"),
		CACert: getenv("GATEWAY_CA_CERT"), Audience: getenv("GATEWAY_AUDIENCE"),
		GitHubActions:  getenv("GITHUB_ACTIONS") == "true",
		OIDCRequestURL: getenv("ACTIONS_ID_TOKEN_REQUEST_URL"), OIDCRequestToken: getenv("ACTIONS_ID_TOKEN_REQUEST_TOKEN"),
	}
	if cfg.Audience == "" {
		cfg.Audience = client.DefaultAudience
	}
	timeout := getenv("GATEWAY_TIMEOUT")
	if timeout == "" {
		timeout = "30s"
	}
	vmWaitTimeout := getenv("GATEWAY_VM_WAIT_TIMEOUT")
	if vmWaitTimeout == "" {
		vmWaitTimeout = defaultVMWaitTimeout.String()
	}
	fs := flagSet("hvst-runner-gw-client", stderr, rootHelp)
	fs.StringVar(&cfg.URL, "url", cfg.URL, "gateway HTTPS URL (GATEWAY_URL)")
	fs.StringVar(&cfg.TokenFile, "token-file", cfg.TokenFile, "bearer token file (GATEWAY_TOKEN_FILE)")
	fs.StringVar(&cfg.CACert, "ca-cert", cfg.CACert, "additional PEM CA certificates (GATEWAY_CA_CERT)")
	fs.StringVar(&cfg.Audience, "audience", cfg.Audience, "Actions OIDC audience (GATEWAY_AUDIENCE)")
	fs.StringVar(&timeout, "timeout", timeout, "positive HTTP request timeout (GATEWAY_TIMEOUT)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	if fs.Arg(0) == "help" {
		if fs.NArg() == 1 {
			fs.Usage()
			return 0
		}
		args = append(append([]string{}, fs.Args()[1:]...), "--help")
	} else {
		args = fs.Args()
	}
	operation, err := parseCommand(args, stderr, vmWaitTimeout)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(stderr, err)
		return 2
	}
	cfg.Timeout, err = time.ParseDuration(timeout)
	if err != nil || cfg.Timeout <= 0 {
		fmt.Fprintln(stderr, "--timeout must be a positive duration, such as 30s")
		return 2
	}
	api, err := client.New(cfg)
	if err == nil {
		defer api.Close()
		var data []byte
		data, err = api.Do(ctx, operation.Request)
		if err == nil && operation.waitForVM {
			data, err = waitForVMReady(ctx, api, data, operation.waitTimeout, vmPollInterval)
		}
		if err == nil && len(data) > 0 {
			_, err = fmt.Fprintln(stdout, strings.TrimSpace(string(data)))
		}
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		var configErr *client.ConfigError
		if errors.As(err, &configErr) {
			return 2
		}
		return 1
	}
	return 0
}

var resourceID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func parseCommand(args []string, output io.Writer, vmWaitTimeout string) (command, error) {
	op := command{Request: client.Request{Method: "GET", Auth: true, Statuses: []int{200}}}
	command := args[0]
	rest := args[1:]
	switch command {
	case "health", "ready", "quota":
		fs := flagSet(command, output, "Usage: hvst-runner-gw-client [global flags] "+command)
		if err := fs.Parse(rest); err != nil {
			return op, err
		}
		if fs.NArg() != 0 {
			return op, fmt.Errorf("%s takes no arguments", command)
		}
		if command == "quota" {
			op.Path = []string{"v1", "quota"}
		} else {
			op.Path = []string{command + "z"}
			op.Auth = false
			op.Statuses = []int{204}
		}
		return op, nil
	case "vm", "volume":
	default:
		return op, fmt.Errorf("unknown command %q; use --help", command)
	}
	if len(rest) == 0 || rest[0] == "--help" || rest[0] == "-h" {
		fmt.Fprint(output, rootHelp)
		if len(rest) == 0 {
			return op, fmt.Errorf("%s requires a subcommand", command)
		}
		return op, flag.ErrHelp
	}
	action := rest[0]
	rest = rest[1:]
	plural := command + "s"
	op.Path = []string{"v1", plural}
	if action == "create" {
		return parseCreate(command, rest, output, vmWaitTimeout)
	}
	expected := 0
	switch action {
	case "list":
	case "get":
		expected = 1
	case "delete":
		expected = 1
		op.Method = "DELETE"
		op.Statuses = []int{204}
	case "power", "reboot", "attach", "detach":
		if command != "vm" {
			return op, fmt.Errorf("unknown volume subcommand %q", action)
		}
		expected = 1
		if action != "reboot" {
			expected = 2
		}
		op.Statuses = []int{202}
	default:
		return op, fmt.Errorf("unknown %s subcommand %q", command, action)
	}
	suffix := ""
	if expected > 0 {
		suffix = " ID"
	}
	if action == "power" {
		suffix += " on|off"
	}
	if action == "attach" || action == "detach" {
		suffix += " VOLUME_ID"
	}
	fs := flagSet(command+" "+action, output, "Usage: hvst-runner-gw-client [global flags] "+command+" "+action+suffix)
	// Permit help after positional IDs as well as immediately after the command.
	for _, arg := range rest {
		if arg == "--help" || arg == "-h" {
			fs.Usage()
			return op, flag.ErrHelp
		}
	}
	if err := fs.Parse(rest); err != nil {
		return op, err
	}
	if fs.NArg() != expected {
		return op, fmt.Errorf("%s %s requires %d argument(s)", command, action, expected)
	}
	for i, arg := range fs.Args() {
		if action == "power" && i == 1 {
			continue
		}
		if !resourceID.MatchString(arg) {
			return op, errors.New("resource IDs must start with an alphanumeric character and contain only letters, digits, '.', '_', or '-'")
		}
	}
	if expected > 0 {
		op.Path = append(op.Path, fs.Arg(0))
	}
	switch action {
	case "power":
		if fs.Arg(1) != "on" && fs.Arg(1) != "off" {
			return op, errors.New("power state must be on or off")
		}
		op.Method = "PUT"
		op.Path = append(op.Path, "power")
		op.Body = struct {
			State string `json:"state"`
		}{fs.Arg(1)}
	case "reboot":
		op.Method = "POST"
		op.Path = append(op.Path, "reboot")
	case "attach", "detach":
		op.Method = "PUT"
		if action == "detach" {
			op.Method = "DELETE"
		}
		op.Path = append(op.Path, "volumes", fs.Arg(1))
	}
	return op, nil
}

type stringList []string

func (s *stringList) String() string         { return strings.Join(*s, ", ") }
func (s *stringList) Set(value string) error { *s = append(*s, value); return nil }

// Validate quantity syntax locally; policy limits remain the server's concern.
var quantityPattern = regexp.MustCompile(`^([+]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))((?:[eE][+-]?[0-9]+)|(?:[KMGTPE]i)|[numkMGTPE]?)$`)

func positiveQuantity(value string) bool {
	parts := quantityPattern.FindStringSubmatch(value)
	if parts == nil {
		return false
	}
	number, ok := new(big.Rat).SetString(parts[1])
	return ok && number.Sign() > 0
}

func parseCreate(kind string, args []string, output io.Writer, defaultWaitTimeout string) (command, error) {
	op := command{Request: client.Request{Method: "POST", Path: []string{"v1", kind + "s"}, Auth: true, Statuses: []int{200, 201}}}
	fs := flagSet(kind+" create", output, "Usage: hvst-runner-gw-client [global flags] "+kind+" create [flags]\nAll size quantities use Kubernetes notation, e.g. 4Gi. VM creation waits for a running VMI with a usable IP unless --no-wait is set.")
	fs.StringVar(&op.IdempotencyKey, "idempotency-key", "", "required: stable key for repeating this request (1-128 printable non-space ASCII characters)")
	ttl := fs.Int("ttl-seconds", 0, "resource lifetime, 1-86400 seconds (omitted: server default)")
	var vm client.VMRequest
	var volume client.VolumeRequest
	var keys stringList
	var userDataFile, waitTimeout string
	var noWait bool
	if kind == "vm" {
		fs.StringVar(&vm.Image, "image", "", "required: approved namespace/name image")
		fs.StringVar(&vm.Network, "network", "", "required: approved namespace/name network")
		fs.IntVar(&vm.CPU, "cpu", 0, "required: positive CPU count")
		fs.StringVar(&vm.Memory, "memory", "", "required: memory quantity")
		fs.StringVar(&vm.BootDiskSize, "boot-disk-size", "", "required: boot disk quantity")
		fs.Var(&keys, "ssh-public-key-file", "SSH public key file; repeat for multiple keys (maximum 10)")
		fs.StringVar(&userDataFile, "user-data-file", "", "cloud-config file (maximum 64 KiB)")
		fs.StringVar(&waitTimeout, "wait-timeout", defaultWaitTimeout, "time to wait for VM readiness (GATEWAY_VM_WAIT_TIMEOUT)")
		fs.BoolVar(&noWait, "no-wait", false, "return after VM creation without waiting for readiness")
	} else {
		fs.StringVar(&volume.Size, "size", "", "required: volume size quantity")
	}
	if err := fs.Parse(args); err != nil {
		return op, err
	}
	if fs.NArg() != 0 {
		return op, errors.New("create accepts flags only")
	}
	if len(op.IdempotencyKey) < 1 || len(op.IdempotencyKey) > 128 {
		return op, errors.New("--idempotency-key requires 1-128 printable non-space ASCII characters")
	}
	for _, r := range op.IdempotencyKey {
		if r < 33 || r > 126 {
			return op, errors.New("--idempotency-key requires printable non-space ASCII characters")
		}
	}
	var ttlSet bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "ttl-seconds" {
			ttlSet = true
		}
	})
	if ttlSet {
		if *ttl < 1 || *ttl > 86400 {
			return op, errors.New("--ttl-seconds must be between 1 and 86400")
		}
		vm.TTLSeconds = ttl
		volume.TTLSeconds = ttl
	}
	if kind == "volume" {
		if !positiveQuantity(volume.Size) {
			return op, errors.New("--size requires a positive Kubernetes quantity")
		}
		op.Body = volume
		return op, nil
	}
	duration, err := time.ParseDuration(waitTimeout)
	if err != nil || duration <= 0 {
		return op, errors.New("--wait-timeout must be a positive duration, such as 5m")
	}
	op.waitForVM = !noWait
	op.waitTimeout = duration
	for _, item := range []struct{ name, value string }{{"image", vm.Image}, {"network", vm.Network}} {
		parts := strings.Split(item.value, "/")
		if len(parts) != 2 || !resourceID.MatchString(parts[0]) || !resourceID.MatchString(parts[1]) {
			return op, fmt.Errorf("--%s requires namespace/name", item.name)
		}
	}
	if vm.CPU < 1 {
		return op, errors.New("--cpu must be positive")
	}
	if !positiveQuantity(vm.Memory) || !positiveQuantity(vm.BootDiskSize) {
		return op, errors.New("--memory and --boot-disk-size require positive Kubernetes quantities")
	}
	if len(keys) > 10 {
		return op, errors.New("at most 10 SSH public keys are allowed")
	}
	for _, path := range keys {
		data, err := readFile(path, 8192)
		if err != nil {
			return op, fmt.Errorf("SSH public key file: %w", err)
		}
		key := strings.TrimSpace(string(data))
		fields := strings.Fields(key)
		if strings.ContainsAny(key, "\r\n") || len(fields) < 2 || (fields[0] != "ssh-ed25519" && fields[0] != "ssh-rsa" && !strings.HasPrefix(fields[0], "ecdsa-sha2-")) {
			return op, errors.New("invalid SSH public key file")
		}
		if _, err := base64.StdEncoding.DecodeString(fields[1]); err != nil {
			return op, errors.New("invalid SSH public key encoding")
		}
		vm.SSHPublicKeys = append(vm.SSHPublicKeys, key)
	}
	if userDataFile != "" {
		data, err := readFile(userDataFile, 64*1024)
		if err != nil {
			return op, fmt.Errorf("user data file: %w", err)
		}
		if !strings.HasPrefix(string(data), "#cloud-config") {
			return op, errors.New("user data must start with #cloud-config")
		}
		vm.UserData = string(data)
	}
	op.Body = vm
	return op, nil
}

func waitForVMReady(parent context.Context, api *client.Client, initial []byte, timeout, interval time.Duration) ([]byte, error) {
	status, err := decodeVMStatus(initial)
	if err != nil {
		return nil, err
	}
	if status.Ready {
		return initial, nil
	}
	waitCtx, cancel := context.WithTimeout(parent, timeout)
	id := status.ID
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return nil, vmWaitError(status, timeout, waitCtx.Err())
		case <-ticker.C:
			data, requestErr := api.Do(waitCtx, client.Request{
				Method: "GET", Path: []string{"v1", "vms", id}, Auth: true, Statuses: []int{200},
			})
			if requestErr != nil {
				if waitCtx.Err() != nil {
					return nil, vmWaitError(status, timeout, waitCtx.Err())
				}
				return nil, fmt.Errorf("wait for VM %s readiness (last phase=%s, IP addresses=%v): %w", status.ID, status.Phase, status.IPAddresses, requestErr)
			}
			status, err = decodeVMStatus(data)
			if err != nil {
				return nil, err
			}
			if status.ID != id {
				return nil, errors.New("gateway returned VM status for a different resource")
			}
			if status.Ready {
				return data, nil
			}
		}
	}
}

func decodeVMStatus(data []byte) (client.VMStatus, error) {
	var status client.VMStatus
	if err := json.Unmarshal(data, &status); err != nil || !resourceID.MatchString(status.ID) {
		return client.VMStatus{}, errors.New("gateway returned an invalid VM status")
	}
	return status, nil
}

func vmWaitError(status client.VMStatus, timeout time.Duration, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("VM %s did not become ready within %s (last phase=%s, IP addresses=%v); VM remains allocated: %w",
			status.ID, timeout, status.Phase, status.IPAddresses, err)
	}
	return fmt.Errorf("waiting for VM %s readiness was canceled (last phase=%s, IP addresses=%v); VM remains allocated: %w",
		status.ID, status.Phase, status.IPAddresses, err)
}

func readFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open file")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, errors.New("cannot read file")
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}
