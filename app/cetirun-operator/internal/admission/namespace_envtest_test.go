// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package sandboxadmission

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestProtectedNamespaceRoutingAPIServer(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, binary := range []string{"kube-apiserver", "etcd"} {
		info, err := os.Stat(filepath.Join(assets, binary))
		if assets == "" || err != nil || info.IsDir() || info.Mode()&0111 == 0 {
			t.Fatal("set KUBEBUILDER_ASSETS to preinstalled Kubernetes 1.37 envtest binaries")
		}
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true,
		ControlPlaneStartTimeout: time.Minute, ControlPlaneStopTimeout: 30 * time.Second,
	}
	env.ControlPlane.GetAPIServer().Configure().Set("authorization-mode", "RBAC")
	cfg, err := env.Start()
	if err != nil {
		_ = env.Stop()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, corev1.AddToScheme, admissionregistrationv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	newClient := func(config *rest.Config) client.Client {
		t.Helper()
		result, err := client.New(config, client.Options{Scheme: scheme})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	cl := newClient(cfg)
	userClient := func(name string) client.Client {
		t.Helper()
		user, err := env.AddUser(envtest.User{Name: name, Groups: []string{"system:masters"}}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return newClient(user.Config())
	}
	member := userClient("namespace-admin@example.com")
	controller := userClient("system:serviceaccount:cetirun-system:cetirun-operator-controller-manager")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	create := func(c client.Client, object client.Object) {
		t.Helper()
		if err := c.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	var unavailable atomic.Bool
	var calls atomic.Int32
	webhook := &admission.Webhook{Handler: admission.HandlerFunc(func(context.Context, admission.Request) admission.Response {
		return admission.Allowed("routing fixture; final Pod content has its own admission suite")
	})}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if unavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		webhook.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	policy, binding, podWebhook := renderedPodAdmission(t, ca)
	podWebhook.Webhooks[0].ClientConfig.Service = nil
	podWebhook.Webhooks[0].ClientConfig.URL = ptr.To(server.URL + PodValidationPath)
	podWebhook.Webhooks[0].TimeoutSeconds = ptr.To[int32](1)
	create(cl, policy)
	create(cl, binding)
	create(cl, podWebhook)

	protected := func(name string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"cetirun.microsoft.com/sandbox-protected": "true"},
			Annotations: map[string]string{
				"cetirun.microsoft.com/agent-name": "example-agent",
				"cetirun.microsoft.com/agent-uid":  "example-agent-uid",
			},
			OwnerReferences: []metav1.OwnerReference{sandboxworkload.AgentOwner("example-agent", "example-agent-uid")},
		}}
	}
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		probe := protected("")
		probe.GenerateName = "namespace-guard-probe-"
		err := member.Create(ctx, probe)
		if err == nil {
			return false, nil
		}
		if apierrors.IsForbidden(err) && strings.Contains(err.Error(), "Only the configured controller") {
			return true, nil
		}
		return false, err
	}); err != nil {
		t.Fatalf("rendered namespace policy did not become effective: %v", err)
	}

	ns := protected("protected-workload")
	create(controller, ns)
	ordinary := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ordinary-workload"}}
	create(member, ordinary)
	create(cl, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-system"}})
	for _, namespace := range []string{ns.Name, ordinary.Name} {
		create(cl, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "runner", Namespace: namespace}})
	}
	for name, change := range map[string]func(*corev1.Namespace){
		"remove marker": func(object *corev1.Namespace) {
			delete(object.Labels, "cetirun.microsoft.com/sandbox-protected")
		},
		"disable marker": func(object *corev1.Namespace) {
			object.Labels["cetirun.microsoft.com/sandbox-protected"] = "false"
		},
		"add AKS control-plane exclusion": func(object *corev1.Namespace) {
			object.Labels["control-plane"] = "true"
		},
		"add AKS managed namespace exclusion": func(object *corev1.Namespace) {
			object.Labels["kubernetes.azure.com/managedby"] = "aks"
		},
		"change Agent UID": func(object *corev1.Namespace) {
			object.Annotations["cetirun.microsoft.com/agent-uid"] = "replacement"
		},
		"change Agent name": func(object *corev1.Namespace) {
			object.Annotations["cetirun.microsoft.com/agent-name"] = "replacement"
		},
		"remove owner": func(object *corev1.Namespace) { object.OwnerReferences = nil },
	} {
		t.Run(name, func(t *testing.T) {
			for _, actor := range []client.Client{member, controller} {
				changed := ns.DeepCopy()
				change(changed)
				if err := actor.Update(ctx, changed); !apierrors.IsForbidden(err) {
					t.Fatalf("protected tuple change must be rejected, including for the controller: %v", err)
				}
			}
		})
	}
	state := &corev1.Namespace{}
	if err := cl.Get(ctx, client.ObjectKey{Name: "cetirun-system"}, state); err != nil {
		t.Fatal(err)
	}
	state.Labels, state.Annotations, state.OwnerReferences = ns.Labels, ns.Annotations, ns.OwnerReferences
	if err := controller.Update(ctx, state); !apierrors.IsForbidden(err) {
		t.Fatalf("controller state namespace must not enter Pod admission: %v", err)
	}

	pod := func(name, namespace string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: corev1.PodSpec{ServiceAccountName: "runner", Containers: []corev1.Container{{Name: "workload", Image: "example.com/agent:v1"}}},
		}
	}
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		probe := pod("", ns.Name)
		probe.GenerateName = "pod-routing-probe-"
		if err := member.Create(ctx, probe); err != nil {
			return false, err
		}
		return calls.Load() > 0, nil
	}); err != nil {
		t.Fatal(err)
	}
	existing := pod("cleanup", ns.Name)
	create(member, existing)
	unavailable.Store(true)
	if err := member.Create(ctx, pod("blocked", ns.Name)); err == nil || !strings.Contains(err.Error(), "webhook") {
		t.Fatalf("protected Pod create must fail closed during the outage: %v", err)
	}
	beforeCalls := calls.Load()
	create(member, pod("unaffected", ordinary.Name))
	existing.Status.Phase = corev1.PodPending
	if err := member.Status().Update(ctx, existing); err != nil {
		t.Fatalf("Pod status must stay independent of webhook availability: %v", err)
	}
	if err := member.Delete(ctx, existing); err != nil {
		t.Fatalf("Pod deletion must stay independent of webhook availability: %v", err)
	}
	ns.Annotations["example.com/note"] = "unrelated metadata remains writable"
	if err := member.Update(ctx, ns); err != nil {
		t.Fatalf("namespace metadata repair must not depend on the webhook: %v", err)
	}
	if calls.Load() != beforeCalls {
		t.Fatal("unprotected Pods, Pod cleanup or namespace updates unexpectedly called Pod admission")
	}
	for _, subresource := range []string{"status", "finalize"} {
		t.Run("marker protection through "+subresource, func(t *testing.T) {
			current := &corev1.Namespace{}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(ns), current); err != nil {
				t.Fatal(err)
			}
			changed := current.DeepCopy()
			delete(changed.Labels, "cetirun.microsoft.com/sandbox-protected")
			if err := member.SubResource(subresource).Update(ctx, changed); err != nil && !apierrors.IsForbidden(err) {
				t.Fatal(err)
			}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(ns), current); err != nil {
				t.Fatal(err)
			}
			if current.Labels["cetirun.microsoft.com/sandbox-protected"] != "true" {
				t.Fatal("namespace subresource removed admission routing protection")
			}
		})
	}
	if err := member.Delete(ctx, ns); err != nil {
		t.Fatalf("namespace deletion must stay independent of webhook availability: %v", err)
	}
}

