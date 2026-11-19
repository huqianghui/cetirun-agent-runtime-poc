// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	sandboxadmission "github.com/Azure/CetiRun/app/cetirun-operator/internal/admission"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	preparationStateNamespace   = "sandbox-preparation-state"
	preparationAdmissionAccount = "sandbox-admission"
	preparationSourceRole       = "cetirun-sandbox-sources"
)

// This exercises the controller, protected chart renderer, API persistence and
// HTTPS Pod admission together. Platform inventory and old-writer quiescence
// below are SYNTHETIC ONLY: no CNI, CSI, runtime or fencing is qualified here.
type preparationHarness struct {
	ctx          context.Context
	client       client.Client
	kubernetes   kubernetes.Interface
	scheme       *runtime.Scheme
	chart        *chart.Chart
	webhookCalls atomic.Int64
}

func newPreparationHarness(t *testing.T, placementConfigMapName ...string) *preparationHarness {
	t.Helper()
	if len(placementConfigMapName) > 1 {
		t.Fatal("at most one placement registry may be selected")
	}
	// TestMain owns a different isolated server and does not expose rest.Config.
	// This explicit false remains authoritative even if the caller's env differs.
	env := &envtest.Environment{
		BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"), UseExistingCluster: ptr.To(false),
		CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true,
		CRDs: []*apiextensionsv1.CustomResourceDefinition{
			preparationExternalCRD("cilium.io", "v2", "CiliumNetworkPolicy", "ciliumnetworkpolicies"),
			preparationExternalCRD("networking.istio.io", "v1beta1", "VirtualService", "virtualservices"),
		},
		ControlPlaneStartTimeout: time.Minute, ControlPlaneStopTimeout: 30 * time.Second,
	}
	env.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	config, err := env.Start()
	if err != nil {
		_ = env.Stop()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	h := &preparationHarness{scheme: runtime.NewScheme()}
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, api.AddToScheme, istiov1beta1.AddToScheme} {
		if err := add(h.scheme); err != nil {
			t.Fatal(err)
		}
	}
	config.QPS, config.Burst = 100, 200
	h.client, err = client.New(config, client.Options{Scheme: h.scheme})
	if err != nil {
		t.Fatal(err)
	}
	h.kubernetes, err = kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	h.chart, err = loader.Load("../../../cetirun-chart")
	if err != nil {
		t.Fatal(err)
	}
	var cancel context.CancelFunc
	h.ctx, cancel = context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	h.create(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: preparationStateNamespace}})
	h.create(t, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: preparationAdmissionAccount, Namespace: preparationStateNamespace,
	}})
	podConfig := sandboxadmission.PodConfig{StateNamespace: preparationStateNamespace}
	if len(placementConfigMapName) == 1 {
		podConfig.PlacementConfigMapName = placementConfigMapName[0]
	}
	handler, err := sandboxadmission.NewPodHandler(h.client, podConfig)
	if err != nil {
		t.Fatal(err)
	}
	webhook := &admission.Webhook{Handler: handler}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.webhookCalls.Add(1)
		webhook.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	rules := &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-preparation-pods"},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{
			Name: "sandbox-preparation.example.com",
			ClientConfig: admissionregistrationv1.WebhookClientConfig{
				URL:      ptr.To(server.URL + sandboxadmission.PodValidationPath),
				CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
			},
			Rules: []admissionregistrationv1.RuleWithOperations{{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
				Rule: admissionregistrationv1.Rule{
					APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"},
					Scope: ptr.To(admissionregistrationv1.NamespacedScope),
				},
			}},
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{w.ProtectedNamespaceLabel: "true"}},
			FailurePolicy:     ptr.To(admissionregistrationv1.Fail), SideEffects: ptr.To(admissionregistrationv1.SideEffectClassNone),
			AdmissionReviewVersions: []string{"v1"}, TimeoutSeconds: ptr.To(int32(10)),
		}},
	}
	h.create(t, rules)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := h.client.Delete(ctx, rules); client.IgnoreNotFound(err) != nil {
			t.Error(err)
		}
	})
	return h
}

func preparationExternalCRD(group, version, kind, plural string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + "." + group},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group, Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: kind, ListKind: kind + "List", Plural: plural, Singular: strings.ToLower(kind)},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: version, Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"spec": {Type: "object", XPreserveUnknownFields: ptr.To(true)},
					},
				}},
			}},
		},
	}
}

func (h *preparationHarness) create(t *testing.T, object client.Object) {
	t.Helper()
	object.SetUID("")
	object.SetResourceVersion("")
	object.SetGeneration(0)
	if err := h.client.Create(h.ctx, object); err != nil {
		t.Fatalf("create %T %s: %v", object, object.GetName(), err)
	}
	if object.GetUID() == "" || object.GetResourceVersion() == "" {
		t.Fatalf("%T %s lacks a persisted API identity", object, object.GetName())
	}
}

func (h *preparationHarness) get(t *testing.T, key client.ObjectKey, object client.Object) {
	t.Helper()
	if err := h.client.Get(h.ctx, key, object); err != nil {
		t.Fatal(err)
	}
}

func (h *preparationHarness) update(t *testing.T, object client.Object) {
	t.Helper()
	if err := h.client.Update(h.ctx, object); err != nil {
		t.Fatal(err)
	}
}

