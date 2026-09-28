package auth

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bk201/harvester-runner-gateway/internal/config"
)

type smokeFallback struct{}

func (smokeFallback) Verify(_ context.Context, raw string, cfg config.Config) (Owner, config.RepositoryPolicy, error) {
	if raw == "oidc-token" {
		return Owner{RepositoryID: "123", RunID: "456", RunAttempt: "2"}, cfg.Repositories[0], nil
	}
	return Owner{}, config.RepositoryPolicy{}, ErrUnauthorized
}

func TestLocalSmokeVerifier(t *testing.T) {
	cfg := config.Config{Repositories: []config.RepositoryPolicy{{RepositoryID: "123"}, {RepositoryID: "789"}}}
	fallback := smokeFallback{}
	disabled, err := NewLocalSmokeVerifier(fallback, cfg)
	if err != nil || disabled != fallback {
		t.Fatalf("disabled local auth changed OIDC verifier: verifier=%T error=%v", disabled, err)
	}

	token := strings.Repeat("ab", 32)
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.LocalSmoke = config.LocalSmokeConfig{RepositoryID: "789", TokenFile: path}
	verifier, err := NewLocalSmokeVerifier(fallback, cfg)
	if err != nil {
		t.Fatal(err)
	}
	owner, policy, err := verifier.Verify(context.Background(), token, cfg)
	if err != nil || owner != (Owner{RepositoryID: "789", RunID: "local-smoke", RunAttempt: "1"}) || policy.RepositoryID != "789" {
		t.Fatalf("local token: owner=%+v policy=%+v error=%v", owner, policy, err)
	}
	owner, policy, err = verifier.Verify(context.Background(), "oidc-token", cfg)
	if err != nil || owner.RunID != "456" || policy.RepositoryID != "123" {
		t.Fatalf("OIDC fallback: owner=%+v policy=%+v error=%v", owner, policy, err)
	}
	if _, _, err := verifier.Verify(context.Background(), strings.Repeat("cd", 32), cfg); err == nil {
		t.Fatal("wrong local token accepted")
	}
}

func TestLocalSmokeVerifierRejectsUnsafeTokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	cfg := config.Config{Repositories: []config.RepositoryPolicy{{RepositoryID: "123"}},
		LocalSmoke: config.LocalSmokeConfig{RepositoryID: "123", TokenFile: path}}
	for _, tc := range []struct {
		name  string
		value string
		mode  os.FileMode
	}{
		{"short", "abcd\n", 0600},
		{"non-hex", strings.Repeat("zz", 32) + "\n", 0600},
		{"world-readable", strings.Repeat("ab", 32) + "\n", 0644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.value), tc.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := NewLocalSmokeVerifier(smokeFallback{}, cfg); err == nil {
				t.Fatal("unsafe local token file accepted")
			}
		})
	}
}
