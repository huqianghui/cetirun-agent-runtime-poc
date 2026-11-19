// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package broker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

var errConflict = errors.New("record update conflict")
var errNotFound = errors.New("record not found")
var errDependencyUnavailable = errors.New("dependency unavailable")
var errWorkloadProofInvalid = errors.New("workload proof is invalid")

type secretDocument struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   secretMetadata    `json:"metadata"`
	Data       map[string]string `json:"data"`
}

type secretMetadata struct {
	DeletionTimestamp *time.Time        `json:"deletionTimestamp,omitempty"`
	UID               string            `json:"uid,omitempty"`
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	OwnerReferences   []ownerReference  `json:"ownerReferences,omitempty"`
}

type ownerReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
}

type secretList struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []secretDocument `json:"items"`
}

type agentDocument struct {
	Metadata struct {
		Name              string            `json:"name"`
		UID               string            `json:"uid"`
		Annotations       map[string]string `json:"annotations"`
		DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
		Finalizers        []string          `json:"finalizers"`
	} `json:"metadata"`
	Spec struct {
		Suspended   bool   `json:"suspended"`
		SandboxRef  string `json:"sandboxRef"`
		ModelAccess *struct {
			MonthlyBudgetUSD string `json:"monthlyBudgetUSD"`
		} `json:"modelAccess"`
		Agent struct {
			Type string `json:"type"`
		} `json:"agent"`
		User struct {
			Alias string `json:"alias"`
		} `json:"user"`
		AgentIdentity *struct {
			Enabled bool `json:"enabled"`
		} `json:"agentIdentity"`
	} `json:"spec"`
	Status struct {
		Phase     string           `json:"phase"`
		Namespace string           `json:"namespace"`
		Policy    *json.RawMessage `json:"policy"`
		Sandbox   *json.RawMessage `json:"sandbox"`
		AIGateway *struct {
			PolicyRevision string `json:"policyRevision"`
		} `json:"aiGateway"`
		Conditions []struct {
			Type string `json:"type"`
		} `json:"conditions"`
		AgentIdentity *struct {
			Phase    string `json:"phase"`
			ClientID string `json:"clientId"`
			ObjectID string `json:"objectId"`
		} `json:"agentIdentity"`
	} `json:"status"`
}

type agentList struct {
	Metadata struct {
		Continue string `json:"continue"`
	} `json:"metadata"`
	Items []agentDocument `json:"items"`
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

type workloadIdentity struct {
	CredentialPolicy *agenttools.CredentialPolicyReference
	Agent            Agent
	PodUID           string
	SAUID            string
}

type kubeClient struct {
	nativeOperatorNamespace  string
	nativeAudience           string
	nativeAdmissionTransport http.RoundTripper
	recordNamespaceDeleting  bool
	agentNamespaces          bool
	recordAgent              *Agent
	baseURL                  string
	namespace                string
	tokenFile                string
	client                   *http.Client
	wrapper                  keyWrapper
}

func newKubeClient(cfg Config, wrapper keyWrapper) (*kubeClient, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("Kubernetes service environment is unavailable")
	}
	tokenFile := cfg.KubernetesTokenFile
	if _, err := os.Stat(tokenFile); err != nil {
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
	return &kubeClient{
		nativeOperatorNamespace: cfg.OperatorNamespace,
		nativeAudience:          cfg.WorkloadTokenAudience,
		baseURL:                 "https://" + host + ":" + port,
		agentNamespaces:         true,
		namespace:               cfg.StoreNamespace,
		tokenFile:               tokenFile,
		client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    pool,
		}}, Timeout: cfg.RequestTimeout},
		wrapper: wrapper,
	}, nil
}

func (k *kubeClient) serviceAccountToken() (string, error) {
	token, err := os.ReadFile(k.tokenFile)
	if err != nil || strings.TrimSpace(string(token)) == "" {
		return "", errors.New("Kubernetes service account token is unavailable")
	}
	return strings.TrimSpace(string(token)), nil
}

func (k *kubeClient) request(ctx context.Context, method, endpoint string, body any, result any) (int, error) {
	token, err := k.serviceAccountToken()
	if err != nil {
		return 0, err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.baseURL+endpoint, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return 0, errors.New("Kubernetes API is unavailable")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return resp.StatusCode, errors.New("cannot read bounded Kubernetes API response")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusNotFound {
			return resp.StatusCode, errNotFound
		}
		if resp.StatusCode == http.StatusConflict {
			return resp.StatusCode, errConflict
		}
		return resp.StatusCode, fmt.Errorf("Kubernetes API returned HTTP %d", resp.StatusCode)
	}
	if result != nil && len(data) > 0 {
		if err := json.Unmarshal(data, result); err != nil {
			return resp.StatusCode, errors.New("Kubernetes API returned invalid JSON")
		}
	}
	return resp.StatusCode, nil
}

