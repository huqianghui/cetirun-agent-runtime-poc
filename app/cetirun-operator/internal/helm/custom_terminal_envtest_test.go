// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package helm

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/terminal"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

func TestCustomTerminalRBACAndAdmission(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Fatal("KUBEBUILDER_ASSETS must point to preinstalled test binaries")
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	admin, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	input := terminalInput(8080)
	bindings := input["customBindings"].(runtimebindings.Custom)
	namespace := bindings.Namespace.Name
	ns, err := admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: namespace, Annotations: map[string]string{"cetirun.microsoft.com/agent-uid": bindings.AgentUID},
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	operatorChart, err := loader.Load("../../chart")
	if err != nil {
		t.Fatal(err)
	}
	values, err := chartutil.ToRenderValues(operatorChart, map[string]any{"cetiadmin": map[string]any{"enabled": false}},
		chartutil.ReleaseOptions{Name: "operator", Namespace: "cetirun-system", IsInstall: true}, chartutil.DefaultCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := engine.Render(operatorChart, values)
	if err != nil {
		t.Fatal(err)
	}
	for filename, document := range rendered {
		if !strings.HasSuffix(filename, "/clusterrole.yaml") && !strings.HasSuffix(filename, "/terminal-exec-role.yaml") {
			continue
		}
		var role rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(document), &role); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.RbacV1().ClusterRoles().Create(ctx, &role, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	operatorIdentity := "system:serviceaccount:cetirun-system:operator-test"
	requireGrant := func(user string, attributes authorizationv1.ResourceAttributes) {
		t.Helper()
		grantCtx, cancelGrant := context.WithTimeout(ctx, 5*time.Second)
		defer cancelGrant()
		grant, err := waitForRBACGrant(grantCtx, 50*time.Millisecond, func(ctx context.Context) (*authorizationv1.SubjectAccessReview, error) {
			return admin.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
				Spec: authorizationv1.SubjectAccessReviewSpec{User: user, ResourceAttributes: &attributes},
			}, metav1.CreateOptions{})
		})
		if err != nil {
			t.Fatalf("RBAC grant for %s %+v did not become ready: %v; last review: %+v", user, attributes, err, grant)
		}
	}
	if _, err := admin.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "operator-test"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: "operator-test", Namespace: "cetirun-system"}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cetirun-operator-manager-role"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// A successful binding write does not mean the authorizer has observed it.
	requireGrant(operatorIdentity, authorizationv1.ResourceAttributes{
		Namespace: namespace, Group: rbacv1.GroupName, Resource: "roles", Verb: "create",
	})
	operatorConfig := rest.CopyConfig(config)
	operatorConfig.Impersonate.UserName = operatorIdentity
	operator, err := kubernetes.NewForConfig(operatorConfig)
	if err != nil {
		t.Fatal(err)
	}
	workload := text(input["fullnameOverride"])
	role, reader := terminal.ReadRoles(namespace, workload, bindings.AgentName, bindings.AgentUID)
	if _, err := operator.RbacV1().Roles(namespace).Create(ctx, role, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Operator cannot prepare the named Pod reader: %v", err)
	}
	if _, err := operator.RbacV1().ClusterRoles().Create(ctx, reader, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Operator cannot prepare the named Agent reader: %v", err)
	}

	prepared, err := PrepareChart(testChart(t), input)
	if err != nil {
		t.Fatal(err)
	}
	objects := manifestObjects(t, prepared)
	writer, err := dynamic.NewForConfig(operatorConfig)
	if err != nil {
		t.Fatal(err)
	}
	// Persist only the access controls. All workload objects stay dry-runs;
	// this API server has no kubelet and never executes a container command.
	for _, resource := range []struct {
		kind, group, plural string
		cluster, dryRun     bool
	}{
		{"ServiceAccount", "", "serviceaccounts", false, false},
		{"RoleBinding", rbacv1.GroupName, "rolebindings", false, false},
		{"ClusterRoleBinding", rbacv1.GroupName, "clusterrolebindings", true, false},
		{"Deployment", "apps", "deployments", false, true},
		{"Service", "", "services", false, true},
		{"NetworkPolicy", "networking.k8s.io", "networkpolicies", false, true},
		{"ValidatingAdmissionPolicy", "admissionregistration.k8s.io", "validatingadmissionpolicies", true, false},
		{"ValidatingAdmissionPolicyBinding", "admissionregistration.k8s.io", "validatingadmissionpolicybindings", true, false},
	} {
		for _, object := range objects {
			if object["kind"] != resource.kind {
				continue
			}
			api := writer.Resource(schema.GroupVersionResource{Group: resource.group, Version: "v1", Resource: resource.plural})
			var target dynamic.ResourceInterface = api
			if !resource.cluster {
				target = api.Namespace(namespace)
			}
			options := metav1.CreateOptions{FieldValidation: metav1.FieldValidationStrict}
			if resource.dryRun {
				options.DryRun = []string{metav1.DryRunAll}
			}
			if _, err := target.Create(ctx, &unstructured.Unstructured{Object: object}, options); err != nil {
				t.Fatalf("Operator cannot apply %s: %v", resource.kind, err)
			}
		}
	}
	name := terminalpolicy.WorkloadName(workload)
	terminalIdentity := "system:serviceaccount:" + namespace + ":" + name
	checks := []struct {
		user, group, resource, subresource, name, namespace, verb string
		allowed                                                   bool
	}{
		{terminalIdentity, "", "pods", "", workload + "-0", namespace, "get", true},
		{terminalIdentity, "", "pods", "", "other-0", namespace, "get", false},
		{terminalIdentity, "", "pods", "", "", namespace, "list", false},
		{terminalIdentity, "", "pods", "exec", workload + "-0", namespace, "create", true},
		{terminalIdentity, "", "pods", "exec", workload + "-0", "other", "create", false},
		{terminalIdentity, "", "secrets", "", "example", namespace, "get", false},
		{terminalIdentity, "", "pods", "", "example", namespace, "create", false},
		{terminalIdentity, "apps", "statefulsets", "", workload, namespace, "get", true},
		{terminalIdentity, "cetirun.microsoft.com", "agents", "", bindings.AgentName, "", "get", true},
		{terminalIdentity, "cetirun.microsoft.com", "agents", "", "other", "", "get", false},
		{operatorIdentity, "", "pods", "exec", workload + "-0", namespace, "create", false},
		{operatorIdentity, "", "pods", "exec", workload + "-0", namespace, "get", false},
	}
	// Observe every positive grant before a missing binding can make a denial
	// look like proof that the terminal's restrictions work.
	for _, check := range checks {
		if check.allowed {
			requireGrant(check.user, authorizationv1.ResourceAttributes{
				Group: check.group, Resource: check.resource, Subresource: check.subresource,
				Name: check.name, Namespace: check.namespace, Verb: check.verb,
			})
		}
	}
	for _, check := range checks {
		result, err := admin.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
			Spec: authorizationv1.SubjectAccessReviewSpec{User: check.user,
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Group: check.group, Resource: check.resource, Subresource: check.subresource,
					Name: check.name, Namespace: check.namespace, Verb: check.verb,
				}},
		}, metav1.CreateOptions{})
		if err != nil || result.Status.Allowed != check.allowed {
			t.Fatalf("RBAC %s %s/%s: allowed=%v, want=%v, err=%v", check.user, check.resource, check.subresource,
				result, check.allowed, err)
		}
	}
	if _, err := operator.RbacV1().RoleBindings(namespace).Create(ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "must-not-escalate"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: namespace}},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
	}, metav1.CreateOptions{}); !apierrors.IsForbidden(err) {
		t.Fatalf("Operator must not bind cluster-admin: %v", err)
	}

	terminalConfig := rest.CopyConfig(config)
	terminalConfig.Impersonate.UserName = terminalIdentity
	caller, err := kubernetes.NewForConfig(terminalConfig)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(client kubernetes.Interface, method, pod string, options corev1.PodExecOptions) error {
		return client.CoreV1().RESTClient().Verb(method).Resource("pods").
			Namespace(namespace).Name(pod).SubResource("exec").
			VersionedParams(&options, scheme.ParameterCodec).Do(ctx).Error()
	}
	interactive := corev1.PodExecOptions{Container: "cetirun", Command: []string{"sh", "-i"},
		Stdin: true, Stdout: true, TTY: true}
	deniedByPolicy := func(err error) bool {
		return apierrors.IsForbidden(err) && strings.Contains(err.Error(), "kubectl exec is not allowed")
	}
	var lastExecError error
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 10*time.Second, true, func(context.Context) (bool, error) {
		lastExecError = exec(caller, http.MethodPost, "other-0", interactive)
		return deniedByPolicy(lastExecError), nil
	}); err != nil {
		t.Fatalf("exec admission did not become active: %v; last request: %v", err, lastExecError)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, shell := range terminalpolicy.ShellCandidates() {
			t.Run(method+"/"+shell.Name, func(t *testing.T) {
				for _, options := range []corev1.PodExecOptions{
					{Container: "cetirun", Command: shell.Probe, Stdout: true, Stderr: true},
					{Container: "cetirun", Command: []string{shell.Name, "-i"}, Stdin: true, Stdout: true, TTY: true},
				} {
					// Passing admission reaches Pod lookup. No Pod is created,
					// so NotFound is the expected, non-executing control.
					if err := exec(caller, method, workload+"-0", options); !apierrors.IsNotFound(err) {
						t.Fatalf("bound shell rejected before Pod lookup: %v", err)
					}
				}
			})
		}
		for _, edit := range []func(*corev1.PodExecOptions){
			func(o *corev1.PodExecOptions) { o.Container = "istio-proxy" },
			func(o *corev1.PodExecOptions) { o.Container = "desktop" },
			func(o *corev1.PodExecOptions) { o.Container = "" },
			func(o *corev1.PodExecOptions) { o.Command = []string{"sh", "-c", "id"} },
			func(o *corev1.PodExecOptions) { o.Command = []string{"sh", "-i", "-c", "id"} },
			func(o *corev1.PodExecOptions) { o.Stdin = false },
			func(o *corev1.PodExecOptions) { o.Stdout = false },
			func(o *corev1.PodExecOptions) { o.TTY = false },
		} {
			options := *interactive.DeepCopy()
			edit(&options)
			if err := exec(caller, method, workload+"-0", options); !deniedByPolicy(err) {
				t.Fatalf("unapproved %s exec must be rejected by admission: %v", method, err)
			}
		}
		if err := exec(caller, method, name+"-other", interactive); !deniedByPolicy(err) {
			t.Fatalf("terminal Pod exec must be rejected by admission: %v", err)
		}
		if err := exec(admin, method, workload+"-0", interactive); !deniedByPolicy(err) {
			t.Fatalf("ordinary admin exec must still be rejected by admission: %v", err)
		}
	}
	ns.Annotations["cetirun.microsoft.com/agent-uid"] = "replacement-agent"
	if _, err := admin.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 10*time.Second, true, func(context.Context) (bool, error) {
		return deniedByPolicy(exec(caller, http.MethodPost, workload+"-0", interactive)), nil
	}); err != nil {
		t.Fatalf("terminal retained access after namespace owner replacement: %v", err)
	}
}
