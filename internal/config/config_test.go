package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestExampleConfigurationLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OIDC.Issuer != DefaultIssuer || cfg.OIDC.Audience != DefaultAudience {
		t.Fatalf("unexpected OIDC defaults: %+v", cfg.OIDC)
	}
	if len(cfg.Repositories) != 1 || cfg.Repositories[0].Quota.MaxActiveVMs != 3 || cfg.Repositories[0].Quota.MaxActiveVolumes != 4 {
		t.Fatalf("unexpected example quota: %+v", cfg.Repositories)
	}
}

func TestLocalSmokeConfiguration(t *testing.T) {
	base, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		localSmoke LocalSmokeConfig
		wantError  bool
	}{
		{"disabled", LocalSmokeConfig{}, false},
		{"configured", LocalSmokeConfig{RepositoryID: base.Repositories[0].RepositoryID, TokenFile: "/secure/token"}, false},
		{"missing token file", LocalSmokeConfig{RepositoryID: base.Repositories[0].RepositoryID}, true},
		{"missing repository", LocalSmokeConfig{TokenFile: "/secure/token"}, true},
		{"unknown repository", LocalSmokeConfig{RepositoryID: "999", TokenFile: "/secure/token"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.LocalSmoke = tc.localSmoke
			err := cfg.Validate()
			if (err != nil) != tc.wantError {
				t.Fatalf("Validate() error = %v, want error %t", err, tc.wantError)
			}
		})
	}
}

func TestDatabasePathMustBeAbsolute(t *testing.T) {
	base, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "allocations.sqlite"} {
		cfg := base
		cfg.Database.Path = path
		if err := cfg.Validate(); err == nil {
			t.Errorf("accepted database path %q", path)
		}
	}
}

func TestIDPrefixDefaultsAndValidation(t *testing.T) {
	base, err := Load(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if base.VMPrefix != DefaultVMPrefix || base.VolumePrefix != DefaultVolumePrefix {
		t.Fatalf("example prefixes: %q, %q", base.VMPrefix, base.VolumePrefix)
	}
	for _, tc := range []struct {
		name, vm, volume string
		wantError        bool
	}{
		{"defaults", "", "", false},
		{"custom", "build-vm-", "build-vol-", false},
		{"same", "ci-", "ci-", true},
		{"uppercase", "CI-vm-", "ci-vol-", true},
		{"missing dash", "ci-vm", "ci-vol-", true},
		{"dot", "ci.vm-", "ci-vol-", true},
		{"too long", strings.Repeat("a", 42) + "-", "ci-vol-", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.VMPrefix, cfg.VolumePrefix = tc.vm, tc.volume
			err := cfg.Validate()
			if (err != nil) != tc.wantError {
				t.Fatalf("Validate() = %v, want error %t", err, tc.wantError)
			}
			if err == nil {
				p := cfg.IDPrefixes()
				if p.VM == "" || p.Volume == "" {
					t.Fatalf("missing prefixes: %+v", p)
				}
			}
		})
	}
}
