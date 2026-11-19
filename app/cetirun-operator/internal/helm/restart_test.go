// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"reflect"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"helm.sh/helm/v3/pkg/chart"
	appsv1 "k8s.io/api/apps/v1"
)

func TestCustomRetainedRestartChangesOnlyTheRequestedWorkload(t *testing.T) {
	input := customInput(8080)
	before, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	bindings := input["customBindings"].(runtimebindings.Custom)
	original := bindings
	bindings.RestartRequestID = "request-00000001"
	input["customBindings"] = bindings
	after, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	if before.WorkloadHash == after.WorkloadHash || after.WorkloadHash != retry.WorkloadHash || after.PlanHash != retry.PlanHash {
		t.Fatal("restart did not produce a new, deterministic workload")
	}
	if err := after.ValidateCustom(bindings.Namespace.Name, "openclaw-example"); err != nil {
		t.Fatal(err)
	}
	bindings.RestartRequestID = ""
	if !reflect.DeepEqual(bindings, original) {
		t.Fatal("restart changed HOME, model identity, namespace or other bindings")
	}
}

func preRestartChart(t *testing.T) *chart.Chart {
	t.Helper()
	chrt := testChart(t)
	for _, template := range chrt.Templates {
		if template.Name != "templates/_platform.tpl" {
			continue
		}
		template.Data = bytes.ReplaceAll(template.Data, []byte(`  {{- with $bindings.restartRequestID }}
  cetirun.microsoft.com/restart-request-id: {{ . | quote }}
  {{- end }}
`), nil)
		for _, addition := range []string{
			`  {{- if (.Values.sandboxWorkload | default dict).ingressMTLS }}
  command: ["/bin/bash", "/etc/cetirun/desktop-ingress-entrypoint.sh"]
  readinessProbe:
    exec:
      command: ["python3", "/etc/cetirun/desktop-ingress-probe.py"]
    periodSeconds: 5
    timeoutSeconds: 4
    failureThreshold: 3
  {{- end }}
`,
			`    {{- if (.Values.sandboxWorkload | default dict).ingressMTLS }}
    - name: CETIRUN_INGRESS_MTLS
      value: "true"
    {{- end }}
`,
			`    {{- if (.Values.sandboxWorkload | default dict).ingressMTLS }}
    - name: config
      mountPath: /etc/cetirun
      readOnly: true
    {{- end }}
`,
		} {
			if bytes.Count(template.Data, []byte(addition)) != 1 {
				t.Fatal("pre-restart fixture requires the exact managed desktop addition")
			}
			template.Data = bytes.Replace(template.Data, []byte(addition), nil, 1)
		}
		for _, change := range [][2]string{
			{`  {{- $annotations := deepCopy (.Values.podAnnotations | default dict) }}
  {{- if .Values.cetiadmin.enabled }}
  {{- $_ := set $annotations "checksum/cetiadmin" (toJson .Values.cetiadmin | sha256sum) }}
  {{- if .Values.cetiadmin.azureWorkloadIdentity.enabled }}
  {{- $_ := set $annotations "azure.workload.identity/skip-containers" (include "cetirun.cetiadmin.azureSkipContainers" .) }}
  {{- end }}
  {{- end }}
  {{- with $annotations }}
`, `  {{- with .Values.podAnnotations }}
`},
			{`  {{- if and (not $bindings) (or .Values.azureWorkloadIdentity.enabled (and .Values.cetiadmin.enabled .Values.cetiadmin.azureWorkloadIdentity.enabled)) }}`, `  {{- if and (not $bindings) .Values.azureWorkloadIdentity.enabled }}`},
		} {
			if bytes.Count(template.Data, []byte(change[0])) != 1 {
				t.Fatal("pre-restart fixture requires the exact later CetiAdmin addition")
			}
			template.Data = bytes.Replace(template.Data, []byte(change[0]), []byte(change[1]), 1)
		}
		// Exact public template at ef9ca7d421e360e07469d3ac63f5d50b82455e0d.
		if fmt.Sprintf("%x", sha256.Sum256(template.Data)) != "b85759d050fe8f863eaa04997b942d61f3f7d21fd2eeace163f51fb8945919fa" {
			t.Fatal("pre-restart chart fixture no longer matches its pinned source")
		}
		return chrt
	}
	t.Fatal("pre-restart chart is missing its platform template")
	return nil
}

