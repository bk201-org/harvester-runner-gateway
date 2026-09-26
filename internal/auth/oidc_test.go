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
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"

	"github.com/bk201-org/harvester-runner-gateway/internal/config"
)

func TestUnknownKeyIDRefreshIsRateLimited(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	var issuer string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": issuer + "/keys"})
		case "/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
				"kid": "known", "kty": "RSA", "alg": "RS256", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(privateKey.PublicKey.E)).Bytes()),
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	issuer = server.URL
	verifier := NewVerifier(issuer, "audience")
	verifier.client = server.Client()
	if _, err := verifier.key(context.Background(), "missing-one"); err == nil {
		t.Fatal("unknown key ID was accepted")
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("initial refresh made %d requests, want 2", got)
	}
	for _, kid := range []string{"missing-two", "missing-three"} {
		if _, err := verifier.key(context.Background(), kid); err == nil {
			t.Fatal("unknown key ID was accepted")
		}
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("unknown key IDs bypassed refresh cooldown: got %d requests, want 2", got)
	}
}

func TestCachedKeyLookupDoesNotWaitForRefresh(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var issuer string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			close(refreshStarted)
			<-releaseRefresh
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": issuer + "/keys"})
		case "/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
				"kid": "refreshed", "kty": "RSA", "alg": "RS256", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(privateKey.PublicKey.E)).Bytes()),
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	issuer = server.URL
	verifier := NewVerifier(issuer, "audience")
	verifier.client = server.Client()
	verifier.keys = map[string]*rsa.PublicKey{"cached": &privateKey.PublicKey}
	verifier.until = time.Now().Add(time.Hour)
	verifier.lastRefreshAttempt = time.Now().Add(-keyRefreshCooldown)

	refreshDone := make(chan error, 1)
	go func() {
		_, err := verifier.key(context.Background(), "unknown")
		refreshDone <- err
	}()
	<-refreshStarted

	cachedDone := make(chan error, 1)
	go func() {
		_, err := verifier.key(context.Background(), "cached")
		cachedDone <- err
	}()
	select {
	case err := <-cachedDone:
		close(releaseRefresh)
		if err != nil {
			t.Fatalf("cached key lookup failed: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		close(releaseRefresh)
		<-refreshDone
		t.Fatal("cached key lookup blocked behind network refresh")
	}
	if err := <-refreshDone; err == nil {
		t.Fatal("unknown key ID was accepted")
	}
}

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
