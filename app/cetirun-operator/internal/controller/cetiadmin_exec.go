// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *AgentReconciler) reconcileCetiAdminAccess(ctx context.Context, name string) error {
	provider, ok := r.CetiAdmin.(interface {
		ReconcileRuntimeAccess(context.Context, string) error
	})
	if !ok {
		return nil
	}
	return provider.ReconcileRuntimeAccess(ctx, name)
}

type cetiAdminExecObjects struct {
	agent     *api.Agent
	policy    *admissionv1.ValidatingAdmissionPolicy
	binding   *admissionv1.ValidatingAdmissionPolicyBinding
	protected bool
}

// GrantExecAccess updates one recorded policy, without granting RBAC. Callers
// must run RevokeExecAccess for protected/removed targets and before rebinding
// a replacement source or ServiceAccount.
func (r *CetiAdminReconciler) GrantExecAccess(ctx context.Context, binding cetiadmin.Binding, target cetiadmin.TargetIdentity) error {
	if err := r.checkExecGrant(ctx, binding, target); err != nil {
		return err
	}
	want, err := execAdminExpectation(binding.Record(), target)
	if err != nil {
		return err
	}
	objects, err := r.execObjects(ctx, binding.Record(), target)
	if err != nil {
		return err
	}
	if objects.protected || target.Protected || objects.agent.Spec.Team != target.Team {
		return cetiadmin.ErrUnbound
	}
	if objects.policy.UID == "" {
		return nil
	}
	if err := objects.checkBinding(want); err != nil {
		return err
	}
	raw, err := json.Marshal(want)
	if err != nil {
		return err
	}
	if err := r.patchExecAnnotation(ctx, objects.agent, string(raw)); err != nil {
		return err
	}
	if err := r.patchExecReceipt(ctx, objects.policy, string(raw)); err != nil {
		return err
	}
	// Recheck the current binding before the write which enables the exception.
	if err := r.checkExecGrant(ctx, binding, target); err != nil {
		return err
	}
	objects, err = r.execObjects(ctx, binding.Record(), target)
	if err != nil {
		return err
	}
	if objects.protected || objects.agent.Spec.Team != target.Team {
		return cetiadmin.ErrUnbound
	}
	if err := objects.checkBinding(want); err != nil {
		return err
	}
	if objects.agent.Annotations[registryauth.ExecAdminBindingAnnotation] != string(raw) {
		return cetiadmin.ErrConflict
	}
	return r.patchExecConditions(ctx, objects.policy, []admissionv1.MatchCondition{want.MatchCondition()})
}

// RevokeExecAccess uses the retained record even when its source or SA has
// disappeared. It never removes a different installation's exception.
func (r *CetiAdminReconciler) RevokeExecAccess(ctx context.Context, record cetiadmin.Record, target cetiadmin.TargetIdentity) error {
	want, err := execAdminExpectation(record, target)
	if err != nil {
		return err
	}
	objects, err := r.execObjects(ctx, record, target)
	if err != nil {
		return err
	}
	if err := objects.checkBinding(want); err != nil {
		return err
	}
	if objects.policy.UID != "" {
		if err := r.patchExecConditions(ctx, objects.policy, nil); err != nil {
			return err
		}
		if err := r.patchExecReceipt(ctx, objects.policy, ""); err != nil {
			return err
		}
	}
	return r.patchExecAnnotation(ctx, objects.agent, "")
}

func (r *CetiAdminReconciler) checkExecGrant(ctx context.Context, binding cetiadmin.Binding, target cetiadmin.TargetIdentity) error {
	if binding.ServiceAccountUsername() == "" || r.AdmissionReady == nil {
		return cetiadmin.ErrUnbound
	}
	if err := r.AdmissionReady(ctx); err != nil {
		return err
	}
	_, state, err := r.readState(ctx)
	if err != nil {
		return err
	}
	if state.Record != binding.Record() || !state.CanRun() {
		return cetiadmin.ErrUnbound
	}
	current, err := r.currentAccessBinding(ctx, state)
	if err != nil {
		return err
	}
	if current.Record() != binding.Record() {
		return cetiadmin.ErrConflict
	}
	_, actual, err := r.runtimeTarget(ctx, current.Config(), state.Record, target.Agent.Name)
	if err != nil {
		return err
	}
	target.Resources = nil
	if !reflect.DeepEqual(actual, target) || actual.Protected {
		return cetiadmin.ErrConflict
	}
	return nil
}

