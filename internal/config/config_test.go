package config

import (
	"path/filepath"
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
	if len(cfg.Repositories) != 1 || cfg.Repositories[0].Quota.MaxActiveVMs != 2 || cfg.Repositories[0].Quota.MaxActiveVolumes != 4 {
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
