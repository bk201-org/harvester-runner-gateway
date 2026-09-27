package gateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
		resource["resource_id"] != "ci-123-1001-a1-001" || resource["namespace"] != "ci" ||
		resource["repository_id"] != "123" || resource["run_id"] != "1001" ||
		resource["run_attempt"] != "1" {
		t.Fatalf("unexpected resource log: %#v", resource)
	}
}
