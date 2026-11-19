// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxpolicy

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	corev1 "k8s.io/api/core/v1"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
)

// This is a counterexample experiment, not an FQDN backend. Cilium's toFQDNs
// produces IP allows, and egressDeny cannot use it:
// https://docs.cilium.io/en/stable/security/policy/layer3/#dns-based
// https://docs.cilium.io/en/stable/security/policy/deny/
// Try the proposed inverse (resolved IP denies) through the real CIDR compiler.
// The existing synthetic evaluator checks emitted L3/L4 rules, not a dataplane.
func TestHostnameBlockDNSProjectionFeasibility(t *testing.T) {
	const blocked = "blocked.example.com"
	shared := []string{"198.51.100.42", "2001:db8:1::42"}
	unrelated := []string{"203.0.113.77", "2001:db8:2::77"}
	changed := []string{"198.51.100.99", "2001:db8:1::99"}
	resolver := hostnameExperimentResolver(t, map[string]hostnameDNSAnswer{
		blocked:                     {ips: shared},
		"shared.example.com":        {ips: shared},
		"alias.example.com":         {canonicalName: blocked, ips: shared},
		"child.blocked.example.com": {ips: shared},
		"unrelated.example.net":     {ips: unrelated},
	})
	initialIPs := hostnameExperimentLookup(t, resolver, blocked+".")
	if !slices.Equal(initialIPs, shared) {
		t.Fatalf("initial A/AAAA answers = %v, want %v", initialIPs, shared)
	}
	changedResolver := hostnameExperimentResolver(t, map[string]hostnameDNSAnswer{
		blocked: {ips: changed},
	})
	changedIPs := hostnameExperimentLookup(t, changedResolver, blocked+".")
	if !slices.Equal(changedIPs, changed) {
		t.Fatalf("changed A/AAAA answers = %v, want %v", changedIPs, changed)
	}

	for _, profile := range []struct {
		name    string
		fixture func() CompileInput
	}{
		{"candidate", compilerFixture},
		{"native", nativeCompilerFixture},
	} {
		t.Run(profile.name, func(t *testing.T) {
			request := profile.fixture()
			request.Resolution.AgentPolicy.Network.BlockedFQDNs = []string{blocked}
			intent, errs := sandbox.NormalizeAgentPolicy(request.Resolution.AgentPolicy)
			if len(errs) != 0 || !slices.Equal(intent.Network.BlockedFQDNs, []string{blocked}) {
				t.Fatalf("exact-hostname intent did not normalize: %v, %v", intent.Network.BlockedFQDNs, errs)
			}
			if plan, err := Compile(request); plan != nil || !errors.Is(err, ErrUnsupportedControl) {
				t.Fatalf("production unexpectedly accepted hostname intent: plan=%t, err=%v", plan != nil, err)
			}
			wildcard := profile.fixture().Resolution.AgentPolicy
			wildcard.Network.BlockedFQDNs = []string{"*.blocked.example.com"}
			if _, errs := sandbox.NormalizeAgentPolicy(wildcard); len(errs) == 0 {
				t.Fatal("wildcard intent must not be substituted for an exact-hostname block")
			}

			baseline := mustCompile(t, profile.fixture())
			projected, spec := hostnameExperimentCIDRDeny(t, profile.fixture, initialIPs)
			for _, tc := range []struct {
				name          string
				ips           []string
				exactAllows   bool
				projectAllows bool
			}{
				{blocked, shared, false, false},
				{"shared.example.com", shared, true, false},
				{"alias.example.com", shared, true, false},
				{"child.blocked.example.com", shared, true, false},
				{"unrelated.example.net", unrelated, true, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					ips := hostnameExperimentLookup(t, resolver, tc.name+".")
					if !slices.Equal(ips, sortedUnique(slices.Clone(tc.ips))) {
						t.Fatalf("DNS answers for %s = %v, want %v", tc.name, ips, tc.ips)
					}
					if slices.Contains(intent.Network.BlockedFQDNs, tc.name) == tc.exactAllows {
						t.Fatal("fixture disagrees with the API's exact-name contract")
					}
					for _, ip := range ips {
						for _, protocol := range []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP} {
							if !syntheticEgressAllowed(baseline.Network.Policy.Spec, ip, nil, 443, protocol) {
								t.Fatalf("baseline unexpectedly denies %s/%s", ip, protocol)
							}
							got := syntheticEgressAllowed(spec, ip, nil, 443, protocol)
							if got != tc.projectAllows {
								t.Fatalf("projected deny for %s (%s/%s): allowed=%t, want %t",
									tc.name, ip, protocol, got, tc.projectAllows)
							}
							if got != tc.exactAllows {
								t.Logf("NO-GO: %s (%s/%s) should remain allowed by exact-name intent but is denied",
									tc.name, ip, protocol)
							}
						}
					}
				})
			}

			t.Run("direct-IP-has-no-DNS-name", func(t *testing.T) {
				noDNS := &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
					t.Error("IP-literal lookup unexpectedly attempted DNS")
					return nil, errors.New("DNS is forbidden for the IP-literal control")
				}}
				for _, ip := range shared {
					if got := hostnameExperimentLookup(t, noDNS, ip); !slices.Equal(got, []string{ip}) {
						t.Fatalf("IP-literal lookup = %v, want %s", got, ip)
					}
					if syntheticEgressAllowed(spec, ip, nil, 443, corev1.ProtocolTCP) {
						t.Fatal("CIDR projection unexpectedly distinguishes literal access to the same IP")
					}
				}
				// No hostname is observable here. This does not define a new API
				// permission for literal access or claim that DNS filtering stops it.
			})

			t.Run("DNS-rotation", func(t *testing.T) {
				replaced, replacedSpec := hostnameExperimentCIDRDeny(t, profile.fixture, changedIPs)
				union, unionSpec := hostnameExperimentCIDRDeny(t, profile.fixture, append(slices.Clone(initialIPs), changedIPs...))
				if projected.Revision == replaced.Revision || projected.Revision == union.Revision ||
					replaced.Revision == union.Revision {
					t.Fatal("changing the IP projection did not change the compiled revision")
				}
				for _, state := range []struct {
					name       string
					spec       CiliumPolicySpec
					oldAllowed bool
					newAllowed bool
				}{
					{"before-refresh", spec, false, true},
					{"replace-snapshot", replacedSpec, true, false},
					{"retain-old-and-new", unionSpec, false, false},
				} {
					t.Run(state.name, func(t *testing.T) {
						for _, addresses := range []struct {
							ips     []string
							allowed bool
						}{
							{initialIPs, state.oldAllowed},
							{changedIPs, state.newAllowed},
						} {
							for _, ip := range addresses.ips {
								for _, protocol := range []corev1.Protocol{corev1.ProtocolTCP, corev1.ProtocolUDP} {
									if got := syntheticEgressAllowed(state.spec, ip, nil, 443, protocol); got != addresses.allowed {
										t.Fatalf("%s/%s allowed=%t, want %t", ip, protocol, got, addresses.allowed)
									}
								}
							}
						}
					})
				}
			})
		})
	}
}

