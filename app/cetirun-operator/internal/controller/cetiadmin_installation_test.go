// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/google/uuid"
	"helm.sh/helm/v3/pkg/chart/loader"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

type cetiAdminTestHelm struct {
	suspensionHelmClient
	err         error
	unsupported bool
}

func (h *cetiAdminTestHelm) Prepare(_ string, values map[string]any, version string) (*helm.PreparedRelease, error) {
	if h.err != nil {
		return nil, h.err
	}
	chart, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		return nil, err
	}
	chart.Metadata.Version = version
	// The runtime-template worker owns the real projection. This declaration
	// tests installation preflight, not that worker's Pod/asset integration.
	if !h.unsupported {
		chart.Values["cetiadmin"] = map[string]any{"enabled": false}
	} else {
		delete(chart.Values, "cetiadmin")
	}
	image := chart.Values["hermes"].(map[string]any)["image"].(map[string]any)
	image["repository"], image["tag"], image["digest"] = "registry.example.com/hermes-gateway", version, ""
	return helm.PrepareChart(chart, values)
}

type cetiAdminFixture struct {
	t        *testing.T
	ctx      context.Context
	r        *CetiAdminReconciler
	kube     client.Client
	helm     *cetiAdminTestHelm
	config   cetiadmin.Config
	source   *corev1.ConfigMap
	writes   []string
	before   func(string, client.Object) error
	after    func(string, client.Object) error
	getError func(client.ObjectKey, client.Object) error
}

func newCetiAdminFixture(t *testing.T) *cetiAdminFixture {
	t.Helper()
	f := &cetiAdminFixture{t: t, ctx: context.Background(), helm: &cetiAdminTestHelm{}}
	f.config = cetiadmin.DefaultConfig()
	f.config.Owner = cetiadmin.Owner{Alias: "platform-owner", TenantID: "11111111-1111-1111-1111-111111111111", ObjectID: "22222222-2222-2222-2222-222222222222"}
	f.config.Model.InitialMonthlyBudgetUSD = "10.25"
	f.config.Kubernetes.APIAudience, f.config.Kubernetes.APIEgressCIDRs = "api://test-cluster", []string{"192.0.2.44/32"}
	raw, _ := json.Marshal(f.config)
	f.source = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "operations", Namespace: "platform-test", UID: types.UID(uuid.NewString())}, Data: map[string]string{cetiadmin.ConfigKey: string(raw)}}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, api.AddToScheme, istiov1beta1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	opNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: f.source.Namespace, UID: types.UID(uuid.NewString())}}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(cetiAdminTestMapper()).WithStatusSubresource(&api.Agent{}, &api.Team{}, &appsv1.StatefulSet{}, &appsv1.Deployment{}).WithObjects(f.source, opNamespace).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
				if f.before != nil {
					if err := f.before("create", object); err != nil {
						return err
					}
				}
				if object.GetUID() != "" {
					return fmt.Errorf("client-supplied UID in CREATE")
				}
				object.SetUID(types.UID(uuid.NewString()))
				object.SetGeneration(1)
				if err := c.Create(ctx, object, options...); err != nil {
					return err
				}
				f.writes = append(f.writes, "create:"+fmt.Sprintf("%T", object)+":"+client.ObjectKeyFromObject(object).String())
				if f.after != nil {
					return f.after("create", object)
				}
				return nil
			},
			Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
				if f.before != nil {
					if err := f.before("patch", object); err != nil {
						return err
					}
				}
				data, err := patch.Data(object)
				if err != nil {
					return err
				}
				before := object.DeepCopyObject().(client.Object)
				if err := c.Get(ctx, client.ObjectKeyFromObject(object), before); err != nil {
					return err
				}
				if patch.Type() == types.JSONPatchType {
					var operations []struct {
						Op, Path string
						Value    json.RawMessage
					}
					if json.Unmarshal(data, &operations) != nil || len(operations) < 3 || operations[0].Op != "test" || operations[0].Path != "/metadata/uid" ||
						operations[1].Op != "test" || operations[1].Path != "/metadata/resourceVersion" {
						return fmt.Errorf("JSON patch has no UID/resourceVersion preconditions")
					}
					var uid, version string
					if json.Unmarshal(operations[0].Value, &uid) != nil || json.Unmarshal(operations[1].Value, &version) != nil || uid == "" || version == "" ||
						uid != string(before.GetUID()) || version != before.GetResourceVersion() {
						return apierrors.NewConflict(schema.GroupResource{Resource: "test"}, object.GetName(), errors.New("stale UID/version"))
					}
				} else {
					var body struct {
						Metadata struct {
							ResourceVersion string `json:"resourceVersion"`
						} `json:"metadata"`
					}
					if json.Unmarshal(data, &body) != nil || body.Metadata.ResourceVersion == "" {
						return fmt.Errorf("patch has no resourceVersion precondition")
					}
					if before.GetResourceVersion() != body.Metadata.ResourceVersion {
						return apierrors.NewConflict(schema.GroupResource{Resource: "test"}, object.GetName(), errors.New("stale version"))
					}
				}
				if err := c.Patch(ctx, object, patch, options...); err != nil {
					return err
				}
				current := object.DeepCopyObject().(client.Object)
				if err := c.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
					return err
				}
				oldSpec, _ := json.Marshal(before)
				newSpec, _ := json.Marshal(current)
				var oldBody, newBody map[string]any
				_ = json.Unmarshal(oldSpec, &oldBody)
				_ = json.Unmarshal(newSpec, &newBody)
				if !reflect.DeepEqual(oldBody["spec"], newBody["spec"]) {
					current.SetGeneration(before.GetGeneration() + 1)
					if err := c.Update(ctx, current); err != nil {
						return err
					}
				}
				f.writes = append(f.writes, "patch:"+fmt.Sprintf("%T", object)+":"+client.ObjectKeyFromObject(object).String())
				if f.after != nil {
					return f.after("patch", object)
				}
				return nil
			},
			Delete: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.DeleteOption) error {
				if f.before != nil {
					if err := f.before("delete", object); err != nil {
						return err
					}
				}
				deletion := (&client.DeleteOptions{}).ApplyOptions(options)
				if deletion.Preconditions == nil || deletion.Preconditions.UID == nil || deletion.Preconditions.ResourceVersion == nil {
					return fmt.Errorf("delete lacks UID/RV preconditions")
				}
				if err := c.Delete(ctx, object, options...); err != nil {
					return err
				}
				f.writes = append(f.writes, "delete:"+fmt.Sprintf("%T", object)+":"+client.ObjectKeyFromObject(object).String())
				if f.after != nil {
					return f.after("delete", object)
				}
				return nil
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
				if f.getError != nil {
					if err := f.getError(key, object); err != nil {
						return err
					}
				}
				return c.Get(ctx, key, object, options...)
			},
		}).Build()
	f.kube = kube
	runtimeController := &AgentReconciler{Client: kube, APIReader: kube, Scheme: scheme, HelmClient: f.helm,
		OperatorNamespace: f.source.Namespace, OperatorSAName: "operator", ChartVersion: "0.1.1-test.1", ChartRegistry: "registry.example.com", DomainSuffix: "example.com", IstioRevision: "test-istio",
		ConnectionsMCPAudience: "api://connections", RegistryCredentialNamespace: "registry-test", ConnectionsMCPBrokerNamespace: "broker-test",
		AIGateway:         AIGatewayConfig{Enabled: true, URL: "http://gateway.platform-test.svc.cluster.local:4000"},
		CetiRunHelmValues: map[string]any{"skills": map[string]any{"resourceGroup": "test-rg", "storageAccount": "teststorage", "containerName": "shared"}},
	}
	runtimeController.aiGatewayClientFactory = func(context.Context) (aiGatewayKeyClient, error) { return nil, errors.New("unexpected Gateway access") }
	f.r = &CetiAdminReconciler{Client: kube, APIReader: kube, Source: client.ObjectKeyFromObject(f.source), Runtime: runtimeController,
		OperatorImage: "registry.example.com/operator:0.1.1-test.1", ChartVersion: "0.1.1-test.1", AdmissionReady: func(context.Context) error { return nil }}
	f.r.GrantAccess, f.r.RevokeAccess = f.r.GrantInstallationAccess, f.r.RevokeInstallationAccess
	return f
}

