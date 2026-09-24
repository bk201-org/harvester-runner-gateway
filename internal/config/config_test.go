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
