// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	admissionhandler "github.com/Azure/CetiRun/app/cetirun-operator/internal/admission"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/egresscapture"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/ingressmtls"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReconciledSandboxObservedSNILifecycle(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, mtls := range []bool{false, true} {
			t.Run(kind+"/"+map[bool]string{false: "fqdn-only", true: "both"}[mtls], func(t *testing.T) {
				options := []func(*AgentReconciler, client.Client){withNativeHomeAccessMode(t, corev1.ReadWriteOnce)}
				if mtls {
					options = append(options, withIngressMTLS(t, kind))
				}
				options = append(options, withObservedSNI(t, kind))
				f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false, options...)
				ctx := context.Background()
				initial := reconciledExpectationForTest(t, f)
				keyID := f.current(t).Status.AIGateway.KeyID
				request := "request-fqdn0001"
				agent := f.current(t)
				agent.Spec.RestartRequestID = request
				agent.Generation++
				if err := f.kube.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				for range 3 {
					f.reconcileOK(t)
				}
				workload := f.workload(t)
				if workload.Spec.Template.Annotations[api.AnnotationRestartRequestID] != request ||
					workload.Spec.Template.Spec.InitContainers[1].Name != egresscapture.InitContainerName {
					t.Fatal("retained restart did not apply the actual capture workload")
				}
				if f.current(t).Status.ObservedRestartRequestID == request {
					t.Fatal("a template alone acknowledged restart")
				}
				pod := &corev1.Pod{ObjectMeta: *workload.Spec.Template.ObjectMeta.DeepCopy(), Spec: *workload.Spec.Template.Spec.DeepCopy()}
				pod.Name, pod.Namespace, pod.UID = workload.Name+"-0", workload.Namespace, types.UID("fqdn-restart-pod")
				pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(workload, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}
				pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				for _, container := range pod.Spec.InitContainers {
					state := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, FinishedAt: metav1.Now()}}
					if container.RestartPolicy != nil {
						state = corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}}
					}
					pod.Status.InitContainerStatuses = append(pod.Status.InitContainerStatuses, corev1.ContainerStatus{
						Name: container.Name, Ready: true, Started: ptr.To(true), State: state,
					})
				}
				if err := f.kube.Create(ctx, pod); err != nil {
					t.Fatal(err)
				}
				workload.Status = appsv1.StatefulSetStatus{ObservedGeneration: workload.Generation, ReadyReplicas: 1, UpdatedReplicas: 1}
				if err := f.kube.Status().Update(ctx, workload); err != nil {
					t.Fatal(err)
				}
				f.reconcileOK(t)
				if f.current(t).Status.ObservedRestartRequestID != request {
					t.Fatal("current ready capture Pod did not acknowledge restart")
				}
				agent = f.current(t)
				agent.Spec.Suspended = true
				agent.Generation++
				if err := f.kube.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				f.reconcileOK(t)
				if ptr.Deref(f.workload(t).Spec.Replicas, 1) != 0 || !f.current(t).Status.AIGateway.Blocked {
					t.Fatal("suspend did not stop desired execution and model access")
				}
				if err := f.kube.Delete(ctx, pod); err != nil {
					t.Fatal(err)
				}
				workload = f.workload(t)
				workload.Status = appsv1.StatefulSetStatus{}
				if err := f.kube.Status().Update(ctx, workload); err != nil {
					t.Fatal(err)
				}
				policy := &api.AgentPolicy{}
				if err := f.kube.Get(ctx, client.ObjectKey{Name: testfixture.New(kind).Policy.Name}, policy); err != nil {
					t.Fatal(err)
				}
				before := policy.DeepCopy()
				policy.Spec.Network.BlockedFQDNs = nil
				policy.Generation++
				updateObservedSNIPolicy(t, f.r, f.kube, before, policy)
				for range 3 {
					f.reconcileOK(t)
				}
				agent = f.current(t)
				agent.Spec.Suspended = false
				agent.Generation++
				if err := f.kube.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				for range 3 {
					f.reconcileOK(t)
				}
				current := reconciledExpectationForTest(t, f)
				if current.Home != initial.Home || current.Namespace != initial.Namespace ||
					f.current(t).Status.AIGateway.KeyID != keyID {
					t.Fatal("capture lifecycle replaced retained identity or storage")
				}
				spec := f.workload(t).Spec.Template.Spec
				if slices.ContainsFunc(spec.InitContainers, func(container corev1.Container) bool {
					return container.Name == egresscapture.InitContainerName || container.Name == ingressmtls.ContainerName
				}) || slices.Contains(spec.Containers[0].Env, corev1.EnvVar{Name: "CETIRUN_EGRESS_SNI", Value: "true"}) {
					t.Fatal("capture removal retained init rules or readiness state")
				}
				proxyCount := 0
				for _, container := range spec.Containers {
					if container.Name == ingressmtls.ContainerName {
						proxyCount++
					}
				}
				if proxyCount != map[bool]int{false: 0, true: 1}[mtls] {
					t.Fatal("capture removal lost or duplicated the independent mTLS endpoint")
				}
			})
		}
	}
}
func withObservedSNI(t *testing.T, kind string) func(*AgentReconciler, client.Client) {
	t.Helper()
	return func(r *AgentReconciler, kube client.Client) {
		ctx := context.Background()
		cm := &corev1.ConfigMap{}
		if err := kube.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: "sandbox-platform"}, cm); err != nil {
			t.Fatal(err)
		}
		var configuration SandboxPlatformConfiguration
		if err := json.Unmarshal([]byte(cm.Data[SandboxPlatformConfigurationKey]), &configuration); err != nil {
			t.Fatal(err)
		}
		var boxes api.SandboxList
		if err := kube.List(ctx, &boxes); err != nil || len(boxes.Items) != 1 {
			t.Fatal("capture fixture requires one selected Sandbox:", err)
		}
		backend := boxes.Items[0].Spec.Type
		registration := configuration.Registrations[backend]
		registration.Capabilities.BlockedFQDNs = true
		configuration.Registrations[backend] = registration
		platform := configuration.Platforms[backend]
		platform.EgressSNI = &egresscapture.Registration{
			EnvoyImage: "registry.example.com/envoy@sha256:" + strings.Repeat("a", 64),
			InitImage:  "registry.example.com/cetirun-base@sha256:" + strings.Repeat("b", 64),
		}
		if platform.IngressMTLS != nil {
			platform.EgressSNI.EnvoyImage = platform.IngressMTLS.Image
		}
		configuration.Platforms[backend] = platform
		raw, err := json.Marshal(configuration)
		if err != nil {
			t.Fatal(err)
		}
		cm.Data[SandboxPlatformConfigurationKey] = string(raw)
		if err := kube.Update(ctx, cm); err != nil {
			t.Fatal(err)
		}
		policy := &api.AgentPolicy{}
		if err := kube.Get(ctx, client.ObjectKey{Name: testfixture.New(kind).Policy.Name}, policy); err != nil {
			t.Fatal(err)
		}
		before := policy.DeepCopy()
		policy.Spec.Network.BlockedFQDNs = []string{"blocked.example.com"}
		updateObservedSNIPolicy(t, r, kube, before, policy)
	}
}