func (h *preparationHarness) assertUnselectedNamespace(t *testing.T) {
	t.Helper()
	before := h.webhookCalls.Load()
	probe := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "unselected-probe", Namespace: preparationStateNamespace},
		Spec: corev1.PodSpec{
			ServiceAccountName: preparationAdmissionAccount, AutomountServiceAccountToken: ptr.To(false),
			Containers: []corev1.Container{{Name: "probe", Image: "registry.example.com/unused:v1"}},
		},
	}
	if err := h.client.Create(h.ctx, probe, client.DryRunAll); err != nil {
		t.Fatalf("non-selected repair namespace must remain independent of the Pod webhook: %v", err)
	}
	if h.webhookCalls.Load() != before {
		t.Fatal("the webhook namespace selector included the controller state namespace")
	}
}

type preparationCase struct {
	h            *preparationHarness
	fixture      testfixture.Fixture
	spare        *api.Sandbox
	unusedPolicy *api.AgentPolicy
	source       *syntheticPreparationSource
	installer    *localPreparationInstaller
	reconciler   *controller.AgentReconciler
}

func (h *preparationHarness) newCase(t *testing.T, agentType string) *preparationCase {
	return h.newCaseWithPrerequisites(t, agentType, true)
}

func (h *preparationHarness) newCaseWithPrerequisites(t *testing.T, agentType string, provision bool) *preparationCase {
	t.Helper()
	f := &preparationCase{h: h, fixture: testfixture.New(agentType)}
	fixture := &f.fixture
	capabilities := fixture.Input.Resolution.Capabilities
	fixture.Input.Resolution, fixture.Input.Controls = nil, w.Controls{}
	name := "prep-" + agentType
	fixture.Agent.Name, fixture.Agent.Spec.User.Alias = name, name
	fixture.Agent.Annotations = nil
	fixture.Agent.Spec.Model = "example-model"
	fixture.Agent.Spec.Helm.Version = h.chart.Metadata.Version
	if !provision {
		fixture.Agent.Spec.Agent.Image = ptr.To("registry.example.com/" + agentType + "-gateway:" + h.chart.Metadata.AppVersion)
		fixture.Agent.Spec.Agent.Port = ptr.To(int32(18789))
		if agentType == "hermes" {
			fixture.Agent.Spec.Agent.Port = ptr.To(int32(8642))
		}
	}
	fixture.Sandbox.Name, fixture.Sandbox.Spec.Type = name+"-box", ""
	fixture.Sandbox.Spec.Storage.Home.StorageClassName = name + "-csi"
	fixture.Policy.Name = name + "-normal"
	fixture.Policy.Spec.Security.ReadOnlyRootFilesystem = nil
	fixture.Policy.Spec.SandboxSelection.AllowMultiple = true
	h.create(t, fixture.Sandbox)
	h.create(t, fixture.Policy)
	f.spare = fixture.Sandbox.DeepCopy()
	f.spare.ObjectMeta = metav1.ObjectMeta{Name: name + "-spare"}
	h.create(t, f.spare)
	f.unusedPolicy = fixture.Policy.DeepCopy()
	f.unusedPolicy.ObjectMeta = metav1.ObjectMeta{Name: name + "-unrelated"}
	h.create(t, f.unusedPolicy)
	fixture.Team.Name = name + "-team"
	fixture.Team.Spec = api.TeamSpec{
		SandboxRefs: []string{fixture.Sandbox.Name, f.spare.Name}, DefaultSandboxRef: fixture.Sandbox.Name,
		AgentPolicyRef: fixture.Policy.Name,
	}
	h.create(t, fixture.Team)
	fixture.Agent.Spec.Team, fixture.Agent.Spec.SandboxRef = fixture.Team.Name, fixture.Sandbox.Name
	h.create(t, fixture.Agent)
	if ptr.Deref(fixture.Agent.Spec.Agent.Username, "") != "vibe" ||
		fixture.Sandbox.Spec.Type != api.SandboxKata || !ptr.Deref(fixture.Policy.Spec.Security.ReadOnlyRootFilesystem, false) {
		t.Fatal("real CRD defaults were not applied to the persisted fixtures")
	}
	record, err := sandboxbinding.Snapshot(fixture.Agent, fixture.Team, sandbox.Resources{
		Sandboxes:     map[string]*api.Sandbox{fixture.Sandbox.Name: fixture.Sandbox, f.spare.Name: f.spare},
		AgentPolicies: map[string]*api.AgentPolicy{fixture.Policy.Name: fixture.Policy},
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.Agent.Annotations = map[string]string{sandboxbinding.AuthorizationAnnotation: record}
	h.update(t, fixture.Agent)
	fixture.Namespace.ObjectMeta = metav1.ObjectMeta{
		Name:            "cetirun-" + name,
		Labels:          map[string]string{w.ProtectedNamespaceLabel: "true"},
		Annotations:     map[string]string{w.AgentNameAnnotation: name, w.AgentUIDAnnotation: string(fixture.Agent.UID)},
		OwnerReferences: []metav1.OwnerReference{w.AgentOwner(name, fixture.Agent.UID)},
	}
	if provision {
		h.create(t, fixture.Namespace)
	}
	fixture.Input.Namespace = w.ObjectIdentity{Name: fixture.Namespace.Name, UID: fixture.Namespace.UID}
	fixture.Input.AgentName, fixture.Input.AgentGeneration = fixture.Agent.Name, fixture.Agent.Generation
	fixture.Input.WorkloadName, fixture.Input.ReleaseName = f.workloadName(), "openclaw-"+fixture.Agent.Name
	fixture.ServiceAccount.ObjectMeta = metav1.ObjectMeta{Name: "default", Namespace: fixture.Namespace.Name}
	if provision {
		h.create(t, fixture.ServiceAccount)
	}
	fixture.Input.Home.ObjectMeta = metav1.ObjectMeta{
		Name: "home", Namespace: fixture.Namespace.Name,
		Annotations: map[string]string{w.AgentUIDAnnotation: string(fixture.Agent.UID)},
	}
	fixture.Input.Home.Spec.StorageClassName = ptr.To(fixture.Sandbox.Spec.Storage.Home.StorageClassName)
	if provision {
		h.create(t, fixture.Input.Home)
	}
	fixture.Input.Skills.ObjectMeta = metav1.ObjectMeta{Name: "skills", Namespace: fixture.Namespace.Name}
	fixture.Input.Skills.Spec = corev1.PersistentVolumeClaimSpec{
		AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		StorageClassName: ptr.To(fixture.Sandbox.Spec.Storage.Home.StorageClassName),
		Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
	}
	if provision {
		h.create(t, fixture.Input.Skills)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: fixture.Agent.Spec.GatewayTokenSecretRef, Namespace: fixture.Namespace.Name}}
	if provision {
		h.create(t, secret)
	}
	fixture.Input.RuntimeClass.Name = name + "-kata"
	h.create(t, fixture.Input.RuntimeClass)
	storage := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: fixture.Sandbox.Spec.Storage.Home.StorageClassName},
		Provisioner: "csi.example.com", ReclaimPolicy: ptr.To(corev1.PersistentVolumeReclaimRetain),
	}
	h.create(t, storage)
	capabilities.RuntimeClassName = fixture.Input.RuntimeClass.Name
	f.source = &syntheticPreparationSource{
		prerequisites: controller.SandboxPrerequisites{
			AgentUID: fixture.Agent.UID, AgentGeneration: fixture.Agent.Generation, Revision: "synthetic-prerequisites-only",
			NamespaceUID: fixture.Namespace.UID, ChartVersion: h.chart.Metadata.Version, Helpers: fixture.Input.Helpers,
			Home:          w.ObjectIdentity{Name: fixture.Input.Home.Name, UID: fixture.Input.Home.UID},
			Skills:        w.ObjectIdentity{Name: fixture.Input.Skills.Name, UID: fixture.Input.Skills.UID},
			RuntimeSecret: w.ObjectIdentity{Name: secret.Name, UID: secret.UID},
		},
		configuration: controller.SandboxConfigurationSnapshot{
			Registrations: map[api.SandboxType]sandbox.Registration{api.SandboxKata: {Available: true, Capabilities: capabilities}},
			Platforms: map[api.SandboxType]sandboxpolicy.Platform{api.SandboxKata: {
				Adapter: sandboxpolicy.AdapterRegistration{
					Name: sandboxpolicy.AzureCNIOverlayCilium, Available: true, Qualified: true, Revision: "synthetic-adapter-only",
				},
				Topology: sandboxpolicy.Topology{
					Revision: "synthetic-topology-only", ManagedPeersComplete: true,
					PodCIDRs: []string{"10.244.0.0/16"}, ServiceCIDRs: []string{"10.96.0.0/12"},
					NodeCIDRs: []string{"192.168.0.0/16"}, ManagementCIDRs: []string{"10.1.0.0/16"},
					MetadataCIDRs: []string{"169.254.169.254/32"}, DNSResolverIPs: []string{"10.96.0.10"},
					DNS: []sandboxpolicy.Peer{{Name: "dns", Namespace: "kube-system", UID: "synthetic-dns", IPs: []string{"10.96.0.10"}}},
					Gateway: []sandboxpolicy.Destination{{
						Peer:  sandboxpolicy.Peer{Name: "gateway", Namespace: "platform", UID: "synthetic-gateway", IPs: []string{"10.96.1.50"}},
						Ports: []api.AgentPolicyPort{{Port: 8443, Protocol: corev1.ProtocolTCP}},
					}},
				},
			}},
			RuntimeClasses: map[string]controller.SandboxRuntimePrerequisite{
				fixture.Input.RuntimeClass.Name: {UID: fixture.Input.RuntimeClass.UID, Handler: fixture.Input.RuntimeClass.Handler},
			},
			StorageClasses: map[string]controller.SandboxStoragePrerequisite{
				storage.Name: {UID: storage.UID, Provisioner: storage.Provisioner},
			},
		},
	}
	f.restart()
	f.installer.beforeStage = func() {
		f.readReceipt(t)
		_, refs := f.readJournal(t)
		if len(refs) != 1 || refs[0].AgentUID != fixture.Agent.UID || refs[0].Name != preparationSourceRole ||
			!slices.Contains(f.agent(t).Finalizers, sandbox.PreparationFinalizer) {
			t.Fatal("held staging started before durable ownership, grant inventory and finalizer persistence")
		}
	}
	return f
}