func TestCustomRetainedRestartSupportsPinnedPreRestartChart(t *testing.T) {
	chrt := preRestartChart(t)
	version := chrt.Metadata.Version
	input := customInput(8080)
	input["podAnnotations"] = map[string]any{"example.com/retained": "unchanged"}
	before, err := PrepareChart(chrt, input)
	if err != nil {
		t.Fatal(err)
	}
	workload := func(p *PreparedRelease) appsv1.StatefulSet {
		t.Helper()
		var sts appsv1.StatefulSet
		if err := decodeObject(manifestObjects(t, p)["StatefulSet/"+text(input["fullnameOverride"])], &sts); err != nil {
			t.Fatal(err)
		}
		return sts
	}
	original := workload(before)
	previousHash := before.WorkloadHash
	bindings := input["customBindings"].(runtimebindings.Custom)
	for _, requestID := range []string{"request-00000001", "request-00000002"} {
		bindings.RestartRequestID = requestID
		input["customBindings"] = bindings
		requestHash := ""
		for _, replicas := range []int32{1, 1, 0, 1} {
			input["replicaCount"] = int(replicas)
			prepared, err := PrepareChart(chrt, input)
			if err != nil {
				t.Fatalf("pinned chart cannot restart, stop, or resume with request %s and replicas %d: %v", requestID, replicas, err)
			}
			if err := prepared.prepareCustomAction(customActionConfig(), bindings.Namespace.Name, prepared.releaseName); err != nil {
				t.Fatalf("pinned chart cannot prepare the actual Helm action: %v", err)
			}
			sts := workload(prepared)
			if prepared.ChartVersion != version || chrt.Metadata.Version != version ||
				sts.Spec.Replicas == nil || *sts.Spec.Replicas != replicas {
				t.Fatal("restart changed the chart pin or requested replica count")
			}
			if sts.Spec.Template.Annotations[api.AnnotationRestartRequestID] != requestID ||
				sts.Spec.Template.Annotations["example.com/retained"] != "unchanged" {
				t.Fatal("pinned chart did not render the exact restart and existing annotations")
			}
			if !reflect.DeepEqual(sts.Spec.VolumeClaimTemplates, original.Spec.VolumeClaimTemplates) ||
				!reflect.DeepEqual(sts.Spec.PersistentVolumeClaimRetentionPolicy, original.Spec.PersistentVolumeClaimRetentionPolicy) ||
				!reflect.DeepEqual(sts.OwnerReferences, original.OwnerReferences) ||
				!reflect.DeepEqual(sts.Spec.UpdateStrategy, original.Spec.UpdateStrategy) {
				t.Fatal("restart changed HOME, ownership, retention, or the approved update strategy")
			}
			if requestHash == "" {
				requestHash = prepared.WorkloadHash
				if requestHash == previousHash {
					t.Fatal("a new request did not change the desired workload")
				}
			} else if requestHash != prepared.WorkloadHash {
				t.Fatal("an unchanged request changed the workload across retry, stop, or resume")
			}
		}
		previousHash = requestHash
	}
}

func TestCustomRetainedRestartRejectsCallerAnnotations(t *testing.T) {
	for _, key := range []string{api.AnnotationRestartRequestID, "cetirun.microsoft.com/model-revision"} {
		for _, value := range []string{"request-00000001", "request-00000002", ""} {
			input := customInput(8080)
			bindings := input["customBindings"].(runtimebindings.Custom)
			bindings.RestartRequestID = "request-00000001"
			input["customBindings"] = bindings
			input["podAnnotations"] = map[string]any{key: value}
			if _, err := prepareChart(preRestartChart(t), input, false); err == nil {
				t.Fatalf("caller supplied protected annotation %s=%q before rendering", key, value)
			}
		}
	}
}

func TestCustomRetainedRestartRejectsChangedPreparedAnnotation(t *testing.T) {
	for _, value := range []any{nil, "request-00000002", true} {
		input := customInput(8080)
		bindings := input["customBindings"].(runtimebindings.Custom)
		bindings.RestartRequestID = "request-00000001"
		input["customBindings"] = bindings
		prepared, err := PrepareChart(testChart(t), input)
		if err != nil {
			t.Fatal(err)
		}
		if value == nil {
			delete(object(prepared.Values["podAnnotations"]), api.AnnotationRestartRequestID)
		} else {
			object(prepared.Values["podAnnotations"])[api.AnnotationRestartRequestID] = value
		}
		if err := prepared.ValidateCustom(bindings.Namespace.Name, prepared.releaseName); err == nil {
			t.Fatalf("changed prepared restart annotation %v was accepted", value)
		}
	}
}
