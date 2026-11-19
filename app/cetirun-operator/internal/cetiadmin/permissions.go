// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"slices"

	rbacv1 "k8s.io/api/rbac/v1"
)

// DiagnosticResource is an explicit discovery allowlist, not a wildcard grant.
// The controller resolves scope from discovery before delegating access.
type DiagnosticResource struct {
	Group    string
	Kind     string
	Resource string
}

func DiagnosticResources() []DiagnosticResource {
	return []DiagnosticResource{
		{"", "Node", "nodes"}, {"", "Namespace", "namespaces"}, {"", "PersistentVolume", "persistentvolumes"},
		{"", "Pod", "pods"}, {"", "Service", "services"}, {"", "Endpoints", "endpoints"},
		{"", "Event", "events"}, {"", "PersistentVolumeClaim", "persistentvolumeclaims"},
		{"", "ConfigMap", "configmaps"}, {"", "ServiceAccount", "serviceaccounts"},
		{"", "ResourceQuota", "resourcequotas"}, {"", "LimitRange", "limitranges"},
		{"apps", "StatefulSet", "statefulsets"}, {"apps", "Deployment", "deployments"},
		{"apps", "DaemonSet", "daemonsets"}, {"apps", "ReplicaSet", "replicasets"}, {"apps", "ControllerRevision", "controllerrevisions"},
		{"batch", "Job", "jobs"}, {"batch", "CronJob", "cronjobs"},
		{"autoscaling", "HorizontalPodAutoscaler", "horizontalpodautoscalers"}, {"policy", "PodDisruptionBudget", "poddisruptionbudgets"},
		{"events.k8s.io", "Event", "events"}, {"coordination.k8s.io", "Lease", "leases"},
		{"networking.k8s.io", "Ingress", "ingresses"}, {"networking.k8s.io", "IngressClass", "ingressclasses"},
		{"networking.k8s.io", "NetworkPolicy", "networkpolicies"}, {"networking.k8s.io", "IPAddress", "ipaddresses"},
		{"networking.k8s.io", "ServiceCIDR", "servicecidrs"}, {"discovery.k8s.io", "EndpointSlice", "endpointslices"},
		{"networking.istio.io", "Gateway", "gateways"}, {"networking.istio.io", "VirtualService", "virtualservices"},
		{"networking.istio.io", "DestinationRule", "destinationrules"}, {"networking.istio.io", "ServiceEntry", "serviceentries"},
		{"networking.istio.io", "Sidecar", "sidecars"}, {"networking.istio.io", "EnvoyFilter", "envoyfilters"},
		{"networking.istio.io", "WorkloadEntry", "workloadentries"}, {"networking.istio.io", "WorkloadGroup", "workloadgroups"},
		{"security.istio.io", "AuthorizationPolicy", "authorizationpolicies"}, {"security.istio.io", "PeerAuthentication", "peerauthentications"},
		{"security.istio.io", "RequestAuthentication", "requestauthentications"}, {"telemetry.istio.io", "Telemetry", "telemetries"},
		{"gateway.networking.k8s.io", "GatewayClass", "gatewayclasses"}, {"gateway.networking.k8s.io", "Gateway", "gateways"},
		{"gateway.networking.k8s.io", "HTTPRoute", "httproutes"}, {"gateway.networking.k8s.io", "GRPCRoute", "grpcroutes"},
		{"gateway.networking.k8s.io", "TCPRoute", "tcproutes"}, {"gateway.networking.k8s.io", "UDPRoute", "udproutes"},
		{"gateway.networking.k8s.io", "TLSRoute", "tlsroutes"}, {"gateway.networking.k8s.io", "ReferenceGrant", "referencegrants"},
		{"cilium.io", "CiliumNetworkPolicy", "ciliumnetworkpolicies"}, {"cilium.io", "CiliumClusterwideNetworkPolicy", "ciliumclusterwidenetworkpolicies"},
		{"cilium.io", "CiliumEndpoint", "ciliumendpoints"}, {"cilium.io", "CiliumEndpointSlice", "ciliumendpointslices"},
		{"cilium.io", "CiliumNode", "ciliumnodes"}, {"cilium.io", "CiliumIdentity", "ciliumidentities"},
		{"cilium.io", "CiliumEnvoyConfig", "ciliumenvoyconfigs"}, {"cilium.io", "CiliumClusterwideEnvoyConfig", "ciliumclusterwideenvoyconfigs"},
		{"storage.k8s.io", "StorageClass", "storageclasses"}, {"storage.k8s.io", "CSIDriver", "csidrivers"},
		{"storage.k8s.io", "CSINode", "csinodes"}, {"storage.k8s.io", "CSIStorageCapacity", "csistoragecapacities"},
		{"storage.k8s.io", "VolumeAttachment", "volumeattachments"}, {"storage.k8s.io", "VolumeAttributesClass", "volumeattributesclasses"},
		{"snapshot.storage.k8s.io", "VolumeSnapshot", "volumesnapshots"}, {"snapshot.storage.k8s.io", "VolumeSnapshotContent", "volumesnapshotcontents"},
		{"snapshot.storage.k8s.io", "VolumeSnapshotClass", "volumesnapshotclasses"},
		{"rbac.authorization.k8s.io", "Role", "roles"}, {"rbac.authorization.k8s.io", "RoleBinding", "rolebindings"},
		{"rbac.authorization.k8s.io", "ClusterRole", "clusterroles"}, {"rbac.authorization.k8s.io", "ClusterRoleBinding", "clusterrolebindings"},
		{"apiextensions.k8s.io", "CustomResourceDefinition", "customresourcedefinitions"}, {"apiregistration.k8s.io", "APIService", "apiservices"},
		{"admissionregistration.k8s.io", "ValidatingWebhookConfiguration", "validatingwebhookconfigurations"},
		{"admissionregistration.k8s.io", "MutatingWebhookConfiguration", "mutatingwebhookconfigurations"},
		{"admissionregistration.k8s.io", "ValidatingAdmissionPolicy", "validatingadmissionpolicies"},
		{"admissionregistration.k8s.io", "ValidatingAdmissionPolicyBinding", "validatingadmissionpolicybindings"},
		{"admissionregistration.k8s.io", "MutatingAdmissionPolicy", "mutatingadmissionpolicies"},
		{"admissionregistration.k8s.io", "MutatingAdmissionPolicyBinding", "mutatingadmissionpolicybindings"},
		{"certificates.k8s.io", "CertificateSigningRequest", "certificatesigningrequests"},
		{"config.gatekeeper.sh", "Config", "configs"}, {"templates.gatekeeper.sh", "ConstraintTemplate", "constrainttemplates"},
		{"status.gatekeeper.sh", "ConstraintPodStatus", "constraintpodstatuses"}, {"status.gatekeeper.sh", "ConstraintTemplatePodStatus", "constrainttemplatepodstatuses"},
		{"acn.azure.com", "NodeNetworkConfig", "nodenetworkconfigs"}, {"acn.azure.com", "PodNetworkConfig", "podnetworkconfigs"},
		{"acn.azure.com", "NodeInfo", "nodeinfos"}, {"acn.azure.com", "PodNetworkInstanceInfo", "podnetworkinstanceinfos"},
		{"aks.clusterconfig.azure.com", "AKSNodeClass", "aksnodeclasses"}, {"aks.clusterconfig.azure.com", "NodeManagement", "nodemanagements"},
		{"clusterconfig.azure.com", "AzureClusterIdentity", "azureclusteridentities"}, {"clusterconfig.azure.com", "Extension", "extensions"},
		{"azmonitoring.coreos.com", "ServiceMonitor", "servicemonitors"}, {"azmonitoring.coreos.com", "PodMonitor", "podmonitors"},
		{"azmonitoring.coreos.com", "PrometheusRule", "prometheusrules"}, {"azmonitoring.coreos.com", "Probe", "probes"},
		{"node.k8s.io", "RuntimeClass", "runtimeclasses"}, {"scheduling.k8s.io", "PriorityClass", "priorityclasses"},
		{"flowcontrol.apiserver.k8s.io", "FlowSchema", "flowschemas"}, {"flowcontrol.apiserver.k8s.io", "PriorityLevelConfiguration", "prioritylevelconfigurations"},
		{"cetirun.microsoft.com", "Agent", "agents"}, {"cetirun.microsoft.com", "Team", "teams"},
		{"cetirun.microsoft.com", "Sandbox", "sandboxes"}, {"cetirun.microsoft.com", "AgentPolicy", "agentpolicies"},
	}
}

