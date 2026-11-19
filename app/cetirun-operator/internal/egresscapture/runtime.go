// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package egresscapture

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const (
	InitContainerName = "egress-capture-init"
	CaptureVolumeName = "egress-capture-config"
	CapturePath       = "/etc/cetirun-egress"
	PrivateVolumeName = "egress-proxy-private"
	PrivatePath       = "/var/run/cetirun-egress"
	AdminSocketPath   = PrivatePath + "/admin.sock"
	LockVolumeName    = "egress-capture-lock"
	LockPath          = "/var/run/cetirun-egress-lock"
)

var pinnedImage = regexp.MustCompile(`^[a-z0-9][a-z0-9./:_-]*@sha256:[a-f0-9]{64}$`)

// Registration is operator-owned platform input, never an Agent image override.
// InitImage must supply sh, curl, iptables and ip6tables. CetiRun's base image
// supplies these tools; the exact approved artifact must be digest pinned.
type Registration struct {
	EnvoyImage string `json:"envoyImage"`
	InitImage  string `json:"initImage"`
}

type Plan struct {
	Registration
	Hostnames []string `json:"hostnames"`
	IPv4      bool     `json:"ipv4"`
	IPv6      bool     `json:"ipv6"`
}

func (r Registration) Validate() error {
	if !pinnedImage.MatchString(r.EnvoyImage) || !pinnedImage.MatchString(r.InitImage) {
		return fmt.Errorf("observed-SNI registration requires digest-pinned Envoy and trusted capture-init images")
	}
	return nil
}

func (r Registration) Resolve(names []string, ipv4, ipv6 bool) (*Plan, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if len(names) == 0 || !ipv4 && !ipv6 {
		return nil, fmt.Errorf("observed-SNI capture requires hostnames and registered IP families")
	}
	if _, err := Build(names); err != nil {
		return nil, err
	}
	names = slices.Clone(names)
	slices.Sort(names)
	names = slices.Compact(names)
	return &Plan{Registration: r, Hostnames: names, IPv4: ipv4, IPv6: ipv6}, nil
}

func (p Plan) Config() (*Config, error) {
	if err := p.Registration.Validate(); err != nil {
		return nil, err
	}
	if len(p.Hostnames) == 0 || !p.IPv4 && !p.IPv6 {
		return nil, fmt.Errorf("empty or family-less capture plan")
	}
	for i, name := range p.Hostnames {
		if i > 0 && p.Hostnames[i-1] >= name {
			return nil, fmt.Errorf("capture hostnames must be canonical")
		}
	}
	config, err := Build(p.Hostnames)
	if err != nil {
		return nil, err
	}
	var resources struct {
		Listeners []object `json:"listeners"`
		Clusters  []object `json:"clusters"`
	}
	if err := json.Unmarshal(config.StaticResources, &resources); err != nil {
		return nil, err
	}
	resources.Listeners = slices.DeleteFunc(resources.Listeners, func(listener object) bool {
		return !p.IPv4 && listener["name"] == "cetirun_egress_ipv4" ||
			!p.IPv6 && listener["name"] == "cetirun_egress_ipv6"
	})
	config.StaticResources, err = json.Marshal(resources)
	if !p.IPv4 {
		config.IPv4Commands = nil
	}
	if !p.IPv6 {
		config.IPv6Commands = nil
	}
	return config, err
}

