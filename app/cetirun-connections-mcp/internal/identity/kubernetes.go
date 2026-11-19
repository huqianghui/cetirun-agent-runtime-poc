// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package identity

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/config"
	"github.com/Azure/CetiRun/app/internal/agenttools"
)

const maxKubernetesResponse = 1 << 20

// Authentication errors contain no credentials or Kubernetes response details.
// Only a definitive credential or live-binding rejection is unauthenticated.
var (
	ErrUnauthenticated       = errors.New("unauthenticated")
	ErrDependencyUnavailable = errors.New("dependency_unavailable")
	errObjectNotFound        = errors.New("Kubernetes object not found")
)

type Kubernetes struct {
	operatorNamespace        string
	nativeAdmissionTransport http.RoundTripper
	baseURL                  string
	tokenFile                string
	client                   *http.Client
	audience                 string
}

type Caller struct {
	PolicyManaged            bool
	CredentialPolicyRevision string
	Proof                    string
	InstanceUID              string
}

type tokenReview struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		Token     string   `json:"token"`
		Audiences []string `json:"audiences"`
	} `json:"spec"`
	Status struct {
		Authenticated bool     `json:"authenticated"`
		Audiences     []string `json:"audiences"`
		User          struct {
			Username string              `json:"username"`
			UID      string              `json:"uid"`
			Extra    map[string][]string `json:"extra"`
		} `json:"user"`
	} `json:"status"`
}

