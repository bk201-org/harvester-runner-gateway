package gateway

import (
	"net/http"
	"sync"
	"testing"
)

func TestConcurrentCreatesAllocateSeparateVMs(t *testing.T) {
	s := testServer(20, 10)
	var wait sync.WaitGroup
	results := make(chan int, 20)
	for range 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- doRequest(s, http.MethodPost, "/v1/vms", "run-one", vmRequest()).Code
		}()
	}
	wait.Wait()
	close(results)
	created := 0
	for status := range results {
		if status != http.StatusCreated {
			t.Fatalf("unexpected status %d", status)
		}
		created++
	}
	backend := s.backend.(*fakeBackend)
	if created != 20 || len(backend.vms) != 20 {
		t.Fatalf("created=%d VMs=%d", created, len(backend.vms))
	}
}
