// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func sandboxSourcesChartValues(t *testing.T) map[string]any {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true}
	certificate, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"cetiadmin": map[string]any{"enabled": false},
		"namespace": "operator-example",
		"sandboxAdmission": map[string]any{
			"enabled": true, "enforce": true, "podsEnabled": true,
			"adminUsers": []string{"admin@example.com"}, "tlsSecretName": "synthetic-admission-tls",
			"caBundle": base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate})),
		},
		"sandboxSources": map[string]any{
			"enabled": true, "bootstrapOwnedPrerequisites": true,
			"platformConfigMapName": "sandbox-platform",
		},
	}
}

// This uses the repository's pinned Helm SDK, not a Helm CLI or an API server.
func renderSandboxSourcesChart(t *testing.T, values map[string]any) (map[string]*unstructured.Unstructured, error) {
	t.Helper()
	chart, err := loader.Load("../../chart")
	if err != nil {
		t.Fatal(err)
	}
	capabilities := chartutil.DefaultCapabilities.Copy()
	capabilities.KubeVersion = chartutil.KubeVersion{Version: "v1.37.0", Major: "1", Minor: "37"}
	input, err := chartutil.ToRenderValues(chart, values, chartutil.ReleaseOptions{
		Name: "publication-test", Namespace: "release-example", Revision: 1, IsInstall: true,
	}, capabilities)
	if err != nil {
		return nil, err
	}
	rendered, err := engine.Render(chart, input)
	if err != nil {
		return nil, err
	}
	objects := map[string]*unstructured.Unstructured{}
	for _, text := range rendered {
		decoder := kubeyaml.NewYAMLOrJSONDecoder(strings.NewReader(text), 4096)
		for {
			object := &unstructured.Unstructured{}
			if err := decoder.Decode(object); err == io.EOF {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			if object.GetKind() != "" {
				objects[object.GetKind()+"/"+object.GetName()] = object
			}
		}
	}
	return objects, nil
}

func sandboxChartEnvironment(t *testing.T, objects map[string]*unstructured.Unstructured, deployment, variable string) map[string]any {
	t.Helper()
	var object appsv1.Deployment
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(objects["Deployment/"+deployment].Object, &object); err != nil {
		t.Fatal(err)
	}
	for _, env := range object.Spec.Template.Spec.Containers[0].Env {
		if env.Name == variable {
			var value map[string]any
			if err := json.Unmarshal([]byte(env.Value), &value); err != nil {
				t.Fatal(err)
			}
			return value
		}
	}
	t.Fatalf("missing chart environment %s", variable)
	return nil
}

func TestSandboxSkillsCatalogChartWiring(t *testing.T) {
	values := sandboxSourcesChartValues(t)
	baseline, err := renderSandboxSourcesChart(t, values)
	if err != nil {
		t.Fatal(err)
	}
	if _, enabled := sandboxChartEnvironment(t, baseline, "cetirun-operator", "CETIRUN_SANDBOX_SOURCES")["skillsCatalogConfigMapName"]; enabled {
		t.Fatal("catalog setting was enabled by default")
	}
	values["sandboxSources"].(map[string]any)["skillsCatalogConfigMapName"] = "published-skills"
	configured, err := renderSandboxSourcesChart(t, values)
	if err != nil {
		t.Fatal(err)
	}
	config := sandboxChartEnvironment(t, configured, "cetirun-operator", "CETIRUN_SANDBOX_SOURCES")
	if config["skillsCatalogConfigMapName"] != "published-skills" || config["platformConfigMapName"] != "sandbox-platform" {
		t.Fatal("chart did not keep publication selection separate from platform.json")
	}
	if !reflect.DeepEqual(
		sandboxChartEnvironment(t, baseline, "cetirun-sandbox-admission", "CETIRUN_SANDBOX_ADMISSION"),
		sandboxChartEnvironment(t, configured, "cetirun-sandbox-admission", "CETIRUN_SANDBOX_ADMISSION")) {
		t.Fatal("catalog changed the shared configuration-admission wire contract")
	}
	var previous, current rbacv1.ClusterRole
	for _, entry := range []struct {
		objects map[string]*unstructured.Unstructured
		role    *rbacv1.ClusterRole
	}{{baseline, &previous}, {configured, &current}} {
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(entry.objects["ClusterRole/cetirun-sandbox-admission"].Object, entry.role); err != nil {
			t.Fatal(err)
		}
	}
	var otherRules []rbacv1.PolicyRule
	volumeRules := 0
	for _, rule := range current.Rules {
		if slices.Contains(rule.Resources, "persistentvolumes") {
			volumeRules++
			if !reflect.DeepEqual(rule, rbacv1.PolicyRule{
				APIGroups: []string{""}, Resources: []string{"persistentvolumes"}, Verbs: []string{"get"},
			}) {
				t.Fatal("publication metadata added excess admission privileges")
			}
		} else {
			otherRules = append(otherRules, rule)
		}
	}
	if volumeRules != 1 || !reflect.DeepEqual(previous.Rules, otherRules) ||
		!reflect.DeepEqual(baseline["Role/cetirun-sandbox-admission-pod-state"], configured["Role/cetirun-sandbox-admission-pod-state"]) {
		t.Fatal("catalog changed other admission source privileges")
	}
	for _, object := range configured {
		if object.GetKind() == "ConfigMap" && object.GetName() == "cetirun-cetiadmin-config" && reflect.DeepEqual(baseline["ConfigMap/cetirun-cetiadmin-config"], object) {
			continue // The disabled CetiAdmin source is unrelated to published skill content.
		}
		if object.GetKind() == "PersistentVolume" || object.GetKind() == "PersistentVolumeClaim" || object.GetKind() == "ConfigMap" {
			t.Fatal("operator chart provisioned publication content or catalog/bindings itself")
		}
	}
}

func TestSandboxSkillsCatalogChartRejectsInvalidConfiguration(t *testing.T) {
	for _, test := range []struct {
		name, message string
		edit          func(map[string]any)
	}{
		{"without sources", "requires enabled owned bootstrap", func(s map[string]any) {
			s["enabled"], s["bootstrapOwnedPrerequisites"] = false, false
		}},
		{"without bootstrap", "requires enabled owned bootstrap", func(s map[string]any) { s["bootstrapOwnedPrerequisites"] = false }},
		{"wrong type", "must be a resource name", func(s map[string]any) { s["skillsCatalogConfigMapName"] = true }},
		{"invalid name", "separate valid ConfigMap name", func(s map[string]any) { s["skillsCatalogConfigMapName"] = "Other/namespace" }},
		{"shared platform document", "separate valid ConfigMap name", func(s map[string]any) { s["skillsCatalogConfigMapName"] = "sandbox-platform" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := sandboxSourcesChartValues(t)
			sources := values["sandboxSources"].(map[string]any)
			sources["skillsCatalogConfigMapName"] = "published-skills"
			test.edit(sources)
			if _, err := renderSandboxSourcesChart(t, values); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatal("invalid catalog chart setting was accepted or misclassified:", err)
			}
		})
	}
}

