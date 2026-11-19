// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	sandboxadmission "github.com/Azure/CetiRun/app/cetirun-operator/internal/admission"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/agents"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type preparationSource struct {
	inputs     controller.SandboxPrerequisites
	resolveErr error
	fenceErr   error
	fences     int
	resolves   int
	onFence    func()
	onResolve  func()
	lastFenced controller.SandboxPrerequisites
}

func (s *preparationSource) Resolve(context.Context, *api.Agent, *api.Team) (controller.SandboxPrerequisites, error) {
	s.resolves++
	if s.onResolve != nil {
		s.onResolve()
	}
	return s.inputs, s.resolveErr
}

func (s *preparationSource) CheckQuiesced(_ context.Context, _ *api.Agent, prerequisites controller.SandboxPrerequisites) error {
	s.fences++
	s.lastFenced = prerequisites
	if s.onFence != nil {
		s.onFence()
	}
	return s.fenceErr
}

type preparationInstaller struct {
	client      client.Client
	chart       *chart.Chart
	stages      int
	renders     int
	digestReads int
	stageDigest string
	digestErr   error
	fail        error
	after       func(context.Context) error
	prepared    *helm.PreparedRelease
}

func (i *preparationInstaller) PrepareSandbox(_ string, platform, user map[string]any, version string, bundle *sandboxworkload.Bundle) (*helm.PreparedRelease, error) {
	i.renders++
	if version != i.chart.Metadata.Version {
		return nil, fmt.Errorf("test chart version differs")
	}
	if bundle.Identity().AgentType == "hermes" {
		return helm.PrepareSandboxChartWithHarness(i.chart, platform, user, bundle, legacyHeldHermesHarness{})
	}
	return helm.PrepareSandboxChart(i.chart, platform, user, bundle)
}

func (i *preparationInstaller) StageSandbox(ctx context.Context, _, _ string, prepared *helm.PreparedRelease, check func(context.Context) error) error {
	if err := check(ctx); err != nil {
		return err
	}
	i.stages++
	i.prepared = prepared
	manifest, err := prepared.SandboxManifest()
	if err != nil {
		return err
	}
	decoder := kubeyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(object); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		if len(object.Object) == 0 {
			continue
		}
		if object.GetKind() == "StatefulSet" {
			if count, _, _ := unstructured.NestedInt64(object.Object, "spec", "replicas"); count != 0 {
				return fmt.Errorf("test installer refuses execution")
			}
		}
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(object.GroupVersionKind())
		err := i.client.Get(ctx, client.ObjectKeyFromObject(object), current)
		if apierrors.IsNotFound(err) {
			if err := i.client.Create(ctx, object); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			object.SetUID(current.GetUID())
			object.SetResourceVersion(current.GetResourceVersion())
			if err := i.client.Update(ctx, object); err != nil {
				return err
			}
		}
	}
	if i.fail != nil {
		return i.fail
	}
	if i.after != nil {
		if err := i.after(ctx); err != nil {
			return err
		}
	}
	if err := check(ctx); err != nil {
		return err
	}
	i.stageDigest = fmt.Sprintf("%x", sha256.Sum256(manifest))
	return nil
}

func (i *preparationInstaller) SandboxStageDigest(ctx context.Context, _, _ string) (string, error) {
	i.digestReads++
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if i.digestErr != nil {
		return "", i.digestErr
	}
	if i.stageDigest == "" {
		return "", fmt.Errorf("synthetic held release is unavailable")
	}
	return i.stageDigest, nil
}

type preparationFixture struct {
	fixture    testfixture.Fixture
	client     client.Client
	source     *preparationSource
	installer  *preparationInstaller
	config     controller.SandboxConfigurationSnapshot
	reconciler *controller.AgentReconciler
	now        time.Time
}

const legacyHeldChartSHA256 = "b4916f6e804d0d56cc137799934fce17eebc6b70b87ef0d38680a319f39a79e2"

// legacyHeldHermesHarness matches the harness at the archived chart's cfdf7c55 revision.
type legacyHeldHermesHarness struct{}

func (legacyHeldHermesHarness) Describe() agents.Descriptor {
	return agents.Descriptor{
		Type: "hermes", NativeVersion: "0.21.5", ConfigFile: "hermes-config.yaml", PortName: "api",
		DefaultPort: 8642, HealthPath: "/health", ReadinessPath: "/health", StartupPath: "/health",
		NativeSecretKey: "API_SERVER_KEY", EnvironmentFile: "hermes.env",
	}
}

