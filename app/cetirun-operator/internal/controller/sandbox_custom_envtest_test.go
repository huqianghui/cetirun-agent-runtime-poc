// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"slices"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCommonCustomTerminalAPIServer(t *testing.T) {
	for _, backend := range []api.SandboxType{api.SandboxKata, api.SandboxContainer} {
		t.Run(string(backend), func(t *testing.T) {
			h := newNativeStartAPIHarnessWithRuntime(t, true, true, corev1.ReadWriteOnce, "custom", backend, nil,
				api.AgentPolicyNetwork{Ingress: api.AgentPolicyIngress{Mode: api.AgentPolicyIngressDisabled}})
			h.r.CetiRunHelmValues["ttyd"].(map[string]any)["enabled"] = true
			updateContainerPlatformForTest(t, h.r, h.admin, func(document *SandboxPlatformConfiguration) {
				platform := document.Platforms[backend]
				platform.Topology.KubernetesAPI = []sandboxpolicy.Destination{{
					Peer:  sandboxpolicy.Peer{Name: "api", UID: "synthetic-api", IPs: []string{"203.0.113.10"}},
					Ports: []api.AgentPolicyPort{{Port: 6443, Protocol: corev1.ProtocolTCP}},
				}}
				document.Platforms[backend] = platform
			})
			h = startReconciledAPIHarness(t, h)
			expected := reconciledExpectationForTest(t, h.nativeSandboxFixture)
			if expected.Terminal == nil || len(expected.Workloads) != 2 {
				t.Fatal("served common terminal did not join the protected workload inventory")
			}
			gate := &admissionv1.ValidatingAdmissionPolicy{}
			gateBinding := &admissionv1.ValidatingAdmissionPolicyBinding{}
			gateKey := client.ObjectKey{Name: instanceServiceName(h.agent.Name) + "-" + expected.Namespace.Name + "-deny-exec"}
			if err := h.admin.Get(h.ctx, gateKey, gate); err != nil {
				t.Fatal("common terminal exec gate was not served:", err)
			}
			if err := h.admin.Get(h.ctx, gateKey, gateBinding); err != nil {
				t.Fatal("common terminal exec binding was not served:", err)
			}
			gateVersion, bindingVersion := gate.ResourceVersion, gateBinding.ResourceVersion
			if err := h.r.ensureCommonTerminalExecPolicy(h.ctx, h.current(t)); err != nil {
				t.Fatal(err)
			}
			if err := h.admin.Get(h.ctx, gateKey, gate); err != nil {
				t.Fatal(err)
			}
			if err := h.admin.Get(h.ctx, gateKey, gateBinding); err != nil {
				t.Fatal(err)
			}
			if gate.ResourceVersion != gateVersion || gateBinding.ResourceVersion != bindingVersion {
				t.Fatal("API defaults caused a steady terminal exec gate write")
			}
			observed := *h.nativeSandboxFixture
			observed.kube = h.admin
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "terminal-node",
				Labels: map[string]string{corev1.LabelTopologyRegion: "example-region", corev1.LabelTopologyZone: "example-zone"}}}
			h.create(t, node)
			for _, workload := range expected.Workloads {
				pod := reconciledPodForTest(t, &observed, workload)
				pod.Spec.Overhead = nil
				h.create(t, pod)
				if err := h.admin.SubResource("binding").Create(h.ctx, pod, &corev1.Binding{
					ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace, UID: pod.UID},
					Target:     corev1.ObjectReference{Kind: "Node", Name: node.Name},
				}); err != nil {
					t.Fatal("bind approved workload:", err)
				}
				if err := h.admin.Get(h.ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{corev1.LabelTopologyRegion, corev1.LabelTopologyZone} {
					if pod.Labels[key] != node.Labels[key] {
						t.Fatal("API-server binding did not apply observed Node topology")
					}
				}
				if err := h.admin.Update(h.ctx, pod.DeepCopy(), client.DryRunAll); err != nil {
					t.Fatal("current bound workload was rejected:", err)
				}
				forged := pod.DeepCopy()
				forged.Labels[corev1.LabelTopologyZone] = "unapproved-zone"
				if err := h.admin.Update(h.ctx, forged, client.DryRunAll); err == nil {
					t.Fatal("forged bound topology was accepted")
				}
				if workload.Kind == "Deployment" {
					if pod.Spec.ServiceAccountName != expected.Terminal.ServiceAccountName ||
						(backend == api.SandboxKata) != (len(pod.Spec.Overhead) != 0) {
						t.Fatal("served terminal lost its identity or RuntimeClass overhead")
					}
					changed := pod.DeepCopy()
					changed.ResourceVersion, changed.UID = "", ""
					changed.Name += "-changed"
					changed.Spec.Volumes = append(changed.Spec.Volumes, corev1.Volume{
						Name: "foreign-home", VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: expected.Home.Name},
						},
					})
					if err := h.admin.Create(h.ctx, changed, client.DryRunAll); err == nil {
						t.Fatal("terminal was allowed to mount Agent HOME")
					}
				}
			}
			config := rest.CopyConfig(h.localHelm.config)
			config.Impersonate = rest.ImpersonationConfig{
				UserName: "system:serviceaccount:" + expected.Namespace.Name + ":" + expected.Terminal.ServiceAccountName,
				Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:" + expected.Namespace.Name, "system:authenticated"},
			}
			reader, err := client.New(config, client.Options{Scheme: h.admin.Scheme()})
			if err != nil {
				t.Fatal(err)
			}
			h.assertAccess(t, reader, authorizationv1.ResourceAttributes{
				Group: api.GroupVersion.Group, Resource: "agents", Verb: "get", Name: h.agent.Name,
			}, true)
			h.assertAccess(t, reader, authorizationv1.ResourceAttributes{
				Group: api.GroupVersion.Group, Resource: "agents", Verb: "get", Name: "foreign-agent",
			}, false)
			h.assertAccess(t, reader, authorizationv1.ResourceAttributes{
				Resource: "secrets", Verb: "get", Namespace: expected.Namespace.Name, Name: sandboxRuntimeSecretName(h.agent),
			}, false)
		})
	}
}

