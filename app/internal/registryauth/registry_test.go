// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package registryauth

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestHost(t *testing.T) {
	for image, want := range map[string]string{
		"alpine:3": "docker.io", "team/agent:1": "docker.io", "index.docker.io/team/agent:1": "docker.io",
		"registry.example.com:5000/team/agent:1": "registry.example.com:5000", "localhost:5000/agent": "localhost:5000",
		"registry.example.com/agent@sha256:" + strings.Repeat("a", 64): "registry.example.com",
	} {
		if got, err := Host(image); err != nil || got != want {
			t.Fatalf("Host(%q) = %q, %v", image, got, err)
		}
	}
	for _, image := range []string{"", "https://registry.example.com/a", "user:password@registry.example.com/a", "registry.example.com/a b", "registry.example.com/../a", "registry.example.com/a?x=y", "registry.example.com/"} {
		if _, err := Host(image); err == nil {
			t.Fatalf("accepted invalid image %q", image)
		}
	}
}

func TestSecretContract(t *testing.T) {
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "agent-uid"}, Spec: api.AgentSpec{Agent: api.HarnessSpec{Image: ptr.To("registry.example.com/a:1")}}}
	raw, err := Encode(*agent.Spec.Agent.Image, Credentials{Username: "example", Password: "synthetic-token"})
	if err != nil {
		t.Fatal(err)
	}
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{Owner(agent)}}, Type: corev1.SecretTypeDockerConfigJson, Data: map[string][]byte{corev1.DockerConfigJsonKey: raw}}
	if err := Validate(source, agent); err != nil {
		t.Fatal(err)
	}
	large := source.DeepCopy()
	large.Data[corev1.DockerConfigJsonKey], err = Encode(*agent.Spec.Agent.Image, Credentials{Username: "example", Password: strings.Repeat("<", 32<<10)})
	if err != nil || Validate(large, agent) != nil {
		t.Fatal("accepted input cannot survive JSON/base64 expansion")
	}
	for _, change := range []func(*corev1.Secret){
		func(s *corev1.Secret) { s.OwnerReferences[0].UID = "old-agent" },
		func(s *corev1.Secret) { s.OwnerReferences = nil; s.Labels = Labels(agent.UID) },
		func(s *corev1.Secret) { s.Type = corev1.SecretTypeOpaque },
		func(s *corev1.Secret) { s.Data["other"] = []byte("synthetic") },
		func(s *corev1.Secret) {
			s.Data[corev1.DockerConfigJsonKey] = []byte(strings.ReplaceAll(string(raw), "registry.example.com", "other.example.com"))
		},
		func(s *corev1.Secret) {
			s.Data[corev1.DockerConfigJsonKey] = []byte(`{"auths":{},"credsStore":"unsafe"}`)
		},
		func(s *corev1.Secret) { s.Data[corev1.DockerConfigJsonKey] = []byte(`{"auths":{},"auths":{}}`) },
	} {
		s := source.DeepCopy()
		change(s)
		if Validate(s, agent) == nil {
			t.Fatal("accepted invalid source")
		}
	}
	for _, password := range []string{"", " ", "********", "••••"} {
		if _, err := Encode(*agent.Spec.Agent.Image, Credentials{Username: "example", Password: password}); err == nil {
			t.Fatal("accepted empty or masked credentials")
		}
	}
}

func TestIsolationUsesEffectiveGroupsAndIndirectAccess(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	for _, allowed := range []string{"none", "source", "named", "pods", "jobs", "exec-get", "impersonate", "unknown"} {
		t.Run(allowed, func(t *testing.T) {
			var calls atomic.Int64
			c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
				Create: func(_ context.Context, _ client.WithWatch, object client.Object, _ ...client.CreateOption) error {
					review := object.(*authorizationv1.SubjectAccessReview)
					calls.Add(1)
					if review.Spec.User != "system:serviceaccount:cetirun-example:default" || len(review.Spec.Groups) != 3 {
						t.Fatal("service account groups omitted")
					}
					a := review.Spec.ResourceAttributes
					review.Status.Denied = true
					review.Status.Allowed = (allowed == "source" && a.Namespace == DefaultNamespace && a.Resource == "secrets") ||
						(allowed == "named" && a.Name == "registry-source-example") ||
						(allowed == "pods" && a.Resource == "pods" && a.Verb == "create") ||
						(allowed == "jobs" && a.Resource == "jobs" && a.Verb == "create") ||
						(allowed == "exec-get" && a.Resource == "pods" && a.Subresource == "exec" && a.Verb == "get") ||
						(allowed == "impersonate" && a.Verb == "impersonate")
					if allowed == "unknown" {
						review.Status.EvaluationError = "synthetic failure"
					}
					return nil
				},
			}).Build()
			err := CheckIsolation(context.Background(), c, DefaultNamespace, "cetirun-example", "default", "registry-source-example")
			if (err == nil) != (allowed == "none") || calls.Load() == 0 {
				t.Fatalf("unexpected isolation result %v (%d checks)", err, calls.Load())
			}
		})
	}
}
