package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v4"

	"github.com/bk201-org/harvester-runner-gateway/internal/config"
)

var ErrUnauthorized = errors.New("unauthorized")

type rejectionError struct{ reason string }

func (e rejectionError) Error() string { return ErrUnauthorized.Error() }
func (e rejectionError) Unwrap() error { return ErrUnauthorized }

// RejectionReason returns a fixed diagnostic code without exposing token contents.
func RejectionReason(err error) string {
	var rejected rejectionError
	if errors.As(err, &rejected) {
		return rejected.reason
	}
	return "verification_failed"
}

func reject(reason string) error { return rejectionError{reason: reason} }

type Owner struct {
	RepositoryID string
	RunID        string
	RunAttempt   string
	WorkflowRef  string
}

type Verifier struct {
	issuer             string
	audience           string
	client             *http.Client
	mu                 sync.RWMutex
	refreshMu          sync.Mutex
	keys               map[string]*rsa.PublicKey
	until              time.Time
	lastRefreshAttempt time.Time
}

const (
	keyCacheTTL           = time.Hour
	keyRefreshCooldown    = time.Minute
	failedRefreshCooldown = 5 * time.Second
)

func NewVerifier(issuer, audience string) *Verifier {
	return &Verifier{issuer: strings.TrimSuffix(issuer, "/"), audience: audience,
		client: &http.Client{Timeout: 10 * time.Second}}
}

func (v *Verifier) Verify(ctx context.Context, raw string, cfg config.Config) (Owner, config.RepositoryPolicy, error) {
	if len(raw) == 0 || len(raw) > 16*1024 {
		return Owner{}, config.RepositoryPolicy{}, reject("invalid_token")
	}
	claims := jwt.MapClaims{}
	parsed, err := (&jwt.Parser{ValidMethods: []string{jwt.SigningMethodRS256.Alg()}}).ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		kid, ok := token.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, ErrUnauthorized
		}
		return v.key(ctx, kid)
	})
	if err != nil || !parsed.Valid {
		return Owner{}, config.RepositoryPolicy{}, reject("invalid_token")
	}
	if !claims.VerifyIssuer(v.issuer, true) {
		return Owner{}, config.RepositoryPolicy{}, reject("issuer_mismatch")
	}
	if !claims.VerifyAudience(v.audience, true) {
		return Owner{}, config.RepositoryPolicy{}, reject("audience_mismatch")
	}
	if !presentNumeric(claims["exp"]) || !presentNumeric(claims["iat"]) || !presentNumeric(claims["nbf"]) {
		return Owner{}, config.RepositoryPolicy{}, reject("missing_time_claim")
	}
	repoID := claimString(claims["repository_id"])
	policy, ok := cfg.Repository(repoID)
	if !ok {
		return Owner{}, config.RepositoryPolicy{}, reject("repository_not_allowed")
	}
	if claimString(claims["runner_environment"]) != "self-hosted" {
		return Owner{}, config.RepositoryPolicy{}, reject("runner_not_self_hosted")
	}
	if !contains(policy.AllowedWorkflowRefs, claimString(claims["workflow_ref"])) {
		return Owner{}, config.RepositoryPolicy{}, reject("workflow_not_allowed")
	}
	if !contains(policy.AllowedEvents, claimString(claims["event_name"])) {
		return Owner{}, config.RepositoryPolicy{}, reject("event_not_allowed")
	}
	owner := Owner{RepositoryID: repoID, RunID: claimString(claims["run_id"]), RunAttempt: claimString(claims["run_attempt"]), WorkflowRef: claimString(claims["workflow_ref"])}
	if !decimal(owner.RepositoryID) || !decimal(owner.RunID) || !decimal(owner.RunAttempt) {
		return Owner{}, config.RepositoryPolicy{}, reject("invalid_run_identity")
	}
	return owner, policy, nil
}

func (v *Verifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if key, fresh, _ := v.cachedKey(kid, time.Now()); key != nil && fresh {
		return key, nil
	}

	v.refreshMu.Lock()
	defer v.refreshMu.Unlock()

	now := time.Now()
	if key, fresh, recentlyAttempted := v.cachedKey(kid, now); key != nil && fresh {
		return key, nil
	} else if recentlyAttempted {
		return nil, ErrUnauthorized
	}
	v.mu.Lock()
	v.lastRefreshAttempt = now
	v.mu.Unlock()

	if err := v.refresh(ctx); err != nil {
		return nil, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if key := v.keys[kid]; key != nil {
		return key, nil
	}
	return nil, ErrUnauthorized
}

func (v *Verifier) cachedKey(kid string, now time.Time) (*rsa.PublicKey, bool, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	fresh := now.Before(v.until)
	cooldown := keyRefreshCooldown
	if !fresh {
		cooldown = failedRefreshCooldown
	}
	return v.keys[kid], fresh, now.Sub(v.lastRefreshAttempt) < cooldown
}

func (v *Verifier) refresh(ctx context.Context) error {
	var discovery struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := v.getJSON(ctx, v.issuer+"/.well-known/openid-configuration", &discovery); err != nil {
		return err
	}
	issuerURL, _ := url.Parse(v.issuer)
	keysURL, err := url.Parse(discovery.JWKSURI)
	if err != nil || discovery.Issuer != v.issuer || keysURL.Scheme != "https" || keysURL.Host != issuerURL.Host {
		return ErrUnauthorized
	}
	var jwks struct {
		Keys []struct {
			KID string `json:"kid"`
			KTY string `json:"kty"`
			ALG string `json:"alg"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := v.getJSON(ctx, keysURL.String(), &jwks); err != nil {
		return err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, item := range jwks.Keys {
		if item.KTY != "RSA" || (item.ALG != "" && item.ALG != "RS256") || (item.Use != "" && item.Use != "sig") || item.KID == "" {
			continue
		}
		n, nErr := base64.RawURLEncoding.DecodeString(item.N)
		e, eErr := base64.RawURLEncoding.DecodeString(item.E)
		if nErr != nil || eErr != nil || len(n) < 256 || len(e) == 0 || len(e) > 4 {
			continue
		}
		exponent := new(big.Int).SetBytes(e).Int64()
		if exponent < 3 || exponent > math.MaxInt32 || exponent%2 == 0 {
			continue
		}
		keys[item.KID] = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exponent)}
	}
	if len(keys) == 0 {
		return fmt.Errorf("OIDC provider returned no usable signing keys")
	}
	v.mu.Lock()
	v.keys = keys
	v.until = time.Now().Add(keyCacheTTL)
	v.mu.Unlock()
	return nil
}

func (v *Verifier) getJSON(ctx context.Context, endpoint string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("OIDC provider returned HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(out)
}

func claimString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case float64:
		if v >= 0 && v <= 1e15 && v == math.Trunc(v) {
			return fmt.Sprintf("%.0f", v)
		}
	}
	return ""
}

func presentNumeric(value any) bool {
	_, ok := value.(float64)
	return ok
}

func contains(values []string, target string) bool {
	if target == "" {
		return false
	}
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func decimal(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
