// Package client implements the gateway's typed API, HTTPS transport, and authentication.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const DefaultAudience = "api://harvester-runner-gateway"
const maxResponseBytes = 4 << 20

// ConfigError denotes invalid local configuration rather than a remote failure.
type ConfigError struct{ Err error }

func (e *ConfigError) Error() string   { return e.Err.Error() }
func configError(message string) error { return &ConfigError{errors.New(message)} }

type Config struct {
	URL, TokenFile, Token, CACert, Audience string
	Timeout                                 time.Duration
	GitHubActions                           bool
	OIDCRequestURL, OIDCRequestToken        string
}

// Request describes one operation. Path contains individual, unescaped segments.
type Request struct {
	operation string
	Method    string
	Path      []string
	Body      any
	Auth      bool
	Statuses  []int
}

type VMRequest struct {
	Image         string   `json:"image"`
	Network       string   `json:"network"`
	CPU           int      `json:"cpu"`
	Memory        string   `json:"memory"`
	BootDiskSize  string   `json:"bootDiskSize"`
	SSHPublicKeys []string `json:"sshPublicKeys,omitempty"`
	UserData      string   `json:"userData,omitempty"`
	TTLSeconds    *int     `json:"ttlSeconds,omitempty"`
}

type VMStatus struct {
	ID                string    `json:"id"`
	Phase             string    `json:"phase"`
	PowerState        string    `json:"powerState"`
	Ready             bool      `json:"ready"`
	IPAddresses       []string  `json:"ipAddresses"`
	AttachedVolumeIDs []string  `json:"attachedVolumeIDs"`
	Message           string    `json:"message,omitempty"`
	ExpiresAt         time.Time `json:"expiresAt"`
}

type VolumeRequest struct {
	Size       string `json:"size"`
	TTLSeconds *int   `json:"ttlSeconds,omitempty"`
}

type VolumeStatus struct {
	ID              string    `json:"id"`
	Phase           string    `json:"phase"`
	Size            string    `json:"size"`
	AttachedTo      string    `json:"attachedTo,omitempty"`
	AttachmentPhase string    `json:"attachmentPhase,omitempty"`
	ExpiresAt       time.Time `json:"expiresAt"`
}

type Quota struct {
	MaxActiveVMs     int `json:"maxActiveVMs"`
	ActiveVMs        int `json:"activeVMs"`
	MaxActiveVolumes int `json:"maxActiveVolumes"`
	ActiveVolumes    int `json:"activeVolumes"`
}

type PowerState string

const (
	PowerOn  PowerState = "on"
	PowerOff PowerState = "off"
)

type Client struct {
	cfg           Config
	base          *url.URL
	gateway, oidc *http.Client
	logger        *slog.Logger
}

func rejectRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

func New(cfg Config) (*Client, error) {
	return NewWithLogger(cfg, nil)
}

