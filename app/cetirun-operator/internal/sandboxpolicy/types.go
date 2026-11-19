// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package sandboxpolicy compiles trusted, resolved configuration. It neither
// qualifies a dataplane nor supplies execution evidence or an execution permit.
package sandboxpolicy

import (
	"errors"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/egresscapture"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
)

const (
	// AzureCNIOverlayCilium is a candidate, not a production registration.
	AzureCNIOverlayCilium   = "azure-cni-overlay-cilium"
	Cilium                  = "cilium"
	AgentUIDLabel           = "sandbox.cetirun.microsoft.com/agent-uid"
	PeerUIDLabel            = "sandbox.cetirun.microsoft.com/peer-uid"
	RevisionAnnotation      = "sandbox.cetirun.microsoft.com/revision"
	BindingAnnotation       = "sandbox.cetirun.microsoft.com/binding"
	GrantRevisionAnnotation = "sandbox.cetirun.microsoft.com/grant-revision"
	EntraAudience           = "api://AzureADTokenExchange"
)

var (
	ErrUnsupportedControl = errors.New("sandbox control is not qualified")
	ErrUnavailable        = errors.New("trusted sandbox prerequisite is unavailable")
	ErrConflict           = errors.New("sandbox policy conflicts with a required control")
	ErrUnapprovedGrant    = errors.New("sandbox identity or grant is not approved")
	ErrOwnership          = errors.New("sandbox resource ownership is not established")
	ErrRevocationPending  = errors.New("sandbox binding revocation is pending")
)

// AdapterRegistration must describe independently qualified managed versions,
// Cilium deny/port-range and endpoint/CIDR semantics, protected peer labels, and
// restrictive failure behavior. There are deliberately no default registrations.
// The native-single-start profile instead uses Cilium with Qualified=false;
// its explicit baseline declarations do not qualify external observation/fencing.
type AdapterRegistration struct {
	Name      string `json:"name"`
	Revision  string `json:"revision"`
	Qualified bool   `json:"qualified"`
	Available bool   `json:"available"`
}

// Peer is a trusted, resolved destination, including Service and backend IPs.
// A namespaced peer normally selects Pods bearing the protected PeerUIDLabel.
// Native DNS and Gateway peers may select platform-owned Pod labels and pin
// only Service IPs. Native ingress selectors do not pin any IPs.
// An empty namespace denotes a guest-external IP destination.
// The platform owns selector protection and endpoint realization.
type Peer struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	UID         types.UID         `json:"uid"`
	IPs         []string          `json:"ips,omitempty"`
	PodSelector map[string]string `json:"podSelector,omitempty"`
}

type Destination struct {
	Peer              Peer                  `json:"peer"`
	Ports             []api.AgentPolicyPort `json:"ports"`
	TrustedProxyCIDRs []string              `json:"trustedProxyCIDRs,omitempty"`
}

// Topology is supplied by an operator/platform source, never AgentPolicy input.
// Peers is the complete managed-peer inventory needed to translate internal
// CIDRs to Cilium endpoint identities. Its revision changes with membership.
type Topology struct {
	Revision             string `json:"revision"`
	ManagedPeersComplete bool   `json:"managedPeersComplete"`
	// DNSResolverIPs selects one to three Pod nameservers from the approved DNS
	// peers. DNS retains Service/backend IPs for legacy peers, or only Service
	// IPs when protected selectors follow replacement backend Pods.
	DNSResolverIPs  []string      `json:"dnsResolverIPs"`
	DNS             []Peer        `json:"dns"`
	Gateway         []Destination `json:"gateway"`
	Ingress         []Destination `json:"ingress,omitempty"`
	KubernetesAPI   []Destination `json:"kubernetesAPI,omitempty"`
	Peers           []Peer        `json:"peers,omitempty"`
	PodCIDRs        []string      `json:"podCIDRs"`
	ServiceCIDRs    []string      `json:"serviceCIDRs"`
	NodeCIDRs       []string      `json:"nodeCIDRs"`
	ManagementCIDRs []string      `json:"managementCIDRs"`
	MetadataCIDRs   []string      `json:"metadataCIDRs"`
}

