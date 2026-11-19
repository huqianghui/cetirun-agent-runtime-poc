// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestNewHandlerConfig(t *testing.T) {
	f := newFixture(t)
	if f.handler.config.RequestTimeout != 8*time.Second {
		t.Fatal("default timeout is not the total eight-second request budget")
	}
	for name, config := range map[string]Config{
		"missing namespace": {ConfigMapName: "placement"},
		"invalid namespace": {Namespace: "not/a/namespace", ConfigMapName: "placement"},
		"missing name":      {Namespace: "operator-state"},
		"invalid name":      {Namespace: "operator-state", ConfigMapName: "not/a/name"},
		"negative budget":   {Namespace: "operator-state", ConfigMapName: "placement", RequestTimeout: -time.Second},
		"unbounded budget":  {Namespace: "operator-state", ConfigMapName: "placement", RequestTimeout: 30 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewHandler(f.client, config); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	if _, err := NewHandler(nil, f.handler.config); err == nil {
		t.Fatal("nil API reader accepted")
	}
	if _, err := NewHandler(f.client, Config{Namespace: "operator-state", ConfigMapName: "placement", RequestTimeout: time.Second}); err != nil {
		t.Fatal(err)
	}
}

func TestUnrelatedPodPlacement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		node    string
		mutate  func(*corev1.Pod)
		allowed bool
	}{
		{name: "unscheduled", allowed: true},
		{name: "known unreserved", node: "ordinary-worker", allowed: true},
		{name: "unknown", node: "new-worker"},
		{name: "reserved", node: "reserved-worker"},
		{name: "platform namespace", node: "reserved-worker", mutate: func(p *corev1.Pod) { p.Namespace = "platform-system" }},
		{name: "platform service account", node: "reserved-worker", mutate: func(p *corev1.Pod) {
			p.Namespace, p.Spec.ServiceAccountName = "platform-system", "platform-daemon"
		}},
		{name: "platform labels", node: "reserved-worker", mutate: func(p *corev1.Pod) {
			p.Labels = map[string]string{"app": "platform-daemon", "example.com/pool": "sandbox"}
		}},
		{name: "Agent namespace", node: "reserved-worker", mutate: func(p *corev1.Pod) { p.Namespace = "cetirun-example" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			pod := ordinaryPod()
			pod.Spec.NodeName = tc.node
			if tc.mutate != nil {
				tc.mutate(pod)
			}
			response := f.handler.Validate(context.Background(), podRequest(t, pod, nil))
			if tc.allowed {
				expectAllowed(t, response)
			} else {
				expectDenied(t, response, "")
			}
		})
	}
}

func TestUnrelatedPlacementsDoNotResolveAgentAssignments(t *testing.T) {
	f := newFixture(t)
	for _, object := range []client.Object{f.agent.Agent, f.agent.Namespace, f.workload, f.expectation} {
		if err := f.client.Delete(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	pod := ordinaryPod()
	pod.Spec.NodeName = f.ordinary.Name
	expectAllowed(t, f.handler.Validate(context.Background(), podRequest(t, pod, nil)))
	pod.Spec.NodeName = f.reserved.Name
	expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, pod, nil)), "ReservedNode")
	pod.Spec.NodeName, pod.ResourceVersion = "", ""
	if err := f.client.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	expectAllowed(t, f.handler.Validate(context.Background(), bindingRequest(t, pod, f.ordinary.Name)))
	expectDenied(t, f.handler.Validate(context.Background(), bindingRequest(t, pod, f.reserved.Name)), "ReservedNode")
	expectAllowed(t, f.handler.Validate(context.Background(), nodeRequest(t, f.ordinary, nil)))
}

func TestRegistryMustBeProtectedPersistedAndCurrent(t *testing.T) {
	for name, mutate := range map[string]func(*testing.T, *placementFixture){
		"missing": func(t *testing.T, f *placementFixture) {
			if err := f.client.Delete(context.Background(), f.state); err != nil {
				t.Fatal(err)
			}
		},
		"tenant substitute": func(t *testing.T, f *placementFixture) {
			if err := f.client.Delete(context.Background(), f.state); err != nil {
				t.Fatal(err)
			}
			other := f.state.DeepCopy()
			other.Namespace, other.ResourceVersion = "ordinary-workloads", ""
			if err := f.client.Create(context.Background(), other); err != nil {
				t.Fatal(err)
			}
		},
		"extra data": func(t *testing.T, f *placementFixture) {
			f.state.Data["extra"] = "not-authority"
			if err := f.client.Update(context.Background(), f.state); err != nil {
				t.Fatal(err)
			}
		},
		"binary data": func(t *testing.T, f *placementFixture) {
			f.state.BinaryData = map[string][]byte{"extra": []byte("not-authority")}
			if err := f.client.Update(context.Background(), f.state); err != nil {
				t.Fatal(err)
			}
		},
		"empty UID": func(t *testing.T, f *placementFixture) {
			f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
				if object.GetName() == f.state.Name {
					object.SetUID("")
				}
			}}
		},
		"empty resource version": func(t *testing.T, f *placementFixture) {
			f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
				if object.GetName() == f.state.Name {
					object.SetResourceVersion("")
				}
			}}
		},
		"terminating": func(t *testing.T, f *placementFixture) {
			f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
				if object.GetName() == f.state.Name {
					object.SetDeletionTimestamp(ptr.To(metav1.Now()))
				}
			}}
		},
		"wrong returned key": func(t *testing.T, f *placementFixture) {
			f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
				if object.GetName() == f.state.Name {
					object.SetNamespace("ordinary-workloads")
				}
			}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			mutate(t, f)
			expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, ordinaryPod(), nil)), "")
		})
	}
}

