// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package registryauth

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func isolationScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func TestIsolationAPIRequestsScaleLinearly(t *testing.T) {
	check := func(count int) int64 {
		t.Helper()
		var lists, reviews atomic.Int64
		objects := make([]client.Object, 0, count)
		for i := range count {
			objects = append(objects, &api.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("agent-%d", i), UID: types.UID(fmt.Sprintf("uid-%d", i))},
				Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: api.AgentTypeHermes,
					RegistrySecretRef: &api.RegistrySecretRef{Name: fmt.Sprintf("registry-source-%d", i)}}},
			})
		}
		c := fake.NewClientBuilder().WithScheme(isolationScheme(t)).WithObjects(objects...).
			WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					lists.Add(1)
					return c.List(ctx, list, opts...)
				},
				Create: func(_ context.Context, _ client.WithWatch, object client.Object, _ ...client.CreateOption) error {
					reviews.Add(1)
					object.(*authorizationv1.SubjectAccessReview).Status.Denied = true
					return nil
				},
			}).Build()
		if err := CheckIsolation(context.Background(), c, DefaultNamespace, "cetirun-agent-0", "default", "registry-source-0"); err != nil {
			t.Fatal(err)
		}
		t.Logf("agents=%d list_calls=%d subject_access_reviews=%d", count, lists.Load(), reviews.Load())
		if lists.Load() > 8 {
			t.Errorf("workload inventory must not require a list per Agent: got %d lists", lists.Load())
		}
		return reviews.Load()
	}
	small, large := check(10), check(100)
	if small == 0 || large > 10*small {
		t.Fatalf("authorization requests grew faster than the Agent inventory: 10=%d, 100=%d", small, large)
	}
}

func TestIsolationChecksForeignWorkloadSubjectsAndCurrentSecretNames(t *testing.T) {
	for _, workload := range []string{"pod", "statefulset"} {
		for _, target := range []string{"source", "pull"} {
			t.Run(workload+"/"+target, func(t *testing.T) {
				agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "example-uid"},
					Spec: api.AgentSpec{Agent: api.HarnessSpec{RegistrySecretRef: &api.RegistrySecretRef{Name: "registry-source-example"}}}}
				other := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "other", UID: "other-uid"}}
				objects := []client.Object{agent, other}
				if workload == "pod" {
					objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "cetirun-other"},
						Spec: corev1.PodSpec{ServiceAccountName: "custom-account"}})
				} else {
					objects = append(objects, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "cetirun-other"},
						Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ServiceAccountName: "custom-account"}}}})
				}
				namespace, name := DefaultNamespace, "registry-source-example"
				if target == "pull" {
					namespace = "cetirun-example"
					name, _ = PullSecretName(agent.UID)
				}
				c := fake.NewClientBuilder().WithScheme(isolationScheme(t)).WithObjects(objects...).
					WithInterceptorFuncs(interceptor.Funcs{Create: func(_ context.Context, _ client.WithWatch, object client.Object, _ ...client.CreateOption) error {
						review := object.(*authorizationv1.SubjectAccessReview)
						access := review.Spec.ResourceAttributes
						review.Status.Allowed = review.Spec.User == "system:serviceaccount:cetirun-other:custom-account" &&
							access.Namespace == namespace && access.Resource == "secrets" && access.Verb == "get" && access.Name == name
						return nil
					}}).Build()
				if err := CheckIsolation(context.Background(), c, DefaultNamespace, "cetirun-example", "default", "registry-source-example"); err != ErrIsolation {
					t.Fatalf("foreign workload's named access was not rejected: %v", err)
				}
			})
		}
	}
}

func TestIsolationDoesNotReuseAuthorizationAcrossCalls(t *testing.T) {
	var allowed atomic.Bool
	c := fake.NewClientBuilder().WithScheme(isolationScheme(t)).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(_ context.Context, _ client.WithWatch, object client.Object, _ ...client.CreateOption) error {
			review := object.(*authorizationv1.SubjectAccessReview)
			review.Status.Allowed = allowed.Load()
			return nil
		}}).Build()
	if err := CheckIsolation(context.Background(), c, DefaultNamespace, "cetirun-example", "default", "registry-source-example"); err != nil {
		t.Fatal(err)
	}
	allowed.Store(true)
	if err := CheckIsolation(context.Background(), c, DefaultNamespace, "cetirun-example", "default", "registry-source-example"); err != ErrIsolation {
		t.Fatalf("a previous successful check masked changed permissions: %v", err)
	}
}
