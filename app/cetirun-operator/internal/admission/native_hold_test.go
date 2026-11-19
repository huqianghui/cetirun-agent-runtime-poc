// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplatform"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

type holdPlatformSource struct{}

func (holdPlatformSource) Snapshot(context.Context) (sandboxplatform.Snapshot, error) {
	return sandboxplatform.Snapshot{ObservedAt: time.Now()}, nil
}

type holdFixture struct {
	pod     podFixture
	handler *Handler
	before  *api.Agent
	after   *api.Agent
	receipt *corev1.ConfigMap
}

func nativeHoldFixture(t *testing.T) holdFixture {
	t.Helper()
	ctx := context.Background()
	f := nativePodFixture(t)
	agent := f.fixture.Agent.DeepCopy()
	agent.Spec.Agent.Type = api.AgentTypeOpenClaw
	agent.Finalizers = []string{api.Finalizer, sandbox.HoldFinalizer, sandbox.PreparationFinalizer, sandbox.BootstrapFinalizer}
	if err := f.client.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	f.expected.Revision = strings.Repeat("a", 64)
	f.expected.Template.Annotations[w.RevisionAnnotation] = f.expected.Revision
	f.expected.SinglePod, f.expected.ExecutionHeld = false, true
	f.workload.Spec.Template = *f.expected.Template.DeepCopy()
	f.workload.Spec.Replicas = ptr.To[int32](0)
	f.workload.Annotations[w.RevisionAnnotation] = f.expected.Revision
	if err := f.client.Update(ctx, f.workload); err != nil {
		t.Fatal(err)
	}
	for i, source := range f.expected.Sources {
		if source.IsAgent() {
			current, err := w.AgentSourceReference(agent)
			if err != nil {
				t.Fatal(err)
			}
			f.expected.Sources[i] = current
		} else if source.Kind == "StatefulSet" {
			f.expected.Sources[i].ResourceVersion = f.workload.ResourceVersion
		}
	}
	saveNativePodExpectation(t, &f)
	f.state.Annotations[w.RevisionAnnotation] = f.expected.Revision
	if err := f.client.Update(ctx, f.state); err != nil {
		t.Fatal(err)
	}
	receipt := w.PreparationReceipt{
		Version: "v1", AgentName: agent.Name, AgentUID: agent.UID, NativeExecution: true,
		Prerequisites: w.PreparationPrerequisites{
			AgentUID: agent.UID, AgentGeneration: agent.Generation, NamespaceUID: f.expected.Namespace.UID,
			Home: f.expected.Home, Skills: f.expected.Skills, ChartVersion: "0.1.1",
			RuntimeSecret: w.ObjectIdentity{Name: "runtime-secret", UID: "secret-uid"},
		},
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	object := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: w.PreparationReceiptName(agent.UID), Namespace: f.state.Namespace, UID: "preparation-uid",
			OwnerReferences: []metav1.OwnerReference{w.AgentOwner(agent.Name, agent.UID)},
			Finalizers:      []string{sandbox.PreparationFinalizer},
			Annotations:     map[string]string{w.AgentUIDAnnotation: string(agent.UID), w.NativeStartAnnotation: f.expected.Revision},
		},
		Data: map[string]string{w.PreparationReceiptKey: string(raw)},
	}
	if err := f.client.Create(ctx, object); err != nil {
		t.Fatal(err)
	}
	config := testConfig()
	config.ControllerUsername = "system:serviceaccount:operator-state:controller"
	config.NativeExecution, config.PlatformNamespace, config.PlatformConfigMapName = true, f.state.Namespace, "platform"
	handler, err := NewHandler(f.client, config)
	if err != nil {
		t.Fatal(err)
	}
	// Unit tests isolate the metadata/state gate. Served-API tests exercise the
	// actual configured platform source and production controller requests.
	handler.platformSource = holdPlatformSource{}
	next := agent.DeepCopy()
	next.Finalizers = slices.DeleteFunc(next.Finalizers, func(value string) bool { return value == sandbox.HoldFinalizer })
	return holdFixture{pod: f, handler: handler, before: agent, after: next, receipt: object}
}

func (f holdFixture) request(t *testing.T) admission.Request {
	return requestFor(t, admissionv1.Update, f.after, f.before, f.handler.config.ControllerUsername)
}

func TestNativeAgentHoldReleaseRequiresExactOptInTransition(t *testing.T) {
	for _, scenario := range []string{"valid", "disabled", "wrong caller", "spec", "other finalizer", "suspended", "deleting"} {
		t.Run(scenario, func(t *testing.T) {
			f := nativeHoldFixture(t)
			switch scenario {
			case "disabled":
				f.handler.config.NativeExecution = false
			case "spec":
				f.after.Spec.User.DisplayName = "Changed"
			case "other finalizer":
				f.after.Finalizers = []string{api.Finalizer}
			case "suspended":
				f.before.Spec.Suspended, f.after.Spec.Suspended = true, true
			case "deleting":
				now := metav1.Now()
				f.before.DeletionTimestamp, f.after.DeletionTimestamp = &now, &now
			}
			request := f.request(t)
			if scenario == "wrong caller" {
				request.UserInfo.Username = "administrator"
			}
			response := f.handler.Validate(context.Background(), request)
			// Deleting Agents retain the existing controller finalization path.
			want := scenario == "valid" || scenario == "deleting"
			if response.Allowed != want {
				t.Fatalf("%s: allowed=%t want=%t: %+v", scenario, response.Allowed, want, response.Result)
			}
			if scenario == "valid" && controllerFinalizerUpdate(f.after, f.before) {
				t.Fatal("the generic finalizer predicate was weakened")
			}
		})
	}
}