func updateObservedSNIPolicy(t *testing.T, r *AgentReconciler, kube client.Client, before, after *api.AgentPolicy) {
	t.Helper()
	ctx := context.Background()
	if r.SandboxNativeCoordination == nil {
		if err := kube.Update(ctx, after); err != nil {
			t.Fatal(err)
		}
		return
	}
	ticket, err := r.SandboxNativeCoordination.AcquireConfigurationChange(ctx, before, after)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := sandboxcoordination.NewKubernetesChangeAdapter(kube, kube, before, after)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.SandboxNativeCoordination.ReconcileChange(ctx, ticket, adapter); err != nil {
		t.Fatal(err)
	}
}

func TestReconciledSandboxObservedSNIWithEntraAndConnections(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := nativeEntraFixture(t, kind, withObservedSNI(t, kind), withNativeConnections(t))
			f.reconcileOK(t)
			current := f.current(t)
			expected := reconciledExpectationForTest(t, f)
			state, receipt, err := f.r.readSandboxBootstrap(ctx, current)
			if err != nil {
				t.Fatal(err)
			}
			identity := expected.IdentityServiceAccount
			if identity == nil || receipt.ServiceAccount == nil || identity.UID != receipt.ServiceAccount.UID ||
				identity.Annotations[sandboxBootstrapAnnotation] != string(state.UID) {
				t.Fatal("observed-SNI controls lost the bootstrap-owned Entra identity source")
			}
			workload := f.workload(t)
			if workload.Spec.Template.Spec.ServiceAccountName != identity.Name ||
				!slices.ContainsFunc(workload.Spec.Template.Spec.InitContainers, func(container corev1.Container) bool {
					return container.Name == egresscapture.InitContainerName
				}) {
				t.Fatal("combined workload lost the Entra account or observed-SNI capture")
			}
			contract := &w.ConnectionsMCP{URL: f.r.ConnectionsMCPURL, Namespace: "cetirun-tools",
				Audience: f.r.ConnectionsMCPAudience, CASecretName: f.r.ConnectionsMCPCASecretName}
			if err := contract.ValidatePod(workload.Spec.Template.Spec); err != nil {
				t.Fatal("combined workload lost the independent Connections proof:", err)
			}
			policy, err := f.r.resolvePolicy(ctx, current)
			if err != nil {
				t.Fatal(err)
			}
			if policy.sandboxRevision != expected.Revision {
				t.Fatal("policy and workload compilation disagree on the combined control inputs")
			}
			handler, err := admissionhandler.NewPodHandler(f.kube, admissionhandler.PodConfig{
				StateNamespace: f.r.OperatorNamespace, CoordinationConfigMapName: f.r.SandboxNativeCoordination.Identity.Name,
				CoordinationConfigMapUID: string(f.r.SandboxNativeCoordination.Identity.UID),
			})
			if err != nil {
				t.Fatal(err)
			}
			pod := reconciledPodForTest(t, f, expected.Workloads[0])
			if response := handler.Validate(ctx, reconciledPodRequest(t, pod)); !response.Allowed {
				t.Fatal("combined controls failed current Pod admission:", response.Result)
			}
		})
	}
}

