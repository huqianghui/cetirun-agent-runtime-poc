// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package agenttools

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"time"
)

const (
	NativePodWebhookConfiguration = "cetirun-sandbox-pods"
	NativePodWebhookName          = "sandbox-pods.cetirun.microsoft.com"
	NativePodWebhookPath          = "/validate-sandbox-pods"
)

var (
	ErrNativeProofInvalid = errors.New("native workload proof is invalid")
	ErrNativeUnavailable  = errors.New("native workload authority is unavailable")
)

type NativeObjectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid"`
	ResourceVersion   string            `json:"resourceVersion"`
	Generation        int64             `json:"generation"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
	Annotations       map[string]string `json:"annotations"`
	Labels            map[string]string `json:"labels"`
	Finalizers        []string          `json:"finalizers"`
	OwnerReferences   []struct {
		APIVersion, Kind, Name, UID    string
		Controller, BlockOwnerDeletion bool
	} `json:"ownerReferences"`
}

// nativeAgentObservation contains the Agent fields that the native proof
// authorizes. ResourceVersion deliberately remains metadata for the live-read
// checks, but is not part of the final Agent fence: status reconciliation can
// change it without changing this workload's authorization.
type nativeAgentObservation struct {
	Metadata NativeObjectMeta `json:"metadata"`
	Spec     struct {
		Suspended bool `json:"suspended"`
	} `json:"spec"`
	Status struct {
		Namespace string `json:"namespace"`
	} `json:"status"`
}

func sameNativeAgentObservation(before, after nativeAgentObservation) bool {
	if before.Metadata.ResourceVersion == "" || after.Metadata.ResourceVersion == "" ||
		before.Metadata.Generation <= 0 || after.Metadata.Generation <= 0 {
		return false
	}
	before.Metadata.ResourceVersion = ""
	after.Metadata.ResourceVersion = ""
	return reflect.DeepEqual(before, after)
}

// NativePodIdentity is an observed identity, not an eligibility flag. A token
// caller supplies these values only through independently verified TokenReview
// and live API reads; management callers derive them from the live primary Pod.
type NativePodIdentity struct {
	AgentUID, Namespace, PodName, PodUID, ServiceAccountName, ServiceAccountUID string
}

type NativeAPIReader func(context.Context, string, any) error

var nativeReferenceName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// ValidDNSSubdomainName matches Kubernetes DNS1123 subdomain names used by
// ServiceAccounts and Pods, including their 253-byte total length limit.
func ValidDNSSubdomainName(name string) bool {
	return len(name) <= 253 && nativeReferenceName.MatchString(name)
}

// NativeVerifier reuses the existing common-executor Pod admission contract.
// It never writes a Pod or stores a second identity/expectation record.
type NativeVerifier struct {
	Get               NativeAPIReader
	OperatorNamespace string
	Audience          string
	// Transport is only for local served-interface tests. Production uses the
	// current admissionregistration CA and never environment proxy settings.
	Transport http.RoundTripper
}

func (v NativeVerifier) Verify(ctx context.Context, identity NativePodIdentity) error {
	if v.Get == nil || len(v.OperatorNamespace) > 63 || strings.Contains(v.OperatorNamespace, ".") ||
		!nativeReferenceName.MatchString(v.OperatorNamespace) || v.Audience == "" {
		return ErrNativeUnavailable
	}
	if identity.AgentUID == "" || identity.Namespace == "" || identity.PodName == "" || identity.PodUID == "" ||
		identity.ServiceAccountName == "" || identity.ServiceAccountUID == "" {
		return ErrNativeProofInvalid
	}
	for _, name := range []string{identity.Namespace, identity.PodName, identity.ServiceAccountName} {
		if !ValidDNSSubdomainName(name) {
			return ErrNativeProofInvalid
		}
	}
	var namespace, account struct {
		Metadata NativeObjectMeta `json:"metadata"`
	}
	namespacePath := "/api/v1/namespaces/" + identity.Namespace
	accountPath := namespacePath + "/serviceaccounts/" + identity.ServiceAccountName
	if v.Get(ctx, namespacePath, &namespace) != nil || v.Get(ctx, accountPath, &account) != nil {
		return ErrNativeUnavailable
	}
	ns := namespace.Metadata
	if ns.UID == "" || ns.ResourceVersion == "" || ns.Name != identity.Namespace || ns.DeletionTimestamp != nil ||
		ns.Labels["cetirun.microsoft.com/sandbox-protected"] != "true" || len(ns.OwnerReferences) != 1 ||
		ns.OwnerReferences[0].APIVersion != "cetirun.microsoft.com/v1alpha1" || ns.OwnerReferences[0].Kind != "Agent" ||
		ns.OwnerReferences[0].UID != identity.AgentUID || !ns.OwnerReferences[0].Controller ||
		account.Metadata.UID != identity.ServiceAccountUID || account.Metadata.ResourceVersion == "" || account.Metadata.Name != identity.ServiceAccountName ||
		account.Metadata.Namespace != identity.Namespace || account.Metadata.DeletionTimestamp != nil {
		return ErrNativeProofInvalid
	}
	var agent nativeAgentObservation
	agentName := ns.OwnerReferences[0].Name
	if len(agentName) > 253 || !nativeReferenceName.MatchString(agentName) || v.Get(ctx, "/apis/cetirun.microsoft.com/v1alpha1/agents/"+agentName, &agent) != nil {
		return ErrNativeUnavailable
	}
	if agent.Metadata.UID != identity.AgentUID || agent.Metadata.ResourceVersion == "" || agent.Metadata.Generation <= 0 || agent.Metadata.Name != agentName ||
		agent.Metadata.DeletionTimestamp != nil || agent.Spec.Suspended || agent.Status.Namespace != identity.Namespace {
		return ErrNativeProofInvalid
	}
	for _, finalizer := range agent.Metadata.Finalizers {
		if finalizer == "cetirun.microsoft.com/sandbox-hold" {
			return ErrNativeProofInvalid
		}
	}
	path := "/api/v1/namespaces/" + identity.Namespace + "/pods/" + identity.PodName
	var pod json.RawMessage
	if err := v.Get(ctx, path, &pod); err != nil {
		return ErrNativeUnavailable
	}
	if !nativeProjectionMatches(pod, identity, v.Audience) {
		return ErrNativeProofInvalid
	}
	var configuration nativeWebhookConfiguration
	key := "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations/" + NativePodWebhookConfiguration
	if err := v.Get(ctx, key, &configuration); err != nil {
		return ErrNativeUnavailable
	}
	endpoint, ca, ok := configuration.endpoint(v.OperatorNamespace)
	if !ok {
		return ErrNativeUnavailable
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return ErrNativeUnavailable
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return ErrNativeUnavailable
	}
	uid := hex.EncodeToString(nonce)
	review := map[string]any{
		"apiVersion": "admission.k8s.io/v1", "kind": "AdmissionReview",
		"request": map[string]any{
			"uid": uid, "kind": map[string]string{"group": "", "version": "v1", "kind": "Pod"},
			"resource": map[string]string{"group": "", "version": "v1", "resource": "pods"},
			"name":     identity.PodName, "namespace": identity.Namespace, "operation": "UPDATE",
			"subResource": "", "dryRun": true, "object": pod, "oldObject": pod,
		},
	}
	data, err := json.Marshal(review)
	if err != nil {
		return ErrNativeUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return ErrNativeUnavailable
	}
	request.Header.Set("Content-Type", "application/json")
	transport := v.Transport
	if transport == nil {
		transport = &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}}
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return ErrNativeUnavailable
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
	if err != nil || len(body) > 64<<10 || response.StatusCode != http.StatusOK {
		return ErrNativeUnavailable
	}
	var result struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Response   *struct {
			UID     string `json:"uid"`
			Allowed bool   `json:"allowed"`
			Status  *struct {
				Code int `json:"code"`
			} `json:"status"`
		} `json:"response"`
	}
	if json.Unmarshal(body, &result) != nil || result.APIVersion != "admission.k8s.io/v1" ||
		result.Kind != "AdmissionReview" || result.Response == nil || result.Response.UID != uid {
		return ErrNativeUnavailable
	}
	if !result.Response.Allowed {
		if result.Response.Status != nil && result.Response.Status.Code >= 500 {
			return ErrNativeUnavailable
		}
		return ErrNativeProofInvalid
	}
	if result.Response.Status != nil && result.Response.Status.Code != 0 && result.Response.Status.Code != http.StatusOK {
		return ErrNativeUnavailable
	}
	// The common admission handler rechecks expectation/source/reservation
	// identity. Also fence the Pod and trust configuration observed by this caller.
	var currentPod json.RawMessage
	var current nativeWebhookConfiguration
	var currentNamespace, currentAccount struct {
		Metadata NativeObjectMeta `json:"metadata"`
	}
	var currentAgent nativeAgentObservation
	if v.Get(ctx, path, &currentPod) != nil || v.Get(ctx, key, &current) != nil ||
		v.Get(ctx, namespacePath, &currentNamespace) != nil || v.Get(ctx, accountPath, &currentAccount) != nil ||
		v.Get(ctx, "/apis/cetirun.microsoft.com/v1alpha1/agents/"+agentName, &currentAgent) != nil {
		return ErrNativeUnavailable
	}
	var before, after struct {
		Metadata NativeObjectMeta `json:"metadata"`
	}
	if json.Unmarshal(pod, &before) != nil || json.Unmarshal(currentPod, &after) != nil ||
		before.Metadata.UID != after.Metadata.UID || before.Metadata.ResourceVersion != after.Metadata.ResourceVersion ||
		!nativeProjectionMatches(currentPod, identity, v.Audience) ||
		!reflect.DeepEqual(configuration, current) ||
		!sameNativeAgentObservation(agent, currentAgent) ||
		ns.UID != currentNamespace.Metadata.UID || ns.ResourceVersion != currentNamespace.Metadata.ResourceVersion ||
		account.Metadata.UID != currentAccount.Metadata.UID || account.Metadata.ResourceVersion != currentAccount.Metadata.ResourceVersion {
		return ErrNativeProofInvalid
	}
	return nil
}

type nativeWebhookConfiguration struct {
	Metadata NativeObjectMeta `json:"metadata"`
	Webhooks []struct {
		Name                    string   `json:"name"`
		AdmissionReviewVersions []string `json:"admissionReviewVersions"`
		FailurePolicy           string   `json:"failurePolicy"`
		SideEffects             string   `json:"sideEffects"`
		ClientConfig            struct {
			URL      string `json:"url"`
			CABundle []byte `json:"caBundle"`
			Service  *struct {
				Name, Namespace, Path string
				Port                  int
			} `json:"service"`
		} `json:"clientConfig"`
	} `json:"webhooks"`
}

func (c nativeWebhookConfiguration) endpoint(namespace string) (string, []byte, bool) {
	if c.Metadata.Name != NativePodWebhookConfiguration || c.Metadata.UID == "" ||
		c.Metadata.ResourceVersion == "" || c.Metadata.DeletionTimestamp != nil || len(c.Webhooks) != 1 {
		return "", nil, false
	}
	hook := c.Webhooks[0]
	service := hook.ClientConfig.Service
	if hook.Name != NativePodWebhookName || hook.FailurePolicy != "Fail" || hook.SideEffects != "None" ||
		len(hook.AdmissionReviewVersions) != 1 || hook.AdmissionReviewVersions[0] != "v1" ||
		hook.ClientConfig.URL != "" || service == nil || service.Namespace != namespace ||
		service.Name != "cetirun-sandbox-admission" || service.Path != NativePodWebhookPath || service.Port != 443 {
		return "", nil, false
	}
	return "https://" + service.Name + "." + namespace + ".svc" + service.Path, hook.ClientConfig.CABundle, true
}

type nativeContainer struct {
	Name         string `json:"name"`
	VolumeMounts []struct {
		Name, MountPath, SubPath, SubPathExpr string
		ReadOnly                              bool
	} `json:"volumeMounts"`
}

func nativeProjectionMatches(raw []byte, identity NativePodIdentity, audience string) bool {
	var pod struct {
		APIVersion string           `json:"apiVersion"`
		Kind       string           `json:"kind"`
		Metadata   NativeObjectMeta `json:"metadata"`
		Spec       struct {
			ServiceAccountName           string `json:"serviceAccountName"`
			AutomountServiceAccountToken *bool  `json:"automountServiceAccountToken"`
			Volumes                      []struct {
				Name      string `json:"name"`
				Projected *struct {
					Sources []struct {
						ServiceAccountToken *struct {
							Audience          string `json:"audience"`
							Path              string `json:"path"`
							ExpirationSeconds int64  `json:"expirationSeconds"`
						} `json:"serviceAccountToken"`
					} `json:"sources"`
				} `json:"projected"`
			} `json:"volumes"`
			Containers     []nativeContainer `json:"containers"`
			InitContainers []nativeContainer `json:"initContainers"`
		} `json:"spec"`
	}
	if json.Unmarshal(raw, &pod) != nil || pod.APIVersion != "v1" || pod.Kind != "Pod" || pod.Metadata.UID != identity.PodUID ||
		pod.Metadata.Name != identity.PodName || pod.Metadata.Namespace != identity.Namespace ||
		pod.Metadata.ResourceVersion == "" || pod.Metadata.DeletionTimestamp != nil ||
		pod.Metadata.Annotations["sandbox.cetirun.microsoft.com/reconciled"] != "true" ||
		pod.Metadata.Annotations["cetirun.microsoft.com/agent-uid"] != identity.AgentUID ||
		pod.Spec.ServiceAccountName != identity.ServiceAccountName ||
		pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken ||
		len(pod.Spec.Containers) == 0 || pod.Spec.Containers[0].Name != "cetirun" {
		return false
	}
	volumes, mounts := 0, 0
	for _, volume := range pod.Spec.Volumes {
		if volume.Name != "connections-identity" {
			continue
		}
		if volume.Projected == nil || len(volume.Projected.Sources) != 1 {
			return false
		}
		token := volume.Projected.Sources[0].ServiceAccountToken
		if token == nil || token.Audience != audience || token.Path != "token" || token.ExpirationSeconds != 600 {
			return false
		}
		volumes++
	}
	for index, container := range append(pod.Spec.Containers, pod.Spec.InitContainers...) {
		for _, mount := range container.VolumeMounts {
			if mount.Name == "connections-identity" {
				if index != 0 || mount.MountPath != "/var/run/cetirun/connections" || !mount.ReadOnly || mount.SubPath != "" || mount.SubPathExpr != "" {
					return false
				}
				mounts++
			}
		}
	}
	return volumes == 1 && mounts == 1 && !strings.ContainsAny(identity.ServiceAccountName, "/\x00")
}
