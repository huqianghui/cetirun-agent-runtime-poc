// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strconv"
	"strings"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type HookOptions struct {
	Namespace          string
	ConfigMapName      string
	ExpectedConfigHash string // SHA256 of the exact rendered config.json bytes.
	Mode               string // check or stop; main dispatches the process mode.
	Timeout            time.Duration
}

const (
	HookModeCheck = "check"
	HookModeStop  = "stop"
)

// RunHook uses an uncached Kubernetes client supplied by main. It only reads
// objects, except for the stop mode's single UID/RV-conditional source patch.
// Installation PhaseReady/PhaseDisabled must include the controller's current
// access, model, asset and routing observations; this is not another reconciler.
func RunHook(ctx context.Context, kube client.Client, options HookOptions) error {
	if kube == nil || len(validation.IsDNS1123Label(options.Namespace)) != 0 ||
		len(validation.IsDNS1123Subdomain(options.ConfigMapName)) != 0 ||
		(options.Mode != HookModeCheck && options.Mode != HookModeStop) || options.Timeout < 0 || options.Timeout > time.Hour {
		return fmt.Errorf("CetiAdmin hook requires an exact source, check/stop mode and a timeout no greater than one hour")
	}
	if digest, err := hex.DecodeString(options.ExpectedConfigHash); err != nil || len(digest) != sha256.Size {
		return fmt.Errorf("CetiAdmin hook requires the rendered config.json SHA256")
	}
	if options.Timeout == 0 {
		options.Timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	source := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: options.Namespace, Name: options.ConfigMapName}
	if err := kube.Get(ctx, key, source); err != nil {
		return fmt.Errorf("read CetiAdmin source: %w", err)
	}
	if source.UID == "" || source.ResourceVersion == "" || source.DeletionTimestamp != nil {
		return fmt.Errorf("CetiAdmin source identity is absent or terminating")
	}
	raw := source.Data[ConfigKey]
	config, err := ParseConfig(raw)
	if err != nil {
		_, retained, readErr := hookReadInstallation(ctx, kube, key, source)
		if readErr == nil {
			config, err = ReadSourceConfig(source, &retained.Record)
		}
	}
	if err != nil {
		return fmt.Errorf("CetiAdmin source configuration is invalid")
	}
	if hookRawHash(raw) != options.ExpectedConfigHash {
		// Preserve the original byte representation, so retry can recognize only
		// the earlier true -> false edit, even after its acknowledgement was lost.
		enabledRaw, err := hookEnabledJSON(raw, true)
		if options.Mode != "stop" || config.Enabled || err != nil || hookRawHash(enabledRaw) != options.ExpectedConfigHash {
			return fmt.Errorf("CetiAdmin source changed since this Helm hook was rendered")
		}
	}
	if options.Mode == "stop" && config.Enabled {
		if _, _, err := hookReadInstallation(ctx, kube, key, source); err != nil {
			return fmt.Errorf("CetiAdmin stop requires its source-bound installation before any write: %w", err)
		}
		disabledRaw, err := hookEnabledJSON(raw, false)
		if err != nil {
			return err
		}
		patch, err := json.Marshal([]map[string]any{
			{"op": "test", "path": "/metadata/uid", "value": string(source.UID)},
			{"op": "test", "path": "/metadata/resourceVersion", "value": source.ResourceVersion},
			{"op": "test", "path": "/data/config.json", "value": raw},
			{"op": "replace", "path": "/data/config.json", "value": disabledRaw},
		})
		if err != nil {
			return err
		}
		if err := kube.Patch(ctx, source, client.RawPatch(types.JSONPatchType, patch)); err != nil {
			return fmt.Errorf("CetiAdmin stop write was not confirmed; inspect the source before retrying: %w", err)
		}
		raw, config.Enabled = disabledRaw, false
	}
	hash, err := ConfigHash(config)
	if err != nil {
		return err
	}
	uid := source.UID
	var recordUID types.UID
	last := "waiting for the installation checkpoint"
	err = wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		if err := kube.Get(ctx, key, source); err != nil {
			return false, fmt.Errorf("read CetiAdmin source: %w", err)
		}
		if source.UID != uid || source.DeletionTimestamp != nil || source.Data[ConfigKey] != raw {
			return false, fmt.Errorf("CetiAdmin source changed while the hook was waiting")
		}
		object, state, err := hookReadInstallation(ctx, kube, key, source)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if recordUID != "" && object.UID != recordUID {
			return false, fmt.Errorf("CetiAdmin installation checkpoint was replaced")
		}
		recordUID = object.UID
		last = "waiting for current configuration, Operator and installation readiness"
		observedHash, err := ConfigHash(state.Configuration)
		if err != nil || state.ConfigHash != hash || observedHash != hash || state.PendingCreate != "" {
			return false, nil
		}
		if state.Phase == PhaseBlocked {
			return false, fmt.Errorf("CetiAdmin installation is Blocked; inspect its protected status")
		}
		ready, err := hookDeploymentReady(ctx, kube, key.Namespace, "cetirun-operator", state.OperatorImage)
		if err != nil || !ready {
			return false, err
		}
		if config.Enabled {
			ownerSuspended := state.Phase == PhaseDisabled && state.Reason == "UserSuspended"
			if (!ownerSuspended && state.Phase != PhaseReady) || !state.Bootstrapped || state.RuntimeVersion == "" {
				return false, nil
			}
			ready, err = hookAdmissionReady(ctx, kube, key.Namespace, state.OperatorImage, state.ServiceAccount)
			if err != nil || !ready {
				return false, err
			}
			if ownerSuspended {
				agent := &api.Agent{}
				if err := kube.Get(ctx, client.ObjectKey{Name: state.Agent.Name}, agent); err != nil {
					return false, err
				}
				if !agent.Spec.Suspended {
					return false, nil
				}
				ready, err = hookInstallationStopped(ctx, kube, state)
			} else {
				ready, err = hookAgentReady(ctx, kube, source, config, state.Record)
			}
		} else {
			last = "waiting for revoked access, stopped writers and resolved maintenance"
			if state.Phase != PhaseDisabled {
				return false, nil
			}
			ready, err = hookInstallationStopped(ctx, kube, state)
		}
		if err != nil || !ready {
			return false, err
		}
		if err := kube.Get(ctx, key, source); err != nil {
			return false, err
		}
		if source.UID != uid || source.DeletionTimestamp != nil || source.Data[ConfigKey] != raw {
			return false, fmt.Errorf("CetiAdmin source changed before completion")
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("CetiAdmin %s hook: %s: %w", options.Mode, last, err)
	}
	return nil
}