func execAdminExpectation(record cetiadmin.Record, target cetiadmin.TargetIdentity) (registryauth.ExecAdminBinding, error) {
	want := registryauth.ExecAdminBinding{TargetAgentUID: target.Agent.UID, NamespaceUID: target.Namespace.UID,
		Username:          "system:serviceaccount:" + record.ServiceAccount.Namespace + ":" + record.ServiceAccount.Name,
		ServiceAccountUID: record.ServiceAccount.UID, SourceUID: record.Source.UID}
	raw, err := json.Marshal(want)
	if err != nil {
		return want, err
	}
	return registryauth.ParseExecAdminBinding(string(raw), target.Agent.UID, target.Namespace.UID)
}

func (r *CetiAdminReconciler) execObjects(ctx context.Context, record cetiadmin.Record, target cetiadmin.TargetIdentity) (*cetiAdminExecObjects, error) {
	if record.SchemaVersion != 1 || record.Source.Name != r.Source.Name || record.Source.Namespace != r.Source.Namespace ||
		target.Agent.Namespace != "" || target.Namespace.Namespace != "" || target.Agent.Name == "" ||
		target.Namespace.Name != instanceNamespace(target.Agent.Name) || target.Release != helmReleaseName(target.Agent.Name) ||
		target.Agent.UID == record.Agent.UID || target.Agent.Name == record.Agent.Name || target.Namespace.Name == record.Namespace.Name {
		return nil, cetiadmin.ErrConflict
	}
	agent, namespace := &api.Agent{}, &corev1.Namespace{}
	if err := r.reader().Get(ctx, client.ObjectKey{Name: target.Agent.Name}, agent); err != nil {
		return nil, err
	}
	if err := r.reader().Get(ctx, client.ObjectKey{Name: target.Namespace.Name}, namespace); err != nil {
		return nil, err
	}
	if !identityMatches(target.Agent, agent) || !identityMatches(target.Namespace, namespace) || string(agent.Spec.Agent.Type) != target.Runtime {
		return nil, cetiadmin.ErrConflict
	}
	switch agent.Spec.Agent.Type {
	case api.AgentTypeCustom:
		if r.Runtime == nil || (len(namespace.OwnerReferences) != 0 && !execAgentOwned(namespace, agent)) {
			return nil, cetiadmin.ErrConflict
		}
		if agent.Spec.SandboxRef != "" || agent.Status.Sandbox != nil {
			if !cetiadmin.NativeSandboxNamespaceOwned(agent, namespace) {
				return nil, cetiadmin.ErrConflict
			}
		} else {
			receipt, err := r.Runtime.readCustomOwnership(ctx, agent)
			if err != nil {
				return nil, err
			}
			if receipt.NamespaceUID != namespace.UID || receipt.Namespace != namespace.Name || receipt.Release != target.Release {
				return nil, cetiadmin.ErrConflict
			}
		}
	case api.AgentTypeHermes, api.AgentTypeOpenClaw:
		if !execAgentOwned(namespace, agent) {
			return nil, cetiadmin.ErrConflict
		}
	default:
		return nil, cetiadmin.ErrConflict
	}
	objects := &cetiAdminExecObjects{agent: agent, policy: &admissionv1.ValidatingAdmissionPolicy{}, binding: &admissionv1.ValidatingAdmissionPolicyBinding{},
		protected: agent.Spec.SandboxRef != "" || agent.Status.Sandbox != nil}
	if agent.Spec.Team != "" {
		team := &api.Team{}
		if err := r.reader().Get(ctx, client.ObjectKey{Name: agent.Spec.Team}, team); err != nil {
			return nil, err
		}
		if team.UID == "" || team.DeletionTimestamp != nil {
			return nil, cetiadmin.ErrConflict
		}
		objects.protected = objects.protected || team.Spec.PolicyConfigured()
	}
	name := instanceServiceName(agent.Name) + "-" + namespace.Name + "-deny-exec"
	policyErr := r.reader().Get(ctx, client.ObjectKey{Name: name}, objects.policy)
	bindingErr := r.reader().Get(ctx, client.ObjectKey{Name: name}, objects.binding)
	if apierrors.IsNotFound(policyErr) && apierrors.IsNotFound(bindingErr) {
		return objects, nil
	}
	if policyErr != nil || bindingErr != nil {
		return nil, errors.Join(policyErr, bindingErr)
	}
	for _, item := range []struct {
		kind   string
		object client.Object
	}{{"ValidatingAdmissionPolicy", objects.policy}, {"ValidatingAdmissionPolicyBinding", objects.binding}} {
		if item.object.GetUID() == "" || item.object.GetDeletionTimestamp() != nil || item.object.GetResourceVersion() == "" ||
			!cetiAdminManifestOwned(item.object, agent, target) || len(item.object.GetOwnerReferences()) > 1 {
			return nil, cetiadmin.ErrConflict
		}
		annotations := item.object.GetAnnotations()
		if owner := annotations["meta.helm.sh/release-name"]; owner != "" && owner != target.Release {
			return nil, cetiadmin.ErrConflict
		}
		if owner := annotations["meta.helm.sh/release-namespace"]; owner != "" && owner != namespace.Name {
			return nil, cetiadmin.ErrConflict
		}
	}
	expression := "false"
	if agent.Spec.Agent.Type == api.AgentTypeCustom && len(objects.policy.Spec.Validations) == 1 && objects.policy.Spec.Validations[0].Expression != "false" {
		if namespace.Annotations[customAgentUID] != string(agent.UID) {
			return nil, cetiadmin.ErrConflict
		}
		workload := instanceServiceName(agent.Name)
		expression = terminalpolicy.AdmissionExpression(namespace.Name, workload+"-0", "cetirun", terminalpolicy.WorkloadName(workload), string(agent.UID))
	}
	base := objects.policy.DeepCopy()
	base.Spec.MatchConditions = nil
	if !registryauth.ExecPolicyMatches(base, objects.binding, namespace.Name, expression, nil) {
		return nil, cetiadmin.ErrConflict
	}
	return objects, nil
}

