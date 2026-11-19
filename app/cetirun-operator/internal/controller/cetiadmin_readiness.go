// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubeversion "k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const cetiAdminAdmissionName = "cetirun-cetiadmin-admission"

func (r *CetiAdminReconciler) NewAdmissionProbe(config *rest.Config) (func(context.Context) error, error) {
	if config == nil {
		return nil, fmt.Errorf("CetiAdmin admission probe requires Kubernetes configuration")
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var current version.Info
		data, err := discoveryClient.RESTClient().Get().AbsPath("/version").Do(ctx).Raw()
		if err != nil {
			return err
		}
		if json.Unmarshal(data, &current) != nil {
			return fmt.Errorf("Kubernetes version response is invalid")
		}
		parsed, err := kubeversion.ParseSemantic(current.GitVersion)
		if err != nil || !parsed.AtLeast(kubeversion.MustParseSemantic("1.37.0")) {
			return fmt.Errorf("CetiAdmin admission requires Kubernetes >= 1.37")
		}
		return r.CheckAdmissionReady(ctx)
	}, nil
}

// CheckAdmissionReady is the production installation prerequisite. The dry-run
// reaches the served handler with API-server TLS verification but persists no
// object. A Deployment merely existing is not sufficient to grant access.
func (r *CetiAdminReconciler) CheckAdmissionReady(ctx context.Context) error {
	if r.Runtime == nil || r.OperatorImage == "" || r.Source.Namespace == "" || r.Source.Name == "" {
		return fmt.Errorf("CetiAdmin admission prerequisites are not configured")
	}
	source := &corev1.ConfigMap{}
	if err := r.reader().Get(ctx, r.Source, source); err != nil {
		return err
	}
	config, err := cetiadmin.ParseConfig(source.Data[cetiadmin.ConfigKey])
	if err != nil || source.UID == "" || source.DeletionTimestamp != nil {
		return fmt.Errorf("CetiAdmin installation source is not current")
	}
	for _, resource := range []string{"agents", "teams"} {
		crd := &unstructured.Unstructured{}
		crd.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"})
		if err := r.reader().Get(ctx, client.ObjectKey{Name: resource + "." + api.GroupVersion.Group}, crd); err != nil {
			return fmt.Errorf("CetiAdmin CRD prerequisite %s: %w", resource, err)
		}
		conditions, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
		established := false
		for _, item := range conditions {
			condition, _ := item.(map[string]any)
			established = established || (condition["type"] == "Established" && condition["status"] == "True")
		}
		if !established || crd.GetDeletionTimestamp() != nil {
			return fmt.Errorf("CetiAdmin CRD %s is not Established", resource)
		}
		kind := "Agent"
		if resource == "teams" {
			kind = "Team"
		}
		if _, err := r.RESTMapper().RESTMapping(schema.GroupKind{Group: api.GroupVersion.Group, Kind: kind}, api.GroupVersion.Version); err != nil {
			return fmt.Errorf("CetiAdmin API discovery is not ready: %w", err)
		}
	}
	deployment := &appsv1.Deployment{}
	key := client.ObjectKey{Namespace: r.Source.Namespace, Name: "cetirun-sandbox-admission"}
	if err := r.reader().Get(ctx, key, deployment); err != nil {
		return err
	}
	replicas := ptr.Deref(deployment.Spec.Replicas, int32(1))
	if deployment.DeletionTimestamp != nil || replicas < 2 || deployment.Status.ObservedGeneration < deployment.Generation ||
		deployment.Status.UpdatedReplicas != replicas || deployment.Status.ReadyReplicas < replicas || deployment.Status.AvailableReplicas < replicas {
		return fmt.Errorf("CetiAdmin admission Deployment has not completed its current rollout")
	}
	var serving *corev1.Container
	for i := range deployment.Spec.Template.Spec.Containers {
		container := &deployment.Spec.Template.Spec.Containers[i]
		if slices.Contains(container.Args, "--mode=sandbox-admission") {
			if serving != nil {
				return fmt.Errorf("ambiguous admission container")
			}
			serving = container
		}
	}
	if serving == nil || serving.Image != r.OperatorImage {
		return fmt.Errorf("admission must run this Operator image's handler")
	}
	var admissionConfig struct {
		ControllerUsername string `json:"controllerUsername"`
		CetiAdmin          *struct {
			Namespace       string `json:"namespace"`
			ConfigMapName   string `json:"configMapName"`
			RuntimeUsername string `json:"runtimeUsername"`
		} `json:"cetiadmin"`
	}
	configured := false
	for _, env := range serving.Env {
		if env.Name == "CETIRUN_SANDBOX_ADMISSION" {
			err = json.Unmarshal([]byte(env.Value), &admissionConfig)
			configured = err == nil && env.ValueFrom == nil
		}
	}
	if !configured || admissionConfig.CetiAdmin == nil || admissionConfig.CetiAdmin.Namespace != r.Source.Namespace ||
		admissionConfig.CetiAdmin.RuntimeUsername != "system:serviceaccount:"+instanceNamespace(config.InstanceName)+":"+instanceServiceName(config.InstanceName) ||
		admissionConfig.CetiAdmin.ConfigMapName != r.Source.Name || admissionConfig.ControllerUsername != "system:serviceaccount:"+r.Runtime.OperatorNamespace+":"+r.Runtime.OperatorSAName {
		return fmt.Errorf("admission does not serve this CetiAdmin source and controller")
	}
	service := &corev1.Service{}
	if err := r.reader().Get(ctx, key, service); err != nil {
		return err
	}
	if service.DeletionTimestamp != nil || len(service.Spec.Selector) == 0 || deployment.Spec.Selector == nil ||
		!reflect.DeepEqual(service.Spec.Selector, deployment.Spec.Selector.MatchLabels) {
		return fmt.Errorf("admission Service does not select its current Deployment")
	}
	webhooks := &admissionv1.ValidatingWebhookConfiguration{}
	if err := r.reader().Get(ctx, client.ObjectKey{Name: cetiAdminAdmissionName}, webhooks); err != nil {
		return err
	}
	if err := checkCetiAdminWebhookContract(webhooks, r.Source.Namespace, config.InstanceName, r.Source.Name); err != nil {
		return err
	}
	mutators := &admissionv1.MutatingWebhookConfiguration{}
	if err := r.reader().Get(ctx, client.ObjectKey{Name: cetiAdminAdmissionName}, mutators); err != nil {
		return err
	}
	if len(mutators.Webhooks) != 1 || mutators.DeletionTimestamp != nil {
		return fmt.Errorf("CetiAdmin Agent selection/UID authorization mutator is unavailable")
	}
	mutator := mutators.Webhooks[0]
	wantedCaller := fmt.Sprintf(`request.userInfo.username == %q`, "system:serviceaccount:cetirun-"+config.InstanceName+":"+instanceServiceName(config.InstanceName))
	if mutator.Name != "cetiadmin-agent.cetirun.microsoft.com" || !validCetiAdminEndpoint(mutator.ClientConfig, mutator.FailurePolicy, r.Source.Namespace, cetiadmin.AgentDefaultingPath) ||
		mutator.ReinvocationPolicy == nil || *mutator.ReinvocationPolicy != admissionv1.NeverReinvocationPolicy ||
		len(mutator.MatchConditions) != 1 || mutator.MatchConditions[0].Name != "cetiadmin-caller" || normalizedCEL(mutator.MatchConditions[0].Expression) != normalizedCEL(wantedCaller) ||
		!cetiadmin.AdmissionNamespaceSelectorMatches(mutator.NamespaceSelector, nil) ||
		(mutator.ObjectSelector != nil && !reflect.DeepEqual(*mutator.ObjectSelector, metav1.LabelSelector{})) ||
		!ruleCovers(mutator.Rules, api.GroupVersion.Group, api.GroupVersion.Version, "agents", admissionv1.Create) ||
		!ruleCovers(mutator.Rules, api.GroupVersion.Group, api.GroupVersion.Version, "agents", admissionv1.Update) {
		return fmt.Errorf("CetiAdmin mutator does not cover the exact machine caller")
	}
	guardRequired := !cetiadmin.AdmissionSelectorMatches(mutator.NamespaceSelector, nil)
	for _, hook := range webhooks.Webhooks {
		var selector *metav1.LabelSelector
		if hook.Name == "cetiadmin-workloads.cetirun.microsoft.com" {
			selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "cetirun-operator"}}
		}
		guardRequired = guardRequired || !cetiadmin.AdmissionSelectorMatches(hook.NamespaceSelector, selector)
	}
	if guardRequired {
		if err := cetiadmin.CheckNamespaceAdmissionGuard(ctx, r.reader(), r.Source.Namespace, "cetirun-"+config.InstanceName); err != nil {
			return err
		}
		namespace := &corev1.Namespace{}
		if err := r.reader().Get(ctx, client.ObjectKey{Name: r.Source.Namespace}, namespace); err != nil {
			return err
		}
		before := namespace.DeepCopy()
		if namespace.Labels == nil {
			namespace.Labels = map[string]string{}
		}
		namespace.Labels["control-plane"] = "true"
		err := r.Patch(ctx, namespace, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}), client.DryRunAll)
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), cetiadmin.NamespaceAdmissionGuardName) {
			return fmt.Errorf("CetiAdmin namespace admission guard has not confirmed rejection of bypass labels: %v", err)
		}
	}
	probe := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{GenerateName: "cetiadmin-admission-probe-", Namespace: r.Source.Namespace,
		Annotations: map[string]string{cetiadmin.AnnotationSourceUID: string(source.UID)}}}
	if err := r.Create(ctx, probe, client.DryRunAll); err != nil {
		return fmt.Errorf("CetiAdmin served admission dry-run failed: %w", err)
	}
	return nil
}