func hookReadInstallation(ctx context.Context, kube client.Client, key client.ObjectKey, source *corev1.ConfigMap) (*corev1.ConfigMap, Installation, error) {
	object := &corev1.ConfigMap{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: InstallationName(key.Namespace, key.Name)}, object); err != nil {
		return nil, Installation{}, err
	}
	state, err := ParseInstallation(object.Data[BindingKey])
	if err != nil || !state.Source.matches(source) || object.UID == "" || object.DeletionTimestamp != nil ||
		object.Annotations[AnnotationSourceUID] != string(source.UID) {
		return nil, state, fmt.Errorf("CetiAdmin installation source or checkpoint identity conflicts")
	}
	return object, state, nil
}

func hookRawHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Change only the top-level boolean token, not key order, whitespace, strings
// or nested enabled fields. The Chart always emits an explicit enabled field.
func hookEnabledJSON(raw string, enabled bool) (string, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return "", fmt.Errorf("CetiAdmin source must contain a JSON object")
	}
	seen := map[string]bool{}
	start, end := int64(-1), int64(-1)
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return "", fmt.Errorf("CetiAdmin source has invalid or duplicate fields")
		}
		seen[name] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return "", fmt.Errorf("CetiAdmin source has invalid JSON")
		}
		if name == "enabled" {
			if string(value) != "true" && string(value) != "false" {
				return "", fmt.Errorf("CetiAdmin source enabled must be an explicit boolean")
			}
			end = decoder.InputOffset()
			start = end - int64(len(value))
		}
	}
	if start < 0 {
		return "", fmt.Errorf("CetiAdmin source requires an explicit enabled field")
	}
	return raw[:start] + strconv.FormatBool(enabled) + raw[end:], nil
}