func (h legacyHeldHermesHarness) Validate(agent agents.ResolvedAgent, bindings agents.Bindings) error {
	bindings.ReservedPorts = append(slices.Clone(bindings.ReservedPorts), agents.PortBinding{Name: "dashboard", Number: 9119})
	return agents.Validate(agent, bindings, h.Describe())
}

func (h legacyHeldHermesHarness) Build(agent agents.ResolvedAgent, bindings agents.Bindings) (agents.Contribution, error) {
	if err := h.Validate(agent, bindings); err != nil {
		return agents.Contribution{}, err
	}
	contribution, err := agents.Build(agent, bindings, h.Describe())
	if err != nil {
		return agents.Contribution{}, err
	}
	contribution.Ports = slices.Insert(contribution.Ports, 1, agents.PortBinding{Name: "dashboard", Number: 9119})
	return contribution, nil
}

// legacyHeldChart is used only by the deprecated held-runtime preparation
// suite. Reconciled and common-executor fixtures continue to load the working
// chart from the repository.
func legacyHeldChart(t *testing.T) *chart.Chart {
	t.Helper()
	archive, err := os.ReadFile(filepath.Join("testdata", "legacy-held-chart-cfdf7c55.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(archive)) != legacyHeldChartSHA256 {
		t.Fatal("legacy held chart checksum mismatch")
	}
	loaded, err := loader.LoadArchive(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func newPreparationFixture(t *testing.T, typ string) *preparationFixture {
	t.Helper()
	f := testfixture.New(typ)
	chrt := legacyHeldChart(t)
	scheme := testScheme()
	cilium := schema.GroupVersion{Group: "cilium.io", Version: "v2"}
	scheme.AddKnownTypeWithName(cilium.WithKind("CiliumNetworkPolicy"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(cilium.WithKind("CiliumNetworkPolicyList"), &unstructured.UnstructuredList{})
	state := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-system", UID: "state-uid"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: f.Agent.Spec.GatewayTokenSecretRef, Namespace: f.Namespace.Name, UID: "secret-uid", ResourceVersion: "1",
	}}
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{Name: "example-csi", UID: "storage-uid", ResourceVersion: "1"}, Provisioner: "csi.example.com",
	}
	var creates int
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&api.Agent{}, &api.Sandbox{}, &api.AgentPolicy{}, &appsv1.StatefulSet{}).
		WithObjects(f.Agent, f.Team, f.Sandbox, f.Policy, f.Namespace, f.Input.Home, f.Input.Skills,
			f.Input.RuntimeClass, f.ServiceAccount, secret, sc, state).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			creates++
			if object.GetUID() == "" {
				object.SetUID(types.UID(fmt.Sprintf("created-%d", creates)))
			}
			return c.Create(ctx, object, opts...)
		}}).Build()
	_, config, _ := sandboxConfigurationFixture()
	config.Registrations[api.SandboxKata] = sandbox.Registration{Capabilities: f.Input.Resolution.Capabilities, Available: true}
	config.RuntimeClasses = map[string]controller.SandboxRuntimePrerequisite{
		f.Input.RuntimeClass.Name: {UID: f.Input.RuntimeClass.UID, Handler: f.Input.RuntimeClass.Handler},
	}
	config.StorageClasses = map[string]controller.SandboxStoragePrerequisite{sc.Name: {UID: sc.UID, Provisioner: sc.Provisioner}}
	now := time.Now().UTC()
	config.ObservedAt = now
	source := &preparationSource{inputs: controller.SandboxPrerequisites{
		AgentUID: f.Agent.UID, AgentGeneration: f.Agent.Generation, Revision: "synthetic-prerequisites",
		ObservedAt: now, NamespaceUID: f.Namespace.UID, ChartVersion: chrt.Metadata.Version,
		Home:          sandboxworkload.ObjectIdentity{Name: f.Input.Home.Name, UID: f.Input.Home.UID},
		Skills:        sandboxworkload.ObjectIdentity{Name: f.Input.Skills.Name, UID: f.Input.Skills.UID},
		RuntimeSecret: sandboxworkload.ObjectIdentity{Name: secret.Name, UID: secret.UID}, Helpers: f.Input.Helpers,
	}}
	installer := &preparationInstaller{client: c, chart: chrt}
	result := &preparationFixture{fixture: f, client: c, source: source, installer: installer, config: config, now: now}
	result.reconciler = &controller.AgentReconciler{
		Client: c, APIReader: c, Scheme: scheme, OperatorNamespace: state.Name,
		OperatorSAName: "cetirun-operator", DomainSuffix: "example.com", DefaultModel: "example-model",
		CetiRunHelmValues: map[string]any{"logger": map[string]any{
			"image": map[string]any{"repository": "example.com/logger"},
		}},
		SandboxPreparation: &controller.SandboxPreparation{
			Configuration: controller.SandboxConfigurationSourceFunc(func(context.Context) (controller.SandboxConfigurationSnapshot, error) { return result.config, nil }),
			Prerequisites: source, Installer: installer, AdmissionServiceAccount: "cetirun-sandbox-admission",
			Now: func() time.Time { return result.now },
		},
	}
	return result
}

