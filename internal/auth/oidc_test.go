package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"

	"github.com/bk201-org/harvester-runner-gateway/internal/config"
)

func TestVerifyGitHubOIDCClaims(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var issuer string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": issuer + "/keys"})
		case "/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
				"kid": "test-key", "kty": "RSA", "alg": "RS256", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(privateKey.PublicKey.E)).Bytes()),
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	issuer = server.URL
	verifier := NewVerifier(issuer, "api://gateway")
	verifier.client = server.Client()
	policy := config.RepositoryPolicy{RepositoryID: "123", Namespace: "ci",
		AllowedWorkflowRefs: []string{"org/repo/.github/workflows/test.yml@refs/heads/main"},
		AllowedEvents:       []string{"workflow_dispatch"}}
	cfg := config.Config{Repositories: []config.RepositoryPolicy{policy}}
	claims := jwt.MapClaims{
		"iss": issuer, "aud": "api://gateway", "exp": time.Now().Add(5 * time.Minute).Unix(),
		"iat": time.Now().Add(-time.Minute).Unix(), "nbf": time.Now().Add(-time.Minute).Unix(),
		"repository_id": "123", "run_id": "456", "run_attempt": "2",
		"runner_environment": "self-hosted", "workflow_ref": policy.AllowedWorkflowRefs[0],
		"event_name": "workflow_dispatch",
	}
	sign := func(values jwt.MapClaims) string {
		t.Helper()
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, values)
		token.Header["kid"] = "test-key"
		text, err := token.SignedString(privateKey)
		if err != nil {
			t.Fatal(err)
		}
		return text
	}
	owner, _, err := verifier.Verify(context.Background(), sign(claims), cfg)
	if err != nil || owner.RepositoryID != "123" || owner.RunID != "456" || owner.RunAttempt != "2" {
		t.Fatalf("valid token rejected: owner=%+v err=%v", owner, err)
	}
	for _, test := range []struct {
		name  string
		claim string
		value any
	}{
		{"wrong audience", "aud", "other"},
		{"wrong repository", "repository_id", "999"},
		{"hosted runner", "runner_environment", "github-hosted"},
		{"wrong workflow", "workflow_ref", "org/repo/.github/workflows/other.yml@refs/heads/main"},
		{"wrong event", "event_name", "pull_request"},
		{"expired", "exp", time.Now().Add(-time.Minute).Unix()},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := jwt.MapClaims{}
			for key, value := range claims {
				changed[key] = value
			}
			changed[test.claim] = test.value
			if _, _, err := verifier.Verify(context.Background(), sign(changed), cfg); err == nil {
				t.Fatal("expected token rejection")
			}
		})
	}
}