func hostnameExperimentCIDRDeny(t *testing.T, fixture func() CompileInput, ips []string) (*Plan, CiliumPolicySpec) {
	t.Helper()
	// Start with separate, supported CIDR-only input; never clear a requested
	// FQDN rule, enable its capability, or call around Compile's support checks.
	input := fixture()
	if input.Resolution.Capabilities.BlockedFQDNs || len(input.Resolution.AgentPolicy.Network.BlockedFQDNs) != 0 {
		t.Fatal("the IP-only experiment must not declare hostname support")
	}
	input.Resolution.AgentPolicy.Network.BlockedCIDRs = ipCIDRs(ips)
	plan := mustCompile(t, input)
	raw, err := plan.Resources[0].MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var rendered CiliumNetworkPolicy
	if err := json.Unmarshal(raw, &rendered); err != nil {
		t.Fatal(err)
	}
	if rendered.Kind != "CiliumNetworkPolicy" {
		t.Fatalf("expected a rendered Cilium policy, got %s", rendered.Kind)
	}
	return plan, rendered.Spec
}

type hostnameDNSAnswer struct {
	canonicalName string
	ips           []string
}

// Go's real resolver consumes framed DNS responses over net.Pipe. There is no
// socket, system DNS query, Cilium process, TTL timer or mock hostname matcher.
func hostnameExperimentResolver(t *testing.T, answers map[string]hostnameDNSAnswer) *net.Resolver {
	t.Helper()
	return &net.Resolver{
		PreferGo: true, StrictErrors: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			client, server := net.Pipe()
			if deadline, ok := ctx.Deadline(); ok {
				if err := server.SetDeadline(deadline); err != nil {
					_ = client.Close()
					_ = server.Close()
					return nil, err
				}
			}
			done := make(chan error, 1)
			go func() {
				defer server.Close()
				done <- hostnameExperimentDNSReply(server, answers)
			}()
			t.Cleanup(func() {
				_ = client.Close()
				_ = server.Close()
				if err := <-done; err != nil {
					t.Errorf("in-memory DNS exchange: %v", err)
				}
			})
			return client, nil
		},
	}
}

