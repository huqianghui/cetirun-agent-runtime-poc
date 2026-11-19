// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	"helm.sh/helm/v3/pkg/chart/loader"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type dashboardProfileHelm struct {
	chartHelm
	prepares int
}

func (h *dashboardProfileHelm) Prepare(_ string, values map[string]any, version string) (*helm.PreparedRelease, error) {
	h.prepares++
	chrt, err := loader.Load("../../../cetirun-chart")
	if err != nil {
		return nil, err
	}
	chrt.Metadata.Version = version
	for _, file := range chrt.Files {
		if file.Name == "agents/hermes/profile.yaml" {
			file.Data = []byte("contractVersion: v1\nnativeVersion: '0.21.5'\nport: 8642\nqualifiedImages: []\n")
		}
	}
	return helm.PrepareChart(chrt, values)
}

func TestHermesUpgradeRejectsOldProfileBeforeChangingSteadyStateRoute(t *testing.T) {
	f := newDeployedAgent(t)
	ctx := context.Background()
	agent := f.agent(t)
	agent.Spec.Agent.Type = api.AgentTypeHermes
	agent.Spec.Agent.Image = ptr.To("registry.example.com/hermes-gateway:old")
	agent.Spec.Agent.Port = ptr.To[int32](8642)
	if err := f.r.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: agent.Spec.GatewayTokenSecretRef}
	if err := f.r.Get(ctx, key, secret); err != nil {
		t.Fatal(err)
	}
	secret.Data["API_SERVER_KEY"] = []byte("synthetic-retained-api-key")
	if err := f.r.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	team := &api.Team{}
	if err := f.r.Get(ctx, client.ObjectKey{Name: agent.Spec.Team}, team); err != nil {
		t.Fatal(err)
	}
	// The previous config hash had no Hermes contract component. These
	// ordinary built-ins share all of the fixture's remaining hash inputs.
	oldInputs := agent.DeepCopy()
	oldInputs.Spec.Agent.Type = api.AgentTypeOpenClaw
	oldHash := f.r.configHash(oldInputs, team, secretContentHash(secret.Data), teamRuntimeInputFingerprint(team))
	if oldHash == f.r.configHash(agent, team, secretContentHash(secret.Data), teamRuntimeInputFingerprint(team)) {
		t.Fatal("new Hermes contract reused the previous steady-state hash")
	}
	agent.Status.ObservedOperatorHash = oldHash
	if err := f.r.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	route := &istiov1beta1.VirtualService{}
	if err := f.r.Get(ctx, client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: virtualServiceName(agent.Name)}, route); err != nil {
		t.Fatal(err)
	}
	buildVirtualService(agent, f.r.DomainSuffix, f.r.IstioGateway).Spec.DeepCopyInto(&route.Spec)
	for _, http := range route.Spec.Http {
		for _, destination := range http.Route {
			if destination.Destination.Port.Number == 9120 {
				destination.Destination.Port.Number = 9119
			}
		}
	}
	if err := f.r.Update(ctx, route); err != nil {
		t.Fatal(err)
	}
	workload := &appsv1.StatefulSet{}
	if err := f.r.Get(ctx, client.ObjectKey{Namespace: key.Namespace, Name: instanceServiceName(agent.Name)}, workload); err != nil {
		t.Fatal(err)
	}
	before := []client.Object{route.DeepCopy(), secret.DeepCopy(), workload.DeepCopy()}
	chart := &dashboardProfileHelm{}
	f.r.HelmClient = chart
	_, err := f.r.Reconcile(ctx, f.req)
	if err == nil || !strings.Contains(err.Error(), "supported hermes runtime profile") || chart.prepares != 1 || chart.installs != 0 {
		t.Fatalf("old profile did not stop before installation: prepares=%d installs=%d err=%v", chart.prepares, chart.installs, err)
	}
	current := f.agent(t)
	if current.Status.Phase != api.PhaseFailed || current.Spec.Helm.Version != agent.Spec.Helm.Version || *current.Spec.Agent.Image != *agent.Spec.Agent.Image {
		t.Fatal("incompatible profile was accepted or runtime pins changed")
	}
	for _, name := range []string{api.ConditionReady, api.ConditionAgentConfigReady} {
		condition := meta.FindStatusCondition(current.Status.Conditions, name)
		if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "AgentConfigInvalid" {
			t.Fatal("incompatible profile was reported Ready")
		}
	}
	for _, original := range before {
		actual := original.DeepCopyObject().(client.Object)
		if err := f.r.Get(ctx, client.ObjectKeyFromObject(original), actual); err != nil {
			t.Fatal(err)
		}
		got, gotErr := json.Marshal(actual)
		want, wantErr := json.Marshal(original)
		if gotErr != nil || wantErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("rejected upgrade changed %T", original)
		}
	}
}