// skillMarketplaceChartValues are valid native Sandbox sources without a
// published skills catalog, which the Skill Marketplace requires.
func skillMarketplaceChartValues(t *testing.T) map[string]any {
	t.Helper()
	values := sandboxSourcesChartValues(t)
	values["sandboxSources"].(map[string]any)["nativeExecution"] = true
	return values
}

// enabledSkillMarketplace is a synthetic enabled Skill Marketplace setting.
func enabledSkillMarketplace() map[string]any {
	return map[string]any{
		"enabled":    true,
		"repository": map[string]any{"url": "https://git.example.invalid/example/skills.git"},
		"storage":    map[string]any{"resourceGroup": "example-rg", "storageAccount": "exampleskills", "containerName": "marketplace"},
	}
}

func operatorChartDeployment(t *testing.T, objects map[string]*unstructured.Unstructured) appsv1.Deployment {
	t.Helper()
	var deployment appsv1.Deployment
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(objects["Deployment/cetirun-operator"].Object, &deployment); err != nil {
		t.Fatal(err)
	}
	return deployment
}

// The Skill Marketplace switch renders CETIRUN_SKILL_MARKETPLACE and the
// worker only while it is on, and changes nothing else that the operator chart
// renders.
func TestSkillMarketplaceChartSwitch(t *testing.T) {
	values := skillMarketplaceChartValues(t)
	baseline, err := renderSandboxSourcesChart(t, values)
	if err != nil {
		t.Fatal(err)
	}
	isSwitch := func(env corev1.EnvVar) bool { return env.Name == "CETIRUN_SKILL_MARKETPLACE" }
	if slices.ContainsFunc(operatorChartDeployment(t, baseline).Spec.Template.Spec.Containers[0].Env, isSwitch) {
		t.Fatal("the Skill Marketplace was configured by default")
	}
	// Off, and absent as after a --reuse-values upgrade from an older chart.
	for _, setting := range []any{map[string]any{"enabled": false}, nil} {
		values["skillMarketplace"] = setting
		disabled, err := renderSandboxSourcesChart(t, values)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(disabled, baseline) {
			t.Fatalf("skillMarketplace %v changed the operator chart", setting)
		}
	}
	values["skillMarketplace"] = enabledSkillMarketplace()
	enabled, err := renderSandboxSourcesChart(t, values)
	if err != nil {
		t.Fatal(err)
	}
	deployment := operatorChartDeployment(t, enabled)
	env := deployment.Spec.Template.Spec.Containers[0].Env
	index := slices.IndexFunc(env, isSwitch)
	want := `{"enabled":true,"storage":{"containerName":"marketplace","resourceGroup":"example-rg",` +
		`"storageAccount":"exampleskills","storageClassName":"azureblob-nfs-premium"}}`
	if index < 0 || env[index].Value != want || env[index].ValueFrom != nil {
		t.Fatalf("operator environment = %+v, want CETIRUN_SKILL_MARKETPLACE %s", env, want)
	}
	deployment.Spec.Template.Spec.Containers[0].Env = slices.Delete(env, index, index+1)
	if !reflect.DeepEqual(deployment, operatorChartDeployment(t, baseline)) {
		t.Fatal("the switch changed the operator Deployment beyond its variable")
	}
	volume, claim := workerStorageNames(t, enabled)
	delete(baseline, "Deployment/cetirun-operator")
	delete(enabled, "Deployment/cetirun-operator")
	// The switch adds the worker and its access, and nothing else.
	var added []string
	for key := range enabled {
		if _, ok := baseline[key]; !ok {
			added = append(added, key)
			delete(enabled, key)
		}
	}
	slices.Sort(added)
	wantAdded := []string{
		"Deployment/cetirun-skill-marketplace", "PersistentVolume/" + volume,
		"PersistentVolumeClaim/" + claim, "Role/cetirun-skill-marketplace-catalog",
		"RoleBinding/cetirun-skill-marketplace-catalog", "ServiceAccount/cetirun-skill-marketplace",
	}
	if !reflect.DeepEqual(added, wantAdded) {
		t.Fatalf("the switch added %v, want %v", added, wantAdded)
	}
	if !reflect.DeepEqual(enabled, baseline) {
		t.Fatal("the switch changed other operator chart objects")
	}
}

