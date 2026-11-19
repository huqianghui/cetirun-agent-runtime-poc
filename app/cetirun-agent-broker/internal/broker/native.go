// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"context"
	"errors"
	"net/http"
	"reflect"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

func nativeAgentCandidate(agent Agent) bool {
	return agent.PolicyManaged && (agent.RuntimeType == "openclaw" || agent.RuntimeType == "hermes" || agent.RuntimeType == "custom")
}

func (k *kubeClient) nativeStorageRecorded(ctx context.Context, agent Agent) (bool, error) {
	var binding struct {
		Metadata secretMetadata `json:"metadata"`
		RoleRef  struct {
			APIGroup, Kind, Name string
		} `json:"roleRef"`
	}
	_, err := k.request(ctx, http.MethodGet, "/apis/rbac.authorization.k8s.io/v1/namespaces/"+agent.Namespace+"/rolebindings/cetirun-agent-tools-broker", nil, &binding)
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !recordOwnedBy(secretDocument{Metadata: binding.Metadata}, agent) ||
		binding.Metadata.UID == "" ||
		binding.RoleRef.APIGroup != "rbac.authorization.k8s.io" || binding.RoleRef.Kind != "Role" ||
		binding.RoleRef.Name != "cetirun-agent-tools-broker" {
		return false, errDependencyUnavailable
	}
	return true, nil
}

func (k *kubeClient) nativeGet(ctx context.Context, path string, target any) error {
	_, err := k.request(ctx, http.MethodGet, path, nil, target)
	return err
}

func (k *kubeClient) qualifyNativeAgent(ctx context.Context, agent Agent, proof *agenttools.NativePodIdentity) (Agent, error) {
	if !nativeAgentCandidate(agent) {
		return agent, errWorkloadProofInvalid
	}
	if k.nativeOperatorNamespace == "" {
		return agent, errDependencyUnavailable
	}
	if proof == nil {
		var pod struct {
			Metadata agenttools.NativeObjectMeta `json:"metadata"`
			Spec     struct {
				ServiceAccountName string `json:"serviceAccountName"`
			} `json:"spec"`
		}
		podName := workloadServiceAccount(agent.Name) + "-0"
		if err := k.nativeGet(ctx, "/api/v1/namespaces/"+agent.Namespace+"/pods/"+podName, &pod); err != nil {
			return agent, errDependencyUnavailable
		}
		var account struct {
			Metadata agenttools.NativeObjectMeta `json:"metadata"`
		}
		if !agenttools.ValidDNSSubdomainName(pod.Spec.ServiceAccountName) ||
			k.nativeGet(ctx, "/api/v1/namespaces/"+agent.Namespace+"/serviceaccounts/"+pod.Spec.ServiceAccountName, &account) != nil {
			return agent, errDependencyUnavailable
		}
		proof = &agenttools.NativePodIdentity{
			AgentUID: agent.UID, Namespace: agent.Namespace, PodName: podName, PodUID: pod.Metadata.UID,
			ServiceAccountName: pod.Spec.ServiceAccountName, ServiceAccountUID: account.Metadata.UID,
		}
	}
	verifier := agenttools.NativeVerifier{
		Get: k.nativeGet, OperatorNamespace: k.nativeOperatorNamespace,
		Audience: k.nativeAudience, Transport: k.nativeAdmissionTransport,
	}
	if err := verifier.Verify(ctx, *proof); err != nil {
		if errors.Is(err, agenttools.ErrNativeUnavailable) {
			return agent, errDependencyUnavailable
		}
		return agent, errWorkloadProofInvalid
	}
	current, err := k.getAgent(ctx, agent.Name)
	if err != nil {
		return agent, errDependencyUnavailable
	}
	if !reflect.DeepEqual(agent, current) {
		return agent, errWorkloadProofInvalid
	}
	agent.UnsupportedRuntime = false
	agent.ServiceAccount = proof.ServiceAccountName
	return agent, nil
}