func TestReconciledSandboxObservedSNIWithKubernetesAccess(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			input := testfixture.New(kind)
			f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false,
				withObservedSNI(t, kind), func(r *AgentReconciler, author client.Client) {
					configureReconciledKubernetesAccess(t, r, author, input.Agent.Name, input.Policy.Name)
					installKubernetesFixtureCA(t, r, author, input.Agent.Name)
				})
			f.reconcileOK(t)
			assertKubernetesFixtureGrants(t, f, true)
			expected := reconciledExpectationForTest(t, f)
			pod := reconciledPodForTest(t, f, expected.Workloads[0])
			if pod.Spec.ServiceAccountName != kubernetesFixtureAccount ||
				ptr.Deref(pod.Spec.AutomountServiceAccountToken, true) ||
				len(pod.Spec.InitContainers) < 3 ||
				pod.Spec.InitContainers[0].Name != ingressmtls.ContainerName ||
				pod.Spec.InitContainers[1].Name != egresscapture.InitContainerName {
				t.Fatal("combined workload lost its explicit Kubernetes identity or observed-SNI capture")
			}
			handler, err := admissionhandler.NewPodHandler(f.kube, admissionhandler.PodConfig{
				StateNamespace: f.r.OperatorNamespace,
			})
			if err != nil {
				t.Fatal(err)
			}
			if response := handler.Validate(context.Background(), reconciledPodRequest(t, pod)); !response.Allowed {
				t.Fatal("combined Kubernetes and observed-SNI controls failed current Pod admission:", response.Result)
			}
		})
	}
}

