package gateway

import (
	"net/http"
	"sync"
	"testing"
)

func TestConcurrentSameKeyAllocatesOneVM(t *testing.T) {
	s := testServer(10, 10)
	var wait sync.WaitGroup
	results := make(chan int, 20)
	for range 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- doRequest(s, http.MethodPost, "/v1/vms", "run-one", "same-key", vmRequest()).Code
		}()
	}
	wait.Wait()
	close(results)
	created, existing := 0, 0
	for status := range results {
		switch status {
		case http.StatusCreated:
			created++
		case http.StatusOK:
			existing++
		default:
			t.Fatalf("unexpected status %d", status)
		}
	}
	backend := s.backend.(*fakeBackend)
	if created != 1 || existing != 19 || len(backend.vms) != 1 {
		t.Fatalf("created=%d existing=%d VMs=%d", created, existing, len(backend.vms))
	}
	if _, ok := backend.vms["ci-123-1001-a1-001"]; !ok {
		t.Fatalf("unexpected VM map: %#v", backend.vms)
	}
}