func cetiAdminTestMapper() *meta.DefaultRESTMapper {
	versions := []schema.GroupVersion{}
	seen := map[string]bool{}
	for _, resource := range cetiadmin.DiagnosticResources() {
		version := "v1"
		if resource.Group == api.GroupVersion.Group {
			version = api.GroupVersion.Version
		}
		if !seen[resource.Group] {
			versions = append(versions, schema.GroupVersion{Group: resource.Group, Version: version})
			seen[resource.Group] = true
		}
	}
	mapper := meta.NewDefaultRESTMapper(versions)
	clusterKinds := map[string]bool{}
	for _, kind := range []string{"Node", "Namespace", "PersistentVolume", "StorageClass", "CSIDriver", "CSINode", "VolumeAttachment", "VolumeAttributesClass", "VolumeSnapshotContent", "VolumeSnapshotClass", "ClusterRole", "ClusterRoleBinding", "CustomResourceDefinition", "APIService", "ValidatingWebhookConfiguration", "MutatingWebhookConfiguration", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding", "MutatingAdmissionPolicy", "MutatingAdmissionPolicyBinding", "CertificateSigningRequest", "RuntimeClass", "PriorityClass", "FlowSchema", "PriorityLevelConfiguration", "IngressClass", "IPAddress", "ServiceCIDR", "GatewayClass", "ConstraintTemplate", "CiliumClusterwideNetworkPolicy", "CiliumClusterwideEnvoyConfig", "CiliumNode", "CiliumIdentity", "Agent", "Team", "Sandbox", "AgentPolicy"} {
		clusterKinds[kind] = true
	}
	for _, resource := range cetiadmin.DiagnosticResources() {
		version := "v1"
		if resource.Group == api.GroupVersion.Group {
			version = api.GroupVersion.Version
		}
		gv := schema.GroupVersion{Group: resource.Group, Version: version}
		scope := meta.RESTScopeNamespace
		if clusterKinds[resource.Kind] {
			scope = meta.RESTScopeRoot
		}
		mapper.AddSpecific(gv.WithKind(resource.Kind), gv.WithResource(resource.Resource), gv.WithResource(strings.ToLower(resource.Kind)), scope)
	}
	mapper.AddSpecific(corev1.SchemeGroupVersion.WithKind("Secret"), corev1.SchemeGroupVersion.WithResource("secrets"), corev1.SchemeGroupVersion.WithResource("secret"), meta.RESTScopeNamespace)
	return mapper
}

func (f *cetiAdminFixture) run() error {
	_, err := f.r.Reconcile(f.ctx, ctrl.Request{NamespacedName: f.r.Source})
	return err
}

