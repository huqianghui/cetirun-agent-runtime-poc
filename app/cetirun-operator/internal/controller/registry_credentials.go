// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const registrySourceIndex = "spec.agent.registrySecretRef.name"

type registryAuthorizationClient struct {
	client.Client
	reader client.Reader
}

func (c registryAuthorizationClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.reader.List(ctx, list, opts...)
}

func (c registryAuthorizationClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	return c.reader.Get(ctx, key, object, opts...)
}

func (r *AgentReconciler) registryClient() client.Client {
	return registryAuthorizationClient{Client: r.Client, reader: r.customReader()}
}

func (r *AgentReconciler) registryNamespace() string {
	if r.RegistryCredentialNamespace != "" {
		return r.RegistryCredentialNamespace
	}
	return registryauth.DefaultNamespace
}

// The source namespace is platform-owned. Even a deleting legacy Agent must
// not enter namespace cleanup or release finalizers before explicit recovery.
func (r *AgentReconciler) rejectReservedRegistryInstance(ctx context.Context, agent *api.Agent) (ctrl.Result, error) {
	const reason = "ReservedInstanceName"
	const message = "instance namespace is reserved for registry credentials"
	before := agent.DeepCopy().Status
	agent.Status.AccessURL = ""
	agent.Status.Phase = api.PhaseFailed
	if !agent.DeletionTimestamp.IsZero() {
		agent.Status.Phase = api.PhaseDeleting
	}
	for _, condition := range []string{api.ConditionAgentConfigReady, api.ConditionReady, api.ConditionIngressReady} {
		setCondition(&agent.Status.Conditions, condition, metav1.ConditionFalse, reason, message)
	}
	if !reflect.DeepEqual(before, agent.Status) {
		if err := r.updateStatus(ctx, agent); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, fmt.Errorf("%s: %s", reason, message)
}

func (r *AgentReconciler) enqueueRegistrySecret(ctx context.Context, obj client.Object) []reconcile.Request {
	requests := []reconcile.Request{}
	if obj.GetNamespace() == r.registryNamespace() {
		instances := &api.AgentList{}
		if r.List(ctx, instances, client.MatchingFields{registrySourceIndex: obj.GetName()}) == nil {
			for _, agent := range instances.Items {
				requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: agent.Name}})
			}
		}
	}
	for _, owner := range obj.GetOwnerReferences() {
		if owner.APIVersion == api.GroupVersion.String() && owner.Kind == "Agent" {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: owner.Name}})
		}
	}
	return requests
}

