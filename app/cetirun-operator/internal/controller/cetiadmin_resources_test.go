// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

type cetiAdminManifestReader struct {
	cetiAdminTestHelm
	release *release.Release
	err     error
}

func (r *cetiAdminManifestReader) CurrentRelease(_ context.Context, _, _ string) (*release.Release, error) {
	return r.release, r.err
}

func TestCetiAdminRuntimeManifestBindsActualUIDs(t *testing.T) {
	for _, scenario := range []string{"current", "missing", "foreign namespace", "foreign owner", "duplicate", "release unavailable", "release absent"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			agent, namespace := f.target("example", "team", api.AgentTypeHermes, true)
			target := cetiadmin.TargetIdentity{Agent: identityOf(agent), Namespace: identityOf(namespace), Release: helmReleaseName(agent.Name), Runtime: "hermes"}
			object := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "runtime-config", Namespace: namespace.Name, Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{"meta.helm.sh/release-name": target.Release, "meta.helm.sh/release-namespace": namespace.Name}}, Data: map[string]string{"retained": "synthetic-value-not-exported"}}
			f.seed(object)
			manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: runtime-config\n"
			reader := &cetiAdminManifestReader{release: &release.Release{Name: target.Release, Namespace: namespace.Name, Version: 1, Info: &release.Info{Status: release.StatusDeployed}, Manifest: manifest}}
			switch scenario {
			case "missing":
				reader.release.Manifest = strings.ReplaceAll(manifest, "runtime-config", "missing")
			case "foreign namespace":
				reader.release.Manifest += "  namespace: foreign\n"
			case "foreign owner":
				object.Annotations["meta.helm.sh/release-name"] = "another"
				if err := f.kube.Update(f.ctx, object); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				reader.release.Manifest += "---\n" + manifest
			case "release unavailable":
				reader.err = errors.New("unavailable")
			case "release absent":
				reader.err = driver.ErrReleaseNotFound
			}
			f.r.Runtime.HelmClient = reader
			objects, err := f.r.Runtime.CetiAdminRuntimeResources(f.ctx, agent, target)
			allowed := scenario == "current" || scenario == "missing" || scenario == "release absent"
			if (err == nil) != allowed {
				t.Fatalf("wrong result: %v", err)
			}
			if scenario == "current" && (len(objects) != 1 || objects[0].UID != object.UID || objects[0].Kind != "ConfigMap") {
				t.Fatal("manifest did not resolve the actual ConfigMap UID")
			}
			if (scenario == "missing" || scenario == "release absent") && len(objects) != 0 {
				t.Fatal("missing object granted a write identity")
			}
			raw, _ := json.Marshal(objects)
			if strings.Contains(string(raw), "synthetic-value-not-exported") {
				t.Fatal("configuration content entered access state")
			}
		})
	}
}

func TestCetiAdminRuntimeManifestIncludesOwnedRootObjectsAndClaimsOnce(t *testing.T) {
	f := newCetiAdminFixture(t)
	agent, namespace := f.target("example", "team", api.AgentTypeHermes, true)
	target := cetiadmin.TargetIdentity{Agent: identityOf(agent), Namespace: identityOf(namespace), Release: helmReleaseName(agent.Name), Runtime: "hermes"}
	metadata := func(name, namespace string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{"meta.helm.sh/release-name": target.Release, "meta.helm.sh/release-namespace": target.Namespace.Name}}
	}
	set := &appsv1.StatefulSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"}, ObjectMeta: metadata(instanceServiceName(agent.Name), namespace.Name), Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "home", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "home"}}}}}}}}
	claim := &corev1.PersistentVolumeClaim{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"}, ObjectMeta: metadata("home", namespace.Name)}
	binding := &rbacv1.ClusterRoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding"}, ObjectMeta: metadata("runtime-binding", "")}
	var manifest strings.Builder
	for _, object := range []client.Object{set, claim, binding} {
		raw, err := yaml.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		manifest.WriteString("---\n")
		manifest.Write(raw)
		f.seed(object)
	}
	f.r.Runtime.HelmClient = &cetiAdminManifestReader{release: &release.Release{Name: target.Release, Namespace: namespace.Name, Version: 1, Info: &release.Info{Status: release.StatusDeployed}, Manifest: manifest.String()}}
	objects, err := f.r.Runtime.CetiAdminRuntimeResources(f.ctx, agent, target)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 3 {
		t.Fatalf("manifest or claim duplicate lost an identity: %#v", objects)
	}
}

func TestCetiAdminRuntimeManifestReadsIstioSecurityObjects(t *testing.T) {
	f := newCetiAdminFixture(t)
	agent, namespace := f.target("security-objects", "team", api.AgentTypeCustom, true)
	target := cetiadmin.TargetIdentity{Agent: identityOf(agent), Namespace: identityOf(namespace), Release: helmReleaseName(agent.Name), Runtime: "custom"}
	var manifest strings.Builder
	wanted := map[string]string{}
	for _, kind := range []string{"AuthorizationPolicy", "PeerAuthentication"} {
		object := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "security.istio.io/v1", "kind": kind,
			"metadata": map[string]any{"name": "runtime-" + strings.ToLower(kind), "namespace": namespace.Name,
				"labels":      map[string]any{"app.kubernetes.io/managed-by": "Helm"},
				"annotations": map[string]any{"meta.helm.sh/release-name": target.Release, "meta.helm.sh/release-namespace": namespace.Name}},
			"spec": map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app": "synthetic-runtime"}}},
		}}
		raw, err := yaml.Marshal(object.Object)
		if err != nil {
			t.Fatal(err)
		}
		manifest.WriteString("---\n")
		manifest.Write(raw)
		f.seed(object)
		wanted[kind] = string(object.GetUID())
	}
	f.r.Runtime.HelmClient = &cetiAdminManifestReader{release: &release.Release{Name: target.Release, Namespace: namespace.Name, Version: 1,
		Info: &release.Info{Status: release.StatusDeployed}, Manifest: manifest.String()}}
	objects, err := f.r.Runtime.CetiAdminRuntimeResources(f.ctx, agent, target)
	if err != nil || len(objects) != 2 {
		t.Fatalf("existing Custom security resources were not readable: %v", err)
	}
	for _, object := range objects {
		if string(object.UID) != wanted[object.Kind] || object.Namespace != namespace.Name {
			t.Fatal("security resource UID/namespace was not bound to the observed object")
		}
	}
}