func (f *cetiAdminFixture) state() *cetiadmin.Installation {
	f.t.Helper()
	_, state, err := f.r.readState(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return state
}

func (f *cetiAdminFixture) converge() {
	f.t.Helper()
	for i := 0; i < 30; i++ {
		if err := f.run(); err != nil {
			f.t.Fatalf("reconcile %d: %v", i, err)
		}
		state := f.state()
		if state.Bootstrapped && state.CanRun() {
			return
		}
	}
	f.t.Fatalf("installation did not converge: phase=%s reason=%s", f.state().Phase, f.state().Reason)
}

func (f *cetiAdminFixture) agent() *api.Agent {
	f.t.Helper()
	agent := &api.Agent{}
	if err := f.kube.Get(f.ctx, client.ObjectKey{Name: f.config.InstanceName}, agent); err != nil {
		f.t.Fatal(err)
	}
	return agent
}

func (f *cetiAdminFixture) updateConfig() {
	f.t.Helper()
	current := &corev1.ConfigMap{}
	if err := f.kube.Get(f.ctx, f.r.Source, current); err != nil {
		f.t.Fatal(err)
	}
	raw, err := json.Marshal(f.config)
	if err != nil {
		f.t.Fatal(err)
	}
	current.Data[cetiadmin.ConfigKey] = string(raw)
	if err := f.kube.Update(f.ctx, current); err != nil {
		f.t.Fatal(err)
	}
	f.source = current
}

func (f *cetiAdminFixture) seed(object client.Object) {
	f.t.Helper()
	if err := f.kube.Create(f.ctx, object); err != nil {
		f.t.Fatal(err)
	}
}

func (f *cetiAdminFixture) noBindings() {
	f.t.Helper()
	roles, cluster := &rbacv1.RoleBindingList{}, &rbacv1.ClusterRoleBindingList{}
	if err := f.kube.List(f.ctx, roles); err != nil {
		f.t.Fatal(err)
	}
	if err := f.kube.List(f.ctx, cluster); err != nil {
		f.t.Fatal(err)
	}
	if len(roles.Items)+len(cluster.Items) != 0 {
		f.t.Fatalf("authority survived: roles=%d cluster=%d", len(roles.Items), len(cluster.Items))
	}
}

func TestCetiAdminInstallationBootstrapAndIdempotence(t *testing.T) {
	f := newCetiAdminFixture(t)
	createdSuspended := false
	f.before = func(operation string, object client.Object) error {
		if operation == "create" {
			if agent, ok := object.(*api.Agent); ok {
				createdSuspended = agent.Spec.Suspended
				state := f.state()
				if state.Creation == nil || state.Creation.Resource.CreationID != agent.Annotations[cetiadmin.AnnotationCreationID] {
					t.Fatal("Agent CREATE preceded its durable intent")
				}
			}
		}
		return nil
	}
	f.converge()
	f.before = nil
	state, agent := f.state(), f.agent()
	if !createdSuspended || agent.Spec.Suspended || !state.Bootstrapped || state.Hold || state.BootstrapReleasePending {
		t.Fatal("bootstrap did not clear exactly its initial suspension")
	}
	if f.helm.installCalled {
		t.Fatal("installer became another runtime release writer")
	}
	if state.ResolvedRuntime.Image != "registry.example.com/hermes-gateway:0.1.1-test.1" || ptr.Deref(agent.Spec.Agent.Image, "") != state.ResolvedRuntime.Image {
		t.Fatal("Hermes image was not pinned with the selected chart")
	}
	for _, ref := range []cetiadmin.ObjectIdentity{state.Agent, state.Team, state.Namespace, state.ServiceAccount} {
		if _, err := uuid.Parse(string(ref.UID)); err != nil {
			t.Fatal("installation did not record actual server UIDs")
		}
	}
	for _, resource := range state.Resources {
		if resource.UID == "" || resource.CreationID == "" {
			t.Fatal("unconfirmed authority object in record")
		}
	}
	if binding, handled, err := f.r.BindingForAgent(f.ctx, agent); err != nil || !handled || binding.ServiceAccountUsername() == "" {
		t.Fatalf("runtime binding: handled=%v err=%v", handled, err)
	}
	if f.r.OperatorImageReference() != f.r.OperatorImage {
		t.Fatal("runtime provider lost the resolved Operator image")
	}
	agent.Status.Phase, agent.Status.ObservedGeneration = api.PhaseReady, agent.Generation
	if err := f.kube.Status().Update(f.ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := f.run(); err != nil || f.state().Phase != cetiadmin.PhaseReady {
		t.Fatal("installation did not observe Ready", err)
	}
	writes := len(f.writes)
	for range 4 {
		if err := f.run(); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.writes) != writes {
		t.Fatalf("unchanged installation keeps writing: %v", f.writes[writes:])
	}
}

func TestCetiAdminInstallationDisabledColdStart(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.config.Enabled = false
	f.updateConfig()
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	agents, teams := &api.AgentList{}, &api.TeamList{}
	if err := f.kube.List(f.ctx, agents); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.List(f.ctx, teams); err != nil {
		t.Fatal(err)
	}
	if len(agents.Items)+len(teams.Items) != 0 {
		t.Fatal("disabled cold install created instances")
	}
	f.noBindings()
}

func TestCetiAdminInstallationPairedUpgradeKeepsUserState(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.converge()
	before := f.agent()
	before.Spec.Suspended = true
	before.Spec.User.DisplayName = "Kept display name"
	before.Spec.User.Email = "owner@example.com"
	before.Spec.Helm.Values["unrelated"] = runtime.RawExtension{Raw: []byte(`{"keep":true}`)}
	before.Spec.ModelAccess.MonthlyBudgetUSD = "22.75"
	before.Annotations["example.com/keep"] = "yes"
	before.Generation++
	if err := f.kube.Update(f.ctx, before); err != nil {
		t.Fatal(err)
	}
	f.config.Model.Default, f.config.SharedSkills.Enabled = "approved-model", true
	f.config.AzureWorkloadIdentity = cetiadmin.AzureWorkloadIdentity{Enabled: true, ClientID: "33333333-3333-3333-3333-333333333333"}
	f.updateConfig()
	f.r.OperatorImage, f.r.ChartVersion = "registry.example.com/operator:0.1.1-test.2", "0.1.1-test.2"
	for range 16 {
		if err := f.run(); err != nil {
			t.Fatal(err)
		}
	}
	after := f.agent()
	if !after.Spec.Suspended || after.UID != before.UID || after.Spec.Helm.Version != "0.1.1-test.2" || ptr.Deref(after.Spec.Agent.Image, "") != "registry.example.com/hermes-gateway:0.1.1-test.2" {
		t.Fatal("upgrade changed user suspension/UID or left stale runtime pins")
	}
	if after.Spec.Model != "approved-model" || string(after.Spec.Helm.Values["skills"].Raw) != `{"shared":true}` || after.Spec.ModelAccess.MonthlyBudgetUSD != "22.75" || after.Spec.User.Email != before.Spec.User.Email || after.Annotations["example.com/keep"] != "yes" || string(after.Spec.Helm.Values["unrelated"].Raw) != `{"keep":true}` {
		t.Fatal("installer did not update its fields while preserving user fields")
	}
	account := &corev1.ServiceAccount{}
	if err := f.kube.Get(f.ctx, client.ObjectKey{Namespace: f.state().ServiceAccount.Namespace, Name: f.state().ServiceAccount.Name}, account); err != nil {
		t.Fatal(err)
	}
	if account.Annotations["azure.workload.identity/client-id"] != f.config.AzureWorkloadIdentity.ClientID {
		t.Fatal("workload identity upgrade was omitted")
	}
	f.noBindings()
	after.Spec.Suspended = false
	after.Generation++
	if err := f.kube.Update(f.ctx, after); err != nil {
		t.Fatal(err)
	}
	f.converge()
	if f.agent().Spec.Suspended {
		t.Fatal("explicit user resume did not recover the retained installation")
	}
}

func TestCetiAdminInstallationLostCreateResponses(t *testing.T) {
	for _, kind := range []string{"Team", "Agent", "Namespace", "ServiceAccount", "ClusterRole", "RoleBinding"} {
		t.Run(kind, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			f.config.PlatformReadNamespaces = []string{f.source.Namespace}
			f.updateConfig()
			lostUID := types.UID("")
			f.after = func(operation string, object client.Object) error {
				if operation == "create" && strings.HasSuffix(fmt.Sprintf("%T", object), "."+kind) && lostUID == "" {
					lostUID = object.GetUID()
					return errors.New("synthetic lost create response")
				}
				return nil
			}
			f.converge()
			if lostUID == "" {
				t.Fatal("lost response fault did not execute")
			}
			state := f.state()
			identities := []types.UID{state.Team.UID, state.Agent.UID, state.Namespace.UID, state.ServiceAccount.UID}
			for _, resource := range state.Resources {
				identities = append(identities, resource.UID)
			}
			if kind == "RoleBinding" {
				bindings := &rbacv1.RoleBindingList{}
				if err := f.kube.List(f.ctx, bindings, client.InNamespace(f.source.Namespace)); err != nil {
					t.Fatal(err)
				}
				for _, binding := range bindings.Items {
					identities = append(identities, binding.UID)
				}
			}
			found := false
			for _, identity := range identities {
				found = found || identity == lostUID
			}
			if !found {
				t.Fatal("retry replaced the successfully created object")
			}
		})
	}
}

func TestCetiAdminInstallationRejectsForeignPreexistingObjects(t *testing.T) {
	for _, kind := range []string{"Team", "Agent", "Namespace", "ServiceAccount"} {
		t.Run(kind, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			for range 2 {
				if err := f.run(); err != nil {
					t.Fatal(err)
				}
			}
			for _, preceding := range []string{"Team", "Agent", "Namespace"} {
				if preceding == kind {
					break
				}
				if err := f.run(); err != nil {
					t.Fatal(err)
				}
			}
			state := f.state()
			config := f.config
			config.Runtime = state.ResolvedRuntime
			var foreign client.Object
			switch kind {
			case "Team":
				foreign, _ = desiredCetiAdminTeam(config, f.source)
			case "Agent":
				foreign, _ = desiredCetiAdminAgent(config, f.source, f.r.ChartVersion)
			case "Namespace":
				foreign = &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instanceNamespace(config.InstanceName)}}
			case "ServiceAccount":
				foreign = &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(config.InstanceName), Namespace: instanceNamespace(config.InstanceName)}, AutomountServiceAccountToken: ptr.To(false)}
			}
			f.seed(foreign)
			uid := foreign.GetUID()
			if err := f.run(); err == nil {
				t.Fatal("foreign same-name object was adopted")
			}
			if err := f.kube.Get(f.ctx, client.ObjectKeyFromObject(foreign), foreign); err != nil || foreign.GetUID() != uid {
				t.Fatal("foreign same-name object was removed or replaced")
			}
			f.noBindings()
		})
	}
}