func hostnameExperimentLookup(t *testing.T, resolver *net.Resolver, name string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// One answer per family avoids Go's UDP source-address probes used when
	// sorting multiple destinations. Literal addresses still use the real resolver.
	networks := []string{"ip4", "ip6"}
	if _, err := netip.ParseAddr(name); err == nil {
		networks = []string{"ip"}
	}
	var result []string
	for _, network := range networks {
		ips, err := resolver.LookupNetIP(ctx, network, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, ip := range ips {
			// LookupNetIP can return an IPv4 literal as a mapped IPv6 address.
			result = append(result, ip.Unmap().String())
		}
	}
	return sortedUnique(result)
}

func hostnameExperimentDNSReply(conn net.Conn, answers map[string]hostnameDNSAnswer) error {
	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return err
	}
	queryWire := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err := io.ReadFull(conn, queryWire); err != nil {
		return err
	}
	var query dnsmessage.Message
	if err := query.Unpack(queryWire); err != nil {
		return err
	}
	if len(query.Questions) != 1 {
		return fmt.Errorf("expected one DNS question, got %d", len(query.Questions))
	}
	q := query.Questions[0]
	if q.Class != dnsmessage.ClassINET || q.Type != dnsmessage.TypeA && q.Type != dnsmessage.TypeAAAA {
		return fmt.Errorf("unexpected DNS question type/class: %v/%v", q.Type, q.Class)
	}
	answer, ok := answers[strings.TrimSuffix(q.Name.String(), ".")]
	if !ok {
		return fmt.Errorf("DNS name is not in the synthetic fixture: %s", q.Name)
	}
	reply := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID: query.ID, Response: true, Authoritative: true,
			RecursionDesired: query.RecursionDesired, RecursionAvailable: true,
		},
		Questions: query.Questions,
	}
	owner := q.Name
	if answer.canonicalName != "" {
		canonical, err := dnsmessage.NewName(answer.canonicalName + ".")
		if err != nil {
			return err
		}
		reply.Answers = append(reply.Answers, dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{Name: owner, Type: dnsmessage.TypeCNAME, Class: dnsmessage.ClassINET, TTL: 30},
			Body:   &dnsmessage.CNAMEResource{CNAME: canonical},
		})
		owner = canonical
	}
	for _, raw := range answer.ips {
		ip, err := netip.ParseAddr(raw)
		if err != nil {
			return err
		}
		resource := dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{Name: owner, Type: q.Type, Class: dnsmessage.ClassINET, TTL: 30},
		}
		switch {
		case q.Type == dnsmessage.TypeA && ip.Is4():
			resource.Body = &dnsmessage.AResource{A: ip.As4()}
		case q.Type == dnsmessage.TypeAAAA && ip.Is6():
			resource.Body = &dnsmessage.AAAAResource{AAAA: ip.As16()}
		default:
			continue
		}
		reply.Answers = append(reply.Answers, resource)
	}
	wire, err := reply.Pack()
	if err != nil {
		return err
	}
	binary.BigEndian.PutUint16(size[:], uint16(len(wire)))
	_, err = conn.Write(append(size[:], wire...))
	return err
}
