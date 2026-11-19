// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandbox

import (
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

func TestNormalizeAgentPolicyRateLimits(t *testing.T) {
	limit := func(value int64) *int64 { return &value }
	policy := func(limits *v1alpha1.AgentPolicyRateLimits) v1alpha1.AgentPolicySpec {
		return v1alpha1.AgentPolicySpec{
			Harnesses:   v1alpha1.AgentPolicyHarnesses{Allowed: []v1alpha1.AgentType{v1alpha1.AgentTypeOpenClaw}},
			ModelAccess: v1alpha1.AgentPolicyModelAccess{RateLimits: limits},
		}
	}
	valid := &v1alpha1.AgentPolicyRateLimits{
		MaxParallelRequests: limit(1), RPM: limit(1), TPM: limit(4096),
		PerModel: map[string]v1alpha1.AgentPolicyModelRateLimit{"example-model": {RPM: limit(1)}, "auto": {TPM: limit(4096)}},
	}
	normalized, errs := NormalizeAgentPolicy(policy(valid))
	if len(errs) != 0 {
		t.Fatal(errs.ToAggregate())
	}
	*valid.RPM = 99
	if *normalized.ModelAccess.RateLimits.RPM != 1 {
		t.Fatal("normalized rate limits share state with the input")
	}
	if _, errs := NormalizeAgentPolicy(policy(nil)); len(errs) != 0 {
		t.Fatalf("a policy without rate limits is valid: %v", errs.ToAggregate())
	}
	tooMany := map[string]v1alpha1.AgentPolicyModelRateLimit{}
	for i := range 65 {
		tooMany["example-model-"+strings.Repeat("x", i)] = v1alpha1.AgentPolicyModelRateLimit{RPM: limit(1)}
	}
	for name, limits := range map[string]*v1alpha1.AgentPolicyRateLimits{
		"zero parallel":      {MaxParallelRequests: limit(0)},
		"parallel too large": {MaxParallelRequests: limit(2147483648)},
		"zero rpm":           {RPM: limit(0)},
		"small tpm":          {TPM: limit(4095)},
		"model zero rpm":     {PerModel: map[string]v1alpha1.AgentPolicyModelRateLimit{"m": {RPM: limit(0)}}},
		"model small tpm":    {PerModel: map[string]v1alpha1.AgentPolicyModelRateLimit{"m": {TPM: limit(1)}}},
		"model without both": {PerModel: map[string]v1alpha1.AgentPolicyModelRateLimit{"m": {}}},
		"model name space":   {PerModel: map[string]v1alpha1.AgentPolicyModelRateLimit{"a b": {RPM: limit(1)}}},
		"too many models":    {PerModel: tooMany},
	} {
		if _, errs := NormalizeAgentPolicy(policy(limits)); len(errs) == 0 {
			t.Errorf("%s: accepted %+v", name, limits)
		}
	}
}
