// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"reflect"
	"strconv"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	strictjson "sigs.k8s.io/json"
)

func (h *CetiAdminHandler) cetiAdminHelmWrite(ctx context.Context, request admission.Request, agent *api.Agent, target cetiadmin.TargetIdentity) admission.Response {
	if request.Operation != admissionv1.Create && request.Operation != admissionv1.Update {
		return admission.Denied("direct Helm writes require the current Held ordinary target; deletes are never allowed")
	}
	snapshot, err := h.heldMaintenanceTarget(ctx, agent, target)
	if err != nil {
		return admission.Denied("direct Helm writes require the current protected Held snapshot")
	}
	if request.Resource.Group == "" && request.Resource.Resource == "secrets" {
		return h.cetiAdminRevision(ctx, request, agent, snapshot)
	}
	if request.Operation != admissionv1.Update {
		return admission.Denied("Helm maintenance cannot create an unbound runtime object")
	}
	var next, old unstructured.Unstructured
	if next.UnmarshalJSON(request.Object.Raw) != nil || old.UnmarshalJSON(request.OldObject.Raw) != nil ||
		!matchingObjectIdentity(&next, &old) || !boundHelmObject(snapshot, &old) ||
		next.GetAnnotations()["meta.helm.sh/release-name"] != agent.Status.HelmReleaseName ||
		next.GetAnnotations()["meta.helm.sh/release-namespace"] != agent.Status.Namespace {
		return admission.Denied("Helm maintenance requires the recorded runtime object and current UID/version")
	}
	if !safeMaintenanceObject(next, old) {
		return admission.Denied("Helm maintenance may change only stopped runtime configuration, not identity, storage or permission objects")
	}
	return admission.Allowed("bound stopped runtime object during Helm maintenance")
}

func boundHelmObject(target cetiadmin.TargetIdentity, object *unstructured.Unstructured) bool {
	matches := 0
	for _, ref := range target.Resources {
		if ref.APIVersion == object.GetAPIVersion() && ref.Kind == object.GetKind() && ref.Name == object.GetName() &&
			ref.Namespace == object.GetNamespace() && ref.UID != "" && ref.UID == object.GetUID() {
			matches++
		}
	}
	return matches == 1
}

func safeMaintenanceObject(next, old unstructured.Unstructured) bool {
	if next.GetAPIVersion() != old.GetAPIVersion() || next.GetKind() != old.GetKind() {
		return false
	}
	next, old = *next.DeepCopy(), *old.DeepCopy()
	for _, object := range []*unstructured.Unstructured{&next, &old} {
		metadata, _, _ := unstructured.NestedMap(object.Object, "metadata")
		for _, key := range []string{"generation", "managedFields", "resourceVersion"} {
			delete(metadata, key)
		}
		labels, _ := metadata["labels"].(map[string]any)
		for _, key := range []string{"helm.sh/chart", "app.kubernetes.io/version"} {
			delete(labels, key)
		}
		object.Object["metadata"] = metadata
		delete(object.Object, "status")
	}
	switch next.GroupVersionKind().GroupKind() {
	case schema.GroupKind{Kind: "ConfigMap"}:
		for _, object := range []*unstructured.Unstructured{&next, &old} {
			delete(object.Object, "data")
			delete(object.Object, "binaryData")
		}
	case schema.GroupKind{Group: "apps", Kind: "StatefulSet"}, schema.GroupKind{Group: "apps", Kind: "Deployment"}:
		for _, object := range []*unstructured.Unstructured{&next, &old} {
			spec, _, _ := unstructured.NestedMap(object.Object, "spec")
			if replicas, ok := spec["replicas"].(int64); !ok || replicas < 0 {
				return false
			}
			delete(spec, "replicas")
			template, _ := spec["template"].(map[string]any)
			pod, _ := template["spec"].(map[string]any)
			for _, key := range []string{"containers", "initContainers"} {
				containers, _ := pod[key].([]any)
				for _, item := range containers {
					container, ok := item.(map[string]any)
					if !ok {
						return false
					}
					for _, mutable := range []string{"image", "imagePullPolicy", "command", "args", "env", "envFrom", "resources", "livenessProbe", "readinessProbe", "startupProbe"} {
						delete(container, mutable)
					}
				}
			}
			metadata, _ := template["metadata"].(map[string]any)
			annotations, _ := metadata["annotations"].(map[string]any)
			for key := range annotations {
				if strings.HasPrefix(key, "checksum/") {
					delete(annotations, key)
				}
			}
			labels, _ := metadata["labels"].(map[string]any)
			delete(labels, "helm.sh/chart")
			delete(labels, "app.kubernetes.io/version")
			object.Object["spec"] = spec
		}
	case schema.GroupKind{Kind: "Service"}:
		// Service routing, selectors and ports remain defined by Agent intent.
	default:
		return false
	}
	return reflect.DeepEqual(next.Object, old.Object)
}

