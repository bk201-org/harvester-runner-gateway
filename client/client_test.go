package client

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func file(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func configuration(t *testing.T, s *httptest.Server) Config {
	t.Helper()
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
	return Config{URL: s.URL, CACert: file(t, string(cert)), Token: "secret", Timeout: time.Second}
}
func newClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}
func quotaRequest() Request {
	return Request{Method: "GET", Path: []string{"v1", "quota"}, Auth: true, Statuses: []int{200}}
}

func TestTokenPrecedence(t *testing.T) {
	cases := []struct {
		name, tokenFile, token string
		want                   string
		bad                    bool
	}{
		{"file wins", file(t, "file-token\n"), "env-token", "file-token", false},
		{"environment", "", "env-token", "env-token", false},
		{"missing file", "/nonexistent", "env-token", "", true},
		{"empty file", file(t, " \n"), "env-token", "", true},
		{"multiline file", file(t, "one\ntwo"), "env-token", "", true},
		{"malformed environment", "", "bad\nvalue", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, Config{URL: "https://gateway.example", Timeout: time.Second, TokenFile: tc.tokenFile, Token: tc.token, GitHubActions: true})
			token, err := c.token(context.Background())
			if tc.bad {
				var configErr *ConfigError
				if !errors.As(err, &configErr) {
					t.Fatalf("expected ConfigError: %v", err)
				}
			} else if err != nil || token != tc.want {
				t.Fatalf("incorrect token or error: %v", err)
			}
		})
	}
}

func TestOIDC(t *testing.T) {
	var oidcCalls, gatewayCalls atomic.Int32
	oidc := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oidcCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer request-secret" {
			t.Error("wrong OIDC authorization")
		}
		if r.URL.Query().Get("audience") != "api://gateway/a?b=c &d" || r.URL.Query().Get("existing") != "yes" {
			t.Errorf("query=%s", r.URL.RawQuery)
		}
		fmt.Fprint(w, `{"value":"issued-token"}`)
	}))
	defer oidc.Close()
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gatewayCalls.Add(1)
		if r.Header.Get("Authorization") != "Bearer issued-token" {
			t.Error("wrong gateway authorization")
		}
		fmt.Fprint(w, `{"activeVMs":0}`)
	}))
	defer gateway.Close()
	cfg := configuration(t, gateway)
	cfg.Token = ""
	cfg.GitHubActions = true
	cfg.OIDCRequestURL = oidc.URL + "?existing=yes&audience=old"
	cfg.OIDCRequestToken = "request-secret"
	cfg.Audience = "api://gateway/a?b=c &d"
	c := newClient(t, cfg)
	// Only the test supplies trust for the mock OIDC server. Production uses
	// system trust separately from the gateway CA.
	c.oidc.Transport = oidc.Client().Transport
	if _, err := c.Do(context.Background(), quotaRequest()); err != nil {
		t.Fatal(err)
	}
	if oidcCalls.Load() != 1 || gatewayCalls.Load() != 1 {
		t.Fatal("incorrect request counts")
	}
}

func TestOIDCFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{{"unauthorized", 401, `request-secret`}, {"redirect", 302, `request-secret`}, {"malformed", 200, `{`}, {"missing", 200, `{}`}, {"empty", 200, `{"value":""}`}, {"invalid token", 200, `{"value":"a\nb"}`}} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "https://other.example")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			c := newClient(t, Config{URL: "https://gateway.example", Timeout: time.Second, GitHubActions: true, OIDCRequestURL: s.URL, OIDCRequestToken: "request-secret"})
			c.oidc.Transport = s.Client().Transport
			_, err := c.token(context.Background())
			if err == nil || strings.Contains(err.Error(), "request-secret") {
				t.Fatalf("unsafe or absent error: %v", err)
			}
		})
	}
	for _, u := range []string{"", "http://example.test", "https://user:pass@example.test", "https://example.test/#fragment"} {
		c := newClient(t, Config{URL: "https://gateway.example", Timeout: time.Second, GitHubActions: true, OIDCRequestURL: u, OIDCRequestToken: "secret"})
		if _, err := c.token(context.Background()); err == nil {
			t.Errorf("accepted OIDC URL %q", u)
		}
	}
}

