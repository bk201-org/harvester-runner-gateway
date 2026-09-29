package gateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bk201-org/harvester-runner-gateway/internal/auth"
)

func decodeLogs(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	var records []map[string]any
	for decoder.More() {
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

func TestRequestLogContainsResponseMetadataWithoutSecrets(t *testing.T) {
	s := testServer(1, 1)
	var output bytes.Buffer
	s.logger = slog.New(slog.NewJSONHandler(&output, nil))

	request := httptest.NewRequest(http.MethodGet, "/healthz?token=query-secret", nil)
	request.Header.Set("Authorization", "Bearer header-secret")
	response := httptest.NewRecorder()
	s.Handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if strings.Contains(output.String(), "query-secret") || strings.Contains(output.String(), "header-secret") {
		t.Fatalf("log contains request secret: %s", output.String())
	}

	records := decodeLogs(t, output.Bytes())
	if len(records) != 1 {
		t.Fatalf("got %d log records, want 1", len(records))
	}
	record := records[0]
	if record["msg"] != "request completed" || record["method"] != http.MethodGet ||
		record["path"] != "/healthz" || record["status"] != float64(http.StatusNoContent) {
		t.Fatalf("unexpected request log: %#v", record)
	}
	if _, ok := record["duration_ms"]; !ok {
		t.Fatalf("request log has no duration: %#v", record)
	}
}

func TestResourceLogContainsOwnershipWithoutCredentials(t *testing.T) {
	s := testServer(1, 1)
	var output bytes.Buffer
	s.logger = slog.New(slog.NewJSONHandler(&output, nil))

	response := doRequest(s, http.MethodPost, "/v1/vms", "run-one", vmRequest())
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", response.Code, response.Body.String())
	}
	if strings.Contains(output.String(), "run-one") {
		t.Fatalf("log contains a credential: %s", output.String())
	}

	records := decodeLogs(t, output.Bytes())
	var resource map[string]any
	for _, record := range records {
		if record["msg"] == "resource operation completed" {
			resource = record
			break
		}
	}
	if resource == nil {
		t.Fatalf("resource event not logged: %s", output.String())
	}
	if resource["operation"] != "create" || resource["resource_type"] != "vm" ||
		resource["resource_id"] != "ci-vm-00000001" || resource["namespace"] != "ci" ||
		resource["repository_id"] != "123" || resource["run_id"] != "1001" ||
		resource["run_attempt"] != "1" {
		t.Fatalf("unexpected resource log: %#v", resource)
	}
}

func TestAuthenticationRejectionLogsReasonWithoutCredentials(t *testing.T) {
	for _, test := range []struct {
		name, header, reason, response string
		realVerifier                   bool
	}{
		{"missing bearer", "", "bearer_required", "Bearer token required", false},
		{"rejected bearer", "Bearer header-secret", "verification_failed", "Bearer token rejected (verification_failed)", false},
		{"invalid token", "Bearer header-secret", "invalid_token", "Bearer token rejected (invalid_token)", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := testServer(1, 1)
			if test.realVerifier {
				s.verifier = auth.NewVerifier("https://issuer.example", "api://gateway")
			}
			var output bytes.Buffer
			s.logger = slog.New(slog.NewJSONHandler(&output, nil))
			request := httptest.NewRequest(http.MethodGet, "/v1/quota", nil)
			if test.header != "" {
				request.Header.Set("Authorization", test.header)
			}
			response := httptest.NewRecorder()
			s.Handler.ServeHTTP(response, request)

			if response.Code != http.StatusUnauthorized || !strings.Contains(response.Body.String(), test.response) {
				t.Fatalf("response = %d %q", response.Code, response.Body.String())
			}
			if strings.Contains(output.String()+response.Body.String(), "header-secret") {
				t.Fatalf("log contains credential: %s", output.String())
			}
			records := decodeLogs(t, output.Bytes())
			if len(records) != 2 || records[0]["msg"] != "authentication rejected" ||
				records[0]["reason"] != test.reason || records[1]["msg"] != "request completed" ||
				records[1]["status"] != float64(http.StatusUnauthorized) {
				t.Fatalf("unexpected request logs: %#v", records)
			}
		})
	}
}