func TestCetiAdminInstallationMissingDependenciesWithdrawAuthority(t *testing.T) {
	for _, item := range []struct {
		name            string
		breakDependency func(*cetiAdminFixture)
	}{
		{"admission callback", func(f *cetiAdminFixture) { f.r.AdmissionReady = nil }},
		{"admission outage", func(f *cetiAdminFixture) {
			f.r.AdmissionReady = func(context.Context) error { return errors.New("admission unavailable") }
		}},
		{"grant callback", func(f *cetiAdminFixture) { f.r.GrantAccess = nil }},
		{"revoke callback", func(f *cetiAdminFixture) { f.r.RevokeAccess = nil }},
		{"runtime", func(f *cetiAdminFixture) { f.r.Runtime = nil }},
		{"Helm preflight", func(f *cetiAdminFixture) { f.r.Runtime.HelmClient = nil }},
		{"operator image", func(f *cetiAdminFixture) { f.r.OperatorImage = "" }},
	} {
		t.Run(item.name, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			f.converge()
			item.breakDependency(f)
			if err := f.run(); err == nil {
				t.Fatal("missing dependency was treated as success")
			}
			f.noBindings()
			if f.agent().Spec.Suspended {
				t.Fatal("component failure overwrote user suspension")
			}
			if f.state().CanRun() {
				t.Fatal("component failure left a runnable installation")
			}
		})
	}
}