func hookDeploymentReady(ctx context.Context, kube client.Client, namespace, name, image string) (bool, error) {
	object := &appsv1.Deployment{}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, object); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if object.DeletionTimestamp != nil || object.Generation < 1 || object.Spec.Replicas == nil || *object.Spec.Replicas < 1 ||
		object.Status.ObservedGeneration != object.Generation || object.Status.UpdatedReplicas != *object.Spec.Replicas ||
		object.Status.ReadyReplicas != *object.Spec.Replicas || object.Status.AvailableReplicas != *object.Spec.Replicas {
		return false, nil
	}
	containerName := "manager"
	if name == "cetirun-sandbox-admission" {
		containerName = "admission"
	}
	for _, container := range object.Spec.Template.Spec.Containers {
		if container.Name == containerName && container.Image == image && image != "" {
			return true, nil
		}
	}
	return false, nil
}

func hookAdmissionReady(ctx context.Context, kube client.Client, namespace, image string, account ObjectIdentity) (bool, error) {
	ready, err := hookDeploymentReady(ctx, kube, namespace, "cetirun-sandbox-admission", image)
	if err != nil || !ready {
		return false, err
	}
	object := &admissionv1.ValidatingWebhookConfiguration{}
	if err := kube.Get(ctx, client.ObjectKey{Name: "cetirun-cetiadmin-admission"}, object); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if object.DeletionTimestamp != nil || len(object.Webhooks) != 3 {
		return false, nil
	}
	seen := map[string]bool{}
	guardRequired := false
	for _, hook := range object.Webhooks {
		service := hook.ClientConfig.Service
		var expectedSelector *metav1.LabelSelector
		if hook.Name == "cetiadmin-workloads.cetirun.microsoft.com" {
			expectedSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "cetirun-operator"}}
		}
		if hook.FailurePolicy == nil || *hook.FailurePolicy != admissionv1.Fail || service == nil ||
			service.Namespace != namespace || service.Name != "cetirun-sandbox-admission" || service.Path == nil || *service.Path != "/validate-cetiadmin" ||
			service.Port == nil || *service.Port != 443 || !AdmissionNamespaceSelectorMatches(hook.NamespaceSelector, expectedSelector) || !AdmissionSelectorMatches(hook.ObjectSelector, nil) {
			return false, nil
		}
		guardRequired = guardRequired || !AdmissionSelectorMatches(hook.NamespaceSelector, expectedSelector)
		seen[hook.Name] = true
		rest := strings.TrimSpace(string(hook.ClientConfig.CABundle))
		if rest == "" {
			return false, nil
		}
		for rest != "" {
			block, tail := pem.Decode([]byte(rest))
			if block == nil || block.Type != "CERTIFICATE" || !strings.HasPrefix(rest, "-----BEGIN CERTIFICATE-----") {
				return false, fmt.Errorf("CetiAdmin admission CA is not certificate PEM")
			}
			cert, err := x509.ParseCertificate(block.Bytes)
			if err != nil || !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 || time.Now().Before(cert.NotBefore) || !time.Now().Before(cert.NotAfter) {
				return false, fmt.Errorf("CetiAdmin admission CA is invalid or outside its validity period")
			}
			rest = strings.TrimSpace(string(tail))
		}
	}
	for _, kind := range []string{"configuration", "workloads", "operations"} {
		if !seen["cetiadmin-"+kind+".cetirun.microsoft.com"] {
			return false, nil
		}
	}
	mutating := &admissionv1.MutatingWebhookConfiguration{}
	if err := kube.Get(ctx, client.ObjectKey{Name: "cetirun-cetiadmin-admission"}, mutating); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if mutating.DeletionTimestamp != nil || len(mutating.Webhooks) != 1 {
		return false, nil
	}
	hook := mutating.Webhooks[0]
	username, _ := json.Marshal("system:serviceaccount:" + account.Namespace + ":" + account.Name)
	service := hook.ClientConfig.Service
	if hook.Name != "cetiadmin-agent.cetirun.microsoft.com" || hook.FailurePolicy == nil || *hook.FailurePolicy != admissionv1.Fail ||
		service == nil || service.Namespace != namespace || service.Name != "cetirun-sandbox-admission" ||
		service.Path == nil || *service.Path != "/mutate-sandbox-agent" || service.Port == nil || *service.Port != 443 ||
		!AdmissionNamespaceSelectorMatches(hook.NamespaceSelector, nil) || !AdmissionSelectorMatches(hook.ObjectSelector, nil) || len(hook.MatchConditions) != 1 ||
		hook.MatchConditions[0].Name != "cetiadmin-caller" || hook.MatchConditions[0].Expression != "request.userInfo.username == "+string(username) ||
		string(hook.ClientConfig.CABundle) != string(object.Webhooks[0].ClientConfig.CABundle) {
		return false, nil
	}
	guardRequired = guardRequired || !AdmissionSelectorMatches(hook.NamespaceSelector, nil)
	if guardRequired {
		if err := CheckNamespaceAdmissionGuard(ctx, kube, namespace, account.Namespace); err != nil {
			return false, err
		}
	}
	return true, nil
}