func (f *preparationFixture) reconcile() error {
	_, err := f.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Name: f.fixture.Agent.Name}})
	return err
}

func (f *preparationFixture) current(t *testing.T) *api.Agent {
	t.Helper()
	result := &api.Agent{}
	if err := f.client.Get(context.Background(), client.ObjectKey{Name: f.fixture.Agent.Name}, result); err != nil {
		t.Fatal(err)
	}
	return result
}

func (f *preparationFixture) expectation(t *testing.T) sandboxworkload.Expectation {
	t.Helper()
	object := &corev1.ConfigMap{}
	if err := f.client.Get(context.Background(), client.ObjectKey{
		Namespace: "cetirun-system", Name: sandboxadmission.PodExpectationName(f.fixture.Namespace.Name, f.fixture.Namespace.UID),
	}, object); err != nil {
		t.Fatal(err)
	}
	var expected sandboxworkload.Expectation
	if err := json.Unmarshal([]byte(object.Data[sandboxadmission.PodExpectationKey]), &expected); err != nil {
		t.Fatal(err)
	}
	return expected
}

func TestSandboxPreparationNativeHeldPipeline(t *testing.T) {
	for _, typ := range []string{"openclaw", "hermes"} {
		t.Run(typ, func(t *testing.T) {
			f := newPreparationFixture(t, typ)
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			assertSandboxPrepared(t, f)
		})
	}
}

func TestSandboxPreparationClearsAccessBeforePrerequisiteCalls(t *testing.T) {
	f := newPreparationFixture(t, "hermes")
	agent := f.current(t)
	agent.Status.Phase, agent.Status.AccessURL = api.PhaseReady, "https://example.example.com"
	apimeta.SetStatusCondition(&agent.Status.Conditions, metav1.Condition{
		Type: api.ConditionReady, Status: metav1.ConditionTrue, Reason: "PreviousObservation",
		ObservedGeneration: agent.Generation, Message: "Synthetic previous readiness",
	})
	if err := f.client.Status().Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	called := false
	f.source.onResolve = func() {
		called = true
		current := f.current(t)
		if current.Status.AccessURL != "" || apimeta.IsStatusConditionTrue(current.Status.Conditions, api.ConditionReady) {
			t.Fatal("old readiness/access was retained while waiting for platform prerequisites")
		}
	}
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("prerequisite provider was not exercised")
	}
}

func TestSandboxPreparationJournalFailurePrecedesGrantWrites(t *testing.T) {
	f := newPreparationFixture(t, "openclaw")
	f.reconciler.Client = interceptor.NewClient(f.client.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if _, ok := object.(*corev1.ConfigMap); ok && object.GetName() == sandboxpolicy.GrantJournalName(f.fixture.Agent.UID) {
				return fmt.Errorf("synthetic durable journal failure")
			}
			return c.Create(ctx, object, options...)
		},
	})
	if err := f.reconcile(); err == nil || f.installer.stages != 0 {
		t.Fatal("journal failure was ignored before staging")
	}
	var bindings rbacv1.RoleBindingList
	if err := f.client.List(context.Background(), &bindings); err != nil {
		t.Fatal(err)
	}
	if len(bindings.Items) != 0 {
		t.Fatal("a binding was written before write-ahead inventory")
	}
}

func TestSandboxPreparationForeignSourceRoleIsNotAdopted(t *testing.T) {
	f := newPreparationFixture(t, "hermes")
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "cetirun-sandbox-sources", Namespace: f.fixture.Namespace.Name},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get"}}},
	}
	if err := f.client.Create(context.Background(), role); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err == nil || f.installer.stages != 0 {
		t.Fatal("foreign source access was adopted")
	}
	current := &rbacv1.Role{}
	if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(role), current); err != nil {
		t.Fatal(err)
	}
	if current.ResourceVersion != role.ResourceVersion || len(current.OwnerReferences) != 0 {
		t.Fatal("foreign source-read Role was modified")
	}
}