func TestCetiAdminInstallationSourceAndRecordFailureRetainsExactHold(t *testing.T) {
	for _, failure := range []string{"source-read", "source-invalid", "record-read", "record-invalid"} {
		t.Run(failure, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			f.converge()
			agent := f.agent()
			switch failure {
			case "source-read", "record-read":
				key := f.r.Source
				if failure == "record-read" {
					key = f.r.stateKey()
				}
				f.getError = func(got client.ObjectKey, _ client.Object) error {
					if got == key {
						return errors.New("synthetic read unavailable")
					}
					return nil
				}
			case "source-invalid", "record-invalid":
				key, field := f.r.Source, cetiadmin.ConfigKey
				if failure == "record-invalid" {
					key, field = f.r.stateKey(), cetiadmin.BindingKey
				}
				object := &corev1.ConfigMap{}
				if err := f.kube.Get(f.ctx, key, object); err != nil {
					t.Fatal(err)
				}
				object.Data[field] = `{"unexpected":true}`
				if err := f.kube.Update(f.ctx, object); err != nil {
					t.Fatal(err)
				}
			}
			if _, handled, err := f.r.BindingForAgent(f.ctx, agent); !handled || err == nil {
				t.Fatal("failed source/record lost the previously managed Agent UID")
			}
			imitation := agent.DeepCopy()
			imitation.UID = types.UID(uuid.NewString())
			if _, handled, err := f.r.BindingForAgent(f.ctx, imitation); handled || err != nil {
				t.Fatal("source marker or same name held an unrelated Agent")
			}
			if err := f.run(); err == nil {
				t.Fatal("failed source/record was treated as healthy")
			}
			f.getError = nil
			f.noBindings()
			current := f.agent()
			if current.UID != agent.UID || current.Spec.Suspended {
				t.Fatal("failure changed retained identity or user suspension")
			}
		})
	}
}

func TestCetiAdminInstallationReinstallRequiresRecordedUID(t *testing.T) {
	for _, recovery := range []string{"missing", "wrong", "matching"} {
		t.Run(recovery, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			f.converge()
			original := f.state()
			config := f.config
			if recovery == "matching" {
				config.RetainedAgentUID = string(original.Agent.UID)
			}
			if recovery == "wrong" {
				config.RetainedAgentUID = "foreign-agent-uid"
			}
			source := &corev1.ConfigMap{}
			if err := f.kube.Get(f.ctx, f.r.Source, source); err != nil {
				t.Fatal(err)
			}
			if err := f.kube.Delete(f.ctx, source, client.Preconditions{UID: &source.UID, ResourceVersion: &source.ResourceVersion}); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(config)
			f.source = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: source.Name, Namespace: source.Namespace}, Data: map[string]string{cetiadmin.ConfigKey: string(raw)}}
			f.seed(f.source)
			if recovery != "matching" {
				if err := f.run(); err == nil {
					t.Fatal("replacement source adopted an Agent by name")
				}
				f.noBindings()
				if f.state().Source.UID != original.Source.UID {
					t.Fatal("recorded source was overwritten")
				}
				return
			}
			f.config = config
			f.converge()
			current := f.state()
			if current.Source.UID != f.source.UID || current.Agent != original.Agent || current.Team != original.Team || current.Namespace != original.Namespace || current.ServiceAccount != original.ServiceAccount || current.PreviousSource != nil {
				t.Fatal("reinstall did not preserve the recorded identities")
			}
			if f.agent().Spec.Suspended {
				t.Fatal("reinstall overwrote the user's runtime intent")
			}
		})
	}
}

