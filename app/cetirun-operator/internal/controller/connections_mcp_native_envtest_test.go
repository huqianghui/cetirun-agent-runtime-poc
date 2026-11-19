// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/azure"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/internal/agenttools"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type nativeServedTransport func(*http.Request) (*http.Response, error)

func (f nativeServedTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func servedNativePodIdentity(t *testing.T, h *nativeStartAPIHarness, account *corev1.ServiceAccount, pod *corev1.Pod) agenttools.NativePodIdentity {
	t.Helper()
	request := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
		Audiences: []string{h.r.ConnectionsMCPAudience}, ExpirationSeconds: ptr.To(int64(600)),
		BoundObjectRef: &authenticationv1.BoundObjectReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID},
	}}
	if err := h.admin.SubResource("token").Create(h.ctx, account, request); err != nil {
		t.Fatal("local Pod-bound token request:", err)
	}
	review := &authenticationv1.TokenReview{Spec: authenticationv1.TokenReviewSpec{
		Token: request.Status.Token, Audiences: []string{h.r.ConnectionsMCPAudience},
	}}
	if err := h.admin.Create(h.ctx, review); err != nil {
		t.Fatal("local native TokenReview:", err)
	}
	user := review.Status.User
	if !review.Status.Authenticated || user.UID != string(account.UID) ||
		user.Username != "system:serviceaccount:"+pod.Namespace+":"+account.Name ||
		len(user.Extra["authentication.kubernetes.io/pod-name"]) != 1 ||
		len(user.Extra["authentication.kubernetes.io/pod-uid"]) != 1 {
		t.Fatal("local token did not authenticate the current Pod and ServiceAccount")
	}
	return agenttools.NativePodIdentity{AgentUID: string(h.agent.UID), Namespace: pod.Namespace,
		PodName:            user.Extra["authentication.kubernetes.io/pod-name"][0],
		PodUID:             user.Extra["authentication.kubernetes.io/pod-uid"][0],
		ServiceAccountName: account.Name, ServiceAccountUID: user.UID}
}

func enableServedBrokerIdentity(t *testing.T, h *nativeStartAPIHarness) {
	t.Helper()
	agent := h.current(t)
	team := &api.Team{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Name: agent.Spec.Team}, team); err != nil {
		t.Fatal(err)
	}
	before := &api.AgentPolicy{}
	if err := h.admin.Get(h.ctx, client.ObjectKey{Name: team.Spec.AgentPolicyRef}, before); err != nil {
		t.Fatal(err)
	}
	policy := before.DeepCopy()
	policy.Spec.AgentIdentity = api.AgentPolicyAgentIdentity{Allowed: true, Required: false, CredentialServices: []string{"azure-resource-manager"}}
	ticket, err := h.r.SandboxNativeCoordination.AcquireConfigurationChange(h.ctx, before, policy)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := sandboxcoordination.NewKubernetesChangeAdapter(h.admin, h.admin, before, policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.SandboxNativeCoordination.ReconcileChange(h.ctx, ticket, adapter); err != nil {
		t.Fatal(err)
	}
	reconciler := &AgentPolicyReconciler{Client: h.kube, APIReader: h.kube,
		Source: h.r.SandboxPreparation.Configuration, NativeExecution: true}
	if _, err := reconciler.Reconcile(h.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
		t.Fatal(err)
	}
	agent.Spec.AgentIdentity = &api.AgentIdentitySpec{Enabled: true}
	if agent.Annotations == nil {
		agent.Annotations = map[string]string{}
	}
	agent.Annotations[api.AnnotationOwnerTenantID] = testAgentIdentityTenantID
	agent.Annotations[api.AnnotationOwnerObjectID] = testAgentIdentityOwnerObject
	if err := h.admin.Update(h.ctx, agent); err != nil {
		t.Fatal(err)
	}
	h.r.AgentIdentityEnabled = true
	h.r.AgentIdentityTenantID = testAgentIdentityTenantID
	h.r.AgentIdentityBlueprintClientID = testAgentIdentityBlueprintID
	principal := &azure.ServicePrincipal{ID: testAgentIdentityObjectID, AppID: testAgentIdentityClientID,
		AgentIdentityBlueprintID: testAgentIdentityBlueprintID, DisplayName: h.r.agentIdentityDisplayName(agent)}
	directory := &fakeAgentIdentityDirectory{byObject: principal, createResult: principal}
	h.r.agentIdentityGraphClientFactory = func() (agentIdentityDirectoryClient, error) { return directory, nil }
}