func TestNodeIdentityNeverComesFromLabels(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Node){
		"same name replacement": func(n *corev1.Node) { n.UID = "replacement-uid" },
		"provider replacement":  func(n *corev1.Node) { n.Spec.ProviderID = "example://replacement" },
		"missing provider":      func(n *corev1.Node) { n.Spec.ProviderID = "" },
		"missing UID":           func(n *corev1.Node) { n.UID = "" },
		"missing version":       func(n *corev1.Node) { n.ResourceVersion = "" },
		"terminating":           func(n *corev1.Node) { n.DeletionTimestamp = ptr.To(metav1.Now()) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
				if node, ok := object.(*corev1.Node); ok {
					mutate(node)
				}
			}}
			pod := ordinaryPod()
			pod.Spec.NodeName = f.ordinary.Name
			expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, pod, nil)), "")
		})
	}
	f := newFixture(t)
	f.reserved.Labels = nil
	if err := f.client.Update(context.Background(), f.reserved); err != nil {
		t.Fatal(err)
	}
	pod := ordinaryPod()
	pod.Spec.NodeName = f.reserved.Name
	expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, pod, nil)), "ReservedNode")
	f.registry.Nodes = f.registry.Nodes[1:]
	f.saveRegistry(t)
	expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, pod, nil)), "NodeUnknown")
}

func TestBindingReadsPersistedPodAndDestination(t *testing.T) {
	f := newFixture(t)
	pod := ordinaryPod()
	pod.ResourceVersion = ""
	if err := f.client.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	expectAllowed(t, f.handler.Validate(context.Background(), bindingRequest(t, pod, f.ordinary.Name)))
	expectDenied(t, f.handler.Validate(context.Background(), bindingRequest(t, pod, f.reserved.Name)), "ReservedNode")
	expectDenied(t, f.handler.Validate(context.Background(), bindingRequest(t, pod, "unknown-worker")), "NodeUnknown")
	for name, mutate := range map[string]func(*corev1.Binding){
		"missing Pod UID":   func(b *corev1.Binding) { b.UID = "" },
		"replaced Pod UID":  func(b *corev1.Binding) { b.UID = "old-pod-uid" },
		"wrong Pod name":    func(b *corev1.Binding) { b.Name = "different-pod" },
		"wrong namespace":   func(b *corev1.Binding) { b.Namespace = "different-namespace" },
		"stale Pod version": func(b *corev1.Binding) { b.ResourceVersion = "old-version" },
		"non-Node target":   func(b *corev1.Binding) { b.Target.Kind = "Pod" },
		"namespaced target": func(b *corev1.Binding) { b.Target.Namespace = pod.Namespace },
		"target field":      func(b *corev1.Binding) { b.Target.FieldPath = "spec.nodeName" },
		"wrong target UID":  func(b *corev1.Binding) { b.Target.UID = "replaced-node-uid" },
		"wrong target RV":   func(b *corev1.Binding) { b.Target.ResourceVersion = "old-version" },
		"wrong target API":  func(b *corev1.Binding) { b.Target.APIVersion = "example.com/v1" },
		"empty target name": func(b *corev1.Binding) { b.Target.Name = "" },
	} {
		t.Run(name, func(t *testing.T) {
			request := bindingRequest(t, pod, f.ordinary.Name)
			var binding corev1.Binding
			if err := json.Unmarshal(request.Object.Raw, &binding); err != nil {
				t.Fatal(err)
			}
			mutate(&binding)
			request.Object.Raw = marshal(t, binding)
			expectDenied(t, f.handler.Validate(context.Background(), request), "")
		})
	}
	pod.Spec.NodeName = f.ordinary.Name
	if err := f.client.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	expectDenied(t, f.handler.Validate(context.Background(), bindingRequest(t, pod, f.ordinary.Name)), "BindingPodMismatch")
	if err := f.client.Delete(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	expectDenied(t, f.handler.Validate(context.Background(), bindingRequest(t, pod, f.ordinary.Name)), "PodMissing")
}

func TestUpdatePathsCannotRemoveOrReplacePlacement(t *testing.T) {
	for _, subresource := range []string{"", "ephemeralcontainers", "resize"} {
		for _, node := range []string{"", "ordinary-worker", "unknown-worker"} {
			t.Run(subresource+"/"+node, func(t *testing.T) {
				f := newFixture(t)
				f.agentPod.Spec.NodeName = f.reserved.Name
				if err := f.client.Update(context.Background(), f.agentPod); err != nil {
					t.Fatal(err)
				}
				pod := f.agentPod.DeepCopy()
				pod.Spec.NodeName = node
				request := podRequest(t, pod, f.agentPod)
				request.SubResource = subresource
				expectDenied(t, f.handler.Validate(context.Background(), request), "SchedulingChanged")
			})
		}
	}
	f := newFixture(t)
	old := f.agentPod.DeepCopy()
	old.Spec.NodeName = f.reserved.Name
	request := podRequest(t, f.agentPod, old)
	expectDenied(t, f.handler.Validate(context.Background(), request), "PreviousPodMismatch")
	current := f.agentPod.DeepCopy()
	current.UID = "replacement-uid"
	expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, current, f.agentPod)), "PreviousPodMismatch")
}

