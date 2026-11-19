// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"helm.sh/helm/v3/pkg/chart/loader"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type imageUpgradeHelm struct{ fakeHelmClient }

func (h *imageUpgradeHelm) ResolveImageUpgrade(namespace, release, version, kind, image, registry string) (string, error) {
	return "registry.example.com/" + kind + "-gateway:2.0.0", nil
}

func (h *imageUpgradeHelm) Prepare(_ string, values map[string]any, version string) (*helm.PreparedRelease, error) {
	chrt, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		return nil, err
	}
	chrt.Metadata.Version, chrt.Metadata.AppVersion = version, version
	return helm.PrepareChart(chrt, values)
}

func TestChartOnlyUpgradeRepinsDefaultBeforeDeployment(t *testing.T) {
	for _, kind := range []api.AgentType{api.AgentTypeOpenClaw, api.AgentTypeHermes} {
		t.Run(string(kind), func(t *testing.T) {
			agent := newCR("upgrade", api.PhaseReady, nil, 2, 1, "old-config")
			agent.Spec.Helm.Version = "2.0.0"
			agent.Spec.Agent.Type = kind
			if kind == api.AgentTypeHermes {
				agent.Spec.Agent.Port = ptr.To[int32](8642)
			}
			agent.Spec.Agent.Image = ptr.To("registry.example.com/" + string(kind) + "-gateway:1.0.0")
			agent.Status.HelmReleaseName = "openclaw-upgrade"
			scheme := testScheme()
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent).
				WithStatusSubresource(&api.Agent{}).Build()
			h := &imageUpgradeHelm{fakeHelmClient: fakeHelmClient{useHarness: true}}
			r := &controller.AgentReconciler{
				Client: kube, Scheme: scheme, HelmClient: h, DefaultModel: "test-model",
				ChartRegistry: "registry.example.com", ChartVersion: "2.0.0",
				EventRecorder: record.NewFakeRecorder(100), DomainSuffix: "example.com", IstioRevision: "test",
			}
			controller.ConfigureTestGateway(t, r)
			ctx := context.Background()
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)}
			result, err := r.Reconcile(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			current := &api.Agent{}
			if err := kube.Get(ctx, request.NamespacedName, current); err != nil {
				t.Fatal(err)
			}
			want := "registry.example.com/" + string(kind) + "-gateway:2.0.0"
			if *current.Spec.Agent.Image != want || !result.Requeue || h.installCalled {
				t.Fatalf("upgrade must persist new default before deployment: image=%q result=%+v installed=%t conditions=%+v",
					*current.Spec.Agent.Image, result, h.installCalled, current.Status.Conditions)
			}
			if current.UID != agent.UID || *current.Spec.Agent.Port != *agent.Spec.Agent.Port {
				t.Fatal("image migration changed Agent identity or port")
			}
			if _, err := r.Reconcile(ctx, request); err != nil {
				t.Fatal(err)
			}
			if !h.installCalled {
				t.Fatal("upgrade did not reach Helm after persisting the new image")
			}
			runtime := h.lastValues["runtime"].(map[string]any)["agent"].(map[string]any)
			if runtime["image"] != want {
				t.Fatalf("deployed image = %v, want %s", runtime["image"], want)
			}
		})
	}
}