func execAgentOwned(object client.Object, agent *api.Agent) bool {
	return len(object.GetOwnerReferences()) == 1 && hasAgentOwner(object, agent)
}

func (o *cetiAdminExecObjects) checkBinding(want registryauth.ExecAdminBinding) error {
	if raw, present := o.agent.Annotations[registryauth.ExecAdminBindingAnnotation]; present {
		got, err := registryauth.ParseExecAdminBinding(raw, want.TargetAgentUID, want.NamespaceUID)
		if err != nil || got != want {
			return cetiadmin.ErrConflict
		}
	}
	conditions := o.policy.Spec.MatchConditions
	if raw := o.policy.Annotations[cetiAdminExecReceipt]; raw != "" {
		got, err := registryauth.ParseExecAdminBinding(raw, want.TargetAgentUID, want.NamespaceUID)
		if err != nil || got != want {
			return cetiadmin.ErrConflict
		}
	}
	if len(conditions) > 1 || len(conditions) == 1 && conditions[0] != want.MatchCondition() {
		return cetiadmin.ErrConflict
	}
	return nil
}

const cetiAdminExecReceipt = "cetirun.microsoft.com/cetiadmin-exec-grant"

func (r *CetiAdminReconciler) patchExecReceipt(ctx context.Context, policy *admissionv1.ValidatingAdmissionPolicy, raw string) error {
	if policy.Annotations[cetiAdminExecReceipt] == raw {
		return nil
	}
	annotations := maps.Clone(policy.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}
	if raw == "" {
		delete(annotations, cetiAdminExecReceipt)
	} else {
		annotations[cetiAdminExecReceipt] = raw
	}
	patch, err := execConditionalPatch(policy, "/metadata/annotations", annotations)
	if err != nil {
		return err
	}
	writeErr := r.Patch(ctx, policy.DeepCopy(), patch)
	current := &admissionv1.ValidatingAdmissionPolicy{}
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(policy), current); err != nil {
		return errors.Join(writeErr, err)
	}
	if current.UID != policy.UID || !reflect.DeepEqual(current.Spec, policy.Spec) || !maps.Equal(current.Annotations, annotations) {
		return errors.Join(writeErr, cetiadmin.ErrConflict)
	}
	*policy = *current
	return nil
}