func TestReconciledSandboxObservedSNI(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes"} {
		for _, mtls := range []bool{false, true} {
			t.Run(kind+"/"+map[bool]string{false: "fqdn-only", true: "both"}[mtls], func(t *testing.T) {
				options := []func(*AgentReconciler, client.Client){withNativeHomeAccessMode(t, corev1.ReadWriteOnce)}
				if mtls {
					options = append(options, withIngressMTLS(t, kind))
				}
				options = append(options, withObservedSNI(t, kind))
				f := newSandboxRuntimeFixtureForAgent(t, true, nil, true, kind, true, false, options...)
				f.reconcileOK(t)
				expected := reconciledExpectationForTest(t, f)
				workload := f.workload(t)
				spec := workload.Spec.Template.Spec
				if len(spec.InitContainers) < 3 || spec.InitContainers[0].Name != ingressmtls.ContainerName ||
					ptr.Deref(spec.InitContainers[0].RestartPolicy, "") != corev1.ContainerRestartPolicyAlways ||
					spec.InitContainers[1].Name != egresscapture.InitContainerName ||
					ptr.Deref(spec.InitContainers[0].SecurityContext.RunAsUser, 0) != egresscapture.ProxyUID ||
					!slices.Equal(spec.InitContainers[1].SecurityContext.Capabilities.Add, []corev1.Capability{"NET_ADMIN"}) {
					t.Fatal("compiled capture startup sequence was not applied to the workload")
				}
				for _, container := range spec.Containers {
					if container.Name == ingressmtls.ContainerName || len(container.SecurityContext.Capabilities.Add) != 0 ||
						ptr.Deref(container.SecurityContext.RunAsUser, 0) == egresscapture.ProxyUID {
						t.Fatal("duplicated Envoy or delegated capture privilege")
					}
				}
				config := &corev1.ConfigMap{}
				if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: workload.Namespace, Name: ingressmtls.ConfigName}, config); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(config.Data["capture.sh"], "--uid-owner 1337") ||
					strings.Contains(config.Data["envoy.json"], "DownstreamTlsContext") != mtls {
					t.Fatal("compiler-owned shared proxy config was not applied correctly")
				}
				exportObservedSNIProof(t, f, kind, mtls, spec, config)
				platformSource := &corev1.ConfigMap{}
				if err := f.kube.Get(context.Background(), client.ObjectKey{Namespace: f.r.OperatorNamespace, Name: "sandbox-platform"}, platformSource); err != nil {
					t.Fatal(err)
				}
				if !slices.ContainsFunc(expected.Sources, func(ref w.SourceReference) bool {
					return ref.Kind == "ConfigMap" && ref.Name == platformSource.Name &&
						ref.UID == platformSource.UID && ref.ResourceVersion == platformSource.ResourceVersion
				}) {
					t.Fatal("capture image/capability input is not guarded by actual source UID/version")
				}
				if !slices.ContainsFunc(expected.Controls, func(object unstructured.Unstructured) bool {
					data, _, _ := unstructured.NestedStringMap(object.Object, "data")
					return object.GetKind() == "ConfigMap" && object.GetName() == config.Name &&
						data["envoy.json"] == config.Data["envoy.json"] && data["capture.sh"] == config.Data["capture.sh"]
				}) {
					t.Fatal("capture ConfigMap output is absent from exact desired-control validation")
				}
				if !mtls && slices.ContainsFunc(expected.Sources, func(ref w.SourceReference) bool {
					return ref.Kind == "Secret" && (ref.Name == "agent-ingress-tls" || ref.Name == "gateway-agent-example")
				}) {
					t.Fatal("FQDN-only execution acquired ingress certificate dependencies")
				}
				handler, err := admissionhandler.NewPodHandler(f.kube, admissionhandler.PodConfig{StateNamespace: f.r.OperatorNamespace})
				if err != nil {
					t.Fatal(err)
				}
				pod := reconciledPodForTest(t, f, expected.Workloads[0])
				if response := handler.Validate(context.Background(), reconciledPodRequest(t, pod)); !response.Allowed {
					t.Fatal("actual Pod admission rejected complete capture wiring:", response.Result)
				}
				for name, mutate := range map[string]func(*corev1.Pod){
					"missing capture":  func(p *corev1.Pod) { p.Spec.InitContainers = slices.Delete(p.Spec.InitContainers, 1, 2) },
					"wrong init image": func(p *corev1.Pod) { p.Spec.InitContainers[1].Image = "registry.example.com/unapproved:latest" },
					"agent NET_ADMIN": func(p *corev1.Pod) {
						p.Spec.Containers[0].SecurityContext.Capabilities.Add = []corev1.Capability{"NET_ADMIN"}
					},
					"agent proxy UID": func(p *corev1.Pod) {
						p.Spec.Containers[0].SecurityContext.RunAsUser = ptr.To[int64](egresscapture.ProxyUID)
					},
					"wrong init order": func(p *corev1.Pod) {
						p.Spec.InitContainers[0], p.Spec.InitContainers[1] = p.Spec.InitContainers[1], p.Spec.InitContainers[0]
					},
					"private admin mount": func(p *corev1.Pod) {
						p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: egresscapture.PrivateVolumeName, MountPath: "/admin"})
					},
				} {
					t.Run(name, func(t *testing.T) {
						changed := pod.DeepCopy()
						mutate(changed)
						if response := handler.Validate(context.Background(), reconciledPodRequest(t, changed)); response.Allowed {
							t.Fatal("admission accepted drift in protected capture wiring")
						}
					})
				}
				config.Data["capture.sh"] = "# removed"
				if err := f.kube.Update(context.Background(), config); err != nil {
					t.Fatal(err)
				}
				if response := handler.Validate(context.Background(), reconciledPodRequest(t, pod)); response.Allowed {
					t.Fatal("admission accepted stale capture ConfigMap source version")
				}
			})
		}
	}
}