func TestCetiAdminInstallationWaitsForObservedWriterStop(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.converge()
	agent, state := f.agent(), f.state()
	workload := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(agent.Name), Namespace: state.Namespace.Name,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID}}},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))}, Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: 1, ReadyReplicas: 1}}
	f.seed(workload)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "runtime-0", Namespace: state.Namespace.Name}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	f.seed(pod)
	home := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "home", Namespace: state.Namespace.Name}}
	f.seed(home)
	homeUID := home.UID
	f.config.Enabled = false
	f.updateConfig()
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	f.noBindings()
	if f.state().Phase != cetiadmin.PhaseStopping {
		t.Fatal("zero desired replicas was reported as stopped")
	}
	if err := f.kube.Get(f.ctx, client.ObjectKeyFromObject(workload), workload); err != nil {
		t.Fatal(err)
	}
	if ptr.Deref(workload.Spec.Replicas, 1) != 0 || workload.Status.Replicas == 0 {
		t.Fatal("stop test did not retain a live writer observation")
	}
	workload.Status = appsv1.StatefulSetStatus{ObservedGeneration: workload.Generation}
	if err := f.kube.Status().Update(f.ctx, workload); err != nil {
		t.Fatal(err)
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if f.state().Phase != cetiadmin.PhaseStopping {
		t.Fatal("live Pod was ignored after workload acknowledgement")
	}
	pod.Status.Phase = corev1.PodSucceeded
	if err := f.kube.Status().Update(f.ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if f.state().Phase != cetiadmin.PhaseDisabled {
		t.Fatal("confirmed stop did not finish")
	}
	if err := f.kube.Get(f.ctx, client.ObjectKeyFromObject(home), home); err != nil || home.UID != homeUID {
		t.Fatal("stop removed HOME")
	}
	if f.agent().Spec.Suspended {
		t.Fatal("component stop changed the user's spec.suspended")
	}
	f.config.Enabled = true
	f.updateConfig()
	f.converge()
	if f.agent().UID != agent.UID {
		t.Fatal("reenable replaced the retained Agent")
	}
}

func TestCetiAdminInstallationCheckpointFailureWithdrawsGrant(t *testing.T) {
	f := newCetiAdminFixture(t)
	failed := false
	f.before = func(operation string, object client.Object) error {
		if operation != "patch" || client.ObjectKeyFromObject(object) != f.r.stateKey() {
			return nil
		}
		candidate, err := cetiadmin.ParseInstallation(object.(*corev1.ConfigMap).Data[cetiadmin.BindingKey])
		if err == nil && candidate.CanRun() {
			failed = true
			return errors.New("synthetic activation checkpoint failure")
		}
		return nil
	}
	var err error
	for range 15 {
		if err = f.run(); err != nil {
			break
		}
	}
	if err == nil || !failed {
		t.Fatal("checkpoint fault did not execute")
	}
	f.noBindings()
	agentUID := f.agent().UID
	if !f.agent().Spec.Suspended || f.state().CanRun() {
		t.Fatal("failed activation released the bootstrap suspension")
	}
	f.before = nil
	f.converge()
	if f.agent().UID != agentUID {
		t.Fatal("checkpoint retry recreated Agent")
	}
}

func TestCetiAdminInstallationFailedHoldSaveStillRevokes(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.converge()
	f.before = func(operation string, object client.Object) error {
		if operation == "patch" && client.ObjectKeyFromObject(object) == f.r.stateKey() {
			return errors.New("checkpoint storage unavailable")
		}
		return nil
	}
	f.config.Enabled = false
	f.updateConfig()
	if err := f.run(); err == nil {
		t.Fatal("save failure was ignored")
	}
	f.noBindings()
	if _, handled, err := f.r.BindingForAgent(f.ctx, f.agent()); !handled || err == nil {
		t.Fatal("historical Active flag allowed execution without current source/grants")
	}
	f.before = nil
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if f.state().Phase != cetiadmin.PhaseDisabled {
		t.Fatal("save recovery did not finish the hold")
	}
}

func TestCetiAdminInstallationLostCheckpointRepliesRecoverByReadback(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.after = func(operation string, object client.Object) error {
		if operation == "patch" && client.ObjectKeyFromObject(object) == f.r.stateKey() {
			return errors.New("synthetic lost checkpoint response")
		}
		return nil
	}
	f.converge()
	if !f.state().CanRun() {
		t.Fatal("acknowledged state readback did not recover")
	}
}

func TestCetiAdminBindingRereadsCurrentObjects(t *testing.T) {
	for _, drift := range []string{"source", "namespace-owner", "serviceaccount", "runtime", "model", "team"} {
		t.Run(drift, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			f.converge()
			agent, state := f.agent(), f.state()
			switch drift {
			case "source":
				f.config.Model.Default = "another-model"
				f.updateConfig()
			case "namespace-owner":
				namespace := &corev1.Namespace{}
				if err := f.kube.Get(f.ctx, client.ObjectKey{Name: state.Namespace.Name}, namespace); err != nil {
					t.Fatal(err)
				}
				namespace.OwnerReferences = nil
				if err := f.kube.Update(f.ctx, namespace); err != nil {
					t.Fatal(err)
				}
			case "serviceaccount":
				account := &corev1.ServiceAccount{}
				if err := f.kube.Get(f.ctx, client.ObjectKey{Name: state.ServiceAccount.Name, Namespace: state.ServiceAccount.Namespace}, account); err != nil {
					t.Fatal(err)
				}
				account.AutomountServiceAccountToken = ptr.To(true)
				if err := f.kube.Update(f.ctx, account); err != nil {
					t.Fatal(err)
				}
			case "team":
				team := &api.Team{}
				if err := f.kube.Get(f.ctx, client.ObjectKey{Name: state.Team.Name}, team); err != nil {
					t.Fatal(err)
				}
				team.Spec.AgentPolicyRef = "policy"
				if err := f.kube.Update(f.ctx, team); err != nil {
					t.Fatal(err)
				}
			default:
				updated := agent.DeepCopy()
				if drift == "runtime" {
					updated.Spec.Agent.Image = ptr.To("registry.example.com/unqualified:other")
				} else {
					updated.Spec.Model = "changed-model"
				}
				updated.Generation++
				if err := f.kube.Update(f.ctx, updated); err != nil {
					t.Fatal(err)
				}
			}
			if !f.state().CanRun() {
				t.Fatal("test did not retain stale Active state")
			}
			if _, handled, err := f.r.BindingForAgent(f.ctx, agent); !handled || err == nil {
				t.Fatal("stale Active flag was treated as identity proof")
			}
		})
	}
}