func (r *AgentReconciler) registryWaiting(ctx context.Context, instance *api.Agent, reason string) (ctrl.Result, error) {
	before := instance.DeepCopy().Status
	setCondition(&instance.Status.Conditions, api.ConditionRegistryCredentialsReady, metav1.ConditionFalse,
		reason, "Registry credentials are not synchronized; existing workloads are retained")
	if !instance.DeletionTimestamp.IsZero() {
		instance.Status.Phase, instance.Status.AccessURL = api.PhaseDeleting, ""
		setCondition(&instance.Status.Conditions, api.ConditionReady, metav1.ConditionFalse, reason, "Waiting for registry credential cleanup")
	} else if instance.Status.Phase != api.PhaseReady && instance.Status.Phase != api.PhaseSuspended {
		instance.Status.Phase = api.PhasePending
		setCondition(&instance.Status.Conditions, api.ConditionReady, metav1.ConditionFalse, reason, "Waiting for registry credentials")
	} else if instance.Status.Phase == api.PhaseReady {
		sts := &appsv1.StatefulSet{}
		err := r.customReader().Get(ctx, client.ObjectKey{Namespace: instanceNamespace(instance.Name), Name: instanceServiceName(instance.Name)}, sts)
		if apierrors.IsNotFound(err) || (err == nil && sts.Status.ReadyReplicas == 0) {
			instance.Status.Phase = api.PhasePending
			setCondition(&instance.Status.Conditions, api.ConditionPodReady, metav1.ConditionFalse, "ExistingPodNotReady", "The previously applied workload is not ready")
			setCondition(&instance.Status.Conditions, api.ConditionReady, metav1.ConditionFalse, "ExistingPodNotReady", "Waiting for workload recovery and registry credentials")
		}
	}
	if !reflect.DeepEqual(before, instance.Status) {
		if err := r.updateStatus(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// Namespace ownership is checked before the first credential is copied. A
// legacy native namespace can be bound only from already-observed CR status.
func (r *AgentReconciler) ensureRegistryNamespace(ctx context.Context, agent *api.Agent, approvedUID string) error {
	nsName := instanceNamespace(agent.Name)
	if nsName == r.registryNamespace() || agent.UID == "" {
		return fmt.Errorf("RegistryNamespaceRejected")
	}
	ns := &corev1.Namespace{}
	err := r.customReader().Get(ctx, client.ObjectKey{Name: nsName}, ns)
	if apierrors.IsNotFound(err) {
		ns = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nsName,
			OwnerReferences: []metav1.OwnerReference{registryauth.Owner(agent)}}}
		if err := r.Create(ctx, ns); err != nil {
			return fmt.Errorf("RegistryNamespacePending")
		}
	} else if err != nil || !ns.DeletionTimestamp.IsZero() {
		return fmt.Errorf("RegistryNamespacePending")
	}
	if !hasAgentOwner(ns, agent) {
		if agent.Spec.Agent.Type == api.AgentTypeCustom && approvedUID != "" && string(ns.UID) == approvedUID {
			return nil
		}
		if len(ns.OwnerReferences) != 0 || agent.Status.Namespace != nsName ||
			agent.Status.HelmReleaseName != helmReleaseName(agent.Name) {
			return fmt.Errorf("RegistryNamespaceRejected")
		}
		before := ns.DeepCopy()
		ns.OwnerReferences = []metav1.OwnerReference{registryauth.Owner(agent)}
		if r.Patch(ctx, ns, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})) != nil {
			return fmt.Errorf("RegistryNamespacePending")
		}
	}
	return nil
}

func sameRegistryIntent(current, expected *api.Agent) bool {
	return current.UID == expected.UID && current.Generation == expected.Generation && current.DeletionTimestamp.IsZero() &&
		reflect.DeepEqual(current.Spec.Agent, expected.Spec.Agent) &&
		current.Annotations[registryauth.Operation] == expected.Annotations[registryauth.Operation]
}

func (r *AgentReconciler) checkRegistryIntent(ctx context.Context, expected *api.Agent) error {
	current := &api.Agent{}
	if r.customReader().Get(ctx, client.ObjectKeyFromObject(expected), current) != nil || !sameRegistryIntent(current, expected) {
		return fmt.Errorf("RegistryInputChanged")
	}
	return nil
}