func hookAgentReady(ctx context.Context, kube client.Client, source *corev1.ConfigMap, config Config, record Record) (bool, error) {
	observed := ObservedInstallation{Source: source, Agent: &api.Agent{}, Team: &api.Team{}, Namespace: &corev1.Namespace{}, ServiceAccount: &corev1.ServiceAccount{}}
	for _, item := range []struct {
		identity ObjectIdentity
		object   client.Object
	}{
		{record.Agent, observed.Agent}, {record.Team, observed.Team}, {record.Namespace, observed.Namespace}, {record.ServiceAccount, observed.ServiceAccount},
	} {
		if item.identity.Name == "" || item.identity.UID == "" {
			return false, nil
		}
		if err := kube.Get(ctx, client.ObjectKey{Name: item.identity.Name, Namespace: item.identity.Namespace}, item.object); err != nil {
			return false, client.IgnoreNotFound(err)
		}
	}
	if _, err := ResolveBinding(config, &record, observed); err != nil {
		return false, err
	}
	agent := observed.Agent
	if agent.Spec.Suspended || agent.Generation < 1 || agent.Status.ObservedGeneration != agent.Generation || agent.Status.Phase != api.PhaseReady {
		return false, nil
	}
	for _, condition := range agent.Status.Conditions {
		// Ordinary Agent conditions use the enclosing status generation; newer
		// producers may additionally stamp an individual condition generation.
		if condition.Type == api.ConditionReady && condition.Status == metav1.ConditionTrue &&
			(condition.ObservedGeneration == 0 || condition.ObservedGeneration == agent.Generation) {
			return true, nil
		}
	}
	return false, nil
}

// CheckMaintenanceResolved reads current handoffs, including orphaned namespace
// markers. It does not depend on an installation's former target inventory.
func CheckMaintenanceResolved(ctx context.Context, reader client.Reader) error {
	agents := &api.AgentList{}
	if err := reader.List(ctx, agents); err != nil {
		return fmt.Errorf("list Agents for Helm maintenance: %w", err)
	}
	for i := range agents.Items {
		agent := &agents.Items[i]
		if MaintenanceUnfinished(agent) ||
			(agent.Spec.Suspended && agent.Status.HelmMaintenance != nil && agent.Status.HelmMaintenance.ObservedGeneration != agent.Generation) {
			return fmt.Errorf("Agent %s has unresolved Helm maintenance", agent.Name)
		}
	}
	namespaces := &corev1.NamespaceList{}
	if err := reader.List(ctx, namespaces); err != nil {
		return fmt.Errorf("list namespaces for Helm maintenance: %w", err)
	}
	for _, namespace := range namespaces.Items {
		for _, key := range []string{MaintenanceAgentName, MaintenanceAgentUID, MaintenanceRequestID} {
			if namespace.Annotations[key] != "" {
				return fmt.Errorf("namespace %s still has a Helm maintenance startup block", namespace.Name)
			}
		}
	}
	return nil
}