func (h *CetiAdminHandler) cetiAdminRevision(ctx context.Context, request admission.Request, agent *api.Agent, target cetiadmin.TargetIdentity) admission.Response {
	var next, old corev1.Secret
	if decodeJSON(request.Object.Raw, &next, false) != nil {
		return admission.Denied("invalid Helm revision Secret")
	}
	payload, revision, err := decodeMaintenanceRelease(&next, agent)
	if err != nil {
		return admission.Denied(err.Error())
	}
	if request.Operation == admissionv1.Update {
		if decodeJSON(request.OldObject.Raw, &old, false) != nil || !matchingObjectIdentity(&next, &old) {
			return admission.Denied("Helm revision updates require the current Secret UID and version")
		}
		previous, _, err := decodeMaintenanceRelease(&old, agent)
		if err != nil {
			return admission.Denied("existing Helm revision is not recognized")
		}
		anchor := agent.Status.HelmMaintenance.Revision
		superseded := revision < anchor && old.Labels["status"] == "deployed" && next.Labels["status"] == "superseded"
		if revision != anchor && revision != anchor+1 && !superseded {
			return admission.Denied("maintenance may only supersede an older deployed revision or update the current handoff")
		}
		for _, object := range []map[string]any{payload, previous} {
			info, _ := object["info"].(map[string]any)
			delete(info, "status")
			delete(info, "description")
		}
		next.Data["release"] = old.Data["release"]
		next.Labels["status"] = old.Labels["status"]
		if modified := next.Labels["modifiedAt"]; modified != "" {
			if _, err := strconv.ParseInt(modified, 10, 64); err != nil {
				return admission.Denied("Helm modifiedAt must be a driver timestamp")
			}
		}
		if previous, found := old.Labels["modifiedAt"]; found {
			next.Labels["modifiedAt"] = previous
		} else {
			delete(next.Labels, "modifiedAt")
		}
		next.ManagedFields = old.ManagedFields
		if !reflect.DeepEqual(previous, payload) || !reflect.DeepEqual(next, old) {
			return admission.Denied("existing Helm revisions may change status and description only; history and values are retained")
		}
		return admission.Allowed("conditional Helm revision status update")
	}
	if request.Operation != admissionv1.Create || revision != agent.Status.HelmMaintenance.Revision+1 || next.UID != "" || next.ResourceVersion != "" ||
		len(next.OwnerReferences) != 0 || len(next.Finalizers) != 0 || len(next.Data) != 1 || len(next.StringData) != 0 ||
		(next.Labels["status"] != "pending-upgrade" && next.Labels["status"] != "pending-rollback") {
		return admission.Denied("maintenance creates only the next pending revision of the held release")
	}
	manifest, ok := payload["manifest"].(string)
	if !ok || h.validateMaintenanceManifest(ctx, manifest, agent, target) != nil {
		return admission.Denied("new Helm history must retain the bound manifest identities and permissions")
	}
	sourceRevision := agent.Status.HelmMaintenance.Revision
	if next.Labels["status"] == "pending-rollback" {
		info := payload["info"].(map[string]any)
		description, _ := info["description"].(string)
		sourceRevision, err = strconv.ParseInt(strings.TrimPrefix(description, "Rollback to "), 10, 64)
		if err != nil || description != "Rollback to "+strconv.FormatInt(sourceRevision, 10) ||
			sourceRevision < 1 || sourceRevision > agent.Status.HelmMaintenance.Revision {
			return admission.Denied("rollback must identify an existing revision from before the handoff")
		}
	}
	source := &corev1.Secret{}
	key := client.ObjectKey{Namespace: next.Namespace, Name: fmt.Sprintf("sh.helm.release.v1.%s.v%d", agent.Status.HelmReleaseName, sourceRevision)}
	if err := h.reader.Get(ctx, key, source); err != nil || source.UID == "" || source.DeletionTimestamp != nil {
		return admission.Denied("maintenance source revision is unavailable")
	}
	previous, storedRevision, err := decodeMaintenanceRelease(source, agent)
	if err != nil || storedRevision != sourceRevision || !reflect.DeepEqual(maintenanceRevisionContent(previous), maintenanceRevisionContent(payload)) {
		return admission.Denied("maintenance must retain one stored revision's chart, values, manifest and hooks; use Agent intent for configuration changes")
	}
	return admission.Allowed("next revision for the current held release without deleting history")
}