func TestSandboxPreparationMissingAndStaleInputsStayHeld(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *preparationFixture)
		status metav1.ConditionStatus
	}{
		{"no configuration source", func(_ *testing.T, f *preparationFixture) { f.reconciler.SandboxPreparation.Configuration = nil }, metav1.ConditionUnknown},
		{"no prerequisite source", func(_ *testing.T, f *preparationFixture) { f.reconciler.SandboxPreparation.Prerequisites = nil }, metav1.ConditionUnknown},
		{"no installer", func(_ *testing.T, f *preparationFixture) { f.reconciler.SandboxPreparation.Installer = nil }, metav1.ConditionUnknown},
		{"no authoritative reader", func(_ *testing.T, f *preparationFixture) { f.reconciler.APIReader = nil }, metav1.ConditionUnknown},
		{"stale registration", func(_ *testing.T, f *preparationFixture) { f.config.ObservedAt = f.now.Add(-time.Minute) }, metav1.ConditionUnknown},
		{"stale prerequisites", func(_ *testing.T, f *preparationFixture) { f.source.inputs.ObservedAt = f.now.Add(-time.Minute) }, metav1.ConditionUnknown},
		{"future prerequisites", func(_ *testing.T, f *preparationFixture) { f.source.inputs.ObservedAt = f.now.Add(time.Second) }, metav1.ConditionUnknown},
		{"unavailable runtime", func(_ *testing.T, f *preparationFixture) {
			registration := f.config.Registrations[api.SandboxKata]
			registration.Available = false
			f.config.Registrations[api.SandboxKata] = registration
		}, metav1.ConditionFalse},
		{"missing fence", func(_ *testing.T, f *preparationFixture) {
			f.source.fenceErr = fmt.Errorf("synthetic fence unavailable")
		}, metav1.ConditionFalse},
		{"slow fence expires inventory", func(_ *testing.T, f *preparationFixture) {
			f.source.onFence = func() { f.now = f.now.Add(time.Minute) }
		}, metav1.ConditionUnknown},
		{"namespace replacement", func(_ *testing.T, f *preparationFixture) { f.source.inputs.NamespaceUID = "different-namespace" }, metav1.ConditionFalse},
		{"HOME replacement", func(_ *testing.T, f *preparationFixture) { f.source.inputs.Home.UID = "different-home" }, metav1.ConditionFalse},
		{"Secret replacement", func(_ *testing.T, f *preparationFixture) { f.source.inputs.RuntimeSecret.UID = "different-secret" }, metav1.ConditionFalse},
		{"wrong Agent generation", func(_ *testing.T, f *preparationFixture) { f.source.inputs.AgentGeneration++ }, metav1.ConditionFalse},
		{"unapproved chart", func(t *testing.T, f *preparationFixture) {
			agent := f.current(t)
			agent.Spec.Helm.Version = "999.0.0"
			if err := f.client.Update(context.Background(), agent); err != nil {
				t.Fatal(err)
			}
		}, metav1.ConditionFalse},
		{"missing authorization", func(t *testing.T, f *preparationFixture) {
			agent := f.current(t)
			agent.Annotations = nil
			if err := f.client.Update(context.Background(), agent); err != nil {
				t.Fatal(err)
			}
		}, metav1.ConditionFalse},
		{"old Pod", func(t *testing.T, f *preparationFixture) {
			if err := f.client.Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "old-writer", Namespace: f.fixture.Namespace.Name, Finalizers: []string{"example.com/termination"},
			}}); err != nil {
				t.Fatal(err)
			}
		}, metav1.ConditionFalse},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newPreparationFixture(t, "hermes")
			test.mutate(t, f)
			if err := f.reconcile(); err == nil {
				t.Fatal("unsafe preparation was accepted")
			}
			agent := f.current(t)
			condition := apimeta.FindStatusCondition(agent.Status.Conditions, api.ConditionSandboxReady)
			if f.installer.stages != 0 || agent.Status.Sandbox != nil ||
				!slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) ||
				condition == nil || condition.Status != test.status ||
				apimeta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionAgentConfigReady) {
				t.Fatal("invalid inputs produced a prepared or applied result")
			}
		})
	}
}