// workerStorageNames returns the names of the worker's volume and claim, and
// checks that they and the worker's mount belong together.
func workerStorageNames(t *testing.T, objects map[string]*unstructured.Unstructured) (string, string) {
	t.Helper()
	var volume, claim string
	for key, object := range objects {
		switch {
		case strings.HasPrefix(key, "PersistentVolume/"):
			volume = object.GetName()
		case strings.HasPrefix(key, "PersistentVolumeClaim/"):
			claim = object.GetName()
		}
	}
	pv := objects["PersistentVolume/"+volume]
	ref, _, _ := unstructured.NestedString(pv.Object, "spec", "claimRef", "name")
	bound, _, _ := unstructured.NestedString(objects["PersistentVolumeClaim/"+claim].Object, "spec", "volumeName")
	var deployment appsv1.Deployment
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(objects["Deployment/cetirun-skill-marketplace"].Object, &deployment); err != nil {
		t.Fatal(err)
	}
	mounted := ""
	for _, source := range deployment.Spec.Template.Spec.Volumes {
		if source.Name == "storage" && source.PersistentVolumeClaim != nil {
			mounted = source.PersistentVolumeClaim.ClaimName
		}
	}
	if volume == "" || ref != claim || bound != volume || mounted != claim {
		t.Fatalf("volume %q (claim %q), claim %q (volume %q), worker mounts %q", volume, ref, claim, bound, mounted)
	}
	return volume, claim
}

