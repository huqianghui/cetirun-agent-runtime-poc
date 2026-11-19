// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package helm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

func TestCustomServerPreflightAndOperatorRBAC(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Fatal("KUBEBUILDER_ASSETS must point to preinstalled test binaries")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	const namespace = "cetirun-example"
	if _, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	cfg := &action.Configuration{}
	if err := cfg.Init(restClientGetter(config, namespace), namespace, "secret", func(string, ...any) {}); err != nil {
		t.Fatal(err)
	}
	p := customActionPlan(t, testChart(t))
	if err := p.prepareCustomAction(cfg, namespace, p.releaseName); err != nil {
		t.Fatal(err)
	}
	if err := p.validateCustomAPI(ctx, config); err != nil {
		t.Fatalf("legitimate dry-run failed: %v", err)
	}
	corrupt := testChart(t)
	for _, file := range corrupt.Templates {
		if file.Name == "templates/agents/custom/_runtime.tpl" {
			file.Data = []byte(strings.Replace(string(file.Data), "allowPrivilegeEscalation: false",
				"allowPrivilegeEscalation: true\nallowPrivilegeescalation: false", 1))
		}
	}
	rejected := customActionPlan(t, corrupt)
	if err := rejected.prepareCustomAction(cfg, namespace, rejected.releaseName); err == nil || rejected.actionRevision != 0 || rejected.apiValidated {
		t.Fatal("security-context alias reached an approved action against the API")
	}
	if _, err := client.AppsV1().StatefulSets(namespace).Get(ctx, text(p.Values["fullnameOverride"]), metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("preflight persisted a workload: %v", err)
	}
	t.Run("shared skills API dry-run", func(t *testing.T) {
		input := customInput(nil)
		input["skills"] = sharedSkillValues()
		shared, err := prepareChart(testChart(t), input, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := shared.prepareCustomAction(cfg, namespace, shared.releaseName); err != nil {
			t.Fatal(err)
		}
		if err := shared.validateCustomAPI(ctx, config); err != nil {
			t.Fatalf("shared-skills dry-run rejected: %v", err)
		}
		pv, pvc := shared.sharedSkillsNames(namespace)
		if _, err := client.CoreV1().PersistentVolumes().Get(ctx, pv, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("dry-run persisted shared PV: %v", err)
		}
		if _, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvc, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("dry-run persisted shared PVC: %v", err)
		}
	})

	// The invalid metric is accepted by local structural checks but not by the
	// API's semantic validation. No workload exists before or after this call.
	input := customInput(nil)
	input["autoscaling"] = map[string]any{"enabled": true, "minReplicas": 1, "maxReplicas": 1,
		"metrics": []any{map[string]any{"type": "Resource", "resource": map[string]any{
			"name": "memory", "target": map[string]any{"type": "Utilization", "averageUtilization": 0},
		}}}}
	invalid, err := prepareChart(testChart(t), input, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := invalid.prepareCustomAction(cfg, namespace, invalid.releaseName); err != nil {
		t.Fatal(err)
	}
	if err := invalid.validateCustomAPI(ctx, config); err == nil || invalid.apiValidated {
		t.Fatalf("API-invalid plan passed preflight: %v", err)
	}
	workloads, err := client.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
	if err != nil || len(workloads.Items) != 0 {
		t.Fatalf("dry-run wrote resources: %v", err)
	}

	// Updating an existing resource must also remain a dry-run.
	name := text(p.Values["fullnameOverride"])
	var existing appsv1.StatefulSet
	if err := decodeObject(platformRender(t, p, cfg.Capabilities)["StatefulSet"], &existing); err != nil {
		t.Fatal(err)
	}
	const existingImage = "registry.example.com/existing:1"
	existing.Spec.Template.Spec.Containers[0].Image = existingImage
	existing.Annotations["meta.helm.sh/release-name"] = p.releaseName
	existing.Annotations["meta.helm.sh/release-namespace"] = namespace
	created, err := client.AppsV1().StatefulSets(namespace).Create(ctx, &existing, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.validateCustomAPI(ctx, config); err != nil {
		t.Fatal(err)
	}
	for testName, mutate := range map[string]func(*runtimebindings.HomeClaim){
		"HOME template size is immutable":          func(claim *runtimebindings.HomeClaim) { claim.StorageSize = "200Gi" },
		"HOME template storage class is immutable": func(claim *runtimebindings.HomeClaim) { claim.StorageClass = "other" },
		"HOME template access mode is immutable":   func(claim *runtimebindings.HomeClaim) { claim.AccessMode = "ReadWriteMany" },
	} {
		t.Run(testName, func(t *testing.T) {
			input := customInput(8080)
			bindings := input["customBindings"].(runtimebindings.Custom)
			mutate(bindings.Home.Claim)
			input["customBindings"] = bindings
			update, err := prepareChart(testChart(t), input, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := update.prepareCustomAction(cfg, namespace, update.releaseName); err != nil {
				t.Fatal(err)
			}
			if err := update.validateCustomAPI(ctx, config); err == nil || !IsConfigurationError(err) ||
				!strings.Contains(err.Error(), "spec.volumeClaimTemplates") || update.apiValidated {
				t.Fatalf("immutable HOME template update passed preflight or failed for another reason: %v", err)
			}
		})
	}
	unchanged, err := client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil || unchanged.ResourceVersion != created.ResourceVersion || unchanged.Spec.Template.Spec.Containers[0].Image != existingImage {
		t.Fatalf("dry-run patched live workload: %v", err)
	}

	operator, err := loader.Load("../../chart")
	if err != nil {
		t.Fatal(err)
	}
	values, err := chartutil.ToRenderValues(operator, map[string]any{"cetiadmin": map[string]any{"enabled": false}}, chartutil.ReleaseOptions{Name: "operator", Namespace: "cetirun-system", IsInstall: true}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(operator, values)
	if err != nil {
		t.Fatal(err)
	}
	var role rbacv1.ClusterRole
	for path, content := range rendered {
		if strings.HasSuffix(path, "/clusterrole.yaml") {
			if err := yaml.Unmarshal([]byte(content), &role); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := client.RbacV1().ClusterRoles().Create(ctx, &role, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = client.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "operator-test"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: "cetirun-system", Name: "operator-test"}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The authorizer observes new roles and bindings asynchronously. Wait for a
	// positive grant before asserting either allowed or forbidden operations.
	grantCtx, cancelGrant := context.WithTimeout(ctx, 5*time.Second)
	defer cancelGrant()
	grant, err := waitForRBACGrant(grantCtx, 50*time.Millisecond, func(ctx context.Context) (*authorizationv1.SubjectAccessReview, error) {
		return client.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User:               "system:serviceaccount:cetirun-system:operator-test",
			ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Resource: "pods", Verb: "list"},
		}}, metav1.CreateOptions{})
	})
	if err != nil {
		t.Fatalf("RBAC grant did not become ready: %v; last review: %+v", err, grant)
	}
	for _, check := range []struct {
		group, resource, verb string
		allowed               bool
	}{{"", "pods", "list", true}, {"", "pods", "create", false}, {"policy", "poddisruptionbudgets", "create", true},
		{"autoscaling", "horizontalpodautoscalers", "patch", true},
		{"", "persistentvolumes", "create", true}, {"", "persistentvolumeclaims", "create", true}} {
		review, err := client.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
			User:               "system:serviceaccount:cetirun-system:operator-test",
			ResourceAttributes: &authorizationv1.ResourceAttributes{Namespace: namespace, Group: check.group, Resource: check.resource, Verb: check.verb},
		}}, metav1.CreateOptions{})
		if err != nil || review.Status.Allowed != check.allowed {
			t.Fatalf("RBAC %s %s: %v, expected %v", check.verb, check.resource, err, check.allowed)
		}
	}
}