type ownerReference struct {
	APIVersion         string `json:"apiVersion"`
	Kind               string `json:"kind"`
	Name               string `json:"name"`
	UID                string `json:"uid"`
	Controller         bool   `json:"controller"`
	BlockOwnerDeletion bool   `json:"blockOwnerDeletion"`
}
type objectMeta struct {
	OwnerReferences   []ownerReference  `json:"ownerReferences"`
	Annotations       map[string]string `json:"annotations"`
	Labels            map[string]string `json:"labels"`
	Finalizers        []string          `json:"finalizers"`
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid"`
	ResourceVersion   string            `json:"resourceVersion"`
	Generation        int64             `json:"generation"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
}

type pod struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		ServiceAccountName string `json:"serviceAccountName"`
	} `json:"spec"`
	Status struct {
		Phase      string                          `json:"phase"`
		Conditions []struct{ Type, Status string } `json:"conditions"`
	} `json:"status"`
}

type serviceAccount struct {
	Metadata objectMeta `json:"metadata"`
}

type agent struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Suspended bool `json:"suspended"`
	} `json:"spec"`
	Status struct {
		Policy        *json.RawMessage `json:"policy"`
		Phase         string           `json:"phase"`
		Namespace     string           `json:"namespace"`
		AgentIdentity *struct {
			CredentialPolicyRevision string `json:"credentialPolicyRevision"`
			Phase                    string `json:"phase"`
			ClientID                 string `json:"clientId"`
			ObjectID                 string `json:"objectId"`
			DisplayName              string `json:"displayName"`
			CreationAttempted        bool   `json:"creationAttempted"`
			ObservedGeneration       int64  `json:"observedGeneration"`
			Reason                   string `json:"reason"`
		} `json:"agentIdentity"`
	} `json:"status"`
}

// nativeAgentAuthorization is the Agent state that may grant native workload
// access. ResourceVersion intentionally is not included: Operator status
// heartbeats update it without changing authorization. Generation and all
// modeled metadata are retained, so spec/owner/finalizer lifecycle changes
// remain a fail-closed change between the admission verification and caller
// construction.
type nativeAgentAuthorization struct {
	Metadata              objectMeta
	Suspended             bool
	Policy                string
	Phase                 string
	Namespace             string
	AgentIdentityPresent  bool
	CredentialPolicy      string
	AgentIdentityPhase    string
	AgentIdentityClientID string
	AgentIdentityObjectID string
	AgentIdentityName     string
	AgentIdentityCreated  bool
	AgentIdentityObserved int64
	AgentIdentityReason   string
}

func nativeAgentAuthorizationState(item agent) nativeAgentAuthorization {
	metadata := item.Metadata
	metadata.ResourceVersion = ""
	policy := ""
	if item.Status.Policy != nil {
		policy = string(*item.Status.Policy)
	}
	state := nativeAgentAuthorization{
		Metadata: metadata, Suspended: item.Spec.Suspended, Policy: policy,
		Phase: item.Status.Phase, Namespace: item.Status.Namespace,
	}
	if item.Status.AgentIdentity != nil {
		state.AgentIdentityPresent = true
		state.CredentialPolicy = item.Status.AgentIdentity.CredentialPolicyRevision
		state.AgentIdentityPhase = item.Status.AgentIdentity.Phase
		state.AgentIdentityClientID = item.Status.AgentIdentity.ClientID
		state.AgentIdentityObjectID = item.Status.AgentIdentity.ObjectID
		state.AgentIdentityName = item.Status.AgentIdentity.DisplayName
		state.AgentIdentityCreated = item.Status.AgentIdentity.CreationAttempted
		state.AgentIdentityObserved = item.Status.AgentIdentity.ObservedGeneration
		state.AgentIdentityReason = item.Status.AgentIdentity.Reason
	}
	return state
}

func NewKubernetes(cfg config.Config) (*Kubernetes, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("Kubernetes service environment is unavailable")
	}
	if _, err := os.Stat(cfg.KubernetesTokenFile); err != nil {
		return nil, errors.New("Kubernetes service account token is unavailable")
	}
	ca, err := os.ReadFile(cfg.KubernetesCAFile)
	if err != nil {
		return nil, errors.New("Kubernetes service CA is unavailable")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("Kubernetes service CA is invalid")
	}
	return &Kubernetes{
		operatorNamespace: cfg.OperatorNamespace,
		baseURL:           "https://" + host + ":" + port,
		tokenFile:         cfg.KubernetesTokenFile,
		audience:          cfg.WorkloadTokenAudience,
		client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    pool,
		}}},
	}, nil
}

func (k *Kubernetes) Authenticate(ctx context.Context, bearer string) (Caller, error) {
	if strings.TrimSpace(bearer) == "" {
		return Caller{}, ErrUnauthenticated
	}
	var review tokenReview
	review.APIVersion, review.Kind = "authentication.k8s.io/v1", "TokenReview"
	review.Spec.Token = bearer
	review.Spec.Audiences = []string{k.audience}
	if err := k.request(ctx, http.MethodPost, "/apis/authentication.k8s.io/v1/tokenreviews", review, &review); err != nil {
		return Caller{}, err
	}
	// A completed negative TokenReview rejects the credential, including
	// invalid or expired tokens described in its untrusted status.error text.
	if !review.Status.Authenticated || !audienceMatches(review.Status.Audiences, k.audience) {
		return Caller{}, ErrUnauthenticated
	}
	namespace, serviceAccountName, ok := serviceAccountIdentity(review.Status.User.Username)
	if !ok {
		return Caller{}, ErrUnauthenticated
	}
	podName, podUID := extraValue(review.Status.User.Extra, "authentication.kubernetes.io/pod-name"), extraValue(review.Status.User.Extra, "authentication.kubernetes.io/pod-uid")
	serviceAccountUID := review.Status.User.UID
	if !agenttools.ValidDNSSubdomainName(podName) || podUID == "" || serviceAccountUID == "" {
		return Caller{}, ErrUnauthenticated
	}
	var currentPod pod
	if err := k.getWorkload(ctx, "/api/v1/namespaces/"+namespace+"/pods/"+podName, &currentPod); err != nil {
		return Caller{}, err
	}
	if currentPod.Metadata.DeletionTimestamp != nil || currentPod.Metadata.UID != podUID || currentPod.Metadata.Namespace != namespace || currentPod.Spec.ServiceAccountName != serviceAccountName {
		return Caller{}, ErrUnauthenticated
	}
	var currentServiceAccount serviceAccount
	if err := k.getWorkload(ctx, "/api/v1/namespaces/"+namespace+"/serviceaccounts/"+serviceAccountName, &currentServiceAccount); err != nil {
		return Caller{}, err
	}
	if currentServiceAccount.Metadata.DeletionTimestamp != nil || currentServiceAccount.Metadata.UID != serviceAccountUID || currentServiceAccount.Metadata.UID != review.Status.User.UID {
		return Caller{}, ErrUnauthenticated
	}
	// Namespace ownership locates the Agent without a cluster-wide list. The
	// owner reference is only a locator: compare it with the live Agent below.
	var ns struct {
		Metadata objectMeta `json:"metadata"`
	}
	if err := k.getWorkload(ctx, "/api/v1/namespaces/"+namespace, &ns); err != nil {
		return Caller{}, err
	}
	if ns.Metadata.DeletionTimestamp != nil || ns.Metadata.Name != namespace {
		return Caller{}, ErrUnauthenticated
	}
	var owner ownerReference
	foundOwner := false
	for _, ref := range ns.Metadata.OwnerReferences {
		if ref.Kind == "Agent" && ref.APIVersion == "cetirun.microsoft.com/v1alpha1" {
			if foundOwner {
				return Caller{}, ErrUnauthenticated
			}
			owner, foundOwner = ref, true
		}
	}
	if owner.UID == "" || !dnsLabel(owner.Name) {
		return Caller{}, ErrUnauthenticated
	}
	var item agent
	if err := k.getWorkload(ctx, "/apis/cetirun.microsoft.com/v1alpha1/agents/"+owner.Name, &item); err != nil {
		return Caller{}, err
	}
	if item.Metadata.Name != owner.Name || item.Metadata.UID != owner.UID || item.Metadata.DeletionTimestamp != nil || item.Spec.Suspended || item.Status.Phase != "Ready" || item.Status.Namespace != namespace ||
		(item.Status.Policy == nil && instanceServiceName(item.Metadata.Name) != serviceAccountName) {
		return Caller{}, ErrUnauthenticated
	}
	if item.Status.Policy != nil {
		if item.Metadata.ResourceVersion == "" || item.Metadata.Generation <= 0 {
			return Caller{}, ErrUnauthenticated
		}
		ready := false
		for _, condition := range currentPod.Status.Conditions {
			ready = ready || condition.Type == "Ready" && condition.Status == "True"
		}
		if currentPod.Status.Phase != "Running" || !ready {
			return Caller{}, ErrUnauthenticated
		}
		verifier := agenttools.NativeVerifier{Get: k.Get, OperatorNamespace: k.operatorNamespace,
			Audience: k.audience, Transport: k.nativeAdmissionTransport}
		err := verifier.Verify(ctx, agenttools.NativePodIdentity{
			AgentUID: item.Metadata.UID, Namespace: namespace, PodName: podName, PodUID: podUID,
			ServiceAccountName: serviceAccountName, ServiceAccountUID: serviceAccountUID,
		})
		if errors.Is(err, agenttools.ErrNativeUnavailable) {
			return Caller{}, ErrDependencyUnavailable
		}
		if err != nil {
			return Caller{}, ErrUnauthenticated
		}
		var current agent
		if err := k.getWorkload(ctx, "/apis/cetirun.microsoft.com/v1alpha1/agents/"+owner.Name, &current); err != nil {
			return Caller{}, err
		}
		if current.Metadata.ResourceVersion == "" || current.Metadata.Generation <= 0 ||
			!reflect.DeepEqual(nativeAgentAuthorizationState(item), nativeAgentAuthorizationState(current)) {
			return Caller{}, ErrUnauthenticated
		}
	}
	caller := Caller{Proof: bearer, InstanceUID: item.Metadata.UID, PolicyManaged: item.Status.Policy != nil}
	if item.Status.AgentIdentity != nil {
		caller.CredentialPolicyRevision = item.Status.AgentIdentity.CredentialPolicyRevision
	}
	return caller, nil
}

func (k *Kubernetes) getWorkload(ctx context.Context, path string, result any) error {
	err := k.request(ctx, http.MethodGet, path, nil, result)
	if errors.Is(err, errObjectNotFound) {
		// A missing bound object invalidates the workload proof. Other API
		// failures do not establish whether the workload is still valid.
		return ErrUnauthenticated
	}
	return err
}

func audienceMatches(audiences []string, expected string) bool {
	for _, audience := range audiences {
		if audience == expected && expected != "" {
			return true
		}
	}
	return false
}

func (k *Kubernetes) Get(ctx context.Context, path string, result any) error {
	return k.request(ctx, http.MethodGet, path, nil, result)
}

func (k *Kubernetes) Create(ctx context.Context, path string, value, result any) error {
	return k.request(ctx, http.MethodPost, path, value, result)
}

func (k *Kubernetes) Update(ctx context.Context, path string, value, result any) error {
	return k.request(ctx, http.MethodPut, path, value, result)
}

func (k *Kubernetes) request(ctx context.Context, method, path string, body any, result any) error {
	if !strings.HasPrefix(path, "/") || strings.Contains(path, "..") {
		return ErrDependencyUnavailable
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return ErrDependencyUnavailable
		}
		reader = bytes.NewReader(encoded)
	}
	token, err := os.ReadFile(k.tokenFile)
	if err != nil || strings.TrimSpace(string(token)) == "" {
		return ErrDependencyUnavailable
	}
	request, err := http.NewRequestWithContext(ctx, method, k.baseURL+path, reader)
	if err != nil {
		return ErrDependencyUnavailable
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := k.client.Do(request)
	if err != nil {
		return ErrDependencyUnavailable
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxKubernetesResponse+1))
	if err != nil || len(payload) > maxKubernetesResponse {
		return ErrDependencyUnavailable
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if method == http.MethodGet && response.StatusCode == http.StatusNotFound {
			return errObjectNotFound
		}
		// This request uses the MCP server's credential, not the workload
		// proof. Even HTTP 401/403 indicates a server dependency failure.
		return ErrDependencyUnavailable
	}
	if result != nil {
		if err := json.Unmarshal(payload, result); err != nil || bytes.Equal(bytes.TrimSpace(payload), []byte("null")) {
			return ErrDependencyUnavailable
		}
	}
	return nil
}

func serviceAccountIdentity(username string) (string, string, bool) {
	parts := strings.Split(username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" || !dnsLabel(parts[2]) || !agenttools.ValidDNSSubdomainName(parts[3]) {
		return "", "", false
	}
	return parts[2], parts[3], true
}

func extraValue(extra map[string][]string, key string) string {
	values := extra[key]
	if len(values) != 1 {
		return ""
	}
	return values[0]
}

// instanceServiceName mirrors the Agent chart fullname convention used by the Broker.
func instanceServiceName(instanceName string) string {
	name := "openclaw-" + instanceName + "-cetirun"
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

func dnsLabel(value string) bool {
	if len(value) == 0 || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9') || character == '-' {
			continue
		}
		return false
	}
	return true
}