func TestCommonCustomAPIServer(t *testing.T) {
	testCommonCustomAPIServer(t, false)
}

func TestCommonCustomObservedSNIAPIServer(t *testing.T) {
	testCommonCustomAPIServer(t, true)
}

func testCommonCustomAPIServer(t *testing.T, capture bool) {
	t.Helper()
	for _, backend := range []api.SandboxType{api.SandboxKata, api.SandboxContainer} {
		for _, endpoint := range []string{"headless", "chat", "web-ui"} {
			t.Run(string(backend)+"/"+endpoint, func(t *testing.T) {
				mode := corev1.ReadWriteOnce
				if endpoint == "headless" {
					mode = corev1.ReadWriteOncePod
				}
				h := newNativeStartAPIHarnessWithRuntime(t, true, true, mode, "custom", backend, nil)
				if endpoint != "chat" {
					agent := h.current(t)
					before := agent.DeepCopy()
					if endpoint == "headless" {
						agent.Spec.Agent.Port = nil
					} else {
						agent.Spec.Agent.WebUI = true
					}
					if err := h.member.Patch(h.ctx, agent, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
						t.Fatal("served Custom endpoint request:", err)
					}
				}
				if capture {
					withObservedSNI(t, "custom")(h.r, h.admin)
					withApprovedCustomUser(t)(h.r, h.admin)
					controller := &AgentPolicyReconciler{Client: h.kube, APIReader: h.admin,
						Source: h.r.SandboxPreparation.Configuration, NativeExecution: true}
					if _, err := controller.Reconcile(h.ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: "policy-example"}}); err != nil {
						t.Fatal(err)
					}
				}
				h = startReconciledAPIHarness(t, h)
				current := h.current(t)
				if current.Spec.GatewayTokenSecretRef != "" {
					t.Fatal("Custom persisted a caller-controlled runtime Secret reference")
				}
				expected := reconciledExpectationForTest(t, h.nativeSandboxFixture)
				workload := h.workload(t)
				main := workload.Spec.Template.Spec.Containers[0]
				if len(main.Command) != 0 || len(main.Args) != 0 || !ptr.Deref(main.SecurityContext.RunAsNonRoot, false) ||
					(capture && ptr.Deref(main.SecurityContext.RunAsUser, 0) != 1024) ||
					(!capture && main.SecurityContext.RunAsUser != nil) {
					t.Fatal("served Custom template changed the image entrypoint or approved user contract")
				}
				secret := &corev1.Secret{}
				if err := h.admin.Get(h.ctx, client.ObjectKey{
					Namespace: expected.Namespace.Name, Name: sandboxRuntimeSecretName(current),
				}, secret); err != nil {
					t.Fatal(err)
				}
				if !hasAgentOwner(secret, current) || len(secret.Data["CETIRUN_AUTH_KEY"]) == 0 {
					t.Fatal("runtime authentication was not provisioned into the owned bootstrap Secret")
				}
				sources := w.PodSourceReferences(workload.Spec.Template.Spec, workload.Namespace)
				if slices.ContainsFunc(sources, func(source w.SourceReference) bool {
					return source.Kind == "Secret" && source.Name == secret.Name
				}) {
					t.Fatal("Custom main received its controller-only runtime authentication")
				}
				var contextSource *corev1.ConfigMap
				for _, volume := range workload.Spec.Template.Spec.Volumes {
					if volume.Name == "context" && volume.ConfigMap != nil {
						contextSource = &corev1.ConfigMap{}
						if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: workload.Namespace, Name: volume.ConfigMap.Name}, contextSource); err != nil {
							t.Fatal(err)
						}
					}
				}
				if contextSource == nil || !hasAgentOwner(contextSource, current) || !ptr.Deref(contextSource.Immutable, false) ||
					contextSource.Data["AGENTS.md"] == "" || len(contextSource.Data) != 1 {
					t.Fatal("Custom canonical context was not an immutable owned snapshot")
				}
				config := rest.CopyConfig(h.localHelm.config)
				config.Impersonate = rest.ImpersonationConfig{
					UserName: "system:serviceaccount:cetirun-system:cetirun-sandbox-admission",
					Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:cetirun-system", "system:authenticated"},
				}
				reader, err := client.New(config, client.Options{Scheme: h.admin.Scheme()})
				if err != nil {
					t.Fatal(err)
				}
				if err := reader.Get(h.ctx, client.ObjectKeyFromObject(contextSource), &corev1.ConfigMap{}); err != nil {
					t.Fatal("admission cannot read its pinned Custom context:", err)
				}
				h.assertAccess(t, reader, authorizationv1.ResourceAttributes{
					Resource: "configmaps", Verb: "get", Namespace: workload.Namespace, Name: "unrelated-context",
				}, false)
				h.assertAccess(t, reader, authorizationv1.ResourceAttributes{
					Resource: "secrets", Verb: "get", Namespace: workload.Namespace, Name: secret.Name,
				}, true)
				h.assertAccess(t, reader, authorizationv1.ResourceAttributes{
					Resource: "secrets", Verb: "get", Namespace: workload.Namespace, Name: "unrelated-secret",
				}, false)
				h.assertAccess(t, reader, authorizationv1.ResourceAttributes{
					Resource: "secrets", Verb: "list", Namespace: workload.Namespace,
				}, false)
				service := &corev1.Service{}
				if err := h.admin.Get(h.ctx, client.ObjectKeyFromObject(workload), service); err != nil {
					t.Fatal(err)
				}
				if endpoint == "headless" && (service.Spec.ClusterIP != corev1.ClusterIPNone || len(service.Spec.Ports) != 0) {
					t.Fatal("headless Custom did not retain its portless governing Service")
				}
				observed := *h.nativeSandboxFixture
				observed.kube = h.admin
				pod := reconciledPodForTest(t, &observed, expected.Workloads[0])
				pod.Spec.Overhead = nil
				for _, mutate := range []func(*corev1.Pod){
					func(p *corev1.Pod) { p.Spec.Containers[0].Command = []string{"changed"} },
					func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext.RunAsUser = ptr.To[int64](0) },
					func(p *corev1.Pod) { p.Spec.RuntimeClassName = ptr.To("unapproved-runtime") },
				} {
					changed := pod.DeepCopy()
					mutate(changed)
					if err := h.admin.Create(h.ctx, changed, client.DryRunAll); err == nil {
						t.Fatal("served Custom Pod admission accepted a changed runtime contract")
					}
				}
				h.create(t, pod)
				h.observeReadyPod(t, pod)
				workload = h.workload(t)
				workload.Status = appsv1.StatefulSetStatus{
					ObservedGeneration: workload.Generation, Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1,
					UpdateRevision: workload.Status.UpdateRevision,
				}
				if err := h.admin.Status().Update(h.ctx, workload); err != nil {
					t.Fatal(err)
				}
				h.reconcileOK(t)
				h.reconcileOK(t)
				if h.current(t).Status.Phase != api.PhaseReady || !isConditionTrue(h.current(t).Status.Conditions, api.ConditionPolicyApplied) {
					t.Fatal("served Custom did not reach current Ready and applied policy")
				}
				changed := contextSource.DeepCopy()
				changed.Data["AGENTS.md"] = "changed"
				if err := h.admin.Update(h.ctx, changed); !apierrors.IsInvalid(err) {
					t.Fatal("served API allowed immutable context mutation:", err)
				}
				assertNativeHomeAccessMode(t, h.nativeSandboxFixture, mode)
				if err := h.admin.Delete(h.ctx, pod, client.GracePeriodSeconds(0)); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
