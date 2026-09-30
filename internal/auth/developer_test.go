package auth

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bk201-org/harvester-runner-gateway/internal/config"
)

func TestDeveloperIdentityRotationRevocationAndFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	first, second := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	if err := os.WriteFile(path, []byte(first), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Repositories: []config.RepositoryPolicy{{RepositoryID: "123"}}, Developers: []config.DeveloperConfig{{ID: "alice", RepositoryID: "123", TokenFile: path}}}
	old, err := NewDeveloperVerifier(smokeFallback{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := old.Verify(context.Background(), first, cfg)
	if err != nil || owner.RunID != "dev-alice" || owner.RunAttempt != "1" || owner.WorkflowRef != "" {
		t.Fatalf("owner=%+v error=%v", owner, err)
	}
	if err := os.WriteFile(path, []byte(second), 0600); err != nil {
		t.Fatal(err)
	}
	next, err := NewDeveloperVerifier(smokeFallback{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	rotated, _, err := next.Verify(context.Background(), second, cfg)
	if err != nil || rotated != owner {
		t.Fatalf("rotation changed identity: %+v %v", rotated, err)
	}
	if _, _, err := next.Verify(context.Background(), first, cfg); err == nil {
		t.Fatal("old token still authorized after restart")
	}
	if _, _, err := old.Verify(context.Background(), first, cfg); err != nil {
		t.Fatal("token changed without restart")
	}
	if owner, _, err := next.Verify(context.Background(), "oidc-token", cfg); err != nil || owner.RunID != "456" {
		t.Fatalf("OIDC fallback: %+v %v", owner, err)
	}
	cfg.Developers = nil
	revoked, err := NewDeveloperVerifier(smokeFallback{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := revoked.Verify(context.Background(), second, cfg); err == nil {
		t.Fatal("removed token authorized")
	}
}

func TestDeveloperRejectsDuplicateAndUnsafeCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(strings.Repeat("ab", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Repositories: []config.RepositoryPolicy{{RepositoryID: "123"}}, Developers: []config.DeveloperConfig{{ID: "alice", RepositoryID: "123", TokenFile: path}, {ID: "bob", RepositoryID: "123", TokenFile: path}}}
	if _, err := NewDeveloperVerifier(smokeFallback{}, cfg); err == nil {
		t.Fatal("duplicate tokens accepted")
	}
	cfg.Developers = cfg.Developers[:1]
	cfg.LocalSmoke = config.LocalSmokeConfig{RepositoryID: "123", TokenFile: path}
	if _, err := NewDeveloperVerifier(smokeFallback{}, cfg); err == nil {
		t.Fatal("smoke token reuse accepted")
	}
	cfg.LocalSmoke = config.LocalSmokeConfig{}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDeveloperVerifier(smokeFallback{}, cfg); err == nil {
		t.Fatal("publicly readable token accepted")
	}
}
