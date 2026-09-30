package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

var guestUserPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]*[$]?$`)

const (
	DefaultIssuer       = "https://token.actions.githubusercontent.com"
	DefaultAudience     = "api://harvester-runner-gateway"
	DefaultVMPrefix     = "ci-vm-"
	DefaultVolumePrefix = "ci-vol-"
	DefaultTTL          = 6 * time.Hour
	MaximumTTL          = 24 * time.Hour
)

type Config struct {
	ListenAddress string             `json:"listenAddress"`
	TLS           TLSConfig          `json:"tls"`
	Kubeconfig    string             `json:"kubeconfig"`
	KubeContext   string             `json:"kubeContext"`
	Database      DatabaseConfig     `json:"database"`
	VMPrefix      string             `json:"vmPrefix"`
	VolumePrefix  string             `json:"volumePrefix"`
	OIDC          OIDCConfig         `json:"oidc"`
	Developers    []DeveloperConfig  `json:"developers"`
	LocalSmoke    LocalSmokeConfig   `json:"localSmoke"`
	Repositories  []RepositoryPolicy `json:"repositories"`
}

type DatabaseConfig struct {
	Path string `json:"path"`
}

type IDPrefixes struct {
	VM     string
	Volume string
}

func (c Config) IDPrefixes() IDPrefixes {
	vm, volume := c.VMPrefix, c.VolumePrefix
	if vm == "" {
		vm = DefaultVMPrefix
	}
	if volume == "" {
		volume = DefaultVolumePrefix
	}
	return IDPrefixes{VM: vm, Volume: volume}
}

func (p IDPrefixes) ForKind(kind string) string {
	switch kind {
	case "vm":
		return p.VM
	case "volume":
		return p.Volume
	default:
		return ""
	}
}

type TLSConfig struct {
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
}

type OIDCConfig struct {
	Issuer   string `json:"issuer"`
	Audience string `json:"audience"`
}

type DeveloperConfig struct {
	ID           string `json:"id"`
	RepositoryID string `json:"repositoryID"`
	TokenFile    string `json:"tokenFile"`
}

var developerIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func ValidDeveloperID(id string) bool {
	return len(id) <= 59 && developerIDPattern.MatchString(id)
}

func (c Config) ValidateDevelopers() error {
	seen := map[string]bool{}
	for _, d := range c.Developers {
		if !ValidDeveloperID(d.ID) || seen[d.ID] {
			return fmt.Errorf("developers: ID must be unique, lowercase DNS-safe, and at most 59 characters")
		}
		seen[d.ID] = true
		if !filepath.IsAbs(d.TokenFile) {
			return fmt.Errorf("developer %s: tokenFile must be absolute", d.ID)
		}
		if _, ok := c.Repository(d.RepositoryID); !ok {
			return fmt.Errorf("developer %s: repositoryID must match a configured repository", d.ID)
		}
	}
	return nil
}

type LocalSmokeConfig struct {
	RepositoryID string `json:"repositoryID"`
	TokenFile    string `json:"tokenFile"`
}

type QuotaPolicy struct {
	MaxActiveVMs     int `json:"maxActiveVMs"`
	MaxActiveVolumes int `json:"maxActiveVolumes"`
}

type RepositoryPolicy struct {
	RepositoryID        string      `json:"repositoryID"`
	Namespace           string      `json:"namespace"`
	AllowedWorkflowRefs []string    `json:"allowedWorkflowRefs"`
	AllowedEvents       []string    `json:"allowedEvents"`
	Images              []string    `json:"images"`
	Networks            []string    `json:"networks"`
	StorageClass        string      `json:"storageClass"`
	DefaultUser         string      `json:"defaultUser"`
	MaxCPU              int         `json:"maxCPU"`
	MaxMemory           string      `json:"maxMemory"`
	MaxBootDiskSize     string      `json:"maxBootDiskSize"`
	MaxVolumeSize       string      `json:"maxVolumeSize"`
	Quota               QuotaPolicy `json:"quota"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.UnmarshalStrict(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	prefixes := c.IDPrefixes()
	c.VMPrefix, c.VolumePrefix = prefixes.VM, prefixes.Volume
	if c.VMPrefix == c.VolumePrefix {
		return fmt.Errorf("vmPrefix and volumePrefix must differ")
	}
	for name, prefix := range map[string]string{"vmPrefix": c.VMPrefix, "volumePrefix": c.VolumePrefix} {
		dependentSuffix := 0
		if name == "vmPrefix" {
			dependentSuffix = len("-root")
		}
		if !strings.HasSuffix(prefix, "-") || len(validation.IsDNS1123Label(strings.TrimSuffix(prefix, "-"))) != 0 ||
			len(prefix)+16+dependentSuffix > 63 {
			return fmt.Errorf("%s must be a lowercase DNS-safe prefix ending in '-' and leave room for a resource ID", name)
		}
	}
	if c.ListenAddress == "" {
		c.ListenAddress = ":8443"
	}
	if !filepath.IsAbs(c.Database.Path) {
		return fmt.Errorf("database.path must be an absolute filesystem path")
	}
	if c.TLS.CertFile == "" || c.TLS.KeyFile == "" || c.Kubeconfig == "" {
		return fmt.Errorf("tls.certFile, tls.keyFile, and kubeconfig are required")
	}
	if c.OIDC.Issuer == "" {
		c.OIDC.Issuer = DefaultIssuer
	}
	if c.OIDC.Audience == "" {
		c.OIDC.Audience = DefaultAudience
	}
	if !strings.HasPrefix(c.OIDC.Issuer, "https://") || c.OIDC.Audience == "" {
		return fmt.Errorf("OIDC issuer must be HTTPS and audience must be set")
	}
	if len(c.Repositories) == 0 {
		return fmt.Errorf("at least one repository policy is required")
	}
	seen := map[string]bool{}
	for i := range c.Repositories {
		p := &c.Repositories[i]
		if _, err := strconv.ParseUint(p.RepositoryID, 10, 64); err != nil || seen[p.RepositoryID] {
			return fmt.Errorf("repository ID must be nonempty and unique")
		}
		seen[p.RepositoryID] = true
		if errs := validation.IsDNS1123Label(p.Namespace); len(errs) > 0 {
			return fmt.Errorf("repository %s: invalid namespace: %s", p.RepositoryID, strings.Join(errs, ", "))
		}
		if len(p.AllowedWorkflowRefs) == 0 || len(p.AllowedEvents) == 0 || len(p.Images) == 0 || len(p.Networks) == 0 {
			return fmt.Errorf("repository %s: workflow refs, events, images, and networks are required", p.RepositoryID)
		}
		for _, item := range append(append([]string{}, p.AllowedWorkflowRefs...), p.AllowedEvents...) {
			if strings.TrimSpace(item) != item || item == "" {
				return fmt.Errorf("repository %s: workflow refs and events must be nonempty exact values", p.RepositoryID)
			}
		}
		if p.StorageClass == "" || !guestUserPattern.MatchString(p.DefaultUser) {
			return fmt.Errorf("repository %s: storageClass and defaultUser are required", p.RepositoryID)
		}
		for _, name := range append(append([]string{}, p.Images...), p.Networks...) {
			parts := strings.Split(name, "/")
			if len(parts) != 2 || len(validation.IsDNS1123Label(parts[0])) > 0 || len(validation.IsDNS1123Subdomain(parts[1])) > 0 {
				return fmt.Errorf("repository %s: image/network %q must be namespace/name", p.RepositoryID, name)
			}
		}
		if p.Quota.MaxActiveVMs < 0 || p.Quota.MaxActiveVolumes < 0 || p.MaxCPU < 1 {
			return fmt.Errorf("repository %s: quota values must be nonnegative and maxCPU positive", p.RepositoryID)
		}
		for name, value := range map[string]string{"maxMemory": p.MaxMemory, "maxBootDiskSize": p.MaxBootDiskSize, "maxVolumeSize": p.MaxVolumeSize} {
			quantity, err := resource.ParseQuantity(value)
			if err != nil || quantity.Sign() <= 0 {
				return fmt.Errorf("repository %s: invalid %s %q", p.RepositoryID, name, value)
			}
		}
	}
	if c.LocalSmoke.RepositoryID != "" || c.LocalSmoke.TokenFile != "" {
		if c.LocalSmoke.RepositoryID == "" || c.LocalSmoke.TokenFile == "" {
			return fmt.Errorf("localSmoke.repositoryID and localSmoke.tokenFile must be set together")
		}
		if _, ok := c.Repository(c.LocalSmoke.RepositoryID); !ok {
			return fmt.Errorf("localSmoke.repositoryID must match a configured repository")
		}
	}
	return c.ValidateDevelopers()
}

func (c Config) Repository(id string) (RepositoryPolicy, bool) {
	for _, p := range c.Repositories {
		if p.RepositoryID == id {
			return p, true
		}
	}
	return RepositoryPolicy{}, false
}