// Only stable names leave this function. Source revisions are sync receipts,
// never workload inputs, annotations on Pods, or Agent configuration checksums.
func (r *AgentReconciler) syncRegistryCredentials(ctx context.Context, agent *api.Agent, serviceAccount string, approvedNamespace ...string) (string, error) {
	ref := agent.Spec.Agent.RegistrySecretRef
	if ref == nil {
		return "", nil
	}
	namespaceUID := ""
	if len(approvedNamespace) != 0 {
		namespaceUID = approvedNamespace[0]
	}
	if err := r.ensureRegistryNamespace(ctx, agent, namespaceUID); err != nil {
		return "", err
	}
	key := client.ObjectKey{Namespace: r.registryNamespace(), Name: ref.Name}
	source := &corev1.Secret{}
	if err := r.customReader().Get(ctx, key, source); err != nil {
		return "", fmt.Errorf("RegistrySourcePending")
	}
	if registryauth.Validate(source, agent) != nil || source.Annotations[registryauth.Operation] != agent.Annotations[registryauth.Operation] {
		return "", fmt.Errorf("RegistrySourceRejected")
	}
	name, err := registryauth.PullSecretName(agent.UID)
	if err != nil {
		return "", fmt.Errorf("RegistryOwnerRejected")
	}
	target := &corev1.Secret{}
	err = r.customReader().Get(ctx, client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: name}, target)
	missing := apierrors.IsNotFound(err)
	if err != nil && !missing {
		return "", fmt.Errorf("RegistrySyncFailed")
	}
	if !missing && (!registryauth.Owned(target, agent) || !target.DeletionTimestamp.IsZero() || target.Type != corev1.SecretTypeDockerConfigJson) {
		return "", fmt.Errorf("RegistryTargetConflict")
	}
	needsWrite := missing || !bytes.Equal(target.Data[corev1.DockerConfigJsonKey], source.Data[corev1.DockerConfigJsonKey]) ||
		len(target.Data) != 1 || target.Annotations[registryauth.SourceUID] != string(source.UID) ||
		target.Annotations[registryauth.SourceVersion] != source.ResourceVersion
	receipt := agent.Status.RegistryCredentials
	synchronized := receipt != nil && receipt.SourceUID == string(source.UID) &&
		receipt.SourceResourceVersion == source.ResourceVersion && receipt.PullSecretUID == string(target.UID) &&
		isConditionTrue(agent.Status.Conditions, api.ConditionRegistryCredentialsReady)
	// Observing an unchanged, synchronized copy is not a credential write.
	// Every copy change and subsequent workload application checks permissions anew.
	if needsWrite || !synchronized {
		if err := registryauth.CheckIsolation(ctx, r.registryClient(), r.registryNamespace(), instanceNamespace(agent.Name), serviceAccount, ref.Name); err != nil {
			return "", fmt.Errorf("RegistryIsolationUnverified")
		}
	}
	if err := r.checkRegistryIntent(ctx, agent); err != nil {
		return "", err
	}
	if missing {
		target = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: instanceNamespace(agent.Name),
			OwnerReferences: []metav1.OwnerReference{registryauth.Owner(agent)}, Labels: registryauth.Labels(agent.UID)},
			Type: corev1.SecretTypeDockerConfigJson}
	}
	if needsWrite {
		target.Data = map[string][]byte{corev1.DockerConfigJsonKey: bytes.Clone(source.Data[corev1.DockerConfigJsonKey])}
		target.Annotations = map[string]string{registryauth.SourceUID: string(source.UID), registryauth.SourceVersion: source.ResourceVersion}
		if missing {
			err = r.Create(ctx, target)
		} else {
			err = r.Update(ctx, target)
		}
		if err != nil {
			return "", fmt.Errorf("RegistrySyncFailed")
		}
	}
	latest := &corev1.Secret{}
	if r.customReader().Get(ctx, key, latest) != nil || latest.UID != source.UID || latest.ResourceVersion != source.ResourceVersion ||
		r.checkRegistryIntent(ctx, agent) != nil {
		return "", fmt.Errorf("RegistryInputChanged")
	}
	verified := &corev1.Secret{}
	if r.customReader().Get(ctx, client.ObjectKeyFromObject(target), verified) != nil || verified.UID != target.UID ||
		verified.ResourceVersion != target.ResourceVersion || registryauth.Validate(verified, agent) != nil {
		return "", fmt.Errorf("RegistryInputChanged")
	}
	before := agent.DeepCopy().Status
	agent.Status.RegistryCredentials = &api.RegistryCredentialsStatus{SourceUID: string(source.UID),
		SourceResourceVersion: source.ResourceVersion, PullSecretUID: string(target.UID)}
	setCondition(&agent.Status.Conditions, api.ConditionRegistryCredentialsReady, metav1.ConditionTrue,
		"RegistryCredentialsSynced", "Pull credentials synchronized; image pull success is observed separately")
	if !reflect.DeepEqual(before, agent.Status) {
		if r.updateStatus(ctx, agent) != nil {
			return "", fmt.Errorf("RegistryStatusPending")
		}
	}
	return name, nil
}