func validCetiAdminEndpoint(config admissionv1.WebhookClientConfig, failure *admissionv1.FailurePolicyType, namespace, path string) bool {
	return failure != nil && *failure == admissionv1.Fail && config.URL == nil && len(config.CABundle) != 0 &&
		config.Service != nil && config.Service.Name == "cetirun-sandbox-admission" && config.Service.Namespace == namespace &&
		ptr.Deref(config.Service.Path, "") == path && ptr.Deref(config.Service.Port, int32(443)) == 443
}

func cetiAdminMarkedExpression() string {
	var parts []string
	for _, object := range []string{"object", "oldObject"} {
		parts = append(parts, fmt.Sprintf(`(%[1]s != null && has(%[1]s.metadata) && has(%[1]s.metadata.annotations) && %[1]s.metadata.annotations.exists(key, key.startsWith("cetirun.microsoft.com/cetiadmin-") || key.startsWith("cetirun.microsoft.com/helm-maintenance-") || key == "cetirun.microsoft.com/native-auth-rotation-request-id"))`, object))
	}
	return strings.Join(parts, " || ")
}

func cetiAdminWebhookConditions(kind, namespace, instance, source string) []admissionv1.MatchCondition {
	caller := fmt.Sprintf(`request.userInfo.username == %q`, "system:serviceaccount:cetirun-"+instance+":"+instanceServiceName(instance))
	switch kind {
	case "configuration":
		var managed []string
		for _, object := range []string{"object", "oldObject"} {
			managed = append(managed, fmt.Sprintf(`(%[1]s != null && has(%[1]s.metadata) && has(%[1]s.metadata.labels) && "app.kubernetes.io/managed-by" in %[1]s.metadata.labels && %[1]s.metadata.labels["app.kubernetes.io/managed-by"] == "cetirun-operator")`, object))
		}
		expression := `request.resource.group == "cetirun.microsoft.com" || ` + caller + " || " +
			fmt.Sprintf(`(request.resource.group == "" && request.resource.resource == "configmaps" && request.namespace == %q && (request.name in [%q, %q] || request.name.startsWith("cetiadmin-maintenance-"))) || `, namespace, source, cetiadmin.InstallationName(namespace, source)) +
			fmt.Sprintf(`(request.resource.group == "" && request.resource.resource == "namespaces" && (request.name == %q || `, "cetirun-"+instance) + strings.Join(managed, " || ") + ")) || " +
			fmt.Sprintf(`(request.resource.group == "" && request.resource.resource == "serviceaccounts" && request.namespace == %q && request.name == %q) || `, "cetirun-"+instance, instanceServiceName(instance)) + cetiAdminMarkedExpression()
		return []admissionv1.MatchCondition{{Name: "cetiadmin-configuration-scope", Expression: expression}}
	case "workloads":
		return []admissionv1.MatchCondition{{Name: "target-or-protected-workload", Expression: fmt.Sprintf(`request.namespace != %q || `, namespace) + caller + " || " + cetiAdminMarkedExpression()}}
	case "operations":
		return []admissionv1.MatchCondition{{Name: "cetiadmin-or-protected-marker", Expression: caller + " || " + cetiAdminMarkedExpression()}}
	}
	return nil
}

