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
	script.WriteString(startServiceScript("rke2-" + role + ".service"))
	return script.String()
}

// startServiceScript starts unit without blocking and waits until it is
// active. RKE2 units are Type=notify with TimeoutStartSec=0, so a plain
// `systemctl start` would wait silently until the node has joined, which may
// be forever. The loop reports progress every 30 seconds and fails at once if
// systemd gives up on the unit; otherwise the caller's timeout ends it.
func startServiceScript(unit string) string {
	return "unit=" + shellQuote(unit) + `
echo "starting $unit (this waits until the node has joined)"
systemctl enable --now --no-block "$unit" </dev/null
waited=0
while :; do
  if state=$(systemctl is-active "$unit" </dev/null); then
    echo "$unit is active after ${waited}s"
    break
  fi
  if [ "$state" = failed ]; then
    echo "$unit failed after ${waited}s" >&2
    exit 1
  fi
  if [ "$waited" -gt 0 ] && [ $((waited % 30)) -eq 0 ]; then
    echo "waiting for $unit: ${state:-unknown} after ${waited}s; last log line:"
    journalctl -u "$unit" -n 1 --no-pager -o cat </dev/null 2>&1 || true
  fi
  sleep 5
  waited=$((waited + 5))
done
`
}

// yamlQuote returns value as a single-quoted YAML scalar. Callers pass values
// without line breaks, so the here-document cannot be terminated early.
func yamlQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}