func (r *CetiAdminReconciler) revokeRuntimeExec(ctx context.Context, record cetiadmin.Record, agentName string) error {
	if record.ServiceAccount.UID == "" {
		return nil
	}
	want := registryauth.ExecAdminBinding{Username: "system:serviceaccount:" + record.ServiceAccount.Namespace + ":" + record.ServiceAccount.Name, ServiceAccountUID: record.ServiceAccount.UID, SourceUID: record.Source.UID}
	policies := &admissionv1.ValidatingAdmissionPolicyList{}
	if err := r.reader().List(ctx, policies); err != nil {
		return err
	}
	var failures []error
	for i := range policies.Items {
		policy := &policies.Items[i]
		if !slices.Contains(policy.Spec.MatchConditions, want.MatchCondition()) && policy.Annotations[cetiAdminExecReceipt] == "" {
			continue
		}
		match := policy.Spec.MatchConstraints
		if match == nil || match.NamespaceSelector == nil {
			failures = append(failures, cetiadmin.ErrConflict)
			continue
		}
		namespace := match.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]
		name := strings.TrimPrefix(namespace, "cetirun-")
		if agentName != "" && name != agentName {
			continue
		}
		if name == namespace || name == "" || policy.Name != instanceServiceName(name)+"-"+namespace+"-deny-exec" ||
			policy.Annotations["meta.helm.sh/release-name"] != helmReleaseName(name) || policy.Annotations["meta.helm.sh/release-namespace"] != namespace {
			failures = append(failures, cetiadmin.ErrConflict)
			continue
		}
		if raw := policy.Annotations[cetiAdminExecReceipt]; raw != "" {
			var receipt registryauth.ExecAdminBinding
			if json.Unmarshal([]byte(raw), &receipt) != nil {
				failures = append(failures, cetiadmin.ErrConflict)
				continue
			}
			if receipt.SourceUID != record.Source.UID || receipt.ServiceAccountUID != record.ServiceAccount.UID || receipt.Username != want.Username {
				if slices.Contains(policy.Spec.MatchConditions, want.MatchCondition()) {
					failures = append(failures, cetiadmin.ErrConflict)
				}
				continue
			}
			if _, err := registryauth.ParseExecAdminBinding(raw, receipt.TargetAgentUID, receipt.NamespaceUID); err != nil {
				failures = append(failures, err)
				continue
			}
		}
		if len(policy.Spec.MatchConditions) > 1 || len(policy.Spec.MatchConditions) == 1 && policy.Spec.MatchConditions[0] != want.MatchCondition() {
			failures = append(failures, cetiadmin.ErrConflict)
			continue
		}
		if err := r.patchExecConditions(ctx, policy, nil); err != nil {
			failures = append(failures, err)
			continue
		}
		failures = append(failures, r.patchExecReceipt(ctx, policy, ""))
	}
	agents := &api.AgentList{}
	if err := r.reader().List(ctx, agents); err != nil {
		return errors.Join(errors.Join(failures...), err)
	}
	for i := range agents.Items {
		agent := &agents.Items[i]
		if agentName != "" && agent.Name != agentName {
			continue
		}
		raw := agent.Annotations[registryauth.ExecAdminBindingAnnotation]
		if raw == "" {
			continue
		}
		var grant registryauth.ExecAdminBinding
		if json.Unmarshal([]byte(raw), &grant) != nil {
			failures = append(failures, fmt.Errorf("invalid exec binding on Agent %s", agent.Name))
			continue
		}
		if grant.SourceUID != record.Source.UID || grant.ServiceAccountUID != record.ServiceAccount.UID || grant.Username != want.Username {
			continue
		}
		if _, err := registryauth.ParseExecAdminBinding(raw, agent.UID, grant.NamespaceUID); err != nil {
			failures = append(failures, err)
			continue
		}
		failures = append(failures, r.patchExecAnnotation(ctx, agent, ""))
	}
	return errors.Join(failures...)
}