func TestTLSAndUnauthenticatedChecks(t *testing.T) {
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("health request sent credentials")
		}
		w.WriteHeader(204)
	}))
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.StartTLS()
	defer s.Close()
	req := Request{Method: "GET", Path: []string{"healthz"}, Statuses: []int{204}}
	cfg := configuration(t, s)
	cfg.TokenFile = "/missing"
	cfg.Token = ""
	cfg.GitHubActions = true
	c := newClient(t, cfg)
	if _, err := c.Do(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// A gateway CA must never also trust the OIDC service.
	c.cfg.OIDCRequestURL = s.URL
	c.cfg.OIDCRequestToken = "secret"
	if _, err := c.token(context.Background()); err == nil {
		t.Error("gateway CA trusted by OIDC client")
	}
	cfg.CACert = ""
	c = newClient(t, cfg)
	if _, err := c.Do(context.Background(), req); err == nil {
		t.Error("untrusted TLS certificate accepted")
	}
}

func TestConfigurationValidation(t *testing.T) {
	for _, u := range []string{"", "http://example.test", "https://", "https://user:secret@example.test", "https://example.test?", "https://example.test?secret=x", "https://example.test#", "https://example.test#fragment", "https://example.test:bad"} {
		_, err := New(Config{URL: u, Timeout: time.Second})
		var ce *ConfigError
		if !errors.As(err, &ce) {
			t.Errorf("accepted invalid URL %q: %v", u, err)
		}
	}
	for _, ca := range []string{"/missing", file(t, "not a certificate")} {
		if _, err := New(Config{URL: "https://example.test", Timeout: time.Second, CACert: ca}); err == nil {
			t.Error("accepted invalid CA")
		}
	}
	if _, err := New(Config{URL: "https://example.test"}); err == nil {
		t.Error("accepted zero timeout")
	}
}

func TestResponsesAndNoRetries(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body, want string
	}{
		{"api error", 409, `{"code":"quota_exceeded","message":"full"}`, "HTTP 409: quota_exceeded: full"},
		{"redacted error", 401, `{"code":"unauthorized","message":"secret request-secret"}`, "[REDACTED]"},
		{"html", 502, `<html>secret</html>`, "HTTP 502"},
		{"redirect", 307, "", "HTTP 307"},
		{"malformed", 200, "{", "malformed JSON"},
		{"empty", 200, "", "malformed JSON"},
		{"oversized", 200, strings.Repeat("x", maxResponseBytes+1), "exceeds 4 MiB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "https://other.example")
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			cfg := configuration(t, s)
			cfg.OIDCRequestToken = "request-secret"
			_, err := newClient(t, cfg).Do(context.Background(), quotaRequest())
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unexpected error: %v", err)
			}
			if calls.Load() != 1 {
				t.Errorf("made %d calls", calls.Load())
			}
		})
	}
	var calls atomic.Int32
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer s.Close()
	c := newClient(t, configuration(t, s))
	_, err := c.Do(context.Background(), Request{Method: "POST", Path: []string{"v1", "vms"}, Auth: true, Body: VMRequest{CPU: 1}, Statuses: []int{201}})
	if err == nil || calls.Load() != 1 {
		t.Fatal("create should fail without retry")
	}
	_, err = c.Do(context.Background(), Request{Method: "POST", Path: []string{"v1", "vms", "vm1", "reboot"}, Auth: true, Statuses: []int{202}})
	if err == nil || calls.Load() != 2 {
		t.Fatal("reboot should fail without retry")
	}
}

func TestCancellationAndTimeout(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer s.Close()
	cfg := configuration(t, s)
	cfg.Timeout = 30 * time.Millisecond
	c := newClient(t, cfg)
	if _, err := c.Do(context.Background(), quotaRequest()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Do(ctx, quotaRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	cfg.Token = ""
	cfg.GitHubActions = true
	cfg.OIDCRequestURL = s.URL
	cfg.OIDCRequestToken = "secret"
	c = newClient(t, cfg)
	c.oidc.Transport = s.Client().Transport
	if _, err := c.token(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("OIDC timeout: %v", err)
	}
	if _, err := c.token(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("OIDC cancellation: %v", err)
	}
}

func TestSafePaths(t *testing.T) {
	var got string
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.URL.EscapedPath(); fmt.Fprint(w, `{}`) }))
	defer s.Close()
	cfg := configuration(t, s)
	cfg.URL += "/prefix%20name/"
	c := newClient(t, cfg)
	req := quotaRequest()
	req.Path = []string{"v1", "vms", "id?#%"}
	if _, err := c.Do(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got != "/prefix%20name/v1/vms/id%3F%23%25" {
		t.Errorf("path=%s", got)
	}
	for _, id := range []string{"", ".", "..", "a/b", "a\\b"} {
		req.Path = []string{"v1", "vms", id}
		if _, err := c.Do(context.Background(), req); err == nil {
			t.Errorf("accepted %q", id)
		}
	}
}