func TestSandboxPreparationRefreshesMetadataWithoutAgentGenerationChange(t *testing.T) {
	for _, kind := range []string{"Secret", "ConfigMap", "Team"} {
		t.Run(kind, func(t *testing.T) {
			f := newPreparationFixture(t, "openclaw")
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			before := f.expectation(t)
			source := sandboxworkload.SourceReference{}
			for _, candidate := range before.Sources {
				if candidate.Kind == kind {
					source = candidate
					break
				}
			}
			if source.UID == "" {
				t.Fatalf("missing %s source", kind)
			}
			object := &unstructured.Unstructured{}
			object.SetAPIVersion(source.APIVersion)
			object.SetKind(source.Kind)
			if err := f.client.Get(context.Background(), client.ObjectKey{Namespace: source.Namespace, Name: source.Name}, object); err != nil {
				t.Fatal(err)
			}
			labels := object.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}
			labels["example.com/metadata"] = "changed"
			object.SetLabels(labels)
			if err := f.client.Update(context.Background(), object); err != nil {
				t.Fatal(err)
			}
			// A fresh reconciler has no in-memory evidence or preparation cache.
			previous := f.reconciler
			f.reconciler = &controller.AgentReconciler{
				Client: previous.Client, APIReader: previous.APIReader, Scheme: previous.Scheme,
				OperatorNamespace: previous.OperatorNamespace, OperatorSAName: previous.OperatorSAName,
				DomainSuffix: previous.DomainSuffix, CetiRunHelmValues: previous.CetiRunHelmValues, DefaultModel: previous.DefaultModel,
				SandboxPreparation: previous.SandboxPreparation,
			}
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			after := f.expectation(t)
			if f.current(t).Generation != before.AgentGeneration || after.AgentGeneration != before.AgentGeneration || !after.ExecutionHeld {
				t.Fatal("metadata refresh changed generation or lifted execution")
			}
			found := false
			for _, current := range after.Sources {
				if current.APIVersion == source.APIVersion && current.Kind == source.Kind && current.Namespace == source.Namespace && current.Name == source.Name {
					found = current.UID == source.UID && current.ResourceVersion != source.ResourceVersion
				}
			}
			if !found {
				t.Fatal("expectation retained a stale metadata-only source version")
			}
		})
	}
}

func TestSandboxPreparationPartialStageRevokesAndRetries(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprintf("source-race=%v", race), func(t *testing.T) {
			f := newPreparationFixture(t, "hermes")
			if race {
				f.installer.after = func(ctx context.Context) error {
					secret := &corev1.Secret{}
					if err := f.client.Get(ctx, client.ObjectKey{Namespace: f.fixture.Namespace.Name, Name: f.source.inputs.RuntimeSecret.Name}, secret); err != nil {
						return err
					}
					secret.Labels = map[string]string{"example.com/changed": "true"}
					return f.client.Update(ctx, secret)
				}
			} else {
				f.installer.fail = fmt.Errorf("synthetic post-write failure")
			}
			if err := f.reconcile(); err == nil {
				t.Fatal("partial application/race looked successful")
			}
			journal := &sandboxpolicy.GrantJournalStore{Client: f.client, APIReader: f.client, Namespace: "cetirun-system"}
			agent := f.current(t)
			bindings, err := journal.Read(context.Background(), agent.Name, agent.UID)
			if err != nil || len(bindings) != 0 {
				t.Fatalf("partial-stage grants were not revoked: %+v %v", bindings, err)
			}
			binding := &rbacv1.RoleBinding{}
			if err := f.client.Get(context.Background(), client.ObjectKey{Namespace: f.fixture.Namespace.Name, Name: "cetirun-sandbox-sources"}, binding); !apierrors.IsNotFound(err) {
				t.Fatalf("partial-stage source-read grant remains: %v", err)
			}
			if !slices.Contains(agent.Finalizers, sandbox.PreparationFinalizer) ||
				apimeta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionAgentConfigReady) {
				t.Fatal("failed stage lost durable recovery state or claimed success")
			}
			f.installer.fail, f.installer.after = nil, nil
			if err := f.reconcile(); err != nil {
				t.Fatal("safe partial-stage retry failed:", err)
			}
			if !f.expectation(t).ExecutionHeld {
				t.Fatal("retry released the hold")
			}
		})
	}
}

