package auth

import (
	"context"
	"crypto/subtle"
	"fmt"

	"github.com/bk201-org/harvester-runner-gateway/internal/config"
)

type developerCredential struct {
	token  []byte
	owner  Owner
	policy config.RepositoryPolicy
}

type developerVerifier struct {
	fallback    TokenVerifier
	credentials []developerCredential
}

// NewDeveloperVerifier snapshots credentials at startup. Identity survives token
// rotation, and removing a credential does not prevent TTL cleanup or recovery.
func NewDeveloperVerifier(fallback TokenVerifier, cfg config.Config) (TokenVerifier, error) {
	if err := cfg.ValidateDevelopers(); err != nil {
		return nil, err
	}
	if len(cfg.Developers) == 0 {
		return fallback, nil
	}
	v := &developerVerifier{fallback: fallback}
	var smoke []byte
	if cfg.LocalSmoke.TokenFile != "" {
		var err error
		smoke, err = readPrivateToken(cfg.LocalSmoke.TokenFile)
		if err != nil {
			return nil, err
		}
	}
	for _, d := range cfg.Developers {
		token, err := readPrivateToken(d.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("developer %s: %w", d.ID, err)
		}
		if subtle.ConstantTimeCompare(token, smoke) == 1 {
			return nil, fmt.Errorf("developer %s: token duplicates localSmoke credential", d.ID)
		}
		for _, existing := range v.credentials {
			if subtle.ConstantTimeCompare(token, existing.token) == 1 {
				return nil, fmt.Errorf("developer %s: duplicate credential", d.ID)
			}
		}
		policy, _ := cfg.Repository(d.RepositoryID)
		v.credentials = append(v.credentials, developerCredential{token: token, policy: policy,
			owner: Owner{RepositoryID: d.RepositoryID, RunID: "dev-" + d.ID, RunAttempt: "1"}})
	}
	return v, nil
}

func (v *developerVerifier) Verify(ctx context.Context, raw string, cfg config.Config) (Owner, config.RepositoryPolicy, error) {
	found := -1
	for i, credential := range v.credentials {
		if subtle.ConstantTimeCompare([]byte(raw), credential.token) == 1 {
			found = i
		}
	}
	if found >= 0 {
		credential := v.credentials[found]
		return credential.owner, credential.policy, nil
	}
	return v.fallback.Verify(ctx, raw, cfg)
}