func renderedPodAdmission(t *testing.T, ca []byte) (*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding, *admissionregistrationv1.ValidatingWebhookConfiguration) {
	t.Helper()
	command := exec.Command("helm", "template", "namespace-guard", "../../chart", "--kube-version", "1.37.0",
		"--set", "cetiadmin.enabled=false",
		"--set", "sandboxAdmission.enabled=true", "--set", "sandboxAdmission.enforce=true",
		"--set", "sandboxAdmission.podsEnabled=true", "--set", "sandboxAdmission.tlsSecretName=example-tls",
		"--set", "sandboxAdmission.adminUsers[0]=admin@example.com",
		"--set", "sandboxAdmission.adminUsers[1]=system:serviceaccount:cetirun-system:portal",
		"--set-string", "sandboxAdmission.caBundle="+base64.StdEncoding.EncodeToString(ca))
	command.Env = append(os.Environ(), "KUBECONFIG="+os.DevNull, "HELM_REGISTRY_CONFIG="+os.DevNull, "HELM_REPOSITORY_CONFIG="+os.DevNull)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("render Pod admission: %v\n%s", err, output)
	}
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	webhook := &admissionregistrationv1.ValidatingWebhookConfiguration{}
	decoder := kyaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	for {
		var object unstructured.Unstructured
		if err := decoder.Decode(&object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		var target any
		switch {
		case object.GetKind() == "ValidatingAdmissionPolicy" && object.GetName() == "cetirun-sandbox-namespace":
			target = policy
		case object.GetKind() == "ValidatingAdmissionPolicyBinding" && object.GetName() == "cetirun-sandbox-namespace":
			target = binding
		case object.GetKind() == "ValidatingWebhookConfiguration" && object.GetName() == "cetirun-sandbox-pods":
			target = webhook
		default:
			continue
		}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, target); err != nil {
			t.Fatal(err)
		}
	}
	if policy.Name == "" || binding.Name == "" || len(webhook.Webhooks) != 1 {
		t.Fatal("render did not include namespace protection and the Pod webhook")
	}
	return policy, binding, webhook
}