func execConditionalPatch(object client.Object, path string, value any) (client.Patch, error) {
	if object.GetUID() == "" || object.GetResourceVersion() == "" {
		return nil, cetiadmin.ErrConflict
	}
	body, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": object.GetUID()},
		{"op": "test", "path": "/metadata/resourceVersion", "value": object.GetResourceVersion()},
		{"op": "add", "path": path, "value": value},
	})
	return client.RawPatch(types.JSONPatchType, body), err
}

func (r *CetiAdminReconciler) patchExecConditions(ctx context.Context, policy *admissionv1.ValidatingAdmissionPolicy, conditions []admissionv1.MatchCondition) error {
	if slices.Equal(policy.Spec.MatchConditions, conditions) {
		return nil
	}
	if conditions == nil {
		conditions = []admissionv1.MatchCondition{}
	}
	patch, err := execConditionalPatch(policy, "/spec/matchConditions", conditions)
	if err != nil {
		return err
	}
	writeErr := r.Patch(ctx, policy.DeepCopy(), patch)
	current := &admissionv1.ValidatingAdmissionPolicy{}
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(policy), current); err != nil {
		return errors.Join(writeErr, err)
	}
	before, after := policy.Spec, current.Spec
	before.MatchConditions, after.MatchConditions = nil, nil
	if current.UID != policy.UID || current.DeletionTimestamp != nil || !reflect.DeepEqual(before, after) || !slices.Equal(current.Spec.MatchConditions, conditions) ||
		!reflect.DeepEqual(current.OwnerReferences, policy.OwnerReferences) || !maps.Equal(current.Annotations, policy.Annotations) || !maps.Equal(current.Labels, policy.Labels) {
		return errors.Join(writeErr, cetiadmin.ErrConflict)
	}
	*policy = *current
	return nil
}

func (r *CetiAdminReconciler) patchExecAnnotation(ctx context.Context, agent *api.Agent, raw string) error {
	if current, present := agent.Annotations[registryauth.ExecAdminBindingAnnotation]; (present && current == raw) || (!present && raw == "") {
		return nil
	}
	annotations := maps.Clone(agent.Annotations)
	if annotations == nil {
		annotations = map[string]string{}
	}
	if raw == "" {
		delete(annotations, registryauth.ExecAdminBindingAnnotation)
	} else {
		annotations[registryauth.ExecAdminBindingAnnotation] = raw
	}
	patch, err := execConditionalPatch(agent, "/metadata/annotations", annotations)
	if err != nil {
		return err
	}
	writeErr := r.Patch(ctx, agent.DeepCopy(), patch)
	current := &api.Agent{}
	if err := r.reader().Get(ctx, client.ObjectKeyFromObject(agent), current); err != nil {
		return errors.Join(writeErr, err)
	}
	if current.UID != agent.UID || current.DeletionTimestamp != nil || !maps.Equal(current.Annotations, annotations) {
		return errors.Join(writeErr, cetiadmin.ErrConflict)
	}
	*agent = *current
	return nil
}