func (k *kubeClient) getAgent(ctx context.Context, name string) (Agent, error) {
	agent, err := k.readAgent(ctx, name)
	if err != nil {
		return Agent{}, err
	}
	if agent.Namespace == "" {
		return Agent{}, errors.New("Agent resource is not ready")
	}
	return agent, nil
}

// readAgent reads an Agent without treating its runtime namespace as an
// identity field. Callers that need a workload must use getAgent or enforce
// their own readiness contract.
func (k *kubeClient) readAgent(ctx context.Context, name string) (Agent, error) {
	if !dnsLabel(name) {
		return Agent{}, errors.New("instance name is invalid")
	}
	var document agentDocument
	_, err := k.request(ctx, http.MethodGet, "/apis/cetirun.microsoft.com/v1alpha1/agents/"+url.PathEscape(name), nil, &document)
	if err != nil {
		if errors.Is(err, errNotFound) {
			return Agent{}, err
		}
		return Agent{}, fmt.Errorf("%w: Agent lookup: %v", errDependencyUnavailable, err)
	}
	agent := agentFromDocument(document)
	if agent.UID == "" || agent.Name != name {
		return Agent{}, errNotFound
	}
	return agent, nil
}

func (k *kubeClient) getAgentByWorkload(ctx context.Context, namespace, serviceAccount string) (Agent, error) {
	if !dnsLabel(namespace) || !agenttools.ValidDNSSubdomainName(serviceAccount) {
		return Agent{}, errors.New("workload identity is invalid")
	}
	var ns struct {
		Metadata secretMetadata `json:"metadata"`
	}
	if _, err := k.request(ctx, http.MethodGet, "/api/v1/namespaces/"+url.PathEscape(namespace), nil, &ns); err != nil {
		return Agent{}, err
	}
	if ns.Metadata.Name != namespace || ns.Metadata.DeletionTimestamp != nil {
		return Agent{}, errNotFound
	}
	var owner ownerReference
	foundOwner := false
	for _, ref := range ns.Metadata.OwnerReferences {
		if ref.Kind == "Agent" && ref.APIVersion == "cetirun.microsoft.com/v1alpha1" {
			if foundOwner {
				return Agent{}, errNotFound
			}
			owner, foundOwner = ref, true
		}
	}
	if !dnsLabel(owner.Name) || !uuidPattern.MatchString(owner.UID) {
		return Agent{}, errNotFound
	}
	agent, err := k.getAgent(ctx, owner.Name)
	if err != nil {
		return Agent{}, err
	}
	if agent.Name != owner.Name || agent.UID != owner.UID || agent.Namespace != namespace ||
		(!agent.PolicyManaged && workloadServiceAccount(agent.Name) != serviceAccount) {
		return Agent{}, errNotFound
	}
	return agent, nil
}

func (k *kubeClient) getAgentByUID(ctx context.Context, uid string) (Agent, error) {
	if !uuidPattern.MatchString(uid) {
		return Agent{}, errors.New("instance UID is invalid")
	}
	list, err := k.listAgents(ctx)
	if err != nil {
		return Agent{}, err
	}
	var match *Agent
	for _, document := range list.Items {
		if document.Metadata.UID != uid {
			continue
		}
		agent := agentFromDocument(document)
		if match != nil {
			return Agent{}, errors.New("instance UID maps to multiple resources")
		}
		match = &agent
	}
	if match == nil {
		return Agent{}, errNotFound
	}
	return *match, nil
}

