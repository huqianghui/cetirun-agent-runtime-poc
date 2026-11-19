// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/chattest"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestResolveCustomChat(t *testing.T) {
	agent, objects := chattest.Objects()
	c := NewClient(newFakeClient(objects...))
	value, err := c.ResolveChat(context.Background(), agent, chattest.Caller)
	if err != nil || !value.Available || value.Revision == "" || value.BaseURL == "" {
		t.Fatalf("target not resolved: %+v %v", value, err)
	}
	before := value.Revision
	agent.ResourceVersion = "unrelated-status-change"
	value, err = c.ResolveChat(context.Background(), agent, chattest.Caller)
	if err != nil || value.Revision != before {
		t.Fatal("metadata churn changed Chat binding")
	}
}

func TestResolveChatWithoutTeamStillHonorsAgentPolicy(t *testing.T) {
	agent, objects := chattest.Objects()
	agent.Spec.Team = ""
	objects = append(objects[:6], objects[7:]...)
	c := NewClient(newFakeClient(objects...))
	value, err := c.ResolveChat(context.Background(), agent, chattest.Caller)
	if err != nil || !value.Available {
		t.Fatalf("normal unassigned Agent cannot chat: %+v %v", value, err)
	}
	agent.Spec.SandboxRef = "held-sandbox"
	value, err = c.ResolveChat(context.Background(), agent, chattest.Caller)
	if err != nil || value.Available || value.Reason != "sandbox_held" {
		t.Fatalf("unassigned Agent escaped its Sandbox hold: %+v %v", value, err)
	}
}

func TestResolveChatRejectsStaleForeignAndUnsupportedTargets(t *testing.T) {
	cases := map[string]func(*api.Agent, []client.Object){
		"built-in":                 func(a *api.Agent, _ []client.Object) { a.Spec.Agent.Type = api.AgentTypeHermes },
		"headless":                 func(a *api.Agent, _ []client.Object) { a.Spec.Agent.Port = nil },
		"web UI":                   func(a *api.Agent, _ []client.Object) { a.Spec.Agent.WebUI = true },
		"suspended":                func(a *api.Agent, _ []client.Object) { a.Spec.Suspended = true },
		"desired port not applied": func(a *api.Agent, _ []client.Object) { a.Spec.Agent.Port = ptr.To[int32](9000); a.Generation++ },
		"namespace replaced": func(_ *api.Agent, o []client.Object) {
			o[1].(*corev1.Namespace).Annotations["cetirun.microsoft.com/agent-uid"] = "foreign"
		},
		"workload owner": func(_ *api.Agent, o []client.Object) { o[2].(*appsv1.StatefulSet).OwnerReferences[0].UID = "foreign" },
		"old pod": func(_ *api.Agent, o []client.Object) {
			o[3].(*corev1.Pod).Annotations["cetirun.microsoft.com/custom-plan-hash"] = "previous"
		},
		"external service": func(_ *api.Agent, o []client.Object) {
			o[4].(*corev1.Service).Spec.Type = corev1.ServiceTypeExternalName
			o[4].(*corev1.Service).Spec.ExternalName = "outside.example.com"
		},
		"foreign selector": func(_ *api.Agent, o []client.Object) {
			o[4].(*corev1.Service).Spec.Selector = map[string]string{"app": "foreign"}
		},
		"broad caller": func(_ *api.Agent, o []client.Object) {
			o[5].(*networkv1.NetworkPolicy).Spec.Ingress[0].From[0].PodSelector = nil
		},
		"policy-bound Team": func(_ *api.Agent, o []client.Object) { o[6].(*api.Team).Spec.AgentPolicyRef = "held-policy" },
		"missing peer":      func(_ *api.Agent, o []client.Object) { o[5].(*networkv1.NetworkPolicy).Spec.Ingress = nil },
		"retrying route": func(_ *api.Agent, o []client.Object) {
			o[7].(*unstructured.Unstructured).Object["spec"].(map[string]any)["http"].([]any)[0].(map[string]any)["retries"] = map[string]any{"attempts": int64(2)}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			agent, objects := chattest.Objects()
			mutate(agent, objects)
			c := NewClient(newFakeClient(objects...))
			value, err := c.ResolveChat(context.Background(), agent, chattest.Caller)
			if err == nil && value.Available {
				t.Fatalf("accepted %s", name)
			}
		})
	}
}

func TestChatRequiresPrivateRoute(t *testing.T) {
	agent, objects := chattest.Objects()
	c := NewClient(newFakeClient(objects[:7]...))
	value, err := c.ResolveChat(context.Background(), agent, chattest.Caller)
	if err == nil || value.Available {
		t.Fatal("missing route allowed chat forwarding")
	}
}