// Kubernetes cannot change the source of a volume or the class of a bound
// claim. A change of the storage renders a new volume and claim for the
// worker, so the upgrade creates them and removes the earlier ones, which the
// chart does not keep. The same storage keeps its names.
func TestSkillMarketplaceStorageChangeRendersNewWorkerStorage(t *testing.T) {
	names := func(storage map[string]any) (string, string) {
		t.Helper()
		values := skillMarketplaceChartValues(t)
		marketplace := enabledSkillMarketplace()
		for key, value := range storage {
			marketplace["storage"].(map[string]any)[key] = value
		}
		values["skillMarketplace"] = marketplace
		objects, err := renderSandboxSourcesChart(t, values)
		if err != nil {
			t.Fatal(err)
		}
		volume, claim := workerStorageNames(t, objects)
		for _, key := range []string{"PersistentVolume/" + volume, "PersistentVolumeClaim/" + claim} {
			if _, kept := objects[key].GetAnnotations()["helm.sh/resource-policy"]; kept {
				t.Fatalf("%s must not be kept by Helm: a storage change replaces it", key)
			}
		}
		return volume, claim
	}
	volume, claim := names(nil)
	if again, againClaim := names(nil); again != volume || againClaim != claim {
		t.Fatal("the same storage must keep the names of the worker's volume and claim")
	}
	for field, value := range map[string]any{"resourceGroup": "other-rg", "storageAccount": "otherskills",
		"containerName": "moved", "storageClassName": "azureblob-nfs-standard", "size": "200Gi"} {
		if other, otherClaim := names(map[string]any{field: value}); other == volume || otherClaim == claim {
			t.Errorf("a change of storage.%s must rename the worker's volume and claim: %s %s", field, other, otherClaim)
		}
	}
}

func TestSkillMarketplaceChartRejectsInvalidConfiguration(t *testing.T) {
	const replaced = "the operator gives native Agents the Team skills claims, and the marketplace replaces the published skills catalog"
	withRepository := func(repository map[string]any) map[string]any {
		marketplace := enabledSkillMarketplace()
		marketplace["repository"] = repository
		return marketplace
	}
	for _, test := range []struct {
		name, message string
		sources       map[string]any
		marketplace   any
	}{
		{"without sources", replaced, map[string]any{"enabled": false, "nativeExecution": false, "bootstrapOwnedPrerequisites": false}, enabledSkillMarketplace()},
		{"held execution", replaced, map[string]any{"nativeExecution": false}, enabledSkillMarketplace()},
		{"published catalog", replaced, map[string]any{"skillsCatalogConfigMapName": "published-skills"}, enabledSkillMarketplace()},
		{"no claim name", "requires sandboxSources.skillsClaimName", map[string]any{"skillsClaimName": ""}, enabledSkillMarketplace()},
		{"not an object", "skillMarketplace must be an object", nil, true},
		{"not a boolean", "skillMarketplace.enabled must be a boolean", nil, map[string]any{"enabled": "true"}},
		{"unknown field", "unsupported skillMarketplace field enable", nil, map[string]any{"enable": true}},
		{"no repository", "skillMarketplace.repository.url is required", nil, map[string]any{"enabled": true}},
		{"credentials in the URL", "must not contain credentials", nil,
			withRepository(map[string]any{"url": "https://user:synthetic@git.example.invalid/x.git"})},
		{"a query in the URL", "must not contain a query or a fragment", nil,
			withRepository(map[string]any{"url": "https://git.example.invalid/x.git?token=synthetic"})},
		{"a fragment in the URL", "must not contain a query or a fragment", nil,
			withRepository(map[string]any{"url": "https://git.example.invalid/x.git#synthetic"})},
		{"token without a Secret", "existingSecret is required", nil,
			withRepository(map[string]any{"url": "https://git.example.invalid/x.git", "auth": "token"})},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := skillMarketplaceChartValues(t)
			sources := values["sandboxSources"].(map[string]any)
			for key, value := range test.sources {
				sources[key] = value
			}
			// The same sources render without the marketplace.
			if _, err := renderSandboxSourcesChart(t, values); err != nil {
				t.Fatal(err)
			}
			values["skillMarketplace"] = test.marketplace
			if _, err := renderSandboxSourcesChart(t, values); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatal("invalid Skill Marketplace chart setting was accepted or misclassified:", err)
			}
		})
	}
}