func TestStatusAndDeletionRemainIndependent(t *testing.T) {
	f := newFixture(t)
	reader := &countingReader{Reader: f.client}
	f.handler.reader = reader
	for _, operation := range []admissionv1.Operation{admissionv1.Delete, admissionv1.Update} {
		request := podRequest(t, ordinaryPod(), nil)
		request.Operation = operation
		if operation == admissionv1.Update {
			request.SubResource = "status"
		}
		expectAllowed(t, f.handler.Validate(context.Background(), request))
	}
	if reader.gets.Load() != 0 || reader.lists.Load() != 0 {
		t.Fatal("cleanup or status required placement reads")
	}
}

func TestUnsupportedAndAmbiguousRequests(t *testing.T) {
	for name, mutate := range map[string]func(*admission.Request){
		"wrong resource":      func(r *admission.Request) { r.Resource.Resource = "nodes" },
		"wrong version":       func(r *admission.Request) { r.Resource.Version = "v2" },
		"wrong kind":          func(r *admission.Request) { r.Kind.Kind = "Node" },
		"connect":             func(r *admission.Request) { r.Operation = admissionv1.Connect },
		"unknown subresource": func(r *admission.Request) { r.SubResource = "proxy" },
		"create resize":       func(r *admission.Request) { r.SubResource = "resize" },
		"converted resource": func(r *admission.Request) {
			r.RequestResource = &metav1.GroupVersionResource{Version: "v1", Resource: "nodes"}
		},
		"converted kind":        func(r *admission.Request) { r.RequestKind = &metav1.GroupVersionKind{Version: "v1", Kind: "Node"} },
		"converted subresource": func(r *admission.Request) { r.RequestSubResource = "binding" },
		"empty namespace":       func(r *admission.Request) { r.Namespace = "" },
		"wrong name":            func(r *admission.Request) { r.Name = "other-pod" },
		"malformed Pod":         func(r *admission.Request) { r.Object.Raw = []byte("{") },
		"unknown Pod field": func(r *admission.Request) {
			r.Object.Raw = []byte(strings.Replace(string(r.Object.Raw), `"spec":{`, `"spec":{"unknown":true,`, 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			request := podRequest(t, ordinaryPod(), nil)
			mutate(&request)
			expectDenied(t, f.handler.Validate(context.Background(), request), "")
		})
	}
}

func TestEveryAuthoritySnapshotIsRechecked(t *testing.T) {
	for _, target := range []string{"Registry", "Node", "Pod", "Namespace", "DaemonSet", "ServiceAccount"} {
		for _, drift := range []string{"uid", "version", "same-uid-template", "deleted"} {
			t.Run(target+"/"+drift, func(t *testing.T) {
				f := newFixture(t)
				var matches int
				f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
					stage := stageForObject(object, f.state.Name)
					if stage != target {
						return
					}
					matches++
					// Reserved bindings currently read some dependencies twice.
					// Drift on the first re-read must fail either validation or
					// the final snapshot comparison, even without an RV change.
					if matches < 2 {
						return
					}
					switch drift {
					case "uid":
						object.SetUID("replacement-uid")
					case "version":
						object.SetResourceVersion("new-version")
					case "same-uid-template":
						switch value := object.(type) {
						case *appsv1.DaemonSet:
							value.Spec.Template.Spec.Containers[0].Image = "registry.example.com/different:1.0"
						case *corev1.Pod:
							value.Spec.Containers[0].Image = "registry.example.com/different:1.0"
						case *corev1.ConfigMap:
							value.Data[ConfigMapKey] = `{"version":"v1","nodes":[],"platformDaemonSets":[]}`
						default:
							object.SetAnnotations(map[string]string{"example.com/drift": "changed"})
						}
					case "deleted":
						object.SetDeletionTimestamp(ptr.To(metav1.Now()))
					}
				}}
				expectDenied(t, f.handler.Validate(context.Background(), bindingRequest(t, f.platformPod, f.reserved.Name)), "")
			})
		}
	}
}

