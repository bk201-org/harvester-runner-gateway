package harvester

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

func TestKubeVirtSubresourceRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/apis/subresources.kubevirt.io/v1/namespaces/ci/virtualmachines/runner-gw-example/addvolume" {
			t.Errorf("unexpected KubeVirt request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["name"] != "runner-gw-volume" {
			t.Errorf("unexpected request body: %v %v", body, err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	cfg := &rest.Config{Host: server.URL, APIPath: "/apis"}
	cfg.GroupVersion = &schema.GroupVersion{Group: "subresources.kubevirt.io", Version: "v1"}
	cfg.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	cfg.ContentType = runtime.ContentTypeJSON
	client, err := rest.RESTClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	backend := &Backend{sub: client}
	if err := backend.subresource(context.Background(), "ci", "runner-gw-example", "addvolume", map[string]string{"name": "runner-gw-volume"}); err != nil {
		t.Fatal(err)
	}
}