// ApprovedGrant approves exactly one role and binding scope for one policy UID.
// RoleUID, RoleResourceVersion and normalized Rules must match an authoritative
// ClusterRole read at the trusted snapshot boundary (see VerifyPolicyGrants).
// Revision identifies the approval, not the Kubernetes resourceVersion. Compile
// only consumes this evidence; it does not observe live roles or authorize use of
// a stale snapshot. Approval may explicitly include wildcard or secret access.
// An empty Namespace authorizes a ClusterRoleBinding. The current policy API
// requests ClusterRoles only; a Role inventory entry never aliases a ClusterRole.
type ApprovedGrant struct {
	PolicyUID           types.UID           `json:"policyUID"`
	Namespace           string              `json:"namespace,omitempty"`
	RoleRef             rbacv1.RoleRef      `json:"roleRef"`
	RoleUID             types.UID           `json:"roleUID"`
	RoleResourceVersion string              `json:"roleResourceVersion"`
	Revision            string              `json:"revision"`
	Rules               []rbacv1.PolicyRule `json:"rules"`
}

// ApprovedIdentity is per-Agent authorization, not cloud provisioning.
type ApprovedIdentity struct {
	AgentUID           types.UID      `json:"agentUID"`
	Namespace          string         `json:"namespace"`
	ServiceAccountName string         `json:"serviceAccountName"`
	Revision           string         `json:"revision"`
	Kubernetes         bool           `json:"kubernetes"`
	Entra              *EntraIdentity `json:"entra,omitempty"`
}

type EntraIdentity struct {
	ClientID string `json:"clientID"`
	TenantID string `json:"tenantID"`
}

type Platform struct {
	Adapter                AdapterRegistration         `json:"adapter"`
	Topology               Topology                    `json:"topology"`
	Grants                 []ApprovedGrant             `json:"grants,omitempty"`
	Identities             []ApprovedIdentity          `json:"identities,omitempty"`
	KubernetesAudience     string                      `json:"kubernetesAudience,omitempty"`
	TokenExpirationSeconds int64                       `json:"tokenExpirationSeconds,omitempty"`
	IngressMTLS            *ingressmtls.Registration   `json:"ingressMTLS,omitempty"`
	EgressSNI              *egresscapture.Registration `json:"egressSNI,omitempty"`
}

type CompileInput struct {
	ConnectionsMCP  *Destination
	NativeExecution bool
	Resolution      sandbox.Resolution
	AgentName       string
	Namespace       string
	ServiceName     string
	Platform        Platform
	IdentityPin     *IdentityPin
	// CustomIngress is an operator-derived Custom Harness HTTP contract. It is
	// deliberately separate from AgentPolicy so callers cannot request a port
	// or peer through an Agent-owned object.
	CustomIngress *CustomIngress
	// TerminalIngress is the controller-derived port of the isolated Custom
	// terminal. It is separate from CustomIngress because its endpoint selector
	// and egress authority must not apply to the Agent workload.
	TerminalIngress *TerminalIngress
}

type CustomIngress struct {
	Port        int32           `json:"port"`
	WebUI       bool            `json:"webUI,omitempty"`
	ChatCallers []IngressCaller `json:"chatCallers,omitempty"`
}
type IngressCaller struct {
	Namespace string            `json:"namespace"`
	PodLabels map[string]string `json:"podLabels"`
}
type TerminalIngress struct {
	Port int32 `json:"port"`
}

// IdentityPin is supplied from protected bootstrap readback, never policy input.
type IdentityPin struct {
	Name         string
	UID          types.UID
	BootstrapUID types.UID
}

// Plan contains desired resources, never observed status. Render Resources
// unchanged and put AgentUIDLabel on the protected workload. Revision includes
// effective topology, identity and grant contents, not just the resolver hash.
type Plan struct {
	Binding            sandbox.Binding             `json:"binding"`
	AgentName          string                      `json:"agentName"`
	Namespace          string                      `json:"namespace"`
	ResolutionRevision string                      `json:"resolutionRevision"`
	Revision           string                      `json:"revision"`
	Network            NetworkPlan                 `json:"network"`
	Identity           IdentityPlan                `json:"identity"`
	Resources          []unstructured.Unstructured `json:"resources"`
	IngressMTLS        *ingressmtls.Plan           `json:"ingressMTLS,omitempty"`
	EgressSNI          *egresscapture.Plan         `json:"egressSNI,omitempty"`
	ProxyDigest        string                      `json:"proxyDigest,omitempty"`
}

