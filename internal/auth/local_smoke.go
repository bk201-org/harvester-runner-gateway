package auth

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bk201-org/harvester-runner-gateway/internal/config"
)

// TokenVerifier is shared by GitHub OIDC and the optional local smoke credential.
type TokenVerifier interface {
	Verify(context.Context, string, config.Config) (Owner, config.RepositoryPolicy, error)
}

type localSmokeVerifier struct {
	fallback TokenVerifier
	token    []byte
	policy   config.RepositoryPolicy
}

// NewLocalSmokeVerifier leaves OIDC as the only credential unless localSmoke is configured.
func NewLocalSmokeVerifier(fallback TokenVerifier, cfg config.Config) (TokenVerifier, error) {
	local := cfg.LocalSmoke
	if local.RepositoryID == "" && local.TokenFile == "" {
		return fallback, nil
	}
	if local.RepositoryID == "" || local.TokenFile == "" {
		return nil, fmt.Errorf("localSmoke.repositoryID and localSmoke.tokenFile must be set together")
	}
	policy, ok := cfg.Repository(local.RepositoryID)
	if !ok {
		return nil, fmt.Errorf("localSmoke.repositoryID must match a configured repository")
	}
	file, err := os.Open(local.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("open local smoke token file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat local smoke token file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 64 || info.Size() > 65 {
		return nil, fmt.Errorf("local smoke token file must be a private regular file containing 64 hex characters")
	}
	data, err := io.ReadAll(io.LimitReader(file, 66))
	if err != nil {
		return nil, fmt.Errorf("read local smoke token file: %w", err)
	}
	token := strings.TrimSuffix(string(data), "\n")
	if len(token) != 64 {
		return nil, fmt.Errorf("local smoke token must contain 64 hex characters")
	}
	if _, err := hex.DecodeString(token); err != nil {
		return nil, fmt.Errorf("local smoke token must contain 64 hex characters: %w", err)
	}
	return &localSmokeVerifier{fallback: fallback, token: []byte(token), policy: policy}, nil
}

func (v *localSmokeVerifier) Verify(ctx context.Context, raw string, cfg config.Config) (Owner, config.RepositoryPolicy, error) {
	if subtle.ConstantTimeCompare([]byte(raw), v.token) == 1 {
		return Owner{RepositoryID: v.policy.RepositoryID, RunID: "local-smoke", RunAttempt: "1"}, v.policy, nil
	}
	return v.fallback.Verify(ctx, raw, cfg)
}
