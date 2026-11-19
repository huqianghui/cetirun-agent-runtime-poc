// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/yaml"
)

func platformRender(t *testing.T, p *PreparedRelease, capabilities *chartutil.Capabilities) map[string]map[string]any {
	t.Helper()
	values, err := chartutil.ToRenderValues(p.chart, p.Values, chartutil.ReleaseOptions{
		Name: p.releaseName, Namespace: "cetirun-example", IsInstall: true, Revision: 1,
	}, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(p.chart, values)
	if err != nil {
		t.Fatal(err)
	}
	objects := map[string]map[string]any{}
	for name, content := range rendered {
		if strings.HasSuffix(name, "NOTES.txt") {
			continue
		}
		for _, document := range strings.Split(content, "\n---") {
			var obj map[string]any
			if err := yaml.Unmarshal([]byte(document), &obj); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if len(obj) != 0 {
				objects[text(obj["kind"])] = obj
			}
		}
	}
	return objects
}

func TestCustomSharedPlatformNetwork(t *testing.T) {
	input := customInput(nil)
	p, err := prepareChart(testChart(t), input, false)
	if err != nil {
		t.Fatal(err)
	}
	objects := platformRender(t, p, chartutil.DefaultCapabilities)
	bindings := input["customBindings"].(runtimebindings.Custom)
	var expected map[string]any
	if err := decodeObject(p.customNetwork(bindings.Network), &expected); err != nil {
		t.Fatal(err)
	}
	actual := object(objects["NetworkPolicy"]["spec"])
	if !sameJSON(actual, expected) {
		t.Fatalf("network mismatch\nactual=%#v\nexpected=%#v", actual, expected)
	}
}

func TestCustomDesktopAndSharedControls(t *testing.T) {
	for _, port := range []any{nil, 8080} {
		for _, desktop := range []bool{true, false} {
			t.Run(fmt.Sprintf("port-%v-desktop-%v", port, desktop), func(t *testing.T) {
				input := customInput(port)
				input["desktop"] = map[string]any{"enabled": desktop}
				input["podDisruptionBudget"] = map[string]any{"enabled": true}
				input["autoscaling"] = map[string]any{"enabled": true, "minReplicas": 1, "maxReplicas": 1}
				input["podAnnotations"] = map[string]any{"sidecar.istio.io/inject": "true"}
				p, err := PrepareChart(testChart(t), input)
				if err != nil {
					t.Fatal(err)
				}
				objects := platformRender(t, p, chartutil.DefaultCapabilities)
				for _, kind := range []string{"NetworkPolicy", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding", "PodDisruptionBudget", "HorizontalPodAutoscaler"} {
					if objects[kind] == nil {
						t.Errorf("missing shared platform control %s", kind)
					}
				}
				for _, kind := range []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding"} {
					owners := array(object(objects[kind]["metadata"])["ownerReferences"])
					if len(owners) != 1 || object(owners[0])["uid"] != "example-uid" {
						t.Fatal("cluster-scoped policy must follow the Agent UID, not outlive its namespace")
					}
				}
				pod := object(object(object(objects["StatefulSet"]["spec"])["template"])["spec"])
				if got := len(array(pod["containers"])); got != 1+btoi(desktop) {
					t.Fatalf("containers=%d, desktop=%v", got, desktop)
				}
				if _, exists := objects["Service"]; exists != (desktop || port != nil) {
					t.Fatal("Service must follow all enabled listeners, not just the Agent HTTP port")
				}
				for _, servicePort := range array(object(objects["Service"]["spec"])["ports"]) {
					if object(servicePort)["name"] == "ttyd" {
						t.Fatal("Custom ttyd must remain disabled pending revisit")
					}
				}
				if object(p.Values["ttyd"])["enabled"] != false {
					t.Fatal("inherited ttyd default was not disabled")
				}
			})
		}
	}
}