// These providers never inspect Pods as a substitute for a qualified fence.
// They deliberately represent only explicit synthetic platform approvals.
type syntheticPreparationSource struct {
	configuration                           controller.SandboxConfigurationSnapshot
	prerequisites                           controller.SandboxPrerequisites
	configurationError, fenceError          error
	resolveCalls, snapshotCalls, fenceCalls int
	lastFence                               controller.SandboxPrerequisites
}

func (s *syntheticPreparationSource) Snapshot(context.Context) (controller.SandboxConfigurationSnapshot, error) {
	s.snapshotCalls++
	result := s.configuration
	result.ObservedAt = time.Now().UTC().Add(-time.Second)
	return result, s.configurationError
}

func (s *syntheticPreparationSource) Resolve(_ context.Context, agent *api.Agent, _ *api.Team) (controller.SandboxPrerequisites, error) {
	s.resolveCalls++
	if agent.UID != s.prerequisites.AgentUID || agent.Generation != s.prerequisites.AgentGeneration {
		return controller.SandboxPrerequisites{}, errors.New("synthetic approval does not authorize this Agent generation")
	}
	result := s.prerequisites
	result.ObservedAt = time.Now().UTC()
	return result, nil
}

func (s *syntheticPreparationSource) CheckQuiesced(_ context.Context, _ *api.Agent, prior controller.SandboxPrerequisites) error {
	s.fenceCalls++
	s.lastFence = prior
	if prior.NamespaceUID != s.prerequisites.NamespaceUID || prior.Home != s.prerequisites.Home {
		return errors.New("synthetic fence received a different namespace/HOME identity")
	}
	return s.fenceError
}

