// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package sandboxadmission

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// The installation checkpoint and Pods are synthetic. Authentication, RBAC,
// generated CRD validation, rendered match rules and handlers use a real API.
func TestCetiAdminServedAPI(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, binary := range []string{"kube-apiserver", "etcd"} {
		info, err := os.Stat(filepath.Join(assets, binary))
		if assets == "" || err != nil || info.IsDir() {
			t.Fatal("KUBEBUILDER_ASSETS must contain the pinned local envtest binaries")
		}
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false), CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true, ControlPlaneStartTimeout: 45 * time.Second, ControlPlaneStopTimeout: 30 * time.Second}
	environment.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC").Set("api-audiences", "cetiadmin-test-api")
	config, err := environment.Start()
	if err != nil {
		_ = environment.Stop()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	newClient := func(c *rest.Config) client.Client {
		result, err := client.New(c, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	admin := newClient(config)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	create := func(object client.Object) {
		t.Helper()
		if err := admin.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"operator-test", "outside-test"} {
		create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}
	create(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "outside-test"}})
	desired := cetiadmin.DefaultConfig()
	desired.Owner = cetiadmin.Owner{Alias: "owner", TenantID: "11111111-1111-1111-1111-111111111111", ObjectID: "22222222-2222-2222-2222-222222222222"}
	desired.Model.InitialMonthlyBudgetUSD = "100"
	desired.Kubernetes.APIAudience = "cetiadmin-test-api"
	desired.Kubernetes.APIEgressCIDRs = []string{"192.0.2.10/32"}
	raw, _ := json.Marshal(desired)
	source := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-cetiadmin-config", Namespace: "operator-test"}, Data: map[string]string{cetiadmin.ConfigKey: string(raw)}}
	create(source)
	mark := map[string]string{cetiadmin.AnnotationSourceUID: string(source.UID)}
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "cetiadmin", Annotations: mark}}
	create(team)
	create(&api.Team{ObjectMeta: metav1.ObjectMeta{Name: "engineering"}})
	self := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "cetiadmin", Annotations: map[string]string{cetiadmin.AnnotationSourceUID: string(source.UID), api.AnnotationOwnerTenantID: desired.Owner.TenantID, api.AnnotationOwnerObjectID: desired.Owner.ObjectID}}, Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: api.AgentTypeHermes}, User: api.UserSpec{Alias: "owner"}, Team: "cetiadmin", GatewayTokenSecretRef: "native-auth"}}
	create(self)
	target := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "example"}, Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: api.AgentTypeHermes}, User: api.UserSpec{Alias: "owner"}, Team: "engineering", GatewayTokenSecretRef: "native-auth"}}
	create(target)
	makeNamespace := func(agent *api.Agent) *corev1.Namespace {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-" + agent.Name, Labels: map[string]string{"app.kubernetes.io/managed-by": "cetirun-operator"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID}}}}
		create(ns)
		return ns
	}
	namespace, targetNS := makeNamespace(self), makeNamespace(target)
	create(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: targetNS.Name}})
	target.Status = api.AgentStatus{Namespace: targetNS.Name, HelmReleaseName: "openclaw-example", Phase: api.PhaseReady, ObservedGeneration: target.Generation}
	if err := admin.Status().Update(ctx, target); err != nil {
		t.Fatal(err)
	}
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "openclaw-cetiadmin-cetirun", Namespace: namespace.Name, Annotations: mark, OwnerReferences: namespace.OwnerReferences}, AutomountServiceAccountToken: ptr.To(false)}
	create(account)
	identity := func(o metav1.Object) cetiadmin.ObjectIdentity {
		return cetiadmin.ObjectIdentity{Name: o.GetName(), Namespace: o.GetNamespace(), UID: o.GetUID()}
	}
	hash, _ := cetiadmin.ConfigHash(desired)
	state := cetiadmin.Installation{Record: cetiadmin.Record{SchemaVersion: 1, Source: identity(source), ConfigHash: hash, Agent: identity(self), Team: identity(team), Namespace: identity(namespace), ServiceAccount: identity(account)}, Configuration: desired, Phase: cetiadmin.PhaseReady, Bootstrapped: true}
	checkpoint := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cetiadmin.InstallationName(source.Namespace, source.Name), Namespace: source.Namespace, Annotations: mark}}
	create(checkpoint)
	state.RecordUID = checkpoint.UID
	body, _ := json.Marshal(state)
	checkpoint.Data = map[string]string{cetiadmin.BindingKey: string(body)}
	if err := admin.Update(ctx, checkpoint); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "identity-fixture", Namespace: namespace.Name}, Spec: corev1.PodSpec{ServiceAccountName: account.Name, AutomountServiceAccountToken: ptr.To(false), Containers: []corev1.Container{{Name: "fixture", Image: "registry.example.com/fixture:synthetic"}}}}
	create(pod)
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "cetiadmin-fixture"}, Rules: append(cetiadmin.ClusterOperationRules(), cetiadmin.ReadRule(api.GroupVersion.Group, "agents", "teams"), cetiadmin.ReadRule("", "namespaces"))}
	create(role)
	create(&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: role.Name}, RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: account.Name, Namespace: account.Namespace}}})
	machineClient := func(audience string) client.Client {
		request := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{Audiences: []string{audience}, ExpirationSeconds: ptr.To(int64(600)), BoundObjectRef: &authenticationv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}}}
		if err := admin.SubResource("token").Create(ctx, account, request); err != nil {
			t.Fatal(err)
		}
		anonymous := rest.AnonymousClientConfig(config)
		anonymous.BearerToken = request.Status.Token
		return newClient(anonymous)
	}
	machine := machineClient("cetiadmin-test-api")
	settings := testConfig()
	settings.AdminGroups = []string{"system:masters"}
	settings.CetiAdmin = &CetiAdminConfig{Namespace: source.Namespace, ConfigMapName: source.Name, RuntimeUsername: serviceAccountUsername(state.ServiceAccount), StopUsername: "system:serviceaccount:operator-test:cetirun-cetiadmin-stop", AdminGroups: []string{"system:masters"}}
	guard, err := NewCetiAdminHandler(admin, settings)
	if err != nil {
		t.Fatal(err)
	}
	governance, err := NewHandler(admin, settings)
	if err != nil {
		t.Fatal(err)
	}
	var unavailable atomic.Bool
	var observedUID atomic.Value
	mux := http.NewServeMux()
	mux.Handle(CetiAdminValidationPath, &admission.Webhook{Handler: admission.HandlerFunc(func(ctx context.Context, request admission.Request) admission.Response {
		if request.UserInfo.Username == serviceAccountUsername(identity(account)) {
			observedUID.Store(request.UserInfo.UID)
		}
		return guard.Validate(ctx, request)
	})})
	mux.Handle(DefaultingPath, &admission.Webhook{Handler: admission.HandlerFunc(governance.DefaultAgent)})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() {
			http.Error(w, "synthetic outage", http.StatusServiceUnavailable)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	chart, err := loader.Load("../../chart")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"namespace": source.Namespace, "domainSuffix": "agents.example.com", "serviceAccount": map[string]any{"name": "controller"}, "cetiadmin": map[string]any{"owner": map[string]any{"alias": desired.Owner.Alias, "tenantID": desired.Owner.TenantID, "objectID": desired.Owner.ObjectID}, "model": map[string]any{"initialMonthlyBudgetUSD": "100"}, "kubernetes": map[string]any{"apiAudience": desired.Kubernetes.APIAudience, "apiEgressCIDRs": []any{"192.0.2.10/32"}}}, "sandboxAdmission": map[string]any{"tlsSecretName": "fixture-tls", "caBundle": base64.StdEncoding.EncodeToString(ca), "adminUsers": []any{"administrator"}}}
	capabilities := chartutil.DefaultCapabilities.Copy()
	capabilities.KubeVersion.Version = "v1.37.0"
	renderValues, err := chartutil.ToRenderValues(chart, values, chartutil.ReleaseOptions{Name: "operator", Namespace: source.Namespace, IsInstall: true}, capabilities)
	if err != nil {
		t.Fatal(err)
	}
	manifests, err := engine.Render(chart, renderValues)
	if err != nil {
		t.Fatal(err)
	}
	decoder := kyaml.NewYAMLOrJSONDecoder(strings.NewReader(manifests["cetirun-operator/templates/cetiadmin-admission.yaml"]), 4096)
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		switch object.GetKind() {
		case "ValidatingWebhookConfiguration":
			v := &admissionregistrationv1.ValidatingWebhookConfiguration{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, v); err != nil {
				t.Fatal(err)
			}
			for i := range v.Webhooks {
				hook := &v.Webhooks[i]
				hook.ClientConfig.URL = ptr.To(server.URL + *hook.ClientConfig.Service.Path)
				hook.ClientConfig.Service = nil
				hook.TimeoutSeconds = ptr.To(int32(2))
			}
			create(v)
		case "MutatingWebhookConfiguration":
			m := &admissionregistrationv1.MutatingWebhookConfiguration{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, m); err != nil {
				t.Fatal(err)
			}
			for i := range m.Webhooks {
				hook := &m.Webhooks[i]
				hook.ClientConfig.URL = ptr.To(server.URL + *hook.ClientConfig.Service.Path)
				hook.ClientConfig.Service = nil
				hook.TimeoutSeconds = ptr.To(int32(2))
			}
			create(m)
		}
	}
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		agent := &api.Agent{}
		if err := machine.Get(ctx, client.ObjectKey{Name: target.Name}, agent); err != nil {
			return false, nil
		}
		agent.Spec.User.Alias = "wrong-owner"
		err := machine.Update(ctx, agent, client.DryRunAll)
		return apierrors.IsForbidden(err) && strings.Contains(err.Error(), "CetiAdmin cannot change owner"), nil
	}); err != nil {
		t.Fatal("CetiAdmin admission did not become effective")
	}
	t.Run("real Pod-bound identity can update allowed Agent intent", func(t *testing.T) {
		agent := &api.Agent{}
		if err := machine.Get(ctx, client.ObjectKey{Name: target.Name}, agent); err != nil {
			t.Fatal(err)
		}
		agent.Spec.Model = "auto"
		if err := machine.Update(ctx, agent); err != nil {
			t.Fatal(err)
		}
		if observedUID.Load() != string(account.UID) {
			t.Fatal("request did not use the actual ServiceAccount UID")
		}
	})
	t.Run("MCP audience cannot authenticate to the Kubernetes API", func(t *testing.T) {
		if err := machineClient("connections-mcp-test").Get(ctx, client.ObjectKey{Name: target.Name}, &api.Agent{}); !apierrors.IsUnauthorized(err) {
			t.Fatalf("cross-audience API request was not rejected: %v", err)
		}
	})
	t.Run("Held blocks every Pod creator", func(t *testing.T) {
		if err := admin.Get(ctx, client.ObjectKey{Name: target.Name}, target); err != nil {
			t.Fatal(err)
		}
		target.Spec.Suspended = true
		target.Spec.Helm.Maintenance = &api.HelmMaintenanceRequest{RequestID: "maintenance-12345678", ExpectedRevision: 1}
		if err := admin.Update(ctx, target); err != nil {
			t.Fatal(err)
		}
		target.Status.HelmMaintenance = &api.HelmMaintenanceStatus{RequestID: target.Spec.Helm.Maintenance.RequestID, ObservedGeneration: target.Generation, Revision: 1, Phase: api.HelmMaintenancePhaseHeld}
		if err := admin.Status().Update(ctx, target); err != nil {
			t.Fatal(err)
		}
		probe := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "forbidden-writer", Namespace: targetNS.Name}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fixture", Image: "registry.example.com/fixture:synthetic"}}}}
		if err := admin.Create(ctx, probe, client.DryRunAll); !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "Helm maintenance blocks") {
			t.Fatalf("a new writer was not rejected by the maintenance rule: %v", err)
		}
	})
	t.Run("namespace selector cannot be removed to bypass admission", func(t *testing.T) {
		current := &corev1.Namespace{}
		if err := admin.Get(ctx, client.ObjectKey{Name: targetNS.Name}, current); err != nil {
			t.Fatal(err)
		}
		delete(current.Labels, "app.kubernetes.io/managed-by")
		if err := admin.Update(ctx, current, client.DryRunAll); !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "managed namespace admission selector") {
			t.Fatalf("selector removal was not denied: %v", err)
		}
	})
	t.Run("outage does not block unrelated namespace objects", func(t *testing.T) {
		unavailable.Store(true)
		defer unavailable.Store(false)
		if err := admin.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "outside-test"}}); err != nil {
			t.Fatal(err)
		}
		outside := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "unrelated-pod", Namespace: "outside-test"}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fixture", Image: "registry.example.com/fixture:synthetic"}}}}
		if err := admin.Create(ctx, outside, client.DryRunAll); err != nil {
			t.Fatal(err)
		}
		inside := outside.DeepCopy()
		inside.Namespace = targetNS.Name
		if err := admin.Create(ctx, inside, client.DryRunAll); err == nil || !strings.Contains(err.Error(), "failed calling webhook") {
			t.Fatalf("managed namespace did not fail closed at the unavailable webhook: %v", err)
		}
	})
}