// Cleanup runs only after the desired pull list is applied, or during deletion.
// Names and purpose labels identify copies; ownership and UID/RV guard deletion.
func (r *AgentReconciler) cleanupRegistryCredentials(ctx context.Context, agent *api.Agent, deleting bool) error {
	if agent.UID == "" || (agent.Spec.Agent.RegistrySecretRef == nil &&
		agent.Status.RegistryCredentials == nil && agent.Annotations[registryauth.Operation] == "") {
		return nil
	}
	if !deleting && r.checkRegistryIntent(ctx, agent) != nil {
		return fmt.Errorf("RegistryInputChanged")
	}
	keep := ""
	if !deleting && agent.Spec.Agent.RegistrySecretRef != nil {
		keep = agent.Spec.Agent.RegistrySecretRef.Name
	}
	if !deleting && keep == "" {
		sts := &appsv1.StatefulSet{}
		err := r.customReader().Get(ctx, client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: instanceServiceName(agent.Name)}, sts)
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("RegistryCleanupPending")
		}
		name, _ := registryauth.PullSecretName(agent.UID)
		for _, ref := range sts.Spec.Template.Spec.ImagePullSecrets {
			if ref.Name == name {
				return fmt.Errorf("RegistryTemplateUpdatePending")
			}
		}
	}
	localName, _ := registryauth.PullSecretName(agent.UID)
	for _, namespace := range []string{r.registryNamespace(), instanceNamespace(agent.Name)} {
		secrets := &corev1.SecretList{}
		if err := r.customReader().List(ctx, secrets, client.InNamespace(namespace)); err != nil {
			return fmt.Errorf("RegistryCleanupPending")
		}
		for i := range secrets.Items {
			secret := &secrets.Items[i]
			managed := secret.Name == localName
			if namespace == r.registryNamespace() {
				managed = strings.HasPrefix(secret.Name, "registry-source-") || secret.Labels[registryauth.PurposeLabel] == registryauth.Purpose
			}
			if !registryauth.Owned(secret, agent) || !managed ||
				(keep != "" && (namespace != r.registryNamespace() || secret.Name == keep)) {
				continue
			}
			uid, rv := secret.UID, secret.ResourceVersion
			if err := r.Delete(ctx, secret, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}}); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("RegistryCleanupPending")
			}
		}
	}
	if keep == "" && !deleting {
		before := agent.DeepCopy().Status
		agent.Status.RegistryCredentials = nil
		setCondition(&agent.Status.Conditions, api.ConditionRegistryCredentialsReady, metav1.ConditionTrue,
			"RegistryCredentialsNotConfigured", "Managed pull credentials removed; future pulls may require node authentication")
		if !reflect.DeepEqual(before, agent.Status) {
			return r.updateStatus(ctx, agent)
		}
	}
	return nil
}

func registryManaged(agent *api.Agent) bool {
	return agent.Spec.Agent.RegistrySecretRef != nil || agent.Status.RegistryCredentials != nil ||
		agent.Annotations[registryauth.Operation] != "" ||
		meta.FindStatusCondition(agent.Status.Conditions, api.ConditionRegistryCredentialsReady) != nil
}

func (r *AgentReconciler) removeRegistryCredentials(ctx context.Context, agent *api.Agent) error {
	if agent.Spec.Agent.RegistrySecretRef != nil || !registryManaged(agent) {
		return nil
	}
	if r.checkRegistryIntent(ctx, agent) != nil {
		return fmt.Errorf("RegistryInputChanged")
	}
	sts := &appsv1.StatefulSet{}
	err := r.customReader().Get(ctx, client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: instanceServiceName(agent.Name)}, sts)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("RegistryCleanupPending")
	}
	name, _ := registryauth.PullSecretName(agent.UID)
	if err == nil {
		before := sts.DeepCopy()
		refs := []corev1.LocalObjectReference{}
		for _, ref := range sts.Spec.Template.Spec.ImagePullSecrets {
			if ref.Name != name {
				refs = append(refs, ref)
			}
		}
		if len(refs) != len(sts.Spec.Template.Spec.ImagePullSecrets) {
			if agent.Spec.Agent.Type == api.AgentTypeCustom {
				if !hasAgentOwner(sts, agent) || sts.Annotations[customAgentUID] != string(agent.UID) {
					return fmt.Errorf("RegistryOwnershipConflict")
				}
			} else {
				ns := &corev1.Namespace{}
				if r.customReader().Get(ctx, client.ObjectKey{Name: sts.Namespace}, ns) != nil || !hasAgentOwner(ns, agent) {
					return fmt.Errorf("RegistryOwnershipConflict")
				}
				if release := sts.Annotations["meta.helm.sh/release-name"]; release != "" && release != helmReleaseName(agent.Name) {
					return fmt.Errorf("RegistryOwnershipConflict")
				}
				if sts.Annotations == nil {
					sts.Annotations = map[string]string{}
				}
				sts.Annotations["cetirun.microsoft.com/registry-binding-only"] = "true"
			}
			sts.Spec.Template.Spec.ImagePullSecrets = refs
			sts.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}
			if r.Patch(ctx, sts, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})) != nil {
				return fmt.Errorf("RegistryCleanupPending")
			}
		}
	}
	return r.cleanupRegistryCredentials(ctx, agent, false)
}