// This is an API-only installer, not another renderer or a Helm-action test.
// It consumes the sealed manifest from the real protected Helm preparation and
// refuses every resource outside this test's native, zero-replica contract.
type localPreparationInstaller struct {
	client                    client.Client
	chart                     *chart.Chart
	stageCalls                int
	guardsBefore, guardsAfter int
	beforeStage               func()
	afterStageError           error
	stageDigest               string
}

func (i *localPreparationInstaller) PrepareSandbox(namespace string, platform, user map[string]any, version string, bundle *w.Bundle) (*helm.PreparedRelease, error) {
	if version != i.chart.Metadata.Version || namespace != bundle.Identity().Namespace.Name {
		return nil, errors.New("local fixture chart version or namespace mismatch")
	}
	return helm.PrepareSandboxChart(i.chart, platform, user, bundle)
}

func (i *localPreparationInstaller) StageSandbox(ctx context.Context, namespace, release string, prepared *helm.PreparedRelease, guard func(context.Context) error) error {
	i.stageCalls++
	if guard == nil {
		return errors.New("local staging requires a controller guard")
	}
	if err := guard(ctx); err != nil {
		return err
	}
	i.guardsBefore++
	if i.beforeStage != nil {
		i.beforeStage()
	}
	manifest, err := prepared.SandboxManifest()
	if err != nil {
		return err
	}
	decoder := kubeyaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	var objects []*unstructured.Unstructured
	workloads := 0
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(object); err == io.EOF {
			break
		} else if err != nil {
			return err
		}
		if len(object.Object) == 0 {
			continue
		}
		if object.GetNamespace() != namespace {
			return errors.New("local staging refuses out-of-namespace resources")
		}
		switch object.GetAPIVersion() + "/" + object.GetKind() {
		case "v1/ConfigMap", "v1/Service", "cilium.io/v2/CiliumNetworkPolicy":
		case "apps/v1/StatefulSet":
			replicas, found, err := unstructured.NestedInt64(object.Object, "spec", "replicas")
			if err != nil || !found || replicas != 0 || object.GetName() != release+"-cetirun" {
				return fmt.Errorf("local staging requires held %s-cetirun, got %s replicas=%d found=%t: %v",
					release, object.GetName(), replicas, found, err)
			}
			workloads++
		default:
			return fmt.Errorf("local staging refuses %s/%s", object.GetAPIVersion(), object.GetKind())
		}
		objects = append(objects, object)
	}
	if workloads != 1 || len(objects) != 4 {
		return errors.New("local staging requires exactly the native workload, config, private Service and compiled network policy")
	}
	for _, object := range objects {
		if err := i.client.Patch(ctx, object, client.Apply, client.FieldOwner("sandbox-preparation-integration")); err != nil {
			return fmt.Errorf("apply sealed %s %s: %w", object.GetKind(), object.GetName(), err)
		}
	}
	if err := guard(ctx); err != nil {
		return err
	}
	i.guardsAfter++
	if i.afterStageError == nil {
		i.stageDigest = fmt.Sprintf("%x", sha256.Sum256(manifest))
	}
	return i.afterStageError
}

func (i *localPreparationInstaller) SandboxStageDigest(ctx context.Context, _, _ string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if i.stageDigest == "" {
		return "", errors.New("synthetic held release is unavailable")
	}
	return i.stageDigest, nil
}

func (f *preparationCase) restart() {
	f.source = &syntheticPreparationSource{configuration: f.source.configuration, prerequisites: f.source.prerequisites}
	// Preserve only the synthetic release-store identity, not a prepared render.
	installer := &localPreparationInstaller{client: f.h.client, chart: f.h.chart}
	if f.installer != nil {
		installer.stageDigest = f.installer.stageDigest
	}
	f.installer = installer
	f.reconciler = &controller.AgentReconciler{
		Client: f.h.client, APIReader: f.h.client, Scheme: f.h.scheme,
		OperatorNamespace: preparationStateNamespace, DomainSuffix: "example.com", DefaultModel: "example-model",
		SandboxPreparation: &controller.SandboxPreparation{
			Configuration: f.source, Prerequisites: f.source, Installer: f.installer,
			AdmissionServiceAccount: preparationAdmissionAccount,
		},
	}
}