func TestSandboxPreparationSuspensionAndDisabledSourceRevokeGrants(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("disabled=%v", disabled), func(t *testing.T) {
			f := newPreparationFixture(t, "openclaw")
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			count := f.installer.stages
			agent := f.current(t)
			if disabled {
				f.reconciler.SandboxPreparation = nil
			} else {
				agent.Spec.Suspended = true
				agent.Generation++
				if err := f.client.Update(context.Background(), agent); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			bindings, err := (&sandboxpolicy.GrantJournalStore{Client: f.client, APIReader: f.client, Namespace: "cetirun-system"}).
				Read(context.Background(), agent.Name, agent.UID)
			if err != nil || len(bindings) != 0 || f.installer.stages != count {
				t.Fatal("suspension or source disablement retained grants or restaged the workload")
			}
			if f.current(t).Status.AccessURL != "" || !f.expectation(t).ExecutionHeld {
				t.Fatal("suspension or source disablement exposed access")
			}
		})
	}
}

func (f *preparationFixture) makeAdmin(t *testing.T) *rbacv1.ClusterRole {
	t.Helper()
	ctx := context.Background()
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: "example-read", UID: "example-role-uid"},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}}},
	}
	policy := f.fixture.Policy.DeepCopy()
	policy.ObjectMeta = metav1.ObjectMeta{Name: "example-admin", UID: "example-admin-uid", Generation: 1}
	policy.Spec.KubernetesAccess = &api.AgentPolicyAdminKubernetesAccess{
		ServiceAccountName: "example-admin",
		RoleBindings:       []api.AgentPolicyRoleBinding{{Namespace: "operations", ClusterRoleName: role.Name}},
	}
	for _, object := range []client.Object{role, policy, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "operations"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: f.fixture.Namespace.Name}, Data: map[string]string{"ca.crt": "synthetic-only"}},
	} {
		if err := f.client.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	team := f.fixture.Team.DeepCopy()
	team.Spec.AgentPolicyRef = policy.Name
	if err := f.client.Update(ctx, team); err != nil {
		t.Fatal(err)
	}
	agent := f.current(t)
	agent.Generation++
	authorization, err := sandboxbinding.Snapshot(agent, team, sandbox.Resources{
		Sandboxes:     map[string]*api.Sandbox{f.fixture.Sandbox.Name: f.fixture.Sandbox},
		AgentPolicies: map[string]*api.AgentPolicy{f.fixture.Policy.Name: f.fixture.Policy, policy.Name: policy},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.Annotations[sandboxbinding.AuthorizationAnnotation] = authorization
	if err := f.client.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	f.source.inputs.AgentGeneration = agent.Generation
	registration := f.config.Registrations[api.SandboxKata]
	registration.Capabilities.KubernetesAccess = true
	f.config.Registrations[api.SandboxKata] = registration
	platform := f.config.Platforms[api.SandboxKata]
	platform.KubernetesAudience, platform.TokenExpirationSeconds = "https://kubernetes.default.svc", 600
	platform.Topology.KubernetesAPI = []sandboxpolicy.Destination{{
		Peer:  sandboxpolicy.Peer{Name: "api", UID: "example-api-uid", IPs: []string{"203.0.113.10"}},
		Ports: []api.AgentPolicyPort{{Port: 6443, Protocol: corev1.ProtocolTCP}},
	}}
	platform.Identities = []sandboxpolicy.ApprovedIdentity{{
		AgentUID: agent.UID, Namespace: f.fixture.Namespace.Name, ServiceAccountName: "example-admin",
		Revision: "synthetic-identity", Kubernetes: true,
	}}
	platform.Grants = []sandboxpolicy.ApprovedGrant{{
		PolicyUID: policy.UID, Namespace: "operations",
		RoleUID: role.UID, RoleResourceVersion: role.ResourceVersion, Revision: "synthetic-role-approval",
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name}, Rules: role.Rules,
	}}
	f.config.Platforms[api.SandboxKata] = platform
	return role
}

func TestSandboxPreparationAdminGrantsRevokeOnRoleDrift(t *testing.T) {
	f := newPreparationFixture(t, "hermes")
	role := f.makeAdmin(t)
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	journal := &sandboxpolicy.GrantJournalStore{Client: f.client, APIReader: f.client, Namespace: "cetirun-system"}
	agent := f.current(t)
	bindings, err := journal.Read(ctx, agent.Name, agent.UID)
	if err != nil || len(bindings) != 2 {
		t.Fatalf("compiler and admission grants must share write-ahead inventory: %+v %v", bindings, err)
	}
	found := false
	for _, ref := range bindings {
		if ref.Namespace == "operations" {
			found = ref.UID != "" && ref.ServiceAccountNamespace == f.fixture.Namespace.Name && ref.RoleRef.Name == role.Name
		}
	}
	if !found {
		t.Fatal("cross-namespace workload grant was not recorded with its UID")
	}
	role.Labels = map[string]string{"example.com/approval-changed": "true"}
	if err := f.client.Update(ctx, role); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err == nil {
		t.Fatal("stale live role approval was reused")
	}
	for _, ref := range bindings {
		if err := f.client.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
			t.Fatalf("grant survived role approval drift: %s: %v", ref.Name, err)
		}
	}
	platform := f.config.Platforms[api.SandboxKata]
	platform.Grants[0].RoleResourceVersion = role.ResourceVersion
	platform.Grants[0].Revision = "synthetic-refreshed-approval"
	f.config.Platforms[api.SandboxKata] = platform
	if err := f.reconcile(); err != nil {
		t.Fatal("fresh explicit approval did not recover preparation:", err)
	}
	if !f.expectation(t).ExecutionHeld || f.current(t).Status.Sandbox != nil {
		t.Fatal("admin grant preparation claimed execution")
	}
}

