package cluster

import (
	"regexp"
	"strings"
)

const rke2Kubeconfig = "/etc/rancher/rke2/rke2.yaml"

var rke2Dist = distribution{
	name:           "rke2",
	versionInput:   "INPUT_RKE2-VERSION",
	timeoutInput:   "INPUT_RKE2-TIMEOUT-SECONDS",
	defaultTimeout: "1200",
	versionPattern: regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?\+rke2r[0-9]+$`),
	versionExample: "v1.35.2+rke2r1",
	installURL:     "https://get.rke2.io",
	kubeconfig:     rke2Kubeconfig,
	nodesCommand:   "/var/lib/rancher/rke2/bin/kubectl --kubeconfig " + rke2Kubeconfig + " get nodes --no-headers\n",
	serverUnit:     "rke2-server",
	agentUnit:      "rke2-agent",
	joinPort:       "9345",
	scripts:        parseScripts("scripts/rke2.sh.tmpl", "scripts/start-service.sh.tmpl"),
}

// yamlQuote returns value as a single-quoted YAML scalar. Callers pass values
// without line breaks, so the here-document cannot be terminated early.
func yamlQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