func (f *preparationCase) workloadName() string {
	return "openclaw-" + f.fixture.Agent.Name + "-cetirun"
}

func (f *preparationCase) request() ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKey{Name: f.fixture.Agent.Name}}
}

func (f *preparationCase) reconcileConfiguration(t *testing.T) {
	t.Helper()
	boxController := &controller.SandboxReconciler{
		Client: f.h.client, APIReader: f.h.client, Source: f.source,
	}
	policyController := &controller.AgentPolicyReconciler{
		Client: f.h.client, APIReader: f.h.client, Source: f.source,
	}
	for _, original := range []*api.Sandbox{f.fixture.Sandbox, f.spare} {
		key := client.ObjectKeyFromObject(original)
		if _, err := boxController.Reconcile(f.h.ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile Sandbox prerequisites against the shared synthetic inventory: %v", err)
		}
		current := &api.Sandbox{}
		f.h.get(t, key, current)
		ready := apimeta.FindStatusCondition(current.Status.Conditions, api.ConditionReady)
		if ready == nil || ready.Status != metav1.ConditionTrue || ready.ObservedGeneration != current.Generation ||
			current.Status.ObservedGeneration != current.Generation {
			t.Fatalf("Sandbox %s must persist current-generation Ready: %+v", current.Name, current.Status)
		}
	}
	for _, original := range []*api.AgentPolicy{f.fixture.Policy, f.unusedPolicy} {
		key := client.ObjectKeyFromObject(original)
		if _, err := policyController.Reconcile(f.h.ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile AgentPolicy against the shared synthetic inventory: %v", err)
		}
		current := &api.AgentPolicy{}
		f.h.get(t, key, current)
		accepted := apimeta.FindStatusCondition(current.Status.Conditions, api.ConditionAgentPolicyAccepted)
		if accepted == nil || accepted.Status != metav1.ConditionTrue || accepted.ObservedGeneration != current.Generation ||
			current.Status.ObservedGeneration != current.Generation {
			t.Fatalf("AgentPolicy %s must persist current-generation Accepted: %+v", current.Name, current.Status)
		}
	}
	agent := f.agent(t)
	if apimeta.IsStatusConditionTrue(agent.Status.Conditions, api.ConditionSandboxReady) ||
		agent.Status.Sandbox != nil || agent.Status.AccessURL != "" || f.installer.stageCalls != 0 {
		t.Fatal("Ready/Accepted configuration must not create runtime resources, applied binding or an Agent execution permit")
	}
}

func (f *preparationCase) reconcile(t *testing.T) {
	t.Helper()
	if _, err := f.reconciler.Reconcile(f.h.ctx, f.request()); err != nil {
		workload := &appsv1.StatefulSet{}
		if f.h.client.Get(f.h.ctx, client.ObjectKey{Namespace: f.fixture.Namespace.Name, Name: f.workloadName()}, workload) == nil {
			spec := workload.Spec.Template.Spec
			t.Logf("persisted Pod template API defaults: restartPolicy=%q schedulerName=%q terminationGracePeriodSeconds=%d deprecatedServiceAccount=%q",
				spec.RestartPolicy, spec.SchedulerName, ptr.Deref(spec.TerminationGracePeriodSeconds, -1), spec.DeprecatedServiceAccount)
		}
		t.Fatalf("reconcile held native Sandbox: %v", err)
	}
}

func (f *preparationCase) agent(t *testing.T) *api.Agent {
	t.Helper()
	agent := &api.Agent{}
	f.h.get(t, client.ObjectKeyFromObject(f.fixture.Agent), agent)
	return agent
}

func (f *preparationCase) assertCondition(t *testing.T, kind string, status metav1.ConditionStatus) {
	t.Helper()
	agent := f.agent(t)
	condition := apimeta.FindStatusCondition(agent.Status.Conditions, kind)
	if condition == nil || condition.Status != status || condition.ObservedGeneration != agent.Generation {
		t.Fatalf("%s: expected current-generation %s, got %+v", kind, status, condition)
	}
}

