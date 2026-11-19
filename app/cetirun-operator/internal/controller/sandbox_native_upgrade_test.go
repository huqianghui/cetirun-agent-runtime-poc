// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	helmfixture "github.com/Azure/CetiRun/app/cetirun-operator/internal/helm/testfixture"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"helm.sh/helm/v3/pkg/chart/loader"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type oldNativeUpgradeState struct {
	SourceCommit  string                    `json:"sourceCommit"`
	Description   string                    `json:"description"`
	ObjectsSHA256 string                    `json:"objectsSHA256"`
	Revision      string                    `json:"revision"`
	StateName     string                    `json:"stateName"`
	ReceiptName   string                    `json:"receiptName"`
	StageDigest   string                    `json:"stageDigest"`
	GatewayKeys   map[string]*aigateway.Key `json:"gatewayKeys"`
	Objects       []json.RawMessage         `json:"objects"`
}

func oldNativeUpgradeFixture(t *testing.T) (*nativeSandboxFixture, oldNativeUpgradeState) {
	t.Helper()
	f, state := oldNativeUpgradeFixtureAsRecorded(t)
	// The fixture's Gateway grant was made under its source revision's
	// binding rule, which derived each model group from the complete model
	// configuration. Groups now follow the model name, so an Agent granted
	// under the earlier rule gets a new Gateway policy revision once, and a
	// running native Agent is held for it like for any changed input (see
	// TestNativeUpgradeStillHoldsChangedInputs). Record the current Gateway
	// revision, as a completed re-projection would, so that the remaining
	// upgrade checks cover the native runtime itself. The key keeps the
	// earlier group; the operator replaces it without a hold.
	ctx := context.Background()
	resolution, err := f.r.resolvePolicy(ctx, f.agent)
	if err != nil {
		t.Fatal(err)
	}
	agent := f.current(t)
	if agent.Status.Policy.Enforcement.Gateway.DesiredRevision == resolution.gatewayRevision {
		t.Fatal("test setup: the recorded Gateway revision already follows the current binding rule")
	}
	agent.Status.Policy.Enforcement.Gateway.DesiredRevision = resolution.gatewayRevision
	agent.Status.AIGateway.PolicyRevision = resolution.gatewayRevision
	if err := f.kube.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	f.agent = agent
	return f, state
}

// oldNativeUpgradeFixtureAsRecorded loads the fixture exactly as its source
// revision left it, Gateway grant included.
func oldNativeUpgradeFixtureAsRecorded(t *testing.T) (*nativeSandboxFixture, oldNativeUpgradeState) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "native-upgrade", "pre-proxy.json"))
	if err != nil {
		t.Fatal(err)
	}
	entropy := bytes.Repeat([]byte{0x42}, 32)
	raw = []byte(strings.NewReplacer(
		"@SYNTHETIC_GATEWAY_KEY_DATA@", base64.StdEncoding.EncodeToString([]byte("sk-"+base64.RawURLEncoding.EncodeToString(entropy))),
		"@SYNTHETIC_RUNTIME_KEY_DATA@", base64.StdEncoding.EncodeToString([]byte(base64.StdEncoding.EncodeToString(entropy))),
		"@EMPTY_RENDER_CHECKSUM@", fmt.Sprintf("%x", sha256.Sum256([]byte("\n"))),
	).Replace(string(raw)))
	var state oldNativeUpgradeState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	objectsJSON, err := json.Marshal(state.Objects)
	if err != nil {
		t.Fatal(err)
	}
	if state.SourceCommit != "6414494154151b2daf74d39d1e989ebeb7ea5020" ||
		fmt.Sprintf("%x", sha256.Sum256(objectsJSON)) != state.ObjectsSHA256 {
		t.Fatal("old controller fixture provenance or hydrated object bytes changed")
	}
	r, _, gateway := aiGatewayTestReconciler(t)
	cilium := schema.GroupVersion{Group: "cilium.io", Version: "v2"}
	r.Scheme.AddKnownTypeWithName(cilium.WithKind("CiliumNetworkPolicy"), &unstructured.Unstructured{})
	r.Scheme.AddKnownTypeWithName(cilium.WithKind("CiliumNetworkPolicyList"), &unstructured.UnstructuredList{})
	decoder := serializer.NewCodecFactory(r.Scheme).UniversalDeserializer()
	objects := make([]runtime.Object, 0, len(state.Objects))
	var agent *api.Agent
	for _, raw := range state.Objects {
		object, _, err := decoder.Decode(raw, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if value, ok := object.(*api.Agent); ok {
			agent = value
		}
		objects = append(objects, object)
	}
	if agent == nil || agent.Status.Phase != api.PhaseReady || agent.Status.Sandbox == nil ||
		agent.Status.Sandbox.AppliedRevision != state.Revision {
		t.Fatal("fixture is not an old running native Agent")
	}
	kube := fake.NewClientBuilder().WithScheme(r.Scheme).WithRuntimeObjects(objects...).
		WithStatusSubresource(&api.Agent{}, &api.Team{}, &api.AgentPolicy{}).Build()
	r.Client, r.APIReader = kube, kube
	r.OperatorSAName, r.ChartRegistry, r.ChartVersion = "cetirun-operator", "registry.example.com", agent.Spec.Helm.Version
	r.DefaultModel, r.IstioGateway = testSharedModelOne, "platform/ingress"
	gateway.keys = state.GatewayKeys
	chrt, err := loader.Load(filepath.Join("..", "..", "..", "cetirun-chart"))
	if err != nil {
		t.Fatal(err)
	}
	installer := &nativeSandboxInstaller{kube: kube, chart: chrt, digest: state.StageDigest}
	config, err := ParseSandboxSourcesConfig(nativeSandboxSourcesJSON)
	if err != nil {
		t.Fatal(err)
	}
	config.NativeExecution = true
	r.SandboxPreparation, err = NewKubernetesSandboxPreparation(config, kube, installer, r.OperatorNamespace, r.ChartVersion)
	if err != nil {
		t.Fatal(err)
	}
	return &nativeSandboxFixture{r: r, kube: kube, gateway: gateway, installer: installer, agent: agent}, state
}