func normalizedCEL(input string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, input)
}

func checkCetiAdminWebhookContract(object *admissionv1.ValidatingWebhookConfiguration, namespace, instance, source string) error {
	if object.DeletionTimestamp != nil || len(object.Webhooks) != 3 {
		return fmt.Errorf("CetiAdmin requires its three current validation handlers")
	}
	for kind, required := range cetiAdminRequiredRules() {
		var selector *metav1.LabelSelector
		if kind == "workloads" {
			selector = &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "cetirun-operator"}}
		}
		var selected *admissionv1.ValidatingWebhook
		for i := range object.Webhooks {
			if object.Webhooks[i].Name == "cetiadmin-"+kind+".cetirun.microsoft.com" {
				selected = &object.Webhooks[i]
			}
		}
		if selected == nil || !validCetiAdminEndpoint(selected.ClientConfig, selected.FailurePolicy, namespace, cetiadmin.AdmissionValidationPath) ||
			selected.SideEffects == nil || *selected.SideEffects != admissionv1.SideEffectClassNone ||
			selected.MatchPolicy == nil || *selected.MatchPolicy != admissionv1.Equivalent ||
			!cetiadmin.AdmissionNamespaceSelectorMatches(selected.NamespaceSelector, selector) ||
			(selected.ObjectSelector != nil && !reflect.DeepEqual(*selected.ObjectSelector, metav1.LabelSelector{})) {
			return fmt.Errorf("CetiAdmin %s endpoint must be fail-closed without bypass selectors", kind)
		}
		expected := cetiAdminWebhookConditions(kind, namespace, instance, source)
		if len(expected) != len(selected.MatchConditions) {
			return fmt.Errorf("CetiAdmin %s match conditions changed", kind)
		}
		for i := range expected {
			if expected[i].Name != selected.MatchConditions[i].Name || normalizedCEL(expected[i].Expression) != normalizedCEL(selected.MatchConditions[i].Expression) {
				return fmt.Errorf("CetiAdmin %s match conditions changed", kind)
			}
		}
		for _, rule := range required {
			for _, resource := range rule.Resources {
				for _, operation := range rule.Operations {
					if !ruleCovers(selected.Rules, rule.APIGroups[0], rule.APIVersions[0], resource, operation) {
						return fmt.Errorf("CetiAdmin %s rule does not cover %s %s", kind, operation, resource)
					}
				}
			}
		}
	}
	return nil
}

