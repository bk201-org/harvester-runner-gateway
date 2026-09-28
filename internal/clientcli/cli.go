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
	"log/slog"
	"math/big"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/bk201/harvester-runner-gateway/client"
)

const defaultVMWaitTimeout = 5 * time.Minute
const vmPollInterval = 10 * time.Second

type command struct {
	kind, action, id, second string
	vmRequest                client.VMRequest
	volumeRequest            client.VolumeRequest
	waitForVM                bool
	waitTimeout              time.Duration
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
	logger := slog.New(slog.NewJSONHandler(stderr, nil))
	api, err := client.NewWithLogger(cfg, logger)
	if err == nil {
		defer api.Close()
		var output any
		var hasOutput bool
		output, hasOutput, err = execute(ctx, api, operation)
		if err == nil && hasOutput {
			var data []byte
			data, err = json.Marshal(output)
			if err == nil {
				_, err = fmt.Fprintln(stdout, string(data))
			}
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

func execute(ctx context.Context, api *client.Client, op command) (any, bool, error) {
	switch op.kind {
	case "health":
		return nil, false, api.Health(ctx)
	case "ready":
		return nil, false, api.Ready(ctx)
	case "quota":
		value, err := api.Quota(ctx)
		return value, true, err
	case "vm":
		switch op.action {
		case "create":
			status, err := api.CreateVM(ctx, op.vmRequest)
			if err == nil && op.waitForVM && !status.Ready {
				waitCtx, cancel := context.WithTimeout(ctx, op.waitTimeout)
				defer cancel()
				status, err = api.WaitForVMReady(waitCtx, status.ID, vmPollInterval)
			}
			return status, true, err
		case "list":
			value, err := api.ListVMs(ctx)
			return value, true, err
		case "get":
			value, err := api.GetVM(ctx, op.id)
			return value, true, err
		case "delete":
			return nil, false, api.DeleteVM(ctx, op.id)
		case "power":
			return nil, false, api.SetVMPower(ctx, op.id, client.PowerState(op.second))
		case "reboot":
			return nil, false, api.RebootVM(ctx, op.id)
		case "attach":
			return nil, false, api.AttachVolume(ctx, op.id, op.second)
		case "detach":
			return nil, false, api.DetachVolume(ctx, op.id, op.second)
		}
	case "volume":
		switch op.action {
		case "create":
			value, err := api.CreateVolume(ctx, op.volumeRequest)
			return value, true, err
		case "list":
			value, err := api.ListVolumes(ctx)
			return value, true, err
		case "get":
			value, err := api.GetVolume(ctx, op.id)
			return value, true, err
		case "delete":
			return nil, false, api.DeleteVolume(ctx, op.id)
		}
	}
	return nil, false, errors.New("unsupported client operation")
}

var resourceID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func parseCommand(args []string, output io.Writer, vmWaitTimeout string) (command, error) {
	op := command{}
	name := args[0]
	rest := args[1:]
	switch name {
	case "health", "ready", "quota":
		fs := flagSet(name, output, "Usage: hvst-runner-gw-client [global flags] "+name)
		if err := fs.Parse(rest); err != nil {
			return op, err
		}
		if fs.NArg() != 0 {
			return op, fmt.Errorf("%s takes no arguments", name)
		}
		op.kind = name
		return op, nil
	case "vm", "volume":
	default:
		return op, fmt.Errorf("unknown command %q; use --help", name)
	}
	if len(rest) == 0 || rest[0] == "--help" || rest[0] == "-h" {
		fmt.Fprint(output, rootHelp)
		if len(rest) == 0 {
			return op, fmt.Errorf("%s requires a subcommand", name)
		}
		return op, flag.ErrHelp
	}
	action := rest[0]
	rest = rest[1:]
	if action == "create" {
		return parseCreate(name, rest, output, vmWaitTimeout)
	}
	expected := 0
	switch action {
	case "list":
	case "get", "delete":
		expected = 1
	case "power", "reboot", "attach", "detach":
		if name != "vm" {
			return op, fmt.Errorf("unknown volume subcommand %q", action)
		}
		expected = 1
		if action != "reboot" {
			expected = 2
		}
	default:
		return op, fmt.Errorf("unknown %s subcommand %q", name, action)
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
	fs := flagSet(name+" "+action, output, "Usage: hvst-runner-gw-client [global flags] "+name+" "+action+suffix)
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
		return op, fmt.Errorf("%s %s requires %d argument(s)", name, action, expected)
	}
	for i, arg := range fs.Args() {
		if action == "power" && i == 1 {
			continue
		}
		if !resourceID.MatchString(arg) {
			return op, errors.New("resource IDs must start with an alphanumeric character and contain only letters, digits, '.', '_', or '-'")
		}
	}
	op.kind, op.action = name, action
	if expected > 0 {
		op.id = fs.Arg(0)
	}
	if expected > 1 {
		op.second = fs.Arg(1)
	}
	if action == "power" && op.second != "on" && op.second != "off" {
		return op, errors.New("power state must be on or off")
	}
	return op, nil
}

type stringList []string

func (s *stringList) String() string         { return strings.Join(*s, ", ") }
func (s *stringList) Set(value string) error { *s = append(*s, value); return nil }

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
	op := command{kind: kind, action: "create"}
	fs := flagSet(kind+" create", output, "Usage: hvst-runner-gw-client [global flags] "+kind+" create [flags]\nAll size quantities use Kubernetes notation, e.g. 4Gi. VM creation waits for a running VMI with a usable IP unless --no-wait is set.")
	ttl := fs.Int("ttl-seconds", 0, "resource lifetime, 1-86400 seconds (omitted: server default)")
	var keys stringList
	var userDataFile, waitTimeout string
	var noWait bool
	if kind == "vm" {
		fs.StringVar(&op.vmRequest.Image, "image", "", "required: approved namespace/name image")
		fs.StringVar(&op.vmRequest.Network, "network", "", "required: approved namespace/name network")
		fs.IntVar(&op.vmRequest.CPU, "cpu", 0, "required: positive CPU count")
		fs.StringVar(&op.vmRequest.Memory, "memory", "", "required: memory quantity")
		fs.StringVar(&op.vmRequest.BootDiskSize, "boot-disk-size", "", "required: boot disk quantity")
		fs.Var(&keys, "ssh-public-key-file", "SSH public key file; repeat for multiple keys (maximum 10)")
		fs.StringVar(&userDataFile, "user-data-file", "", "cloud-config file (maximum 64 KiB)")
		fs.StringVar(&waitTimeout, "wait-timeout", defaultWaitTimeout, "time to wait for VM readiness (GATEWAY_VM_WAIT_TIMEOUT)")
		fs.BoolVar(&noWait, "no-wait", false, "return after VM creation without waiting for readiness")
	} else {
		fs.StringVar(&op.volumeRequest.Size, "size", "", "required: volume size quantity")
	}
	if err := fs.Parse(args); err != nil {
		return op, err
	}
	if fs.NArg() != 0 {
		return op, errors.New("create accepts flags only")
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
		op.vmRequest.TTLSeconds = ttl
		op.volumeRequest.TTLSeconds = ttl
	}
	if kind == "volume" {
		if !positiveQuantity(op.volumeRequest.Size) {
			return op, errors.New("--size requires a positive Kubernetes quantity")
		}
		return op, nil
	}
	duration, err := time.ParseDuration(waitTimeout)
	if err != nil || duration <= 0 {
		return op, errors.New("--wait-timeout must be a positive duration, such as 5m")
	}
	op.waitForVM = !noWait
	op.waitTimeout = duration
	for _, item := range []struct{ name, value string }{{"image", op.vmRequest.Image}, {"network", op.vmRequest.Network}} {
		parts := strings.Split(item.value, "/")
		if len(parts) != 2 || !resourceID.MatchString(parts[0]) || !resourceID.MatchString(parts[1]) {
			return op, fmt.Errorf("--%s requires namespace/name", item.name)
		}
	}
	if op.vmRequest.CPU < 1 {
		return op, errors.New("--cpu must be positive")
	}
	if !positiveQuantity(op.vmRequest.Memory) || !positiveQuantity(op.vmRequest.BootDiskSize) {
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
		op.vmRequest.SSHPublicKeys = append(op.vmRequest.SSHPublicKeys, key)
	}
	if userDataFile != "" {
		data, err := readFile(userDataFile, 64*1024)
		if err != nil {
			return op, fmt.Errorf("user data file: %w", err)
		}
		if !strings.HasPrefix(string(data), "#cloud-config") {
			return op, errors.New("user data must start with #cloud-config")
		}
		op.vmRequest.UserData = string(data)
	}
	return op, nil
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