func (f *preparationCase) assertHeld(t *testing.T) {
	t.Helper()
	agent := f.agent(t)
	if agent.Status.Sandbox != nil || agent.Status.AccessURL != "" ||
		!slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) || !slices.Contains(agent.Finalizers, sandbox.PreparationFinalizer) {
		t.Fatal("held preparation must retain finalizers without an applied binding or access URL")
	}
	f.assertCondition(t, api.ConditionSandboxReady, metav1.ConditionFalse)
	ready := apimeta.FindStatusCondition(agent.Status.Conditions, api.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatal("held preparation cannot report Ready")
	}
	workload := &appsv1.StatefulSet{}
	f.h.get(t, client.ObjectKey{Namespace: f.fixture.Namespace.Name, Name: f.workloadName()}, workload)
	if ptr.Deref(workload.Spec.Replicas, -1) != 0 || workload.Status.ReadyReplicas != 0 || len(workload.Spec.VolumeClaimTemplates) != 0 {
		t.Fatal("staged workload must stay at zero replicas and reuse existing storage")
	}
	namespace, home := &corev1.Namespace{}, &corev1.PersistentVolumeClaim{}
	f.h.get(t, client.ObjectKeyFromObject(f.fixture.Namespace), namespace)
	f.h.get(t, client.ObjectKeyFromObject(f.fixture.Input.Home), home)
	if namespace.UID != f.fixture.Namespace.UID || !namespace.DeletionTimestamp.IsZero() ||
		namespace.Labels[w.ProtectedNamespaceLabel] != "true" ||
		namespace.Annotations[w.AgentUIDAnnotation] != string(agent.UID) ||
		!equality.Semantic.DeepEqual(namespace.OwnerReferences, []metav1.OwnerReference{w.AgentOwner(agent.Name, agent.UID)}) ||
		home.UID != f.fixture.Input.Home.UID || !home.DeletionTimestamp.IsZero() ||
		!equality.Semantic.DeepEqual(home.Spec, f.fixture.Input.Home.Spec) {
		t.Fatal("held preparation replaced, resized or deleted the existing namespace/HOME")
	}
	var pods corev1.PodList
	if err := f.h.client.List(f.h.ctx, &pods, client.InNamespace(namespace.Name)); err != nil || len(pods.Items) != 0 {
		t.Fatalf("held preparation must never create a Pod: count=%d err=%v", len(pods.Items), err)
	}
}

func (f *preparationCase) expectation(t *testing.T) w.Expectation {
	t.Helper()
	state := &corev1.ConfigMap{}
	f.h.get(t, client.ObjectKey{
		Namespace: preparationStateNamespace,
		Name:      sandboxadmission.PodExpectationName(f.fixture.Namespace.Name, f.fixture.Namespace.UID),
	}, state)
	var expected w.Expectation
	if err := json.Unmarshal([]byte(state.Data[sandboxadmission.PodExpectationKey]), &expected); err != nil {
		t.Fatal(err)
	}
	if !expected.ExecutionHeld || expected.Namespace.UID != f.fixture.Namespace.UID || expected.Agent.UID != f.fixture.Agent.UID ||
		!equality.Semantic.DeepEqual(state.OwnerReferences, []metav1.OwnerReference{w.AgentOwner(f.fixture.Agent.Name, f.fixture.Agent.UID)}) {
		t.Fatal("persisted Pod expectation lost its exact ownership or execution hold")
	}
	return expected
}

func (f *preparationCase) assertPrepared(t *testing.T) w.Expectation {
	t.Helper()
	f.assertHeld(t)
	f.assertCondition(t, api.ConditionAgentConfigReady, metav1.ConditionTrue)
	if f.agent(t).Status.Phase != api.PhasePending {
		t.Fatal("prepared is not running or fully ready")
	}
	expected := f.expectation(t)
	workload := &appsv1.StatefulSet{}
	f.h.get(t, client.ObjectKey{Namespace: f.fixture.Namespace.Name, Name: f.workloadName()}, workload)
	if expected.Workload.UID != workload.UID || expected.AgentGeneration != f.agent(t).Generation ||
		!w.SamePodTemplate(workload.Spec.Template, expected.Template) ||
		workload.Spec.PodManagementPolicy != appsv1.OrderedReadyPodManagement ||
		ptr.Deref(workload.Spec.RevisionHistoryLimit, -1) != 10 ||
		workload.Spec.Template.Spec.RestartPolicy != corev1.RestartPolicyAlways ||
		workload.Spec.Template.Spec.SchedulerName != corev1.DefaultSchedulerName ||
		workload.Spec.Template.Spec.TerminationGracePeriodSeconds == nil {
		t.Fatal("expectation does not match the real API-defaulted StatefulSet")
	}
	seen := map[string]bool{}
	for _, source := range expected.Sources {
		key := source.APIVersion + "/" + source.Kind + "/" + source.Namespace + "/" + source.Name
		if !source.ValidIdentity() || seen[key] {
			t.Fatalf("source does not have one exact persisted identity: %s", key)
		}
		seen[key] = true
		if source.IsAgent() {
			current, err := w.AgentSourceReference(f.agent(t))
			if err != nil || source != current || source.ResourceVersion != "" {
				t.Fatalf("Agent source does not match authorization-relevant intent: %v", err)
			}
			continue
		}
		metadata := &metav1.PartialObjectMetadata{TypeMeta: metav1.TypeMeta{APIVersion: source.APIVersion, Kind: source.Kind}}
		f.h.get(t, client.ObjectKey{Namespace: source.Namespace, Name: source.Name}, metadata)
		if metadata.UID != source.UID || metadata.ResourceVersion != source.ResourceVersion {
			t.Fatalf("expectation source is already stale after successful reconciliation: %s", key)
		}
	}
	for _, object := range []struct{ kind, name string }{
		{"Team", f.fixture.Team.Name}, {"Sandbox", f.fixture.Sandbox.Name}, {"Sandbox", f.spare.Name},
		{"AgentPolicy", f.fixture.Policy.Name},
	} {
		preparationSource(t, expected, object.kind, "", object.name)
	}
	for _, source := range w.PodSourceReferences(workload.Spec.Template.Spec, workload.Namespace) {
		preparationSource(t, expected, source.Kind, source.Namespace, source.Name)
	}
	preparationSource(t, expected, "Role", workload.Namespace, preparationSourceRole)
	preparationSource(t, expected, "RoleBinding", workload.Namespace, preparationSourceRole)
	if expected.Home != f.source.prerequisites.Home || expected.Skills != f.source.prerequisites.Skills ||
		ptr.Deref(workload.Spec.Template.Spec.RuntimeClassName, "") != f.fixture.Input.RuntimeClass.Name ||
		ptr.Deref(workload.Spec.Template.Spec.AutomountServiceAccountToken, true) {
		t.Fatal("rendered template lost protected runtime, storage or token settings")
	}
	service := &corev1.Service{}
	f.h.get(t, client.ObjectKeyFromObject(workload), service)
	if service.Spec.Type != corev1.ServiceTypeClusterIP || len(service.Spec.ExternalIPs) != 0 {
		t.Fatal("held runtime Service must remain private")
	}
	var routes istiov1beta1.VirtualServiceList
	if err := f.h.client.List(f.h.ctx, &routes, client.InNamespace(workload.Namespace)); err != nil || len(routes.Items) != 0 {
		t.Fatalf("preparation must not create an ingress route: %v", err)
	}
	f.readReceipt(t)
	return expected
}