func ruleCovers(rules []admissionv1.RuleWithOperations, group, version, resource string, operation admissionv1.OperationType) bool {
	wantedScope := admissionv1.NamespacedScope
	if group == api.GroupVersion.Group || resource == "namespaces" || strings.HasPrefix(resource, "namespaces/") || (group == "rbac.authorization.k8s.io" && strings.HasPrefix(resource, "cluster")) {
		wantedScope = admissionv1.ClusterScope
	}
	for _, rule := range rules {
		if (rule.Scope == nil || *rule.Scope == admissionv1.AllScopes || *rule.Scope == wantedScope) && slices.Contains(rule.APIGroups, group) && slices.Contains(rule.APIVersions, version) && slices.Contains(rule.Resources, resource) && slices.Contains(rule.Operations, operation) {
			return true
		}
	}
	return false
}

func cetiAdminRequiredRules() map[string][]admissionv1.RuleWithOperations {
	rule := func(group, version string, resources []string, connect bool) admissionv1.RuleWithOperations {
		operations := []admissionv1.OperationType{admissionv1.Create, admissionv1.Update, admissionv1.Delete}
		if connect {
			operations = append(operations, admissionv1.Connect)
		}
		return admissionv1.RuleWithOperations{Operations: operations, Rule: admissionv1.Rule{APIGroups: []string{group}, APIVersions: []string{version}, Resources: resources}}
	}
	return map[string][]admissionv1.RuleWithOperations{
		"configuration": {
			rule(api.GroupVersion.Group, api.GroupVersion.Version, []string{"agents", "agents/status", "agents/finalizers", "teams", "teams/status", "sandboxes", "agentpolicies"}, false),
			rule("", "v1", []string{"namespaces", "namespaces/status", "namespaces/finalize", "configmaps", "serviceaccounts"}, false),
		},
		"workloads": {
			rule("", "v1", []string{"configmaps"}, false),
			rule("", "v1", []string{"pods", "pods/exec", "pods/attach", "pods/portforward", "pods/ephemeralcontainers", "pods/binding"}, true),
			rule("apps", "v1", []string{"statefulsets", "statefulsets/scale", "deployments", "deployments/scale", "replicasets", "replicasets/scale", "daemonsets"}, false),
			rule("batch", "v1", []string{"jobs", "cronjobs"}, false),
		},
		"operations": {
			rule("", "v1", []string{"secrets", "services", "persistentvolumeclaims", "events"}, false),
			rule("events.k8s.io", "v1", []string{"events"}, false),
			rule("rbac.authorization.k8s.io", "v1", []string{"roles", "rolebindings", "clusterroles", "clusterrolebindings"}, false),
		},
	}
}