func ReadRule(group string, resources ...string) rbacv1.PolicyRule {
	return rbacv1.PolicyRule{APIGroups: []string{group}, Resources: slices.Clone(resources), Verbs: []string{"get", "list", "watch"}}
}

func SupportedManifestKind(group, kind string) bool {
	switch group + "/" + kind {
	case "/Secret", "/ConfigMap", "/Service", "/ServiceAccount", "/PersistentVolumeClaim", "/PersistentVolume",
		"apps/StatefulSet", "apps/Deployment", "batch/Job", "rbac.authorization.k8s.io/Role", "rbac.authorization.k8s.io/RoleBinding",
		"rbac.authorization.k8s.io/ClusterRoleBinding", "networking.k8s.io/NetworkPolicy", "networking.k8s.io/Ingress", "networking.istio.io/Sidecar",
		"security.istio.io/AuthorizationPolicy", "security.istio.io/PeerAuthentication",
		"admissionregistration.k8s.io/ValidatingAdmissionPolicy", "admissionregistration.k8s.io/ValidatingAdmissionPolicyBinding",
		"autoscaling/HorizontalPodAutoscaler", "policy/PodDisruptionBudget":
		return true
	}
	return false
}

func ClusterOperationRules() []rbacv1.PolicyRule {
	return []rbacv1.PolicyRule{
		{NonResourceURLs: []string{"/healthz", "/readyz", "/livez", "/metrics", "/version"}, Verbs: []string{"get"}},
		{APIGroups: []string{"cetirun.microsoft.com"}, Resources: []string{"agents"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch"}},
	}
}

// NamespaceOperationRules supplies API verbs. Admission enforces current
// objects, maintenance handoff, allowed fields and storage growth.
func NamespaceOperationRules(protected bool) []rbacv1.PolicyRule {
	rules := []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"pods/log"}, Verbs: []string{"get"}},
		{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "update", "patch"}},
		{APIGroups: []string{"events.k8s.io"}, Resources: []string{"events"}, Verbs: []string{"create", "update", "patch"}},
		{APIGroups: []string{""}, Resources: []string{"persistentvolumeclaims"}, Verbs: []string{"update", "patch"}},
	}
	if !protected {
		rules = slices.Insert(rules, 0, ReadRule("", "secrets"))
		rules = append(rules,
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods/exec"}, Verbs: []string{"create"}},
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"delete"}},
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets", "configmaps"}, Verbs: []string{"create", "update", "patch"}},
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"services"}, Verbs: []string{"update", "patch"}},
			rbacv1.PolicyRule{APIGroups: []string{"apps"}, Resources: []string{"statefulsets", "deployments"}, Verbs: []string{"update", "patch"}},
		)
	}
	return rules
}