func preparationSource(t *testing.T, expected w.Expectation, kind, namespace, name string) w.SourceReference {
	t.Helper()
	for _, source := range expected.Sources {
		if source.Kind == kind && source.Namespace == namespace && source.Name == name {
			return source
		}
	}
	t.Fatalf("expectation is missing %s %s/%s", kind, namespace, name)
	return w.SourceReference{}
}

func (f *preparationCase) assertAdmissionHeld(t *testing.T, expected w.Expectation) {
	t.Helper()
	pod := func() *corev1.Pod {
		result := testfixture.Pod(expected)
		result.Spec.Overhead = nil // RuntimeClass admission supplies actual API overhead.
		return result
	}
	var last error
	if err := wait.PollUntilContextTimeout(f.h.ctx, 100*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		last = f.h.client.Create(ctx, pod(), client.DryRunAll)
		return apierrors.IsForbidden(last) && strings.Contains(last.Error(), "ExecutionHeld"), nil
	}); err != nil {
		t.Fatalf("actual HTTPS Pod admission did not enforce the hold: %v; last=%v", err, last)
	}
	before := f.h.webhookCalls.Load()
	candidate := pod()
	if err := f.h.client.Create(f.h.ctx, candidate); !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "ExecutionHeld") {
		t.Fatalf("API CREATE must be denied specifically by the execution hold: %v", err)
	}
	if f.h.webhookCalls.Load() <= before {
		t.Fatal("Pod rejection did not traverse the HTTPS admission endpoint")
	}
	if err := f.h.client.Get(f.h.ctx, client.ObjectKeyFromObject(candidate), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a denied Pod was persisted: %v", err)
	}
}

func (f *preparationCase) readJournal(t *testing.T) (*corev1.ConfigMap, []sandboxpolicy.BindingReference) {
	t.Helper()
	store := &sandboxpolicy.GrantJournalStore{Client: f.h.client, APIReader: f.h.client, Namespace: preparationStateNamespace}
	refs, err := store.Read(f.h.ctx, f.fixture.Agent.Name, f.fixture.Agent.UID)
	if err != nil {
		t.Fatal(err)
	}
	object := &corev1.ConfigMap{}
	f.h.get(t, client.ObjectKey{Namespace: preparationStateNamespace, Name: sandboxpolicy.GrantJournalName(f.fixture.Agent.UID)}, object)
	if !slices.Contains(object.Finalizers, sandboxpolicy.GrantJournalFinalizer) {
		t.Fatal("durable grant journal must remain protected by its finalizer")
	}
	return object, refs
}

func (f *preparationCase) readReceipt(t *testing.T) (*corev1.ConfigMap, controller.SandboxPrerequisites) {
	t.Helper()
	var objects corev1.ConfigMapList
	if err := f.h.client.List(f.h.ctx, &objects, client.InNamespace(preparationStateNamespace)); err != nil {
		t.Fatal(err)
	}
	for i := range objects.Items {
		object := &objects.Items[i]
		if object.Annotations[w.AgentUIDAnnotation] != string(f.fixture.Agent.UID) || object.Data["preparation.json"] == "" {
			continue
		}
		var receipt struct {
			Version       string
			AgentName     string
			AgentUID      types.UID
			Prerequisites controller.SandboxPrerequisites
		}
		if err := json.Unmarshal([]byte(object.Data["preparation.json"]), &receipt); err != nil {
			t.Fatal(err)
		}
		if !((receipt.Version == "v1" && receipt.Prerequisites.Placement == nil) ||
			(receipt.Version == "v2" && receipt.Prerequisites.Placement != nil)) ||
			receipt.AgentUID != f.fixture.Agent.UID || receipt.AgentName != f.fixture.Agent.Name ||
			receipt.Prerequisites.NamespaceUID != f.fixture.Namespace.UID || receipt.Prerequisites.Home != f.source.prerequisites.Home ||
			!slices.Contains(object.Finalizers, sandbox.PreparationFinalizer) {
			t.Fatal("durable preparation receipt does not bind the original namespace/HOME")
		}
		return object, receipt.Prerequisites
	}
	t.Fatal("durable preparation receipt is missing")
	return nil, controller.SandboxPrerequisites{}
}

