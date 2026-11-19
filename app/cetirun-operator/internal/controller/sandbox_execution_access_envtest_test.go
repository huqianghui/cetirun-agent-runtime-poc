// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package controller_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	istio "istio.io/client-go/pkg/apis/networking/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Route/authority persistence, ingress namespace identity and CAS use the real
// API server. Runtime, gateway authentication and other sources are synthetic.
// There is no proxy, workload execution, cloud or model interaction.
func TestSandboxAccessAPIServer(t *testing.T) {
	for _, scenario := range []string{"publish and withdraw", "split namespaces", "maximum preparation", "delayed publish", "replacement", "lost response"} {
		t.Run(scenario, func(t *testing.T) {
			if os.Getenv("USE_EXISTING_CLUSTER") == "true" {
				t.Fatal("access tests refuse an existing cluster")
			}
			assets := os.Getenv("KUBEBUILDER_ASSETS")
			for _, binary := range []string{"kube-apiserver", "etcd"} {
				info, err := os.Stat(filepath.Join(assets, binary))
				if assets == "" || err != nil || info.IsDir() || info.Mode()&0111 == 0 {
					t.Fatal("explicit local envtest binaries are required")
				}
			}
			environment := &envtest.Environment{
				BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
				CRDs: []*apiextensions.CustomResourceDefinition{{
					ObjectMeta: metav1.ObjectMeta{Name: "virtualservices.networking.istio.io"},
					Spec: apiextensions.CustomResourceDefinitionSpec{
						Group: "networking.istio.io", Scope: apiextensions.NamespaceScoped,
						Names: apiextensions.CustomResourceDefinitionNames{
							Kind: "VirtualService", ListKind: "VirtualServiceList", Singular: "virtualservice", Plural: "virtualservices",
						},
						Versions: []apiextensions.CustomResourceDefinitionVersion{{
							Name: "v1beta1", Served: true, Storage: true,
							Schema: &apiextensions.CustomResourceValidation{OpenAPIV3Schema: &apiextensions.JSONSchemaProps{
								Type: "object", Properties: map[string]apiextensions.JSONSchemaProps{
									"spec": {Type: "object", XPreserveUnknownFields: ptr.To(true)},
								},
							}},
						}},
					},
				}},
			}
			config, err := environment.Start()
			if err != nil {
				if stopErr := environment.Stop(); stopErr != nil {
					t.Error(stopErr)
				}
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := environment.Stop(); err != nil {
					t.Error(err)
				}
			})
			storage, err := client.NewWithWatch(config, client.Options{Scheme: testScheme()})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			workloadNamespace := "gateway-system"
			if scenario == "split namespaces" {
				workloadNamespace = "gateway-workloads"
			}
			f := newAccessFixture(t, true, workloadNamespace)
			if scenario == "maximum preparation" {
				f.fillPreparedSources(t)
			}
			consumer := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: workloadNamespace}}
			if err := storage.Create(ctx, consumer); err != nil {
				t.Fatal(err)
			}
			f.reconciler.APIReader = accessAPIReader{
				Reader: f.client, namespaces: storage, ingressNamespace: workloadNamespace,
			}
			f.enroll(t)
			if len(f.intent.Access.IngressNamespaces) != 1 || f.intent.Access.IngressNamespaces[0].UID != consumer.UID {
				t.Fatal("enrollment did not pin the real ingress workload namespace identity")
			}
			for _, namespace := range []string{f.intent.Namespace.Name, f.reconciler.OperatorNamespace} {
				if err := storage.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
					t.Fatal(err)
				}
			}
			route := f.route(t)
			route.UID, route.ResourceVersion = "", ""
			if err := storage.Create(ctx, route); err != nil {
				t.Fatal(err)
			}
			f.intent.Access.Route.UID = route.UID
			state, err := sandboxcoordination.NewState(f.reconciler.OperatorNamespace, "actual-access-authority")
			if err != nil {
				t.Fatal(err)
			}
			if err := storage.Create(ctx, state); err != nil {
				t.Fatal(err)
			}
			f.access.Coordination = sandboxcoordination.Store{Client: storage, APIReader: storage, Namespace: state.Namespace,
				Identity: w.ObjectIdentity{Name: state.Name, UID: state.UID}}
			ticket, err := f.access.Coordination.AcquireExecution(ctx, f.intent)
			if err != nil {
				t.Fatal(err)
			}
			ticket, err = f.access.Coordination.BeginStart(ctx, ticket)
			if err != nil {
				t.Fatal(err)
			}
			completeRunningFixture(t, f.preparationFixture, f.target, f.intent)
			ticket, err = f.access.Coordination.MarkRunning(ctx, ticket)
			if err != nil {
				t.Fatal(err)
			}
			f.ticket = ticket
			f.observation = sandbox.Observation{ExecutionIdentity: f.intent.Expected, ObservedAt: f.now,
				RuntimeVerified: true, PodVerified: true, ControlsVerified: true}
			f.reconciler.Client = storage
			f.reconciler.APIReader = accessAPIReader{
				Reader: f.client, routes: storage, namespaces: storage, ingressNamespace: workloadNamespace,
			}
			read := func() *istio.VirtualService {
				t.Helper()
				result := &istio.VirtualService{}
				if err := storage.Get(ctx, client.ObjectKeyFromObject(route), result); err != nil {
					t.Fatal(err)
				}
				return result
			}
			switch scenario {
			case "publish and withdraw", "split namespaces", "maximum preparation":
				if !reflect.DeepEqual(read().Spec.ExportTo, []string{workloadNamespace}) {
					t.Fatal("nonforwarding slot was not exported to the enrolled consumer")
				}
				if err := f.access.Publish(ctx, ticket, f.observation, statusTestGuard); err != nil {
					t.Fatal(err)
				}
				published := read()
				if published.UID != route.UID || published.ResourceVersion == route.ResourceVersion || len(published.Spec.Http) != 3 {
					t.Fatal("real API did not persist publication on the enrolled UID")
				}
				if !reflect.DeepEqual(published.Spec.Gateways, []string{"gateway-system/native-gateway"}) ||
					!reflect.DeepEqual(published.Spec.ExportTo, []string{workloadNamespace}) {
					t.Fatalf("real API conflated Gateway resource and proxy namespaces: gateways=%v exportTo=%v",
						published.Spec.Gateways, published.Spec.ExportTo)
				}
				if err := f.access.Publish(ctx, ticket, f.observation, statusTestGuard); err != nil {
					t.Fatal(err)
				}
				if read().ResourceVersion != published.ResourceVersion {
					t.Fatal("unchanged publication was not a no-op")
				}
				stopping, err := f.access.Coordination.BeginStop(ctx, ticket)
				if err != nil {
					t.Fatal(err)
				}
				f.reconciler.SandboxPreparation = nil
				if err := f.access.Withdraw(ctx, stopping, statusTestGuard); err != nil {
					t.Fatal(err)
				}
				if err := storage.Get(ctx, client.ObjectKeyFromObject(route), &istio.VirtualService{}); !apierrors.IsNotFound(err) {
					t.Fatal("real API withdrawal did not finish deletion:", err)
				}
				if err := f.access.Withdraw(ctx, stopping, statusTestGuard); err != nil {
					t.Fatal(err)
				}
			case "delayed publish":
				intercepted := 0
				f.reconciler.Client = interceptor.NewClient(storage, interceptor.Funcs{
					Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
						intercepted++
						stopping, err := f.access.Coordination.BeginStop(ctx, ticket)
						if err != nil {
							return err
						}
						f.reconciler.Client = c
						if err := f.access.Withdraw(ctx, stopping, statusTestGuard); err != nil {
							return err
						}
						err = c.Update(ctx, object, options...)
						if !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
							t.Fatal("real API accepted a late old-version publication:", err)
						}
						return err
					},
				})
				if err := f.access.Publish(ctx, ticket, f.observation, statusTestGuard); err == nil || intercepted != 1 {
					t.Fatal("late publication was accepted or replayed:", err)
				}
			case "replacement":
				stopping, err := f.access.Coordination.BeginStop(ctx, ticket)
				if err != nil {
					t.Fatal(err)
				}
				if err := storage.Delete(ctx, route); err != nil {
					t.Fatal(err)
				}
				replacement := route.DeepCopy()
				replacement.UID, replacement.ResourceVersion = "", ""
				if err := storage.Create(ctx, replacement); err != nil {
					t.Fatal(err)
				}
				if err := f.access.Withdraw(ctx, stopping, statusTestGuard); err == nil {
					t.Fatal("old execution deleted a replacement route")
				}
				if !reflect.DeepEqual(read(), replacement) {
					t.Fatal("replacement route was changed")
				}
			case "lost response":
				writes := 0
				failure := errors.New("synthetic lost response after route publication")
				f.reconciler.Client = interceptor.NewClient(storage, interceptor.Funcs{
					Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
						writes++
						if err := c.Update(ctx, object, options...); err != nil {
							return err
						}
						return failure
					},
				})
				if err := f.access.Publish(ctx, ticket, f.observation, statusTestGuard); !errors.Is(err, failure) {
					t.Fatal("uncertain publication was reported as success:", err)
				}
				f.reconciler.Client = storage
				stopping, err := f.access.Coordination.BeginStop(ctx, ticket)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.access.Withdraw(ctx, stopping, statusTestGuard); err != nil || writes != 1 {
					t.Fatal("uncertain publication was replayed or could not be withdrawn:", err)
				}
			}
		})
	}
}

type accessAPIReader struct {
	client.Reader
	routes           client.Reader
	namespaces       client.Reader
	ingressNamespace string
}

func (r accessAPIReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, route := object.(*istio.VirtualService); route && r.routes != nil {
		return r.routes.Get(ctx, key, object, options...)
	}
	_, namespace := object.(*corev1.Namespace)
	if r.namespaces != nil && key.Namespace == "" && key.Name == r.ingressNamespace &&
		(namespace || object.GetObjectKind().GroupVersionKind().Kind == "Namespace") {
		return r.namespaces.Get(ctx, key, object, options...)
	}
	return r.Reader.Get(ctx, key, object, options...)
}