func TestNativeUpgradePreparesExistingAgentWithPinnedPreProxyChart(t *testing.T) {
	f := newNativeSandboxFixture(t, true)
	if err := helmfixture.RestorePreProxyContract(f.installer.chart); err != nil {
		t.Fatal(err)
	}
	original := f.current(t)
	if original.Spec.Helm.Version == "" || original.Annotations[sandboxbinding.AuthorizationAnnotation] == "" || f.installer.stages != 0 {
		t.Fatal("fixture must be an authorized, pinned Agent awaiting preparation")
	}
	receipt := &corev1.ConfigMap{}
	receiptKey := client.ObjectKey{Namespace: f.r.OperatorNamespace, Name: w.PreparationReceiptName(original.UID)}
	if err := f.kube.Get(context.Background(), receiptKey, receipt); !apierrors.IsNotFound(err) {
		t.Fatal("pending fixture unexpectedly has reusable preparation:", err)
	}
	f.r.ChartVersion = "999.0.0"
	f.prepare(t)
	current := f.current(t)
	if !reflect.DeepEqual(current.Spec, original.Spec) || current.UID != original.UID ||
		current.Annotations[sandboxbinding.AuthorizationAnnotation] != original.Annotations[sandboxbinding.AuthorizationAnnotation] ||
		f.installer.prepared.ChartVersion != original.Spec.Helm.Version {
		t.Fatal("upgrade changed the pending Agent's pin, intent or authorization")
	}
	if err := f.kube.Get(context.Background(), receiptKey, receipt); err != nil {
		t.Fatal(err)
	}
	recorded, err := w.DecodePreparationReceipt(receipt, w.ObjectIdentity{Name: original.Name, UID: original.UID})
	if err != nil || recorded.Prerequisites.ChartVersion != original.Spec.Helm.Version {
		t.Fatal("preparation did not retain the old chart pin:", err)
	}
	_, expected := f.expectation(t)
	if !expected.ExecutionHeld || expected.SinglePod || f.installer.stages != 1 {
		t.Fatal("old pinned chart did not complete guarded held preparation")
	}
	f.assertGatewayProjection(t, expected)
	f.ready(t)
	if f.installer.stages != 1 {
		t.Fatal("completed pinned preparation was not reusable")
	}
}

func TestNativeUpgradeStillHoldsChangedInputs(t *testing.T) {
	// A native Agent granted under the earlier per-configuration model
	// groups gets a new Gateway policy revision once, and is held like for
	// any other changed input: native execution is single-start.
	for _, change := range []string{"ingress peer", "runtime config", "earlier model groups"} {
		t.Run(change, func(t *testing.T) {
			load := oldNativeUpgradeFixture
			if change == "earlier model groups" {
				load = oldNativeUpgradeFixtureAsRecorded
			}
			f, legacy := load(t)
			ctx := context.Background()
			if change == "ingress peer" {
				platform := &corev1.ConfigMap{}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.r.OperatorNamespace, Name: "sandbox-platform"}, platform); err != nil {
					t.Fatal(err)
				}
				var document SandboxPlatformConfiguration
				if err := json.Unmarshal([]byte(platform.Data[SandboxPlatformConfigurationKey]), &document); err != nil {
					t.Fatal(err)
				}
				network := document.Platforms[api.SandboxKata]
				network.Topology.Ingress[0].Peer.IPs = []string{"10.96.1.61"}
				document.Platforms[api.SandboxKata] = network
				raw, err := json.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
				platform.Data[SandboxPlatformConfigurationKey] = string(raw)
				if err := f.kube.Update(ctx, platform); err != nil {
					t.Fatal(err)
				}
			} else if change == "runtime config" {
				workload := f.workload(t)
				config := &corev1.ConfigMap{}
				if err := f.kube.Get(ctx, client.ObjectKey{Namespace: workload.Namespace, Name: workload.Name + "-config"}, config); err != nil {
					t.Fatal(err)
				}
				config.Data["unexpected-change"] = "synthetic"
				if err := f.kube.Update(ctx, config); err != nil {
					t.Fatal(err)
				}
			}
			_, _ = f.reconcile()
			f.assertNotReady(t)
			if ptr.Deref(f.workload(t).Spec.Replicas, -1) != 0 || !slices.Contains(f.current(t).Finalizers, sandbox.HoldFinalizer) {
				t.Fatal("upgrade compatibility retained execution after actual input drift")
			}
			route := &istiov1beta1.VirtualService{}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: instanceNamespace(f.agent.Name), Name: virtualServiceName(f.agent.Name)}, route); !apierrors.IsNotFound(err) {
				t.Fatalf("changed inputs retained native access: %v", err)
			}
			receipt, _, err := f.r.readSandboxPreparationReceipt(ctx, f.current(t))
			if err != nil || receipt.Annotations[nativeStartAnnotation] != legacy.Revision {
				t.Fatalf("hold lost the original single-start marker: %v", err)
			}
			if _, err := f.r.startKubernetesSandbox(ctx, f.current(t)); err == nil || f.installer.stages != 0 {
				t.Fatal("changed legacy lifetime automatically replayed")
			}
		})
	}
}