func btoi(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestCustomPlatformParityWithBuiltins(t *testing.T) {
	input := customInput(nil)
	input["podDisruptionBudget"] = map[string]any{"enabled": true}
	input["autoscaling"] = map[string]any{"enabled": true, "minReplicas": 1, "maxReplicas": 1}
	input["agentRequest"].(map[string]any)["username"] = "vibe"
	custom, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	customObjects := platformRender(t, custom, chartutil.DefaultCapabilities)
	for _, typ := range []string{"hermes", "openclaw"} {
		input := sampleInput(typ, "openai", false)
		input["podDisruptionBudget"] = map[string]any{"enabled": true}
		input["autoscaling"] = map[string]any{"enabled": true, "minReplicas": 1, "maxReplicas": 1}
		native, err := PrepareChart(testChart(t), input)
		if err != nil {
			t.Fatal(err)
		}
		native.releaseName = custom.releaseName
		objects := platformRender(t, native, chartutil.DefaultCapabilities)
		for _, kind := range []string{"ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding", "PodDisruptionBudget", "HorizontalPodAutoscaler"} {
			if !reflect.DeepEqual(customObjects[kind]["spec"], objects[kind]["spec"]) ||
				customObjects[kind]["apiVersion"] != objects[kind]["apiVersion"] {
				t.Fatalf("%s changed the shared %s", typ, kind)
			}
		}
		var customPod, nativePod corev1.PodSpec
		_ = decodeObject(object(object(object(customObjects["StatefulSet"]["spec"])["template"])["spec"]), &customPod)
		_ = decodeObject(object(object(object(objects["StatefulSet"]["spec"])["template"])["spec"]), &nativePod)
		for _, container := range nativePod.Containers {
			if container.Name == "desktop" && !equality.Semantic.DeepEqual(container, customPod.Containers[1]) {
				t.Fatalf("%s and Custom do not share the same desktop container", typ)
			}
		}
	}
}

func TestCustomSidecarScopeSurvivesActionSealing(t *testing.T) {
	p := customActionPlan(t, testChart(t))
	cfg := customActionConfig()
	cfg.Capabilities.APIVersions = append(cfg.Capabilities.APIVersions, "networking.istio.io/v1/Sidecar")
	if err := p.prepareCustomAction(cfg, "cetirun-example", p.releaseName); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(p.manifest), `"kind":"Sidecar"`) {
		t.Fatal("Custom did not inherit supported mesh scope")
	}
	if err := p.installOrUpgradeCustom(cfg, "cetirun-example", p.releaseName); err != nil {
		t.Fatal(err)
	}
}

func TestCustomSharedControlTamperingIsRejected(t *testing.T) {
	for _, test := range []struct{ file, from, to string }{
		{"deny-exec-policy.yaml", "expression: \"false\"", "expression: \"true\""},
		{"poddisruptionbudget.yaml", "minAvailable: {{ .Values.podDisruptionBudget.minAvailable }}", "minAvailable: 0"},
		{"networkpolicy.yaml", "169.254.169.254/32", "169.254.169.253/32"},
		{"_platform.tpl", "allowPrivilegeEscalation: false", "allowPrivilegeEscalation: true"},
	} {
		t.Run(test.file, func(t *testing.T) {
			ch := testChart(t)
			if test.file == "networkpolicy.yaml" {
				// Removing every configured block must not be accepted as the approved policy.
				test.from, test.to = "{{- with .Values.networkPolicy.blockedCIDRs }}", "{{- with list }}"
			}
			for _, file := range ch.Templates {
				if file.Name == "templates/"+test.file {
					if !strings.Contains(string(file.Data), test.from) {
						t.Fatal("test mutation did not match")
					}
					file.Data = []byte(strings.Replace(string(file.Data), test.from, test.to, 1))
				}
			}
			input := customInput(nil)
			input["podDisruptionBudget"] = map[string]any{"enabled": true}
			if _, err := PrepareChart(ch, input); err == nil {
				t.Fatal("modified platform control was accepted")
			}
		})
	}
}

func TestCustomExecPolicyHasNoAdminException(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			input := terminalInput(nil)
			input["ttyd"] = map[string]any{"enabled": enabled}
			p, err := PrepareChart(testChart(t), input)
			if err != nil {
				t.Fatal(err)
			}
			spec := object(platformRender(t, p, chartutil.DefaultCapabilities)["ValidatingAdmissionPolicy"]["spec"])
			if _, found := spec["matchConditions"]; found {
				t.Fatal("Chart granted an exec exception")
			}
			want := "false"
			if enabled {
				want = terminalpolicy.AdmissionExpression("cetirun-example", "openclaw-example-cetirun-0", "cetirun", "openclaw-example-cetirun-terminal", "example-uid")
			}
			if object(array(spec["validations"])[0])["expression"] != want {
				t.Fatal("Custom terminal command/container restriction changed")
			}
		})
	}
}

func TestCustomExecPolicyRejectsInjectedExceptions(t *testing.T) {
	for _, expression := range []string{
		`request.userInfo.username != "system:serviceaccount:cetirun-admin:openclaw-admin-cetirun"`,
		`request.userInfo.username != "system:serviceaccount:cetirun-operations:managed-admin" || request.userInfo.uid != "account-uid"`,
		`false`,
	} {
		ch := testChart(t)
		for _, file := range ch.Templates {
			if file.Name == "templates/deny-exec-policy.yaml" {
				file.Data = []byte(strings.Replace(string(file.Data), "  failurePolicy: Fail", "  failurePolicy: Fail\n  matchConditions:\n    - name: exclude-adminclaw\n      expression: '"+expression+"'", 1))
			}
		}
		if _, err := PrepareChart(ch, terminalInput(nil)); err == nil {
			t.Fatal("unprepared exec exception accepted in a Custom manifest")
		}
	}
}