type NetworkPlan struct {
	Adapter     string                     `json:"adapter"`
	IngressMode api.AgentPolicyIngressMode `json:"ingressMode"`
	DNSIPs      []string                   `json:"dnsIPs"`
	// The existing wire field also carries explicitly registered ingress Pod CIDRs.
	TrustedProxyIPs []string             `json:"trustedProxyIPs,omitempty"`
	Policy          CiliumNetworkPolicy  `json:"policy"`
	TerminalPolicy  *CiliumNetworkPolicy `json:"terminalPolicy,omitempty"`
}

type IdentityPlan struct {
	ServiceAccountName           string                      `json:"serviceAccountName"`
	AutomountServiceAccountToken bool                        `json:"automountServiceAccountToken"`
	Volumes                      []corev1.Volume             `json:"volumes,omitempty"`
	VolumeMounts                 []corev1.VolumeMount        `json:"volumeMounts,omitempty"`
	Env                          []corev1.EnvVar             `json:"env,omitempty"`
	ServiceAccount               *corev1.ServiceAccount      `json:"serviceAccount,omitempty"`
	BootstrapServiceAccount      bool                        `json:"bootstrapServiceAccount,omitempty"`
	RoleBindings                 []rbacv1.RoleBinding        `json:"roleBindings,omitempty"`
	ClusterRoleBindings          []rbacv1.ClusterRoleBinding `json:"clusterRoleBindings,omitempty"`
	Bindings                     []BindingReference          `json:"bindings,omitempty"`
}

// BindingReference is durable revocation inventory. Persist it before applying
// a binding, retain obsolete entries until deletion is observed, and record UID
// after creation when available. Never derive this inventory from policy alone.
type BindingReference struct {
	Kind                    string         `json:"kind"`
	Namespace               string         `json:"namespace,omitempty"`
	Name                    string         `json:"name"`
	UID                     types.UID      `json:"uid,omitempty"`
	AgentName               string         `json:"agentName"`
	AgentUID                types.UID      `json:"agentUID"`
	ServiceAccountName      string         `json:"serviceAccountName"`
	ServiceAccountNamespace string         `json:"serviceAccountNamespace"`
	RoleRef                 rbacv1.RoleRef `json:"roleRef"`
	GrantRevision           string         `json:"grantRevision,omitempty"`
}

// These types intentionally model only the supported subset of Cilium's API.
// In particular, no FQDN deny or guest-external mTLS is implied by an empty rule.
// All metav1.LabelSelector fields below are ONLY the JSON shape of Cilium
// EndpointSelectors: their k8s:-prefixed keys are Cilium source-qualified labels,
// not Kubernetes label keys. Do not pass them to LabelSelectorAsSelector or
// Kubernetes label-selector validation; interpretation belongs to Cilium.
type CiliumNetworkPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata"`
	Spec              CiliumPolicySpec `json:"spec"`
}

type CiliumPolicySpec struct {
	EndpointSelector  metav1.LabelSelector `json:"endpointSelector"`
	EnableDefaultDeny DefaultDeny          `json:"enableDefaultDeny"`
	Ingress           []IngressRule        `json:"ingress"`
	IngressDeny       []IngressRule        `json:"ingressDeny"`
	Egress            []EgressRule         `json:"egress"`
	EgressDeny        []EgressRule         `json:"egressDeny"`
}

type DefaultDeny struct {
	Ingress bool `json:"ingress"`
	Egress  bool `json:"egress"`
}

type CIDRRule struct {
	CIDR   string   `json:"cidr"`
	Except []string `json:"except,omitempty"`
}

type PortRule struct {
	Ports []PortRange `json:"ports"`
}

type PortRange struct {
	Port     string          `json:"port"`
	EndPort  int32           `json:"endPort,omitempty"`
	Protocol corev1.Protocol `json:"protocol"`
}

type EgressRule struct {
	ToCIDRSet   []CIDRRule             `json:"toCIDRSet,omitempty"`
	ToEndpoints []metav1.LabelSelector `json:"toEndpoints,omitempty"`
	ToEntities  []string               `json:"toEntities,omitempty"`
	ToPorts     []PortRule             `json:"toPorts,omitempty"`
}

type IngressRule struct {
	FromCIDRSet   []CIDRRule             `json:"fromCIDRSet,omitempty"`
	FromEndpoints []metav1.LabelSelector `json:"fromEndpoints,omitempty"`
	FromEntities  []string               `json:"fromEntities,omitempty"`
	ToPorts       []PortRule             `json:"toPorts,omitempty"`
}