func registryOnlyRollout(sts *appsv1.StatefulSet) bool {
	return sts.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType &&
		sts.Annotations["cetirun.microsoft.com/registry-binding-only"] == "true"
}

func registryServiceAccount(values map[string]any, fullname string) (string, error) {
	if security, ok := values["securityContext"].(map[string]any); ok {
		if security["privileged"] == true {
			return "", fmt.Errorf("privileged workloads cannot isolate registry credentials")
		}
		if capabilities, ok := security["capabilities"].(map[string]any); ok && capabilities["add"] != nil {
			added := reflect.ValueOf(capabilities["add"])
			if added.Kind() != reflect.Slice || added.Len() != 0 {
				return "", fmt.Errorf("additional capabilities require credential isolation qualification")
			}
		}
	}
	for _, key := range []string{"extraEnvFrom", "extraVolumes", "extraVolumeMounts", "sidecars", "initContainers"} {
		if value := values[key]; value != nil {
			v := reflect.ValueOf(value)
			if (v.Kind() != reflect.Slice && v.Kind() != reflect.Map) || v.Len() != 0 {
				return "", fmt.Errorf("unqualified credential mount surface")
			}
		}
	}
	if env, ok := values["extraEnv"].([]any); ok {
		for _, entry := range env {
			if e, ok := entry.(map[string]any); ok && e["valueFrom"] != nil {
				return "", fmt.Errorf("unqualified credential environment surface")
			}
		}
	}
	sa, _ := values["serviceAccount"].(map[string]any)
	if role, _ := sa["role"].(string); role != "" && role != "view" {
		return "", fmt.Errorf("service account role is not compatible with credential isolation")
	}
	if name, _ := sa["name"].(string); name != "" {
		return name, nil
	}
	if sa["create"] == true {
		return fullname, nil
	}
	return "default", nil
}