func TestCetiAdminInstallationPreservesCheckpointMetadata(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.converge()
	object, state, err := f.r.readState(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	object.Data["unrelated"] = "keep"
	object.Annotations["example.com/keep"] = "yes"
	if err := f.kube.Update(f.ctx, object); err != nil {
		t.Fatal(err)
	}
	state.Reason = "SyntheticCheckpoint"
	if err := f.r.saveState(f.ctx, object, state); err != nil {
		t.Fatal(err)
	}
	current := &corev1.ConfigMap{}
	if err := f.kube.Get(f.ctx, f.r.stateKey(), current); err != nil {
		t.Fatal(err)
	}
	if current.Data["unrelated"] != "keep" || current.Annotations["example.com/keep"] != "yes" {
		t.Fatal("checkpoint save cleared unrelated fields")
	}
}

func TestCetiAdminInstallationRestartRecoversUnobservedCreate(t *testing.T) {
	f := newCetiAdminFixture(t)
	createdUID := types.UID("")
	blocked := false
	f.after = func(operation string, object client.Object) error {
		if operation == "create" {
			if agent, ok := object.(*api.Agent); ok {
				createdUID, blocked = agent.UID, true
				return errors.New("synthetic lost Agent create reply")
			}
		}
		return nil
	}
	f.getError = func(key client.ObjectKey, object client.Object) error {
		if _, ok := object.(*api.Agent); ok && blocked && key.Name == f.config.InstanceName {
			return errors.New("create outcome not yet observable")
		}
		return nil
	}
	var err error
	for range 10 {
		if err = f.run(); err != nil {
			break
		}
	}
	if err == nil || createdUID == "" || f.state().Creation == nil {
		t.Fatal("unobserved CREATE was not durably retained")
	}
	intent := f.state().Creation.Resource.CreationID
	blocked, f.after = false, nil
	restarted := *f.r
	f.r = &restarted
	f.r.GrantAccess, f.r.RevokeAccess = f.r.GrantInstallationAccess, f.r.RevokeInstallationAccess
	f.converge()
	if f.agent().UID != createdUID || f.agent().Annotations[cetiadmin.AnnotationCreationID] != intent {
		t.Fatal("restart adopted or recreated a different Agent")
	}
}

func TestCetiAdminMigrationRequiresRetainedStoppedSource(t *testing.T) {
	for _, failure := range []string{"running", "wrong-agent-uid", "wrong-namespace-uid", "pod-running", "stale-workload-stop", "stopped"} {
		t.Run(failure, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			old, namespace := f.target("previous-admin", "previous-team", api.AgentTypeOpenClaw, true)
			old.Spec.Suspended = failure != "running"
			if err := f.kube.Update(f.ctx, old); err != nil {
				t.Fatal(err)
			}
			old.Status.Phase = api.PhaseSuspended
			if err := f.kube.Status().Update(f.ctx, old); err != nil {
				t.Fatal(err)
			}
			f.config.MigrationSource = &cetiadmin.MigrationSource{Agent: identityOf(old), Namespace: identityOf(namespace)}
			switch failure {
			case "wrong-agent-uid":
				f.config.MigrationSource.Agent.UID = "previous-agent-replaced"
			case "wrong-namespace-uid":
				f.config.MigrationSource.Namespace.UID = "previous-namespace-replaced"
			case "pod-running":
				f.seed(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "old-writer", Namespace: namespace.Name}, Status: corev1.PodStatus{Phase: corev1.PodRunning}})
			case "stale-workload-stop":
				f.seed(&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "old-runtime", Namespace: namespace.Name}, Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(0))}, Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: 1}})
			}
			f.updateConfig()
			if failure == "stopped" {
				f.converge()
			} else {
				var err error
				for range 10 {
					if err = f.run(); err != nil {
						break
					}
				}
				if err == nil {
					t.Fatal("unverified source allowed activation")
				}
				f.noBindings()
			}
			current := &api.Agent{}
			if err := f.kube.Get(f.ctx, client.ObjectKeyFromObject(old), current); err != nil || current.UID != old.UID || !reflect.DeepEqual(current.Spec, old.Spec) {
				t.Fatal("migration modified or deleted the source Agent")
			}
		})
	}
}

func TestCetiAdminInstallationPreservesExternalKeyBlock(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.converge()
	agent, state := f.agent(), f.state()
	value := "sk-" + strings.Repeat("x", 43)
	token := aigateway.Token(value)
	gateway := newFakeAIGateway()
	gateway.keys[token] = &aigateway.Key{Alias: aiGatewayKeyAlias(agent), Blocked: true, MaxBudget: ptr.To(55.25), BudgetDuration: "1mo"}
	f.r.Runtime.aiGatewayClientFactory = func(context.Context) (aiGatewayKeyClient, error) { return gateway, nil }
	key := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: aiGatewayKeySecretName, Namespace: state.Namespace.Name, Labels: map[string]string{aiGatewayAgentUIDLabel: string(agent.UID)}}, Data: map[string][]byte{aiGatewayKeySecretField: []byte(value)}}
	f.seed(key)
	agent.Status.AIGateway = &api.AIGatewayStatus{KeyID: "synthetic-issued-key", Blocked: true, Reason: "AdministratorBlocked", BudgetUSD: "55.25", SpendUSD: "12.50"}
	if err := f.kube.Status().Update(f.ctx, agent); err != nil {
		t.Fatal(err)
	}
	f.config.Enabled = false
	f.updateConfig()
	if err := f.run(); err != nil && !strings.Contains(err.Error(), "blocked outside a CetiRun policy transition") {
		t.Fatal(err)
	}
	f.noBindings()
	if !gateway.keys[token].Blocked || ptr.Deref(gateway.keys[token].MaxBudget, 0) != 55.25 || len(gateway.generated) != 0 || len(gateway.updates) != 0 || len(gateway.deletedTokens) != 0 {
		t.Fatal("installation reset or unblocked the external key")
	}
	current := f.agent()
	if current.Spec.Suspended || current.Status.AIGateway.Reason != "AdministratorBlocked" || current.Status.AIGateway.SpendUSD != "12.50" {
		t.Fatal("component adopted the external block or changed user state")
	}
	if current.Status.Phase != api.PhaseSuspended || !f.state().Hold {
		t.Fatal("external block prevented the independent compute hold")
	}
}

