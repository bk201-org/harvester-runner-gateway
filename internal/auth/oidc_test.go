package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if err != nil || owner.RepositoryID != "123" || owner.RunID != "456" || owner.RunAttempt != "2" || owner.WorkflowRef != policy.AllowedWorkflowRefs[0] {
		t.Fatalf("valid token rejected: owner=%+v err=%v", owner, err)
	}
	for _, test := range []struct {
		name, claim, reason string
		value               any
	}{
		{"wrong issuer", "iss", "issuer_mismatch", "https://other.example"},
		{"wrong audience", "aud", "audience_mismatch", "other"},
		{"missing time claim", "nbf", "missing_time_claim", nil},
		{"wrong repository", "repository_id", "repository_not_allowed", "999"},
		{"hosted runner", "runner_environment", "runner_not_self_hosted", "github-hosted"},
		{"wrong workflow", "workflow_ref", "workflow_not_allowed", "org/repo/.github/workflows/other.yml@refs/heads/main"},
		{"wrong event", "event_name", "event_not_allowed", "pull_request"},
		{"invalid run ID", "run_id", "invalid_run_identity", "invalid"},
		{"expired", "exp", "invalid_token", time.Now().Add(-time.Minute).Unix()},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := jwt.MapClaims{}
			for key, value := range claims {
				changed[key] = value
			}
			if test.value == nil {
				delete(changed, test.claim)
			} else {
				changed[test.claim] = test.value
			}
			_, _, err := verifier.Verify(context.Background(), sign(changed), cfg)
			if !errors.Is(err, ErrUnauthorized) || RejectionReason(err) != test.reason {
				t.Fatalf("rejection = %v, reason = %q; want %q", err, RejectionReason(err), test.reason)
			}
		})
	}
	if _, _, err := verifier.Verify(context.Background(), "not-a-jwt", cfg); !errors.Is(err, ErrUnauthorized) || RejectionReason(err) != "invalid_token" {
		t.Fatalf("malformed token rejection = %v, reason = %q", err, RejectionReason(err))
	}
}

func TestRejectionMessage(t *testing.T) {
	for _, test := range []struct{ reason, hint string }{
		{"repository_not_allowed", "repositories[].repositoryID"},
		{"workflow_not_allowed", "allowedWorkflowRefs"},
		{"event_not_allowed", "allowedEvents"},
		{"runner_not_self_hosted", "self-hosted runner"},
		{"audience_mismatch", "action audience input"},
		{"issuer_mismatch", "OIDC issuer"},
		{"invalid_token", "signing-key access"},
		{"missing_time_claim", "time claims"},
		{"invalid_run_identity", "workflow run identity"},
	} {
		t.Run(test.reason, func(t *testing.T) {
			message := RejectionMessage(reject(test.reason))
			if !strings.Contains(message, "("+test.reason+")") || !strings.Contains(message, test.hint) {
				t.Fatalf("message = %q; want reason %q and hint %q", message, test.reason, test.hint)
			}
		})
	}
	for _, err := range []error{errors.New("provider-secret"), reject("unknown-secret")} {
		message := RejectionMessage(err)
		if strings.Contains(message, "secret") || !strings.Contains(message, "verification_failed") {
			t.Fatalf("unsafe fallback message: %q", message)
		}
	}
}