// Suspension must not need a working registry, a chart pull, or a new hook Pod.
func (r *AgentReconciler) suspendRegistryBuiltin(ctx context.Context, agent *api.Agent) (ctrl.Result, error) {
	if err := r.checkRegistryIntent(ctx, agent); err != nil {
		return ctrl.Result{Requeue: true}, nil
	}
	beforeStatus := agent.DeepCopy().Status
	keyErr := r.suspendGatewayKey(ctx, agent)
	sts := &appsv1.StatefulSet{}
	key := client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: instanceServiceName(agent.Name)}
	err := r.customReader().Get(ctx, key, sts)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("inspect workload for suspension")
	}
	if err == nil {
		ns := &corev1.Namespace{}
		if r.customReader().Get(ctx, client.ObjectKey{Name: key.Namespace}, ns) != nil || !hasAgentOwner(ns, agent) {
			return ctrl.Result{}, fmt.Errorf("workload namespace ownership must be confirmed before suspension")
		}
		if release := sts.Annotations["meta.helm.sh/release-name"]; release != "" && release != helmReleaseName(agent.Name) {
			return ctrl.Result{}, fmt.Errorf("workload ownership conflicts with suspension")
		}
		if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 {
			before := sts.DeepCopy()
			zero := int32(0)
			sts.Spec.Replicas = &zero
			if sts.Spec.PersistentVolumeClaimRetentionPolicy == nil {
				sts.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType}
			}
			sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled = appsv1.RetainPersistentVolumeClaimRetentionPolicyType
			if err := r.Patch(ctx, sts, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	if err := r.suspendCopilotProxy(ctx, agent); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.removeRegistryBuiltinRoute(ctx, agent); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.removeRegistryCredentials(ctx, agent); err != nil {
		return r.registryWaiting(ctx, agent, err.Error())
	}
	agent.Status.Phase, agent.Status.AccessURL = api.PhaseSuspended, ""
	// Invalidate the deployment skip-cache so resume restores one replica even
	// when the suspend/resume spec round trip left other inputs unchanged.
	agent.Status.ObservedGeneration = 0
	for _, condition := range []string{api.ConditionReady, api.ConditionPodReady, api.ConditionIngressReady} {
		setCondition(&agent.Status.Conditions, condition, metav1.ConditionFalse, "Suspended", "Suspension requested; registry credentials are not required")
	}
	if !reflect.DeepEqual(beforeStatus, agent.Status) {
		if err := r.updateStatus(ctx, agent); err != nil {
			return ctrl.Result{}, err
		}
	}
	if beforeStatus.Phase != api.PhaseSuspended && r.EventRecorder != nil {
		r.EventRecorder.Event(agent, corev1.EventTypeNormal, "InstanceSuspended", "Instance suspended — scaled to zero and ingress removed")
	}
	return ctrl.Result{RequeueAfter: time.Minute}, keyErr
}

// suspendCopilotProxy stops the Agent's GitHub Copilot proxy, which its chart
// runs beside the workload, as Helm suspension does with a release of no
// replicas: in place, without a chart or the registry, and only once the
// Deployment is known to be the Agent's own. Resuming upgrades the release,
// which restores it.
func (r *AgentReconciler) suspendCopilotProxy(ctx context.Context, agent *api.Agent) error {
	proxy := &appsv1.Deployment{}
	key := client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: copilotProxyServiceName(agent)}
	if err := r.customReader().Get(ctx, key, proxy); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("inspect the Copilot proxy for suspension")
	}
	ns := &corev1.Namespace{}
	if r.customReader().Get(ctx, client.ObjectKey{Name: key.Namespace}, ns) != nil || !hasAgentOwner(ns, agent) {
		return fmt.Errorf("the Copilot proxy's namespace ownership must be confirmed before suspension")
	}
	if proxy.Annotations["meta.helm.sh/release-name"] != helmReleaseName(agent.Name) {
		return fmt.Errorf("the Copilot proxy's ownership conflicts with suspension")
	}
	if proxy.Spec.Replicas != nil && *proxy.Spec.Replicas == 0 {
		return nil
	}
	before := proxy.DeepCopy()
	zero := int32(0)
	proxy.Spec.Replicas = &zero
	return r.Patch(ctx, proxy, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (r *AgentReconciler) removeRegistryBuiltinRoute(ctx context.Context, agent *api.Agent) error {
	route := &istiov1beta1.VirtualService{}
	key := client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: virtualServiceName(agent.Name)}
	if err := r.customReader().Get(ctx, key, route); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return client.IgnoreNotFound(err)
	}
	// Native routes predate per-route UID ownership. Bind their existing labels
	// to the UID-owned namespace; do not require Custom workload annotations.
	namespace := &corev1.Namespace{}
	if agent.UID == "" || r.customReader().Get(ctx, client.ObjectKey{Name: key.Namespace}, namespace) != nil || !hasAgentOwner(namespace, agent) ||
		route.Labels["app.kubernetes.io/managed-by"] != "cetirun-operator" || route.Labels["app.kubernetes.io/instance"] != agent.Name ||
		(len(route.OwnerReferences) != 0 && (len(route.OwnerReferences) != 1 || !hasAgentOwner(route, agent))) {
		return fmt.Errorf("native route ownership must be confirmed before suspension")
	}
	if err := r.checkRegistryIntent(ctx, agent); err != nil {
		return err
	}
	return client.IgnoreNotFound(r.Delete(ctx, route, client.Preconditions{UID: &route.UID, ResourceVersion: &route.ResourceVersion}))
}