func TestSandboxPreparationMetadataOnlyReauthorization(t *testing.T) {
	f := newPreparationFixture(t, "openclaw")
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	before := f.expectation(t)
	ctx := context.Background()
	if err := f.client.Delete(ctx, f.fixture.Policy); err != nil {
		t.Fatal(err)
	}
	replacement := f.fixture.Policy.DeepCopy()
	replacement.UID, replacement.ResourceVersion = "replacement-policy-uid", ""
	if err := f.client.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err == nil {
		t.Fatal("same-name policy replacement was silently rebound")
	}
	agent := f.current(t)
	// Simulate the admission-owned result, not a controller authorization write.
	authorization, err := sandboxbinding.Snapshot(agent, f.fixture.Team, sandbox.Resources{
		Sandboxes:     map[string]*api.Sandbox{f.fixture.Sandbox.Name: f.fixture.Sandbox},
		AgentPolicies: map[string]*api.AgentPolicy{replacement.Name: replacement},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.Annotations[sandboxbinding.AuthorizationAnnotation] = authorization
	if err := f.client.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	after := f.expectation(t)
	if after.Binding.AgentPolicyUID != replacement.UID || after.Revision == before.Revision ||
		after.AgentGeneration != before.AgentGeneration || f.current(t).Status.Sandbox != nil {
		t.Fatal("metadata-only reauthorization was missed or recorded as applied enforcement")
	}
}

func TestSandboxPreparationMissingJournalRequiresRecovery(t *testing.T) {
	f := newPreparationFixture(t, "hermes")
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	agent := f.current(t)
	journal := &corev1.ConfigMap{}
	if err := f.client.Get(ctx, client.ObjectKey{Namespace: "cetirun-system", Name: sandboxpolicy.GrantJournalName(agent.UID)}, journal); err != nil {
		t.Fatal(err)
	}
	journal.Finalizers = nil
	if err := f.client.Update(ctx, journal); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(ctx, journal); err != nil {
		t.Fatal(err)
	}
	stages := f.installer.stages
	if err := f.reconcile(); err == nil || f.installer.stages != stages {
		t.Fatal("lost cleanup inventory was recreated as successful empty state")
	}
	if err := f.client.Delete(ctx, f.current(t)); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err == nil {
		t.Fatal("deletion bypassed missing grant inventory")
	}
	if err := f.client.Get(ctx, client.ObjectKey{Name: f.fixture.Namespace.Name}, &corev1.Namespace{}); err != nil {
		t.Fatal("namespace removed without revocation proof:", err)
	}
}

func TestSandboxPreparationDeletionWaitsForRevocationFenceAndPods(t *testing.T) {
	f := newPreparationFixture(t, "hermes")
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	agent := f.current(t)
	if err := f.client.Delete(ctx, agent); err != nil {
		t.Fatal(err)
	}
	f.source.fenceErr = fmt.Errorf("synthetic unavailable deletion fence")
	if err := f.reconcile(); err == nil {
		t.Fatal("deletion bypassed the qualified fence")
	}
	namespace := &corev1.Namespace{}
	if err := f.client.Get(ctx, client.ObjectKey{Name: f.fixture.Namespace.Name}, namespace); err != nil {
		t.Fatal("namespace removed before fencing:", err)
	}
	bindings, err := (&sandboxpolicy.GrantJournalStore{Client: f.client, APIReader: f.client, Namespace: "cetirun-system"}).
		Read(ctx, agent.Name, agent.UID)
	if err != nil || len(bindings) != 0 {
		t.Fatal("external grants were retained while waiting for fencing")
	}
	f.source.fenceErr = nil
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "terminating-writer", Namespace: namespace.Name, Finalizers: []string{"example.com/termination"},
	}}
	if err := f.client.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err == nil {
		t.Fatal("deletion treated terminating Pod as absence")
	}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}
	pod.Finalizers = nil
	if err := f.client.Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(); err != nil {
		t.Fatal("cleanup completion failed:", err)
	}
	if err := f.client.Get(ctx, client.ObjectKey{Name: agent.Name}, &api.Agent{}); !apierrors.IsNotFound(err) {
		t.Fatal("completed local cleanup retained the Agent:", err)
	}
}

