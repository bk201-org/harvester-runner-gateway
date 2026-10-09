package cluster

import (
	"context"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

func TestParseRegistryMirrors(t *testing.T) {
	for _, input := range []string{"", "  \n"} {
		if mirrors, err := parseRegistryMirrors(input); err != nil || mirrors != nil {
			t.Fatalf("parse %q = %#v, %v", input, mirrors, err)
		}
	}
	yamlInput := `- registry: docker.io
  endpoint: http://10.0.10.1:5000
- registry: registry.example:5000
  endpoint: https://mirror.example/v2-cache
- registry: docker.io
  endpoint: https://backup.example
- registry: "*"
  endpoint: http://10.0.10.1:5001
`
	want := []registryMirror{
		{Registry: "docker.io", Endpoints: []string{"http://10.0.10.1:5000", "https://backup.example"}},
		{Registry: "registry.example:5000", Endpoints: []string{"https://mirror.example/v2-cache"}},
		{Registry: "*", Endpoints: []string{"http://10.0.10.1:5001"}},
	}
	mirrors, err := parseRegistryMirrors(yamlInput)
	if err != nil || !reflect.DeepEqual(mirrors, want) {
		t.Fatalf("parse YAML = %#v, %v", mirrors, err)
	}
	mirrors, err = parseRegistryMirrors(`[{"registry":"docker.io","endpoint":"http://10.0.10.1:5000"}]`)
	if err != nil || !reflect.DeepEqual(mirrors, []registryMirror{{Registry: "docker.io", Endpoints: []string{"http://10.0.10.1:5000"}}}) {
		t.Fatalf("parse JSON = %#v, %v", mirrors, err)
	}

	for name, input := range map[string]string{
		"not a list":       "registry: docker.io\nendpoint: http://m\n",
		"unknown field":    "- registry: docker.io\n  endpoint: http://m\n  insecure: true\n",
		"missing endpoint": "- registry: docker.io\n",
		"bad scheme":       "- registry: docker.io\n  endpoint: ftp://m\n",
		"no host":          "- registry: docker.io\n  endpoint: http:///v2\n",
		"credentials":      "- registry: docker.io\n  endpoint: https://user:pass@m\n",
		"query":            "- registry: docker.io\n  endpoint: https://m/?ns=x\n",
		"fragment":         "- registry: docker.io\n  endpoint: https://m/#x\n",
		"newline":          "- registry: docker.io\n  endpoint: \"https://m\\nREGISTRIES\"\n",
		"bad registry":     "- registry: \"docker.io'\"\n  endpoint: https://m\n",
		"registry path":    "- registry: docker.io/library\n  endpoint: https://m\n",
		"duplicate key":    "- registry: docker.io\n  registry: quay.io\n  endpoint: https://m\n",
	} {
		if mirrors, err := parseRegistryMirrors(input); err == nil {
			t.Errorf("%s: accepted %q as %#v", name, input, mirrors)
		} else if strings.Contains(err.Error(), "pass") || strings.Contains(err.Error(), "struct") {
			t.Errorf("%s: error leaks the input or Go types: %v", name, err)
		}
	}
}

func TestRegistriesYAML(t *testing.T) {
	if out, err := registriesYAML(nil); err != nil || out != "" {
		t.Fatalf("no mirrors = %q, %v", out, err)
	}
	out, err := registriesYAML([]registryMirror{
		{Registry: "docker.io", Endpoints: []string{"http://10.0.10.1:5000", "https://backup.example"}},
		{Registry: "*", Endpoints: []string{"http://10.0.10.1:5001"}},
	})
	// Only values that YAML requires to be quoted, such as "*", are quoted.
	want := `mirrors:
  '*':
    endpoint:
    - http://10.0.10.1:5001
  docker.io:
    endpoint:
    - http://10.0.10.1:5000
    - https://backup.example
`
	if err != nil || out != want {
		t.Fatalf("registries.yaml = %q, %v; want %q", out, err, want)
	}
}

func TestReadInstallOptionsRegistryMirrors(t *testing.T) {
	for _, dist := range []*distribution{&k3sDist, &rke2Dist} {
		env := map[string]string{"INPUT_REGISTRY-MIRRORS": "- registry: docker.io\n  endpoint: http://10.0.10.1:5000\n"}
		opt, err := readInstallOptions(func(k string) string { return env[k] }, dist)
		if err != nil || len(opt.Mirrors) != 1 || opt.Mirrors[0].Registry != "docker.io" {
			t.Fatalf("%s options = %#v, %v", dist.name, opt, err)
		}
		env["INPUT_REGISTRY-MIRRORS"] = "- registry: docker.io\n  endpoint: ftp://m\n"
		if _, err := readInstallOptions(func(k string) string { return env[k] }, dist); err == nil || !strings.HasPrefix(err.Error(), "registry-mirrors: entry 1: endpoint") {
			t.Fatalf("%s error = %v", dist.name, err)
		}
	}
}

var registriesDoc = regexp.MustCompile(`(?s)cat > '([^']+)' <<'REGISTRIES'\n(.*?)\nREGISTRIES\n`)

func TestInstallScriptWritesRegistries(t *testing.T) {
	mirrors := []registryMirror{
		{Registry: "docker.io", Endpoints: []string{"http://10.0.10.1:5000", "https://backup.example"}},
		{Registry: "*", Endpoints: []string{"http://10.0.10.1:5001"}},
	}
	for _, tc := range []struct {
		dist *distribution
		path string
	}{{&k3sDist, "/etc/rancher/k3s/registries.yaml"}, {&rke2Dist, "/etc/rancher/rke2/registries.yaml"}} {
		script, err := installScript(tc.dist, "agent", "", "tok", "vm-1", "https://10.0.0.1:"+tc.dist.joinPort, mirrors, "")
		if err != nil {
			t.Fatal(err)
		}
		match := registriesDoc.FindStringSubmatch(script)
		if match == nil || match[1] != tc.path {
			t.Fatalf("%s script does not write %s:\n%s", tc.dist.name, tc.path, script)
		}
		var doc struct {
			Mirrors map[string]struct {
				Endpoint []string `json:"endpoint"`
			} `json:"mirrors"`
		}
		if err := yaml.UnmarshalStrict([]byte(match[2]), &doc); err != nil {
			t.Fatalf("%s registries.yaml does not parse: %v\n%s", tc.dist.name, err, match[2])
		}
		if len(doc.Mirrors) != 2 || !reflect.DeepEqual(doc.Mirrors["docker.io"].Endpoint, mirrors[0].Endpoints) ||
			!reflect.DeepEqual(doc.Mirrors["*"].Endpoint, mirrors[1].Endpoints) {
			t.Fatalf("%s registries.yaml = %#v", tc.dist.name, doc)
		}
		written := strings.Index(script, "<<'REGISTRIES'")
		installer := strings.Index(script, "== install "+tc.dist.name)
		download := strings.Index(script, "downloaded installer")
		if !(download < written && written < installer) {
			t.Fatalf("%s registries.yaml is not written between download and install:\n%s", tc.dist.name, script)
		}
		for _, want := range []string{"umask 077", "mkdir -p '" + tc.path[:strings.LastIndex(tc.path, "/")] + "'", "cat '" + tc.path + "'"} {
			if !strings.Contains(script, want) {
				t.Fatalf("%s script lacks %q:\n%s", tc.dist.name, want, script)
			}
		}
	}
	plain := mustInstallScript(t, &k3sDist, "server", "", "tok", "vm-1", "")
	if strings.Contains(plain, "registr") {
		t.Fatalf("script without mirrors mentions registries:\n%s", plain)
	}
}

func TestSetupKubeWritesRegistriesOnEveryNode(t *testing.T) {
	fastPolling(t)
	runner := &fakeRunner{
		nodesOutput: []string{"ci-vm-00000001 Ready\nci-vm-00000002 Ready\nci-vm-00000003 Ready\n"},
		kubeconfig:  "server: https://127.0.0.1:6443\n",
	}
	mirrors := []registryMirror{{Registry: "docker.io", Endpoints: []string{"http://10.0.10.1:5000"}}}
	if _, err := setupKube(context.Background(), runner, testCluster(t), installOptions{Dist: &rke2Dist,
		SSHTimeout: time.Second, InstallTimeout: time.Second, Mirrors: mirrors}); err != nil {
		t.Fatal(err)
	}
	installs := runner.installs()
	if len(installs) != 3 {
		t.Fatalf("installs = %d", len(installs))
	}
	for _, call := range installs {
		if !strings.Contains(call.script, "cat > '/etc/rancher/rke2/registries.yaml'") || !strings.Contains(call.script, "    - http://10.0.10.1:5000\n") {
			t.Fatalf("%s script lacks registries.yaml:\n%s", call.host, call.script)
		}
	}
}

func TestSystemDefaultRegistry(t *testing.T) {
	for _, dist := range []*distribution{&k3sDist, &rke2Dist} {
		env := map[string]string{"INPUT_SYSTEM-DEFAULT-REGISTRY": " registry.example.com:5000 "}
		opt, err := readInstallOptions(func(k string) string { return env[k] }, dist)
		if err != nil || opt.SystemDefaultRegistry != "registry.example.com:5000" {
			t.Fatalf("%s options = %#v, %v", dist.name, opt, err)
		}
		for _, bad := range []string{"https://r.example", "r.example/path", "*", "a b"} {
			env["INPUT_SYSTEM-DEFAULT-REGISTRY"] = bad
			if _, err := readInstallOptions(func(k string) string { return env[k] }, dist); err == nil ||
				!strings.HasPrefix(err.Error(), "system-default-registry:") {
				t.Fatalf("%s %q error = %v", dist.name, bad, err)
			}
		}
		script, err := installScript(dist, "server", "", "tok", "vm-1", "", nil, "registry.example.com:5000")
		if err != nil {
			t.Fatal(err)
		}
		path := "/etc/rancher/" + dist.name + "/config.yaml"
		if !strings.Contains(script, "cat > '"+path+"'") || !strings.Contains(script, "system-default-registry: 'registry.example.com:5000'\n") {
			t.Fatalf("%s script lacks system-default-registry:\n%s", dist.name, script)
		}
		if plain := mustInstallScript(t, dist, "server", "", "tok", "vm-1", ""); strings.Contains(plain, "system-default-registry") {
			t.Fatalf("%s script without the option mentions it:\n%s", dist.name, plain)
		}
	}
}