func (k *kubeClient) verifyWorkloadProof(ctx context.Context, proof, audience string) (workloadIdentity, error) {
	if strings.TrimSpace(proof) == "" || strings.TrimSpace(audience) == "" {
		return workloadIdentity{}, errWorkloadProofInvalid
	}
	review := tokenReview{APIVersion: "authentication.k8s.io/v1", Kind: "TokenReview"}
	review.Spec.Token, review.Spec.Audiences = proof, []string{audience}
	if _, err := k.request(ctx, http.MethodPost, "/apis/authentication.k8s.io/v1/tokenreviews", review, &review); err != nil {
		return workloadIdentity{}, fmt.Errorf("%w: token review: %v", errDependencyUnavailable, err)
	}
	if !review.Status.Authenticated || !tokenReviewAudienceAccepted(review.Status.Audiences, audience) {
		return workloadIdentity{}, errWorkloadProofInvalid
	}
	parts := strings.Split(review.Status.User.Username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" || !dnsLabel(parts[2]) || !agenttools.ValidDNSSubdomainName(parts[3]) {
		return workloadIdentity{}, errWorkloadProofInvalid
	}
	saUID := review.Status.User.UID
	podUID := firstExtra(review.Status.User.Extra, "authentication.kubernetes.io/pod-uid")
	podName := firstExtra(review.Status.User.Extra, "authentication.kubernetes.io/pod-name")
	if saUID == "" || podUID == "" || podName == "" || !agenttools.ValidDNSSubdomainName(podName) {
		return workloadIdentity{}, errWorkloadProofInvalid
	}
	var serviceAccount struct {
		Metadata struct {
			UID               string     `json:"uid"`
			DeletionTimestamp *time.Time `json:"deletionTimestamp,omitempty"`
		} `json:"metadata"`
	}
	if _, err := k.request(ctx, http.MethodGet, "/api/v1/namespaces/"+url.PathEscape(parts[2])+"/serviceaccounts/"+url.PathEscape(parts[3]), nil, &serviceAccount); err != nil {
		if errors.Is(err, errNotFound) {
			return workloadIdentity{}, errWorkloadProofInvalid
		}
		return workloadIdentity{}, fmt.Errorf("%w: service account lookup: %v", errDependencyUnavailable, err)
	} else if serviceAccount.Metadata.UID != saUID || serviceAccount.Metadata.DeletionTimestamp != nil {
		return workloadIdentity{}, errWorkloadProofInvalid
	}
	var pod struct {
		Metadata struct {
			UID               string     `json:"uid"`
			DeletionTimestamp *time.Time `json:"deletionTimestamp,omitempty"`
		} `json:"metadata"`
		Spec struct {
			ServiceAccountName string `json:"serviceAccountName"`
		} `json:"spec"`
		Status struct {
			Phase      string                          `json:"phase"`
			Conditions []struct{ Type, Status string } `json:"conditions"`
		} `json:"status"`
	}
	if _, err := k.request(ctx, http.MethodGet, "/api/v1/namespaces/"+url.PathEscape(parts[2])+"/pods/"+url.PathEscape(podName), nil, &pod); err != nil {
		if errors.Is(err, errNotFound) {
			return workloadIdentity{}, errWorkloadProofInvalid
		}
		return workloadIdentity{}, fmt.Errorf("%w: pod lookup: %v", errDependencyUnavailable, err)
	} else if pod.Metadata.UID != podUID || pod.Spec.ServiceAccountName != parts[3] || pod.Metadata.DeletionTimestamp != nil {
		return workloadIdentity{}, errWorkloadProofInvalid
	}
	agent, err := k.getAgentByWorkload(ctx, parts[2], parts[3])
	if err != nil {
		if errors.Is(err, errNotFound) {
			return workloadIdentity{}, errWorkloadProofInvalid
		}
		return workloadIdentity{}, fmt.Errorf("%w: instance lookup: %v", errDependencyUnavailable, err)
	}
	if !agent.WorkloadReady || !uuidPattern.MatchString(agent.UID) {
		return workloadIdentity{}, errWorkloadProofInvalid
	}
	if agent.PolicyManaged {
		ready := false
		for _, condition := range pod.Status.Conditions {
			ready = ready || condition.Type == "Ready" && condition.Status == "True"
		}
		if pod.Status.Phase != "Running" || !ready {
			return workloadIdentity{}, errWorkloadProofInvalid
		}
		agent, err = k.qualifyNativeAgent(ctx, agent, &agenttools.NativePodIdentity{
			AgentUID: agent.UID, Namespace: parts[2], PodName: podName, PodUID: podUID,
			ServiceAccountName: parts[3], ServiceAccountUID: saUID,
		})
		if err != nil {
			return workloadIdentity{}, err
		}
	}
	return workloadIdentity{Agent: agent, PodUID: podUID, SAUID: saUID}, nil
}

func tokenReviewAudienceAccepted(audiences []string, expected string) bool {
	return len(audiences) == 1 && audiences[0] == expected
}

func agentFromDocument(document agentDocument) Agent {
	agent := Agent{
		PolicyManaged: agentDocumentSandboxRequested(document),
		Deleting:      document.Metadata.DeletionTimestamp != nil, RuntimeType: document.Spec.Agent.Type,
		Name:               document.Metadata.Name,
		UnsupportedRuntime: agentDocumentSandboxRequested(document) || (document.Spec.Agent.Type != "hermes" && document.Spec.Agent.Type != "openclaw"),
		UID:                document.Metadata.UID,
		Namespace:          document.Status.Namespace,
		OwnerTenantID:      document.Metadata.Annotations["agentidentity.cetirun.microsoft.com/owner-tenant-id"],
		OwnerObjectID:      document.Metadata.Annotations["agentidentity.cetirun.microsoft.com/owner-object-id"],
		ServiceAccount:     workloadServiceAccount(document.Metadata.Name),
		WorkloadReady:      document.Status.Phase == "Ready" && !document.Spec.Suspended && document.Metadata.DeletionTimestamp == nil,
	}
	if document.Spec.AgentIdentity != nil && document.Spec.AgentIdentity.Enabled && document.Status.AgentIdentity != nil {
		agent.AgentIdentityClientID = document.Status.AgentIdentity.ClientID
		agent.AgentIdentityObjectID = document.Status.AgentIdentity.ObjectID
		agent.AgentIdentityReady = agent.WorkloadReady && document.Status.AgentIdentity.Phase == "Ready" && agent.AgentIdentityClientID != ""
	}
	return agent
}