func (f *preparationCase) assertRevoked(t *testing.T) {
	t.Helper()
	_, refs := f.readJournal(t)
	if len(refs) != 0 {
		t.Fatalf("successful revocation must confirm an empty journal: %+v", refs)
	}
	key := client.ObjectKey{Namespace: f.fixture.Namespace.Name, Name: preparationSourceRole}
	if err := f.h.client.Get(f.h.ctx, key, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("source-read RoleBinding was not revoked: %v", err)
	}
}

func (f *preparationCase) assertSourceAccess(t *testing.T, granted bool) {
	t.Helper()
	if granted {
		role, binding := &rbacv1.Role{}, &rbacv1.RoleBinding{}
		key := client.ObjectKey{Namespace: f.fixture.Namespace.Name, Name: preparationSourceRole}
		f.h.get(t, key, role)
		f.h.get(t, key, binding)
		want := []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{f.workloadName() + "-config"}, Verbs: []string{"get"}},
			{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{f.fixture.Agent.Spec.GatewayTokenSecretRef}, Verbs: []string{"get"}},
		}
		if !equality.Semantic.DeepEqual(role.Rules, want) ||
			binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name}) ||
			!equality.Semantic.DeepEqual(binding.Subjects, []rbacv1.Subject{{
				Kind: "ServiceAccount", Namespace: preparationStateNamespace, Name: preparationAdmissionAccount,
			}}) {
			t.Fatal("admission source access must be exactly named get-only Secret/ConfigMap rules for the separate admission account")
		}
	}
	for _, resource := range []struct{ kind, name string }{
		{"secrets", f.fixture.Agent.Spec.GatewayTokenSecretRef}, {"configmaps", f.workloadName() + "-config"},
	} {
		for _, check := range []struct {
			namespace, verb, name string
			allowed               bool
		}{
			{f.fixture.Namespace.Name, "get", resource.name, granted},
			{f.fixture.Namespace.Name, "get", "unrelated-source", false},
			{f.fixture.Namespace.Name, "list", "", false},
			{f.fixture.Namespace.Name, "watch", "", false},
			{f.fixture.Namespace.Name, "update", resource.name, false},
			{preparationStateNamespace, "get", resource.name, false},
		} {
			var status authorizationv1.SubjectAccessReviewStatus
			if err := wait.PollUntilContextTimeout(f.h.ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
				review, err := f.h.kubernetes.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
					Spec: authorizationv1.SubjectAccessReviewSpec{
						User:   "system:serviceaccount:" + preparationStateNamespace + ":" + preparationAdmissionAccount,
						Groups: []string{"system:authenticated", "system:serviceaccounts", "system:serviceaccounts:" + preparationStateNamespace},
						ResourceAttributes: &authorizationv1.ResourceAttributes{
							Namespace: check.namespace, Verb: check.verb, Resource: resource.kind, Name: check.name,
						},
					},
				}, metav1.CreateOptions{})
				if err != nil {
					return false, err
				}
				status = review.Status
				return status.Allowed == check.allowed && status.EvaluationError == "", nil
			}); err != nil {
				t.Fatalf("actual RBAC %s %s %s/%s: wanted allowed=%t, got %+v: %v",
					check.verb, resource.kind, check.namespace, check.name, check.allowed, status, err)
			}
		}
	}
}

func (f *preparationCase) completeNamespaceDeletion(t *testing.T) {
	t.Helper()
	namespace := f.fixture.Namespace.Name
	objects := []client.Object{
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: f.workloadName()}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: f.workloadName()}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: f.workloadName() + "-config"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: f.fixture.Agent.Spec.GatewayTokenSecretRef}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "default"}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: preparationSourceRole}},
	}
	var policies unstructured.UnstructuredList
	policies.SetAPIVersion("cilium.io/v2")
	policies.SetKind("CiliumNetworkPolicyList")
	if err := f.h.client.List(f.h.ctx, &policies, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	for i := range policies.Items {
		objects = append(objects, &policies.Items[i])
	}
	for _, object := range objects {
		if err := f.h.client.Delete(f.h.ctx, object); client.IgnoreNotFound(err) != nil {
			t.Fatal(err)
		}
	}
	for _, original := range []*corev1.PersistentVolumeClaim{f.fixture.Input.Home, f.fixture.Input.Skills} {
		claim := &corev1.PersistentVolumeClaim{}
		f.h.get(t, client.ObjectKeyFromObject(original), claim)
		if err := f.h.client.Delete(f.h.ctx, claim); err != nil {
			t.Fatal(err)
		}
		if err := f.h.client.Get(f.h.ctx, client.ObjectKeyFromObject(claim), claim); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(claim.Finalizers, []string{"kubernetes.io/pvc-protection"}) {
			t.Fatalf("unexpected fixture PVC finalizers: %v", claim.Finalizers)
		}
		claim.Finalizers = nil
		f.h.update(t, claim)
	}
	current := &corev1.Namespace{}
	f.h.get(t, client.ObjectKey{Name: namespace}, current)
	current.Spec.Finalizers = nil
	if _, err := f.h.kubernetes.CoreV1().Namespaces().Finalize(f.h.ctx, current, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := f.h.client.Get(f.h.ctx, client.ObjectKey{Name: namespace}, &corev1.Namespace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("local namespace finalization was not observed: %v", err)
	}
}