func hookInstallationStopped(ctx context.Context, kube client.Client, state Installation) (bool, error) {
	if err := CheckMaintenanceResolved(ctx, kube); err != nil {
		return false, err
	}
	if state.Agent.UID == "" {
		return !state.Bootstrapped && len(state.Resources) == 0 && state.ServiceAccount.UID == "" && state.Namespace.UID == "", nil
	}
	agent, namespace := &api.Agent{}, &corev1.Namespace{}
	if err := kube.Get(ctx, client.ObjectKey{Name: state.Agent.Name}, agent); err != nil {
		return false, err
	}
	if !state.Agent.matches(agent) {
		return false, fmt.Errorf("CetiAdmin retained Agent identity changed")
	}
	if state.Namespace.UID == "" {
		return agent.Spec.Suspended && !state.Bootstrapped && state.ServiceAccount.UID == "" && len(state.Resources) == 0, nil
	}
	if err := kube.Get(ctx, client.ObjectKey{Name: state.Namespace.Name}, namespace); err != nil {
		return false, err
	}
	if !state.Namespace.matches(namespace) || !ownedByAgent(namespace, agent) {
		return false, fmt.Errorf("CetiAdmin retained Agent or namespace identity changed")
	}
	account := &corev1.ServiceAccount{}
	if err := kube.Get(ctx, client.ObjectKey{Name: state.ServiceAccount.Name, Namespace: state.ServiceAccount.Namespace}, account); err != nil {
		return false, err
	}
	if !state.ServiceAccount.matches(account) || !ownedByAgent(account, agent) {
		return false, fmt.Errorf("CetiAdmin retained ServiceAccount identity changed")
	}
	// Retained suspension deliberately zeros status.observedGeneration to
	// invalidate the deployment skip-cache. Read the actual writers below.
	if agent.Status.Phase != api.PhaseSuspended || agent.Status.AccessURL != "" {
		return false, nil
	}
	bindings, clusterBindings := &rbacv1.RoleBindingList{}, &rbacv1.ClusterRoleBindingList{}
	if err := kube.List(ctx, bindings); err != nil {
		return false, err
	}
	if err := kube.List(ctx, clusterBindings); err != nil {
		return false, err
	}
	username := "system:serviceaccount:" + state.ServiceAccount.Namespace + ":" + state.ServiceAccount.Name
	grantsAccess := func(subjects []rbacv1.Subject, bindingNamespace string) bool {
		for _, subject := range subjects {
			namespace := subject.Namespace
			if namespace == "" {
				namespace = bindingNamespace
			}
			if (subject.Kind == "ServiceAccount" && subject.Name == state.ServiceAccount.Name && namespace == state.ServiceAccount.Namespace) ||
				(subject.Kind == "User" && subject.Name == username) {
				return true
			}
		}
		return false
	}
	for _, binding := range bindings.Items {
		if grantsAccess(binding.Subjects, binding.Namespace) {
			return false, nil
		}
	}
	for _, binding := range clusterBindings.Items {
		if grantsAccess(binding.Subjects, "") {
			return false, nil
		}
	}
	return hookWritersStopped(ctx, kube, namespace.Name)
}

func hookWritersStopped(ctx context.Context, kube client.Client, namespace string) (bool, error) {
	pods := &corev1.PodList{}
	sets, deployments, replicas, daemons := &appsv1.StatefulSetList{}, &appsv1.DeploymentList{}, &appsv1.ReplicaSetList{}, &appsv1.DaemonSetList{}
	jobs, crons := &batchv1.JobList{}, &batchv1.CronJobList{}
	for _, list := range []client.ObjectList{pods, sets, deployments, replicas, daemons, jobs, crons} {
		if err := kube.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return false, err
		}
	}
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil || (pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed) {
			return false, nil
		}
	}
	for _, set := range sets.Items {
		if set.Spec.Replicas == nil || *set.Spec.Replicas != 0 || set.Status.Replicas != 0 {
			return false, nil
		}
	}
	for _, deployment := range deployments.Items {
		if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 || deployment.Status.Replicas != 0 {
			return false, nil
		}
	}
	for _, replica := range replicas.Items {
		if replica.Spec.Replicas == nil || *replica.Spec.Replicas != 0 || replica.Status.Replicas != 0 {
			return false, nil
		}
	}
	if len(daemons.Items) != 0 {
		// Zero eligible Nodes is not a durable stop for a DaemonSet.
		return false, nil
	}
	for _, job := range jobs.Items {
		finished := false
		for _, condition := range job.Status.Conditions {
			finished = finished || (condition.Status == corev1.ConditionTrue && (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed))
		}
		if !finished || job.Status.Active != 0 {
			return false, nil
		}
	}
	for _, cron := range crons.Items {
		if cron.Spec.Suspend == nil || !*cron.Spec.Suspend || len(cron.Status.Active) != 0 {
			return false, nil
		}
	}
	return true, nil
}