// Match the persisted Agent signals used by the Operator's sandbox.Requested
// and deletion routing. Team policy is materialized into these finalizers or
// status fields before a Sandbox workload is provisioned. Checking sandboxRef
// alone misses policy-selected and recovery-held Agents.
func agentDocumentSandboxRequested(document agentDocument) bool {
	for _, key := range []string{"cetirun.microsoft.com/sandbox-authorization", "cetirun.microsoft.com/sandbox-reauthorize"} {
		if _, present := document.Metadata.Annotations[key]; present {
			return true
		}
	}
	for _, finalizer := range document.Metadata.Finalizers {
		switch finalizer {
		case "cetirun.microsoft.com/sandbox-hold", "cetirun.microsoft.com/sandbox-preparation", "cetirun.microsoft.com/sandbox-bootstrap":
			return true
		}
	}
	if document.Spec.SandboxRef != "" ||
		(document.Spec.ModelAccess != nil && document.Spec.ModelAccess.MonthlyBudgetUSD != "") ||
		document.Status.Policy != nil || document.Status.Sandbox != nil ||
		(document.Status.AIGateway != nil && document.Status.AIGateway.PolicyRevision != "") {
		return true
	}
	for _, condition := range document.Status.Conditions {
		if condition.Type == "SandboxReady" {
			return true
		}
	}
	return false
}

func firstExtra(extra map[string][]string, key string) string {
	if len(extra[key]) != 1 {
		return ""
	}
	return extra[key][0]
}

func workloadServiceAccount(instanceName string) string {
	name := "openclaw-" + instanceName + "-cetirun"
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

func stateHash(state string) string {
	digest := sha256.Sum256([]byte(state))
	return hex.EncodeToString(digest[:])
}

func dnsLabel(value string) bool {
	if len(value) == 0 || len(value) > 63 {
		return false
	}
	for index, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || (char == '-' && index > 0 && index < len(value)-1) {
			continue
		}
		return false
	}
	return true
}

func toolRecordName(kind, id string) string { return "agent-tools-" + kind + "-" + id }

func connectionAAD(connection Connection) []byte {
	return []byte(fmt.Sprintf("agent-tools-connection-v1\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d", connection.ID, connection.Owner.TenantID, connection.Owner.ObjectID, connection.Provider, connection.AccountID, connection.Revision))
}

func bindingAAD(grant ConnectionBinding) []byte {
	return []byte(fmt.Sprintf("agent-tools-binding-v1\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d", grant.ID, grant.Owner.TenantID, grant.Owner.ObjectID, grant.InstanceUID, grant.ConnectionID, grant.Revision))
}

func recordRevision(data map[string]string) uint64 {
	raw, err := base64.StdEncoding.DecodeString(data["revision"])
	if err != nil {
		return 0
	}
	var revision uint64
	_, _ = fmt.Sscanf(string(raw), "%d", &revision)
	return revision
}

func (k *kubeClient) getToolSecret(ctx context.Context, kind, id string) (secretDocument, error) {
	var secret secretDocument
	_, err := k.request(ctx, http.MethodGet, "/api/v1/namespaces/"+url.PathEscape(k.namespace)+"/secrets/"+url.PathEscape(toolRecordName(kind, id)), nil, &secret)
	if err == nil && k.recordAgent != nil && !recordOwnedBy(secret, *k.recordAgent) {
		return secret, errors.New("Agent record ownership is invalid")
	}
	return secret, err
}

func (k *kubeClient) putToolSecret(ctx context.Context, kind, id string, labels, annotations, data map[string]string, resourceVersion string) error {
	labels["app.kubernetes.io/managed-by"] = "cetirun-agent-broker"
	labels["agenttools.cetirun.microsoft.com/record-type"] = kind
	secret := secretDocument{APIVersion: "v1", Kind: "Secret", Metadata: secretMetadata{Name: toolRecordName(kind, id), Namespace: k.namespace, ResourceVersion: resourceVersion, Labels: labels, Annotations: annotations}, Data: data}
	if k.recordAgent != nil {
		secret.Metadata.OwnerReferences = []ownerReference{agentRecordOwner(*k.recordAgent)}
	}
	endpoint, method := "/api/v1/namespaces/"+url.PathEscape(k.namespace)+"/secrets", http.MethodPost
	if resourceVersion != "" {
		endpoint, method = endpoint+"/"+url.PathEscape(secret.Metadata.Name), http.MethodPut
	}
	_, err := k.request(ctx, method, endpoint, secret, nil)
	return err
}