func assertSandboxPrepared(t *testing.T, f *preparationFixture) {
	t.Helper()
	agent := f.current(t)
	if !slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) || !slices.Contains(agent.Finalizers, sandbox.PreparationFinalizer) ||
		agent.Status.Sandbox != nil || agent.Status.AccessURL != "" ||
		!apimeta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionAgentConfigReady) ||
		!apimeta.IsStatusConditionFalse(agent.Status.Conditions, api.ConditionSandboxReady) {
		t.Fatalf("prepared resources were confused with applied enforcement: %+v", agent.Status)
	}
	expected := f.expectation(t)
	if !expected.ExecutionHeld || expected.Workload.UID == "" || len(expected.Sources) < 9 {
		t.Fatal("missing durable held expectation")
	}
	sts := &appsv1.StatefulSet{}
	if err := f.client.Get(context.Background(), client.ObjectKey{Namespace: expected.Namespace.Name, Name: expected.Workload.Name}, sts); err != nil {
		t.Fatal(err)
	}
	if ptr.Deref(sts.Spec.Replicas, -1) != 0 || len(sts.Spec.VolumeClaimTemplates) != 0 ||
		!sandboxworkload.SamePodTemplate(sts.Spec.Template, expected.Template) {
		t.Fatal("prepared workload can execute or recreate HOME")
	}
	for _, source := range expected.Sources {
		if source.IsAgent() {
			current, err := sandboxworkload.AgentSourceReference(agent)
			if err != nil || source != current || source.ResourceVersion != "" {
				t.Fatal("Agent intent snapshot is stale or depends on status resourceVersion")
			}
		}
	}
	journal := &sandboxpolicy.GrantJournalStore{Client: f.client, APIReader: f.client, Namespace: "cetirun-system"}
	bindings, err := journal.Read(context.Background(), agent.Name, agent.UID)
	if err != nil || len(bindings) != 1 || bindings[0].UID == "" || bindings[0].RoleRef.Kind != "Role" {
		t.Fatalf("source-read grant was not durably inventoried: %+v, %v", bindings, err)
	}
	role := &rbacv1.Role{}
	if err := f.client.Get(context.Background(), client.ObjectKey{Namespace: expected.Namespace.Name, Name: bindings[0].RoleRef.Name}, role); err != nil {
		t.Fatal(err)
	}
	for _, rule := range role.Rules {
		if !slices.Equal(rule.Verbs, []string{"get"}) || len(rule.ResourceNames) == 0 ||
			(len(rule.Resources) != 1 || (rule.Resources[0] != "secrets" && rule.Resources[0] != "configmaps")) {
			t.Fatal("admission received blanket source-read permissions")
		}
	}
	handler, err := sandboxadmission.NewPodHandler(f.client, sandboxadmission.PodConfig{StateNamespace: "cetirun-system"})
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: sts.Name + "-0", Namespace: sts.Namespace}, Spec: sts.Spec.Template.Spec,
	}
	raw, err := json.Marshal(pod)
	if err != nil {
		t.Fatal(err)
	}
	response := handler.Validate(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create, Name: pod.Name, Namespace: pod.Namespace,
		Resource: metav1.GroupVersionResource{Version: "v1", Resource: "pods"},
		Object:   runtime.RawExtension{Raw: raw},
	}})
	if response.Allowed || response.Result == nil || !strings.HasPrefix(response.Result.Message, "ExecutionHeld:") {
		t.Fatalf("preparation did not retain the admission hold: %+v", response)
	}
}