func TestCetiAdminInstallationWithdrawFailureStillRequestsComputeStop(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.converge()
	agent, state := f.agent(), f.state()
	workload := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(agent.Name), Namespace: state.Namespace.Name,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID}}}, Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))}}
	f.seed(workload)
	attempted := false
	f.before = func(operation string, object client.Object) error {
		if operation == "delete" {
			if _, ok := object.(*rbacv1.ClusterRoleBinding); ok {
				attempted = true
				return errors.New("synthetic binding deletion outage")
			}
		}
		if operation == "patch" {
			if _, ok := object.(*appsv1.StatefulSet); ok && !attempted {
				t.Fatal("compute stop preceded withdrawal attempt")
			}
		}
		return nil
	}
	f.config.Enabled = false
	f.updateConfig()
	if err := f.run(); err == nil || !attempted {
		t.Fatal("revocation failure was hidden")
	}
	if err := f.kube.Get(f.ctx, client.ObjectKeyFromObject(workload), workload); err != nil {
		t.Fatal(err)
	}
	if ptr.Deref(workload.Spec.Replicas, 1) != 0 || f.state().Phase == cetiadmin.PhaseDisabled || !f.state().Hold {
		t.Fatal("failed withdrawal either left compute running or falsely completed disable")
	}
}

func TestCetiAdminInstallationRejectsUnsupportedOrUnpairedRuntime(t *testing.T) {
	for _, failure := range []string{"unsupported-chart", "unqualified-image", "preflight-unavailable"} {
		t.Run(failure, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			switch failure {
			case "unsupported-chart":
				f.helm.unsupported = true
			case "unqualified-image":
				f.config.Runtime.Image = "registry.example.com/hermes-gateway:unqualified"
				f.updateConfig()
			case "preflight-unavailable":
				f.helm.err = errors.New("synthetic chart preflight unavailable")
			}
			var err error
			for range 10 {
				if err = f.run(); err != nil {
					break
				}
			}
			if err == nil {
				t.Fatal("unsupported runtime bypassed paired profile preflight")
			}
			f.noBindings()
			agents := &api.AgentList{}
			if err := f.kube.List(f.ctx, agents); err != nil {
				t.Fatal(err)
			}
			if len(agents.Items) != 0 {
				t.Fatal("installer created an unresolved runtime")
			}
		})
	}
}

func TestCetiAdminBindingRejectsCurrentUserPauseBeforeNextReconcile(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.converge()
	agent := f.agent()
	agent.Spec.Suspended = true
	agent.Generation++
	if err := f.kube.Update(f.ctx, agent); err != nil {
		t.Fatal(err)
	}
	if !f.state().CanRun() {
		t.Fatal("fixture did not leave a historical Active flag")
	}
	if _, handled, err := f.r.BindingForAgent(f.ctx, agent); !handled || err == nil {
		t.Fatal("user pause retained a runnable binding")
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	writes := len(f.writes)
	for range 3 {
		if err := f.run(); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.writes) != writes {
		t.Fatal("paused installation keeps cycling through installation/stopping")
	}
}

func TestCetiAdminInstallationNeverAdoptsReplacedIdentities(t *testing.T) {
	for _, kind := range []string{"Agent", "Namespace", "ServiceAccount"} {
		t.Run(kind, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			f.converge()
			state := f.state()
			ref := state.Agent
			if kind == "Namespace" {
				ref = state.Namespace
			}
			if kind == "ServiceAccount" {
				ref = state.ServiceAccount
			}
			old, _, _ := installationObject(kind)
			key := client.ObjectKey{Name: ref.Name, Namespace: ref.Namespace}
			if err := f.kube.Get(f.ctx, key, old); err != nil {
				t.Fatal(err)
			}
			version := old.GetResourceVersion()
			if err := f.kube.Delete(f.ctx, old, client.Preconditions{UID: &ref.UID, ResourceVersion: &version}); err != nil {
				t.Fatal(err)
			}
			replacement := old.DeepCopyObject().(client.Object)
			replacement.SetUID("")
			replacement.SetResourceVersion("")
			f.seed(replacement)
			if kind == "Agent" {
				if _, handled, err := f.r.BindingForAgent(f.ctx, replacement.(*api.Agent)); handled || err != nil {
					t.Fatal("same-name replacement was classified as the original Agent")
				}
			}
			if err := f.run(); err == nil {
				t.Fatal("replacement inherited installation authority")
			}
			f.noBindings()
			current, _, _ := installationObject(kind)
			if err := f.kube.Get(f.ctx, key, current); err != nil || current.GetUID() != replacement.GetUID() {
				t.Fatal("replacement was deleted or overwritten")
			}
			if f.state().Agent != state.Agent || f.state().Namespace != state.Namespace || f.state().ServiceAccount != state.ServiceAccount {
				t.Fatal("recorded identities were replaced by names")
			}
		})
	}
}

func TestCetiAdminInstallationRetainedReinstallResumesPartialBootstrap(t *testing.T) {
	f := newCetiAdminFixture(t)
	for range 4 {
		if err := f.run(); err != nil {
			t.Fatal(err)
		}
	}
	state := f.state()
	if state.Agent.UID == "" || state.Namespace.UID != "" || state.ServiceAccount.UID != "" {
		t.Fatal("fixture is not interrupted between Agent and namespace creation")
	}
	source := &corev1.ConfigMap{}
	if err := f.kube.Get(f.ctx, f.r.Source, source); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(f.ctx, source, client.Preconditions{UID: &source.UID, ResourceVersion: &source.ResourceVersion}); err != nil {
		t.Fatal(err)
	}
	f.config.RetainedAgentUID = string(state.Agent.UID)
	raw, _ := json.Marshal(f.config)
	f.source = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: source.Name, Namespace: source.Namespace}, Data: map[string]string{cetiadmin.ConfigKey: string(raw)}}
	f.seed(f.source)
	f.converge()
	if f.state().Agent != state.Agent || f.state().Team != state.Team || f.state().Namespace.UID == "" || f.state().ServiceAccount.UID == "" {
		t.Fatal("partial reinstall failed to retain recorded objects and create only missing objects")
	}
}