func maintenanceRevisionContent(payload map[string]any) map[string]any {
	content := maps.Clone(payload)
	delete(content, "version")
	info, _ := content["info"].(map[string]any)
	info = maps.Clone(info)
	for _, key := range []string{"status", "description", "first_deployed", "last_deployed"} {
		delete(info, key)
	}
	content["info"] = info
	if hooks, ok := content["hooks"].([]any); ok {
		definitions := make([]any, len(hooks))
		for i, raw := range hooks {
			definitions[i] = raw
			if hook, ok := raw.(map[string]any); ok {
				hook = maps.Clone(hook)
				delete(hook, "last_run")
				definitions[i] = hook
			}
		}
		content["hooks"] = definitions
	}
	return content
}

func decodeMaintenanceRelease(secret *corev1.Secret, agent *api.Agent) (map[string]any, int64, error) {
	failure := fmt.Errorf("Helm revision must use the fixed Secret format and matching release, namespace, revision and status")
	if secret.Type != "helm.sh/release.v1" || secret.Labels["owner"] != "helm" || secret.Labels["name"] != agent.Status.HelmReleaseName ||
		secret.Namespace != agent.Status.Namespace || len(secret.Data["release"]) > 2<<20 {
		return nil, 0, failure
	}
	revision, err := strconv.ParseInt(secret.Labels["version"], 10, 64)
	if err != nil || revision < 1 || secret.Name != fmt.Sprintf("sh.helm.release.v1.%s.v%d", agent.Status.HelmReleaseName, revision) {
		return nil, 0, failure
	}
	compressed, err := base64.StdEncoding.DecodeString(string(secret.Data["release"]))
	if err != nil {
		return nil, 0, failure
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, 0, failure
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, (8<<20)+1))
	closeErr := reader.Close()
	var payload map[string]any
	if readErr != nil || closeErr != nil || len(data) > 8<<20 {
		return nil, 0, failure
	}
	problems, err := strictjson.UnmarshalStrict(data, &payload, strictjson.DisallowDuplicateFields)
	if err != nil || len(problems) != 0 {
		return nil, 0, failure
	}
	info, _ := payload["info"].(map[string]any)
	version, _ := payload["version"].(int64)
	if version != revision || payload["name"] != agent.Status.HelmReleaseName ||
		payload["namespace"] != secret.Namespace || info["status"] != secret.Labels["status"] {
		return nil, 0, failure
	}
	return payload, revision, nil
}

func (h *CetiAdminHandler) validateMaintenanceManifest(ctx context.Context, manifest string, agent *api.Agent, target cetiadmin.TargetIdentity) error {
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
	count := 0
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(&object.Object); err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		if len(object.Object) == 0 {
			continue
		}
		count++
		if count > 256 || !cetiadmin.SupportedManifestKind(object.GroupVersionKind().Group, object.GetKind()) {
			return fmt.Errorf("unsupported maintenance manifest")
		}
		if object.GetNamespace() == "" && object.GetKind() != "PersistentVolume" && object.GetKind() != "ClusterRoleBinding" &&
			object.GetKind() != "ValidatingAdmissionPolicy" && object.GetKind() != "ValidatingAdmissionPolicyBinding" {
			object.SetNamespace(agent.Status.Namespace)
		}
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(object.GroupVersionKind())
		if err := h.reader.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
			return fmt.Errorf("manifest object is not currently bound")
		}
		if !boundHelmObject(target, current) || (object.GetNamespace() != "" && object.GetNamespace() != agent.Status.Namespace) {
			return fmt.Errorf("manifest object is outside the held binding")
		}
		// Compare the candidate to current API shape while retaining server defaults.
		// The direct resource request is checked again after Helm computes its patch.
		for key, value := range object.Object {
			if key == "metadata" || key == "status" {
				continue
			}
			if key != "spec" && key != "data" && key != "binaryData" && key != "apiVersion" && key != "kind" && !reflect.DeepEqual(value, current.Object[key]) {
				return fmt.Errorf("maintenance manifest changes an unsupported field")
			}
		}
	}
	if count == 0 {
		return fmt.Errorf("maintenance manifest is empty")
	}
	return nil
}