// NewWithLogger creates a client that emits one structured completion entry
// for every gateway and OIDC HTTP call. A nil logger disables logging.
func NewWithLogger(cfg Config, logger *slog.Logger) (*Client, error) {
	base, err := url.Parse(cfg.URL)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" || strings.Contains(cfg.URL, "#") || base.Opaque != "" {
		return nil, configError("--url must be an HTTPS URL without credentials, query, or fragment")
	}
	if cfg.Timeout <= 0 {
		return nil, configError("--timeout must be positive")
	}
	if cfg.Audience == "" {
		cfg.Audience = DefaultAudience
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CACert != "" {
		pem, err := os.ReadFile(cfg.CACert)
		if err != nil {
			return nil, configError("cannot read CA certificate file")
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			return nil, configError("cannot load system CA certificates")
		}
		if !roots.AppendCertsFromPEM(pem) {
			return nil, configError("CA certificate file contains no valid PEM certificates")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Client{cfg: cfg, base: base,
		gateway: &http.Client{Transport: transport, Timeout: cfg.Timeout, CheckRedirect: rejectRedirect},
		oidc:    &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone(), Timeout: cfg.Timeout, CheckRedirect: rejectRedirect},
		logger:  logger,
	}, nil
}

func (c *Client) Close() { c.gateway.CloseIdleConnections(); c.oidc.CloseIdleConnections() }

func validToken(token string) bool {
	if token == "" {
		return false
	}
	for _, r := range token {
		if r < 33 || r > 126 {
			return false
		}
	}
	return true
}

func (c *Client) token(ctx context.Context) (token string, err error) {
	if c.cfg.TokenFile != "" {
		data, err := os.ReadFile(c.cfg.TokenFile)
		if err != nil {
			return "", configError("cannot read bearer token file")
		}
		token := strings.TrimSpace(string(data))
		if !validToken(token) {
			return "", configError("bearer token file must contain one nonempty token")
		}
		return token, nil
	}
	if c.cfg.Token != "" {
		if !validToken(c.cfg.Token) {
			return "", configError("GATEWAY_TOKEN must contain one nonempty token")
		}
		return c.cfg.Token, nil
	}
	if !c.cfg.GitHubActions {
		return "", errors.New("authentication required: set --token-file or GATEWAY_TOKEN, or run inside GitHub Actions with id-token: write")
	}
	u, err := url.Parse(c.cfg.OIDCRequestURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || strings.Contains(c.cfg.OIDCRequestURL, "#") || !validToken(c.cfg.OIDCRequestToken) {
		return "", errors.New("GitHub Actions OIDC requires a valid HTTPS ACTIONS_ID_TOKEN_REQUEST_URL and ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	}
	q := u.Query()
	q.Set("audience", c.cfg.Audience)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", errors.New("cannot construct GitHub Actions OIDC request")
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.OIDCRequestToken)
	started := time.Now()
	status := 0
	responseBytes := 0
	defer func() {
		c.logHTTP(ctx, "oidc request", http.MethodGet, "", status, responseBytes, started, err)
	}()
	res, requestErr := c.oidc.Do(req)
	if requestErr != nil {
		err = transportError("OIDC request", requestErr)
		return "", err
	}
	status = res.StatusCode
	defer res.Body.Close()
	body, readErr := readBody(res.Body)
	responseBytes = len(body)
	if readErr != nil {
		err = fmt.Errorf("OIDC response: %w", readErr)
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		err = fmt.Errorf("OIDC request: HTTP %d", res.StatusCode)
		return "", err
	}
	var response struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(body, &response) != nil || !validToken(response.Value) {
		err = errors.New("OIDC response did not contain a valid token")
		return "", err
	}
	return response.Value, nil
}

func (c *Client) logHTTP(ctx context.Context, operation, method, path string, status, responseBytes int, started time.Time, err error) {
	level := slog.LevelInfo
	if err != nil {
		if status >= http.StatusBadRequest && status < http.StatusInternalServerError {
			level = slog.LevelWarn
		} else {
			level = slog.LevelError
		}
	}
	attrs := []slog.Attr{
		slog.String("operation", operation),
		slog.String("method", method),
		slog.Int64("duration_ms", time.Since(started).Milliseconds()),
		slog.Int("response_bytes", responseBytes),
	}
	if path != "" {
		attrs = append(attrs, slog.String("path", path))
	}
	if status != 0 {
		attrs = append(attrs, slog.Int("status", status))
	}
	if err != nil {
		loggedError := operation + " failed"
		if status != 0 {
			loggedError = fmt.Sprintf("%s failed with HTTP %d", operation, status)
		}
		attrs = append(attrs, slog.String("error", loggedError))
	}
	c.logger.LogAttrs(ctx, level, "client request completed", attrs...)
}

func transportError(operation string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("%s failed: %w", operation, err)
}

func readBody(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return data, errors.New("cannot read response body")
	}
	if len(data) > maxResponseBytes {
		return data, errors.New("response body exceeds 4 MiB")
	}
	return data, nil
}

// Do performs a single request without application-level retries. JSON is
// returned unchanged; successful operations without response bodies return nil.
func (c *Client) Do(ctx context.Context, operation Request) (data []byte, err error) {
	var body []byte
	if operation.Body != nil {
		body, err = json.Marshal(operation.Body)
		if err != nil {
			return nil, configError("cannot encode request body")
		}
	}
	target := strings.TrimRight(c.base.String(), "/")
	for _, segment := range operation.Path {
		if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, "/\\") {
			return nil, configError("invalid resource path segment")
		}
		target += "/" + url.PathEscape(segment)
	}
	req, err := http.NewRequestWithContext(ctx, operation.Method, target, bytes.NewReader(body))
	if err != nil {
		return nil, configError("cannot construct gateway request")
	}
	// Disable automatic body replay.
	req.GetBody = nil
	req.Header.Set("Accept", "application/json")
	if operation.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	var token string
	if operation.Auth {
		token, err = c.token(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	started := time.Now()
	operationName := operation.operation
	if operationName == "" {
		operationName = "gateway request"
	}
	status := 0
	responseBytes := 0
	defer func() {
		c.logHTTP(ctx, operationName, req.Method, req.URL.EscapedPath(), status, responseBytes, started, err)
	}()
	res, requestErr := c.gateway.Do(req)
	if requestErr != nil {
		err = transportError("gateway request", requestErr)
		return nil, err
	}
	status = res.StatusCode
	defer res.Body.Close()
	data, err = readBody(res.Body)
	responseBytes = len(data)
	if err != nil {
		err = fmt.Errorf("gateway HTTP %d: %w", res.StatusCode, err)
		return nil, err
	}
	success := false
	for _, status := range operation.Statuses {
		if res.StatusCode == status {
			success = true
			break
		}
	}
	if !success {
		var remote struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &remote) == nil && remote.Code != "" {
			message := remote.Code + ": " + remote.Message
			for _, secret := range []string{token, c.cfg.OIDCRequestToken} {
				if secret != "" {
					message = strings.ReplaceAll(message, secret, "[REDACTED]")
				}
			}
			err = fmt.Errorf("gateway HTTP %d: %s", res.StatusCode, message)
			return nil, err
		}
		err = fmt.Errorf("gateway HTTP %d: unexpected or non-JSON error response", res.StatusCode)
		return nil, err
	}
	if res.StatusCode == http.StatusNoContent || res.StatusCode == http.StatusAccepted {
		if len(bytes.TrimSpace(data)) != 0 {
			err = errors.New("gateway returned an unexpected response body")
			return nil, err
		}
		return nil, nil
	}
	if !json.Valid(data) {
		err = errors.New("gateway returned malformed JSON")
		return nil, err
	}
	return data, nil
}
