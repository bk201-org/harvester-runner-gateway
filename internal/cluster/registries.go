package cluster

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// registryPattern matches the registry key of a k3s/RKE2 registries.yaml
// mirror: a host with an optional port, or "*" for the default mirror.
var registryPattern = regexp.MustCompile(`^(\*|[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?)$`)

// registryMirror is one entry of the registries.yaml mirrors map.
type registryMirror struct {
	Registry  string
	Endpoints []string
}

// parseRegistryMirrors parses the registry-mirrors input: a YAML (or JSON)
// list of {registry, endpoint} objects. Entries for the same registry are
// merged into one endpoint list, in input order. Empty input means no mirrors.
func parseRegistryMirrors(input string) ([]registryMirror, error) {
	if strings.TrimSpace(input) == "" {
		return nil, nil
	}
	converted, err := yaml.YAMLToJSONStrict([]byte(input))
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	var entries []struct {
		Registry string `json:"registry"`
		Endpoint string `json:"endpoint"`
	}
	decoder := json.NewDecoder(bytes.NewReader(converted))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&entries); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return nil, errors.New("must be a list of entries with registry and endpoint strings")
		}
		return nil, fmt.Errorf("must be a list of entries with registry and endpoint strings: %w", err)
	}
	var mirrors []registryMirror
	index := map[string]int{}
	for i, entry := range entries {
		if !registryPattern.MatchString(entry.Registry) {
			return nil, fmt.Errorf("entry %d: registry %q must be a host, host:port, or *", i+1, entry.Registry)
		}
		if err := validateEndpoint(entry.Endpoint); err != nil {
			// The endpoint is not echoed because it may contain credentials.
			return nil, fmt.Errorf("entry %d: endpoint %w", i+1, err)
		}
		if j, ok := index[entry.Registry]; ok {
			mirrors[j].Endpoints = append(mirrors[j].Endpoints, entry.Endpoint)
			continue
		}
		index[entry.Registry] = len(mirrors)
		mirrors = append(mirrors, registryMirror{Registry: entry.Registry, Endpoints: []string{entry.Endpoint}})
	}
	return mirrors, nil
}

// registriesYAML renders mirrors as k3s/RKE2 registries.yaml content, or ""
// when there are none. The YAML library quotes values only where needed.
// Validated values have no line breaks, so the content can be written
// through a shell here-document.
func registriesYAML(mirrors []registryMirror) (string, error) {
	if len(mirrors) == 0 {
		return "", nil
	}
	type endpoints struct {
		Endpoint []string `json:"endpoint"`
	}
	doc := struct {
		Mirrors map[string]endpoints `json:"mirrors"`
	}{Mirrors: map[string]endpoints{}}
	for _, mirror := range mirrors {
		doc.Mirrors[mirror.Registry] = endpoints{Endpoint: mirror.Endpoints}
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("render registries.yaml: %w", err)
	}
	return string(out), nil
}

func validateEndpoint(endpoint string) error {
	if strings.ContainsAny(endpoint, " \t\r\n") {
		return fmt.Errorf("must not contain whitespace")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("must be an http or https URL with a host")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		return fmt.Errorf("must not contain credentials, a query, or a fragment")
	}
	return nil
}

// validateSystemDefaultRegistry accepts an empty value or a registry host with
// an optional port, without a scheme or path.
func validateSystemDefaultRegistry(registry string) error {
	if registry == "" {
		return nil
	}
	if registry == "*" || !registryPattern.MatchString(registry) {
		return fmt.Errorf("%q must be a host or host:port without a scheme or path", registry)
	}
	return nil
}