func TestNativeAgentHoldReleaseRejectsStalePreparation(t *testing.T) {
	for _, scenario := range []string{"missing receipt", "marker", "native mode", "replicas", "template", "released", "snapshot missing", "changed Agent"} {
		t.Run(scenario, func(t *testing.T) {
			f := nativeHoldFixture(t)
			ctx := context.Background()
			switch scenario {
			case "missing receipt":
				f.receipt.Finalizers = nil
				if err := f.pod.client.Update(ctx, f.receipt); err != nil {
					t.Fatal(err)
				}
				if err := f.pod.client.Delete(ctx, f.receipt); err != nil {
					t.Fatal(err)
				}
			case "marker", "native mode":
				if scenario == "marker" {
					delete(f.receipt.Annotations, w.NativeStartAnnotation)
				} else {
					receipt, err := w.DecodePreparationReceipt(f.receipt, f.pod.expected.Agent)
					if err != nil {
						t.Fatal(err)
					}
					receipt.NativeExecution = false
					raw, err := json.Marshal(receipt)
					if err != nil {
						t.Fatal(err)
					}
					f.receipt.Data[w.PreparationReceiptKey] = string(raw)
				}
				if err := f.pod.client.Update(ctx, f.receipt); err != nil {
					t.Fatal(err)
				}
			case "replicas", "template":
				if scenario == "replicas" {
					f.pod.workload.Spec.Replicas = ptr.To[int32](1)
				} else {
					f.pod.workload.Spec.Template.Spec.Containers[0].Image = "example.com/other:1"
				}
				if err := f.pod.client.Update(ctx, f.pod.workload); err != nil {
					t.Fatal(err)
				}
			case "released", "snapshot missing":
				if scenario == "released" {
					f.pod.expected.ExecutionHeld = false
				} else {
					f.pod.expected.SourceDigests = nil
				}
				saveNativePodExpectation(t, &f.pod)
			case "changed Agent":
				changed := f.before.DeepCopy()
				changed.Spec.Suspended = true
				if err := f.pod.client.Update(ctx, changed); err != nil {
					t.Fatal(err)
				}
			}
			if response := f.handler.Validate(ctx, f.request(t)); response.Allowed {
				t.Fatal("stale native start state authorized hold release")
			}
		})
	}
}

func TestNativeAgentHoldReleaseRechecksConcurrentAuthority(t *testing.T) {
	f := nativeHoldFixture(t)
	var changed bool
	f.handler.reader = interceptor.NewClient(f.pod.client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if _, ok := object.(*appsv1.StatefulSet); ok && !changed {
				changed = true
				f.receipt.Annotations[w.NativeStartAnnotation] = strings.Repeat("b", 64)
				if err := c.Update(ctx, f.receipt); err != nil {
					return err
				}
			}
			return c.Get(ctx, key, object, opts...)
		},
	})
	response := f.handler.Validate(context.Background(), f.request(t))
	if !changed || response.Allowed {
		t.Fatalf("authority race was not rejected: changed=%t result=%+v", changed, response.Result)
	}
}

func TestNativeAgentHoldReleaseRequiresCurrentReservation(t *testing.T) {
	f := nativeHoldFixture(t)
	ctx := context.Background()
	state, err := sandboxcoordination.NewState(f.pod.state.Namespace, "coordination")
	if err != nil {
		t.Fatal(err)
	}
	state.UID = "authority-uid"
	if err := f.pod.client.Create(ctx, state); err != nil {
		t.Fatal(err)
	}
	store := &sandboxcoordination.Store{
		Client: f.pod.client, APIReader: f.pod.client, Namespace: state.Namespace,
		Identity: w.ObjectIdentity{Name: state.Name, UID: state.UID},
	}
	f.handler.coordination = store
	if response := f.handler.Validate(ctx, f.request(t)); response.Allowed {
		t.Fatal("missing native reservation was accepted")
	}
	intent, err := sandboxcoordination.NativeIntentFor(f.pod.expected)
	if err != nil {
		t.Fatal(err)
	}
	ticket, err := store.AcquireNative(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	if response := f.handler.Validate(ctx, f.request(t)); !response.Allowed {
		t.Fatalf("current native reservation was rejected: %+v", response.Result)
	}
	if _, err := store.BeginNativeStop(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	if response := f.handler.Validate(ctx, f.request(t)); response.Allowed {
		t.Fatal("stopping native reservation was accepted")
	}
}