// Script is mounted from the exact compiler-owned ConfigMap, not HOME or a
// user-controlled image entrypoint. It runs before all ordinary initializers.
// Retries accept only a canonical prefix of the reserved chain and exact OUTPUT
// hooks. They append missing rules, remove duplicate owned hooks, and verify the
// result; foreign contents and operational errors fail closed.
func (p Plan) Script(healthPort int32) (string, error) {
	config, err := p.Config()
	if err != nil {
		return "", err
	}
	if healthPort < 1024 || healthPort > 65535 {
		return "", fmt.Errorf("invalid proxy health port")
	}
	var script strings.Builder
	script.WriteString("#!/bin/sh\nset -eu\numask 077\n")
	script.WriteString("export XTABLES_LOCKFILE=" + LockPath + "/xtables.lock\n")
	script.WriteString("command -v curl >/dev/null\n")
	for _, group := range []struct {
		tool string
		args [][]string
	}{{"iptables", config.IPv4Commands}, {"ip6tables", config.IPv6Commands}} {
		if len(group.args) > 0 {
			fmt.Fprintf(&script, "command -v %s >/dev/null\n%s -w 5 -t nat -S >/dev/null\n", group.tool, group.tool)
		}
	}
	fmt.Fprintf(&script, `attempt=0
until curl --fail --silent --show-error --noproxy '*' --max-time 1 http://127.0.0.1:%d/ready >/dev/null; do
  attempt=$((attempt + 1))
  [ "$attempt" -lt 60 ] || exit 1
  sleep 1
done
`, healthPort)
	fmt.Fprintf(&script, "chain=%s\n", captureChain)
	fmt.Fprintf(&script, `
capture_error() {
  printf '%%s\n' "capture init: $tool has unexpected reserved chain or hook contents" >&2
  exit 1
}

inspect_capture() {
  chain_found=0
  rule_count=0
  hook_count=0
  while IFS= read -r line; do
    case "$line" in
      "-N $chain")
        [ "$chain_found" -eq 0 ] || capture_error
        chain_found=1
        ;;
      "-A $chain "*)
        case "$rule_count:$line" in
          "0:-A $chain -m owner --uid-owner %[1]d -j RETURN") ;;
          "1:-A $chain -d $loopback -j RETURN") ;;
          "2:-A $chain -p tcp -j REDIRECT --to-ports %[2]d") ;;
          *) capture_error ;;
        esac
        rule_count=$((rule_count + 1))
        ;;
      "-A OUTPUT -p tcp -j $chain")
        hook_count=$((hook_count + 1))
        ;;
      *" -j $chain"|*" -g $chain")
        capture_error
        ;;
    esac
  done <<CETIRUN_NAT_SNAPSHOT
$snapshot
CETIRUN_NAT_SNAPSHOT
  if [ "$chain_found" -eq 0 ]; then
    [ "$rule_count:$hook_count" = "0:0" ] || capture_error
  fi
}

reconcile_capture() {
  tool=$1
  loopback=$2
  snapshot=$("$tool" -w 5 -t nat -S)
  inspect_capture
  if [ "$chain_found" -eq 0 ]; then
    "$tool" -w 5 -t nat -N "$chain"
  fi
  if [ "$rule_count" -lt 1 ]; then
    "$tool" -w 5 -t nat -A "$chain" -m owner --uid-owner %[1]d -j RETURN
  fi
  if [ "$rule_count" -lt 2 ]; then
    "$tool" -w 5 -t nat -A "$chain" -d "$loopback" -j RETURN
  fi
  if [ "$rule_count" -lt 3 ]; then
    "$tool" -w 5 -t nat -A "$chain" -p tcp -j REDIRECT --to-ports %[2]d
  fi
  while [ "$hook_count" -gt 1 ]; do
    "$tool" -w 5 -t nat -D OUTPUT -p tcp -j "$chain"
    hook_count=$((hook_count - 1))
  done
  if [ "$hook_count" -eq 0 ]; then
    "$tool" -w 5 -t nat -A OUTPUT -p tcp -j "$chain"
  fi
  snapshot=$("$tool" -w 5 -t nat -S)
  inspect_capture
  [ "$chain_found:$rule_count:$hook_count" = "1:3:1" ] || capture_error
}
`, ProxyUID, CapturePort)
	for _, group := range []struct {
		tool, loopback string
		args           [][]string
	}{{"iptables", "127.0.0.0/8", config.IPv4Commands}, {"ip6tables", "::1/128", config.IPv6Commands}} {
		if len(group.args) > 0 {
			fmt.Fprintf(&script, "reconcile_capture %s %s\n", group.tool, group.loopback)
		}
	}
	return script.String(), nil
}