func TestRequestBudgetAndReadFailureAreFailClosed(t *testing.T) {
	f := newFixture(t)
	for name, cause := range map[string]error{
		"API outage": fmt.Errorf("synthetic-private-read-error"),
		"canceled":   context.Canceled,
	} {
		t.Run(name, func(t *testing.T) {
			f.handler.reader = errorReader{Reader: f.client, err: cause}
			response := f.handler.Validate(context.Background(), podRequest(t, ordinaryPod(), nil))
			expectDenied(t, response, "ReadFailed")
			if response.Result.Code != http.StatusServiceUnavailable || strings.Contains(response.Result.Message, "synthetic-private-read-error") {
				t.Fatal("authoritative read error was allowed or disclosed")
			}
		})
	}
	deadlineReader := &budgetReader{Reader: f.client}
	var err error
	f.handler, err = NewHandler(deadlineReader, Config{Namespace: f.state.Namespace, ConfigMapName: f.state.Name, RequestTimeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	pod := ordinaryPod()
	pod.Spec.NodeName = f.ordinary.Name
	start := time.Now()
	response := f.handler.Validate(context.Background(), podRequest(t, pod, nil))
	expectDenied(t, response, "RequestDeadlineExceeded")
	if response.Result.Code != http.StatusGatewayTimeout || time.Since(start) > time.Second || deadlineReader.gets < 2 {
		t.Fatal("request did not respect one total deadline across reads")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response = f.handler.Validate(ctx, podRequest(t, ordinaryPod(), nil))
	expectDenied(t, response, "RequestCanceled")
}

func TestReaderOnlyAndConcurrentRequests(t *testing.T) {
	f := newFixture(t)
	reader := &countingReader{Reader: f.client}
	handler, err := NewHandler(reader, f.handler.config)
	if err != nil {
		t.Fatal(err)
	}
	request := bindingRequest(t, f.platformPod, f.reserved.Name)
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			response := handler.Handle(context.Background(), request)
			if !response.Allowed || len(response.Patches) != 0 || len(response.Patch) != 0 {
				t.Errorf("read-only concurrent binding failed: %+v", response.Result)
			}
		})
	}
	workers.Wait()
	if reader.gets.Load() == 0 || reader.lists.Load() != 0 {
		t.Fatal("handler did not use bounded authoritative GETs")
	}
}

type mutateReader struct {
	client.Reader
	mutate func(client.Object)
}

func (r mutateReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	if err := r.Reader.Get(ctx, key, object, opts...); err != nil {
		return err
	}
	r.mutate(object)
	return nil
}

type countingReader struct {
	client.Reader
	gets  atomic.Int64
	lists atomic.Int64
}

func (r *countingReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	r.gets.Add(1)
	return r.Reader.Get(ctx, key, object, opts...)
}

func (r *countingReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	r.lists.Add(1)
	return fmt.Errorf("placement must not list unbounded inventories")
}

type errorReader struct {
	client.Reader
	err error
}

func (r errorReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

type budgetReader struct {
	client.Reader
	gets     int
	deadline time.Time
}

func (r *budgetReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	deadline, exists := ctx.Deadline()
	if !exists || (!r.deadline.IsZero() && r.deadline != deadline) {
		return fmt.Errorf("request deadline was absent or reset")
	}
	r.deadline = deadline
	r.gets++
	if r.gets == 1 {
		return r.Reader.Get(ctx, key, object, opts...)
	}
	<-ctx.Done()
	return ctx.Err()
}

func stageForObject(object client.Object, registryName string) string {
	switch object.(type) {
	case *corev1.ConfigMap:
		if object.GetName() == registryName {
			return "Registry"
		}
		return "Expectation"
	case *corev1.Node:
		return "Node"
	case *corev1.Pod:
		return "Pod"
	case *corev1.Namespace:
		return "Namespace"
	case *appsv1.DaemonSet:
		return "DaemonSet"
	case *corev1.ServiceAccount:
		return "ServiceAccount"
	}
	return ""
}

var _ client.Reader = (*countingReader)(nil)
var _ admission.Handler = (*Handler)(nil)
