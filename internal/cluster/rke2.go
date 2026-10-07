package cluster

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	rke2ConfigDir  = "/etc/rancher/rke2"
	rke2Kubeconfig = rke2ConfigDir + "/rke2.yaml"
)

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
	environment:    func(_, _, _, _, _ string) string { return "" },
	install:        rke2Install,
}

// rke2Install writes the RKE2 config file, runs get.rke2.io, and starts the
// service, which the installer does not do. The config file holds the token,
// so it is written through a quoted here-document and is never printed.
func rke2Install(role, version, token, nodeName, joinURL string) string {
	var script strings.Builder
	fmt.Fprintf(&script, "(\numask 077\nmkdir -p %s\ncat > %s <<'RKE2_CONFIG'\n", shellQuote(rke2ConfigDir), shellQuote(rke2ConfigDir+"/config.yaml"))
	fmt.Fprintf(&script, "token: %s\nnode-name: %s\n", yamlQuote(token), yamlQuote(nodeName))
	if joinURL != "" {
		fmt.Fprintf(&script, "server: %s\n", yamlQuote(joinURL))
	}
	script.WriteString("RKE2_CONFIG\n)\n")
	fmt.Fprintf(&script, "INSTALL_RKE2_TYPE=%s", shellQuote(role))
	if version != "" {
		fmt.Fprintf(&script, " INSTALL_RKE2_VERSION=%s", shellQuote(version))
	}
	script.WriteString(" sh \"$installer\" </dev/null\n")
	unit := "rke2-" + role + ".service"
	fmt.Fprintf(&script, "echo \"starting %s (this waits until the node has joined)\"\n", unit)
	fmt.Fprintf(&script, "systemctl enable --now %s </dev/null\n", shellQuote(unit))
	return script.String()
}

// yamlQuote returns value as a single-quoted YAML scalar. Callers pass values
// without line breaks, so the here-document cannot be terminated early.
func yamlQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