// recordInstanceMetadata projects display snapshots without changing the
// encrypted UID/owner binding. Full names remain in annotations; short DNS
// labels are also available to kubectl -L and label selectors.
func recordInstanceMetadata(labels map[string]string, name, namespace string) map[string]string {
	annotations := map[string]string{}
	for key, value := range map[string]string{"instance-name": name, "instance-namespace": namespace} {
		if value == "" {
			continue
		}
		qualified := "agenttools.cetirun.microsoft.com/" + key
		annotations[qualified] = value
		if dnsLabel(value) {
			labels[qualified] = value
		}
	}
	return annotations
}

func (k *kubeClient) encodeConnection(ctx context.Context, record *connectionRecord, kinds ...string) (map[string]string, error) {
	plain, err := json.Marshal(storedConnectionRecord{connectionRecord: record, OwnerTenantID: record.Connection.Owner.TenantID, OwnerObjectID: record.Connection.Owner.ObjectID, AccountID: record.Connection.AccountID, UpdatedAt: record.Connection.UpdatedAt})
	if err != nil {
		return nil, err
	}
	aad := connectionAAD(record.Connection)
	if len(kinds) == 1 {
		aad = append([]byte(kinds[0]+"\x00"), aad...)
	}
	envelope, err := seal(ctx, k.wrapper, plain, aad)
	if err != nil {
		return nil, err
	}
	return map[string]string{"envelope.json": base64.StdEncoding.EncodeToString(envelope), "revision": base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%d", record.Connection.Revision)))}, nil
}

type storedConnectionRecord struct {
	*connectionRecord
	OwnerTenantID string    `json:"ownerTenantId"`
	OwnerObjectID string    `json:"ownerObjectId"`
	AccountID     string    `json:"accountId"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

func (k *kubeClient) decodeConnection(ctx context.Context, secret secretDocument) (*connectionRecord, error) {
	id, ownerTenant, ownerObject, provider := secret.Metadata.Labels["agenttools.cetirun.microsoft.com/id"], secret.Metadata.Labels["agenttools.cetirun.microsoft.com/owner-tenant"], secret.Metadata.Labels["agenttools.cetirun.microsoft.com/owner-object"], secret.Metadata.Labels["agenttools.cetirun.microsoft.com/provider"]
	revision := recordRevision(secret.Data)
	if id == "" || ownerTenant == "" || ownerObject == "" || provider == "" || revision == 0 {
		return nil, errors.New("connection record metadata is invalid")
	}
	envelope, err := base64.StdEncoding.DecodeString(secret.Data["envelope.json"])
	if err != nil {
		return nil, errors.New("connection record encoding is invalid")
	}
	stub := Connection{ID: id, Owner: Owner{TenantID: ownerTenant, ObjectID: ownerObject}, Provider: provider, AccountID: secret.Metadata.Labels["agenttools.cetirun.microsoft.com/account-id"], Revision: revision}
	aad := connectionAAD(stub)
	kind := secret.Metadata.Labels[recordLabelPrefix+"record-type"]
	if kind == "authorization" || kind == "account-index" {
		aad = append([]byte(kind+"\x00"), aad...)
	}
	plain, err := openEnvelope(ctx, k.wrapper, envelope, aad)
	if err != nil {
		return nil, err
	}
	stored := storedConnectionRecord{connectionRecord: &connectionRecord{}}
	if json.Unmarshal(plain, &stored) != nil {
		return nil, errors.New("connection record is invalid")
	}
	record := stored.connectionRecord
	record.Connection.Owner = Owner{TenantID: stored.OwnerTenantID, ObjectID: stored.OwnerObjectID}
	record.Connection.AccountID, record.Connection.UpdatedAt = stored.AccountID, stored.UpdatedAt
	if record.Connection.ID != id || record.Connection.Revision != revision || !sameOwner(record.Connection.Owner, stub.Owner) || record.Connection.Provider != provider || record.Connection.AccountID != stub.AccountID {
		return nil, errors.New("connection record is invalid")
	}
	return record, nil
}

func (k *kubeClient) getConnection(ctx context.Context, id string) (*connectionRecord, string, error) {
	secret, err := k.getToolSecret(ctx, "connection", id)
	if err != nil {
		return nil, "", err
	}
	record, err := k.decodeConnection(ctx, secret)
	if err == nil {
		err = k.validateConnectionLocation(record)
	}
	return record, secret.Metadata.ResourceVersion, err
}

func (k *kubeClient) putConnection(ctx context.Context, record *connectionRecord, resourceVersion string) error {
	if k.agentNamespaces && connectionMode(record.Connection) == modeAgent {
		if record.Connection.Identity == nil {
			return errors.New("Agent identity binding is missing")
		}
		target, err := k.agentRecordStore(ctx, record.Connection.Identity.InstanceUID, record.InstanceName)
		if err != nil {
			return err
		}
		if target.recordAgent.Deleting {
			return errors.New("Agent is deleting")
		}
		if err := target.validateConnectionLocation(record); err != nil {
			return err
		}
		record.InstanceName, record.InstanceNamespace = target.recordAgent.Name, target.recordAgent.Namespace
		return target.putConnection(ctx, record, resourceVersion)
	}
	if err := k.validateConnectionLocation(record); err != nil {
		return err
	}
	if k.recordAgent != nil && (k.recordAgent.Deleting || k.recordNamespaceDeleting) {
		return errors.New("Agent is deleting")
	}

	data, err := k.encodeConnection(ctx, record)
	if err != nil {
		return err
	}
	labels := map[string]string{
		"agenttools.cetirun.microsoft.com/id":           record.Connection.ID,
		"agenttools.cetirun.microsoft.com/owner-tenant": record.Connection.Owner.TenantID,
		"agenttools.cetirun.microsoft.com/owner-object": record.Connection.Owner.ObjectID,
		"agenttools.cetirun.microsoft.com/provider":     record.Connection.Provider,
		"agenttools.cetirun.microsoft.com/account-id":   record.Connection.AccountID,
		"agenttools.cetirun.microsoft.com/mode":         connectionMode(record.Connection),
	}
	var annotations map[string]string
	if connectionMode(record.Connection) == modeAgent && record.Connection.Identity != nil {
		labels["agenttools.cetirun.microsoft.com/instance-uid"] = record.Connection.Identity.InstanceUID
		annotations = recordInstanceMetadata(labels, record.InstanceName, record.InstanceNamespace)
	}
	if record.Transaction != nil {
		labels["agenttools.cetirun.microsoft.com/state-hash"] = record.Transaction.StateHash[:48]
	}
	return k.putToolSecret(ctx, "connection", record.Connection.ID, labels, annotations, data, resourceVersion)
}

func (k *kubeClient) connectionRecords(ctx context.Context, selector string) ([]struct {
	record  *connectionRecord
	version string
}, error) {
	list, err := k.listToolSecrets(ctx, "connection", selector)
	if err != nil {
		return nil, err
	}
	result := make([]struct {
		record  *connectionRecord
		version string
	}, 0, len(list.Items))
	for _, secret := range list.Items {
		if secret.Metadata.Labels["agenttools.cetirun.microsoft.com/record-type"] != "connection" {
			continue
		}
		record, err := k.decodeConnection(ctx, secret)
		if err != nil {
			return nil, err
		}
		if err := k.validateConnectionLocation(record); err != nil {
			return nil, err
		}
		result = append(result, struct {
			record  *connectionRecord
			version string
		}{record, secret.Metadata.ResourceVersion})
	}
	return result, nil
}

func (k *kubeClient) listConnections(ctx context.Context, owner Owner) ([]Connection, error) {
	records, err := k.connectionRecords(ctx, "agenttools.cetirun.microsoft.com/owner-tenant="+owner.TenantID+",agenttools.cetirun.microsoft.com/owner-object="+owner.ObjectID)
	if err != nil {
		return nil, err
	}
	connections := make([]Connection, 0, len(records))
	for _, item := range records {
		if !item.record.Deleted && sameOwner(item.record.Connection.Owner, owner) && (!k.agentNamespaces || isSharedConnectionMode(connectionMode(item.record.Connection))) {
			connections = append(connections, item.record.Connection)
		}
	}
	if k.agentNamespaces {
		stores, err := k.ownerAgentStores(ctx, owner)
		if err != nil {
			return nil, err
		}
		for _, target := range stores {
			local, err := target.listConnections(ctx, owner)
			if err != nil {
				return nil, err
			}
			connections = append(connections, local...)
		}
	}

	sort.Slice(connections, func(i, j int) bool { return connections[i].CreatedAt.Before(connections[j].CreatedAt) })
	return connections, nil
}

func (k *kubeClient) findConnectionByState(ctx context.Context, state string) (*connectionRecord, string, error) {
	hash := stateHash(state)
	records, err := k.connectionRecords(ctx, "agenttools.cetirun.microsoft.com/state-hash="+hash[:48])
	if err != nil {
		return nil, "", err
	}
	if len(records) != 1 || records[0].record.Deleted || records[0].record.Transaction == nil || !strings.EqualFold(records[0].record.Transaction.StateHash, hash) {
		return nil, "", errNotFound
	}
	return records[0].record, records[0].version, nil
}

func (k *kubeClient) encodeBinding(ctx context.Context, record *bindingRecord) (map[string]string, error) {
	plain, err := json.Marshal(storedBindingRecord{bindingRecord: record, OwnerTenantID: record.Binding.Owner.TenantID, OwnerObjectID: record.Binding.Owner.ObjectID, Revoked: record.Binding.Revoked, CreatedAt: record.Binding.CreatedAt, UpdatedAt: record.Binding.UpdatedAt})
	if err != nil {
		return nil, err
	}
	envelope, err := seal(ctx, k.wrapper, plain, bindingAAD(record.Binding))
	if err != nil {
		return nil, err
	}
	return map[string]string{"envelope.json": base64.StdEncoding.EncodeToString(envelope), "revision": base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%d", record.Binding.Revision)))}, nil
}

type storedBindingRecord struct {
	*bindingRecord
	OwnerTenantID string    `json:"ownerTenantId"`
	OwnerObjectID string    `json:"ownerObjectId"`
	Revoked       bool      `json:"revoked"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

func (k *kubeClient) decodeBinding(ctx context.Context, secret secretDocument) (*bindingRecord, error) {
	id, ownerTenant, ownerObject, instanceUID, connectionID := secret.Metadata.Labels["agenttools.cetirun.microsoft.com/id"], secret.Metadata.Labels["agenttools.cetirun.microsoft.com/owner-tenant"], secret.Metadata.Labels["agenttools.cetirun.microsoft.com/owner-object"], secret.Metadata.Labels["agenttools.cetirun.microsoft.com/instance-uid"], secret.Metadata.Labels["agenttools.cetirun.microsoft.com/connection-id"]
	stub := ConnectionBinding{ID: id, Owner: Owner{TenantID: ownerTenant, ObjectID: ownerObject}, InstanceUID: instanceUID, ConnectionID: connectionID, Revision: recordRevision(secret.Data)}
	if id == "" || ownerTenant == "" || ownerObject == "" || instanceUID == "" || connectionID == "" || stub.Revision == 0 {
		return nil, errors.New("grant record metadata is invalid")
	}
	envelope, err := base64.StdEncoding.DecodeString(secret.Data["envelope.json"])
	if err != nil {
		return nil, errors.New("grant record encoding is invalid")
	}
	plain, err := openEnvelope(ctx, k.wrapper, envelope, bindingAAD(stub))
	if err != nil {
		return nil, err
	}
	stored := storedBindingRecord{bindingRecord: &bindingRecord{}}
	if json.Unmarshal(plain, &stored) != nil {
		return nil, errors.New("grant record is invalid")
	}
	record := stored.bindingRecord
	record.Binding.Owner = Owner{TenantID: stored.OwnerTenantID, ObjectID: stored.OwnerObjectID}
	record.Binding.Revoked, record.Binding.CreatedAt, record.Binding.UpdatedAt = stored.Revoked, stored.CreatedAt, stored.UpdatedAt
	if record.Binding.ID != id || record.Binding.Revision != stub.Revision || !sameOwner(record.Binding.Owner, stub.Owner) || record.Binding.InstanceUID != instanceUID || record.Binding.ConnectionID != connectionID {
		return nil, errors.New("grant record is invalid")
	}
	return record, nil
}

func (k *kubeClient) getBinding(ctx context.Context, id string, agents ...Agent) (*bindingRecord, string, error) {
	if k.agentNamespaces {
		if len(agents) != 1 {
			return nil, "", errors.New("Agent scope is required")
		}
		target, err := k.agentRecordStore(ctx, agents[0].UID, agents[0].Name)
		if err != nil {
			return nil, "", err
		}
		return target.getBinding(ctx, id)
	}
	secret, err := k.getToolSecret(ctx, "binding", id)
	if err != nil {
		return nil, "", err
	}
	record, err := k.decodeBinding(ctx, secret)
	if err == nil {
		err = k.validateBindingLocation(record)
	}
	return record, secret.Metadata.ResourceVersion, err
}

func (k *kubeClient) putBinding(ctx context.Context, record *bindingRecord, resourceVersion string) error {
	if k.agentNamespaces {
		target, err := k.agentRecordStore(ctx, record.Binding.InstanceUID, record.InstanceName)
		if err != nil {
			return err
		}
		if target.recordAgent.Deleting && !record.Binding.Revoked {
			return errors.New("Agent is deleting")
		}
		if err := target.validateBindingLocation(record); err != nil {
			return err
		}
		if !record.Binding.Revoked {
			connection, _, err := k.getConnectionForAgent(ctx, *target.recordAgent, record.Binding.ConnectionID)
			if err != nil {
				return err
			}
			if err := k.ensureConnectionReference(ctx, *target.recordAgent, connection.Connection); err != nil {
				return err
			}
		}
		record.InstanceName, record.InstanceNamespace = target.recordAgent.Name, target.recordAgent.Namespace
		return target.putBinding(ctx, record, resourceVersion)
	}
	if err := k.validateBindingLocation(record); err != nil {
		return err
	}
	if k.recordAgent != nil && (k.recordAgent.Deleting || k.recordNamespaceDeleting) {
		return errors.New("Agent is deleting")
	}

	data, err := k.encodeBinding(ctx, record)
	if err != nil {
		return err
	}
	grant := record.Binding
	labels := map[string]string{
		"agenttools.cetirun.microsoft.com/id":            grant.ID,
		"agenttools.cetirun.microsoft.com/owner-tenant":  grant.Owner.TenantID,
		"agenttools.cetirun.microsoft.com/owner-object":  grant.Owner.ObjectID,
		"agenttools.cetirun.microsoft.com/instance-uid":  grant.InstanceUID,
		"agenttools.cetirun.microsoft.com/connection-id": grant.ConnectionID,
	}
	annotations := recordInstanceMetadata(labels, record.InstanceName, record.InstanceNamespace)
	return k.putToolSecret(ctx, "binding", grant.ID, labels, annotations, data, resourceVersion)
}

func (k *kubeClient) bindingRecords(ctx context.Context, selector string) ([]struct {
	record  *bindingRecord
	version string
}, error) {
	list, err := k.listToolSecrets(ctx, "binding", selector)
	if err != nil {
		return nil, err
	}
	result := make([]struct {
		record  *bindingRecord
		version string
	}, 0, len(list.Items))
	for _, secret := range list.Items {
		if secret.Metadata.Labels["agenttools.cetirun.microsoft.com/record-type"] != "binding" {
			continue
		}
		record, err := k.decodeBinding(ctx, secret)
		if err != nil {
			return nil, err
		}
		if err := k.validateBindingLocation(record); err != nil {
			return nil, err
		}
		result = append(result, struct {
			record  *bindingRecord
			version string
		}{record, secret.Metadata.ResourceVersion})
	}
	return result, nil
}

func (k *kubeClient) listBindings(ctx context.Context, owner Owner, instanceUID string, names ...string) ([]ConnectionBinding, error) {
	if k.agentNamespaces {
		target, err := k.agentRecordStore(ctx, instanceUID, names...)
		if err != nil {
			return nil, err
		}
		return target.listBindings(ctx, owner, instanceUID)
	}
	selector := "agenttools.cetirun.microsoft.com/owner-tenant=" + owner.TenantID + ",agenttools.cetirun.microsoft.com/owner-object=" + owner.ObjectID
	if instanceUID != "" {
		selector += ",agenttools.cetirun.microsoft.com/instance-uid=" + instanceUID
	}
	records, err := k.bindingRecords(ctx, selector)
	if err != nil {
		return nil, err
	}
	grants := make([]ConnectionBinding, 0, len(records))
	for _, item := range records {
		if sameOwner(item.record.Binding.Owner, owner) {
			grants = append(grants, item.record.Binding)
		}
	}
	sort.Slice(grants, func(i, j int) bool { return grants[i].CreatedAt.Before(grants[j].CreatedAt) })
	return grants, nil
}

func sameOwner(left, right Owner) bool {
	return sameIdentityID(left.TenantID, right.TenantID) && sameIdentityID(left.ObjectID, right.ObjectID)
}

func activeBinding(grant ConnectionBinding, now time.Time) bool {
	return !grant.Revoked && grant.Enabled
}

// listAgents bounds each API response while preserving complete management results.
func (k *kubeClient) listAgents(ctx context.Context) (agentList, error) {
	result := agentList{}
	continuation := ""
	seen := map[string]bool{}
	for {
		query := url.Values{"limit": {"50"}}
		if continuation != "" {
			query.Set("continue", continuation)
		}
		var page agentList
		if _, err := k.request(ctx, http.MethodGet, "/apis/cetirun.microsoft.com/v1alpha1/agents?"+query.Encode(), nil, &page); err != nil {
			return agentList{}, err
		}
		result.Items = append(result.Items, page.Items...)
		if page.Metadata.Continue == "" {
			return result, nil
		}
		if seen[page.Metadata.Continue] {
			return agentList{}, errors.New("Kubernetes pagination did not advance")
		}
		seen[page.Metadata.Continue] = true
		continuation = page.Metadata.Continue
	}
}