func TestNativeConnectionsServedCurrentSourceProof(t *testing.T) {
	for _, kind := range []string{"openclaw", "hermes", "custom"} {
		t.Run(kind, func(t *testing.T) {
			h := newNativeStartAPIHarnessConfigured(t, true, true, corev1.ReadWriteOnce, kind, nil)
			if kind == "custom" {
				h.create(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-tools"}})
				h.create(t, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "connections", Namespace: "cetirun-tools"},
					Spec: corev1.ServiceSpec{Selector: map[string]string{sandboxpolicy.PeerUIDLabel: "connections-peer"},
						Ports: []corev1.ServicePort{{Name: "https", Protocol: corev1.ProtocolTCP, Port: 443, TargetPort: intstr.FromInt32(8443)}}}})
				withCustomConnections(t)(h.r, h.admin)
			}
			enableServedBrokerIdentity(t, h)
			var nativeVerifier *agenttools.NativeVerifier
			var nativeIdentity agenttools.NativePodIdentity
			broker := &credentialPolicyTestBroker{}
			brokerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && (nativeVerifier == nil || nativeVerifier.Verify(r.Context(), nativeIdentity) != nil) {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				broker.ServeHTTP(w, r)
			}))
			t.Cleanup(brokerServer.Close)
			h.r.AgentIdentityBrokerURL, h.r.AgentIdentityHTTPClient = brokerServer.URL, brokerServer.Client()
			installer := &nativeSandboxInstaller{kube: h.kube, chart: h.localHelm.chart}
			h.installer = installer
			h.r.HelmClient = &sharedSandboxInstaller{nativeSandboxInstaller: installer}
			if _, err := h.r.reconcilePolicy(h.ctx, h.current(t)); err != nil {
				t.Fatal(err)
			}
			if err := h.r.bootstrapReconciledSandbox(h.ctx, h.current(t)); err != nil {
				t.Fatal(err)
			}
			h.create(t, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: instanceNamespace(h.agent.Name)}})
			if kind != "custom" {
				h.create(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-tools"}})
				h.create(t, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "connections", Namespace: "cetirun-tools"},
					Spec: corev1.ServiceSpec{Selector: map[string]string{sandboxpolicy.PeerUIDLabel: "connections-peer"},
						Ports: []corev1.ServicePort{{Name: "https", Protocol: corev1.ProtocolTCP, Port: 443, TargetPort: intstr.FromInt32(8443)}}}})
				withNativeConnections(t)(h.r, h.admin)
			}
			role := &rbacv1.ClusterRole{}
			if err := h.admin.Get(h.ctx, client.ObjectKey{Name: "cetirun-sandbox-admission"}, role); err != nil {
				t.Fatal(err)
			}
			// The harness was installed before native opt-in. Apply the exact
			// additional read-only rule rendered by the production chart.
			role.Rules = append(role.Rules, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"services"},
				ResourceNames: []string{"connections"}, Verbs: []string{"get"}})
			if err := h.admin.Update(h.ctx, role); err != nil {
				t.Fatal(err)
			}
			for range 4 {
				h.reconcileOK(t)
			}
			expected := reconciledExpectationForTest(t, h.nativeSandboxFixture)
			observed := *h.nativeSandboxFixture
			observed.kube = h.admin
			pod := reconciledPodForTest(t, &observed, expected.Workloads[0])
			pod.Spec.Overhead = nil
			h.create(t, pod)
			account := &corev1.ServiceAccount{}
			if err := h.admin.Get(h.ctx, client.ObjectKey{Namespace: pod.Namespace, Name: pod.Spec.ServiceAccountName}, account); err != nil {
				t.Fatal(err)
			}
			if account.Name != "default" {
				t.Fatal("served native path unexpectedly needs dedicated-SA lifecycle")
			}
			webhooks := &admissionv1.ValidatingWebhookConfiguration{}
			if err := h.admin.Get(h.ctx, client.ObjectKey{Name: agenttools.NativePodWebhookConfiguration}, webhooks); err != nil {
				t.Fatal(err)
			}
			target, err := url.Parse(*webhooks.Webhooks[0].ClientConfig.URL)
			if err != nil {
				t.Fatal(err)
			}
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(webhooks.Webhooks[0].ClientConfig.CABundle)
			transport := &http.Transport{DisableKeepAlives: true, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}}
			verifier := agenttools.NativeVerifier{OperatorNamespace: h.r.OperatorNamespace, Audience: h.r.ConnectionsMCPAudience}
			calls := 0
			verifier.Transport = nativeServedTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				copy := r.Clone(r.Context())
				copy.URL.Scheme, copy.URL.Host, copy.Host = target.Scheme, target.Host, target.Host
				response, err := transport.RoundTrip(copy)
				if err == nil {
					raw, readErr := io.ReadAll(response.Body)
					response.Body.Close()
					response.Body = io.NopCloser(bytes.NewReader(raw))
					var result struct {
						Response struct {
							Allowed bool
							Status  struct {
								Message string
								Code    int
							}
						}
					}
					if readErr == nil && json.Unmarshal(raw, &result) == nil && !result.Response.Allowed {
						t.Logf("native admission denial: %d %s", result.Response.Status.Code, result.Response.Status.Message)
					}
				}
				return response, err
			})
			verifier.Get = func(ctx context.Context, path string, output any) error {
				var object client.Object
				parts := strings.Split(strings.Trim(path, "/"), "/")
				key := client.ObjectKey{}
				switch {
				case path == "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations/"+agenttools.NativePodWebhookConfiguration:
					current := &admissionv1.ValidatingWebhookConfiguration{}
					if err := h.admin.Get(ctx, client.ObjectKey{Name: agenttools.NativePodWebhookConfiguration}, current); err != nil {
						return err
					}
					// Envtest has no Service routing. Preserve the real object
					// identity/CA while resolving its production Service address
					// to the same actual local admission server in the transport.
					current.Webhooks[0].ClientConfig.URL = nil
					current.Webhooks[0].ClientConfig.Service = &admissionv1.ServiceReference{
						Namespace: h.r.OperatorNamespace, Name: "cetirun-sandbox-admission",
						Path: ptr.To(agenttools.NativePodWebhookPath), Port: ptr.To[int32](443),
					}
					object = current
				case len(parts) == 4 && parts[0] == "api" && parts[2] == "namespaces":
					object, key.Name = &corev1.Namespace{}, parts[3]
				case len(parts) == 6 && parts[0] == "api" && parts[2] == "namespaces":
					key.Namespace, key.Name = parts[3], parts[5]
					if parts[4] == "pods" {
						object = &corev1.Pod{}
					} else if parts[4] == "serviceaccounts" {
						object = &corev1.ServiceAccount{}
					}
				case len(parts) == 5 && parts[0] == "apis" && parts[3] == "agents":
					object, key.Name = &api.Agent{}, parts[4]
				}
				if object == nil {
					return errors.New("unexpected native authority read")
				}
				if key.Name != "" {
					if err := h.admin.Get(ctx, key, object); err != nil {
						return err
					}
				}
				if pod, ok := object.(*corev1.Pod); ok {
					pod.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "Pod"})
				}
				raw, err := json.Marshal(object)
				if err != nil {
					return err
				}
				return json.Unmarshal(raw, output)
			}
			identity := servedNativePodIdentity(t, h, account, pod)
			nativeVerifier, nativeIdentity = &verifier, identity
			if err := verifier.Verify(h.ctx, identity); err != nil {
				current := h.current(t)
				t.Fatalf("actual common-executor source proof was not accepted: %v calls=%d agentUID=%s currentUID=%s namespace=%q expected=%q podAnno=%s",
					err, calls, identity.AgentUID, current.UID, current.Status.Namespace, identity.Namespace,
					pod.Annotations["cetirun.microsoft.com/agent-uid"])
			}
			h.observeReadyPod(t, pod)
			workload := h.workload(t)
			workload.Status.ObservedGeneration = workload.Generation
			workload.Status.Replicas, workload.Status.ReadyReplicas, workload.Status.UpdatedReplicas = 1, 1, 1
			if err := h.admin.Status().Update(h.ctx, workload); err != nil {
				t.Fatal("synthetic StatefulSet-controller observation:", err)
			}
			keyCount := len(h.gateway.generated)
			for range 3 {
				h.reconcileOK(t)
			}
			current := h.current(t)
			if current.Status.Policy == nil || current.Status.Policy.Phase != "Applied" ||
				!meta.IsStatusConditionTrue(current.Status.Conditions, api.ConditionPolicyApplied) ||
				current.Status.AgentIdentity == nil || current.Status.AgentIdentity.CredentialPolicyRevision == "" ||
				len(h.gateway.generated) != keyCount {
				t.Fatalf("native credential evidence did not complete policy without key churn: phase=%s policy=%+v identity=%+v conditions=%+v gets=%d puts=%d",
					current.Status.Phase, current.Status.Policy, current.Status.AgentIdentity, current.Status.Conditions, broker.gets, broker.puts)
			}
			oldUID := account.UID
			if err := h.admin.Delete(h.ctx, account, client.Preconditions{UID: &oldUID}); err != nil {
				t.Fatal(err)
			}
			replacement := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: account.Name, Namespace: account.Namespace}}
			h.create(t, replacement)
			// Even a new API-issued proof for the replacement SA must not
			// satisfy the old common-executor source pin.
			identity = servedNativePodIdentity(t, h, replacement, pod)
			if err := verifier.Verify(h.ctx, identity); !errors.Is(err, agenttools.ErrNativeProofInvalid) {
				t.Fatalf("recreated source retained native access: %v", err)
			}
			_, _ = h.r.Reconcile(h.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(h.agent)})
			current = h.current(t)
			if meta.IsStatusConditionTrue(current.Status.Conditions, api.ConditionPolicyApplied) ||
				current.Status.AgentIdentity == nil || current.Status.AgentIdentity.CredentialPolicyRevision != "" {
				t.Fatal("lost native source authority retained aggregate credential readiness")
			}
		})
	}
}
