// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package sandboxadmission

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestPlannedMigrationServedAPIAndBinding(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, name := range []string{"kube-apiserver", "etcd"} {
		if info, err := os.Stat(filepath.Join(assets, name)); assets == "" || err != nil || info.Mode()&0111 == 0 {
			t.Fatal("preinstalled Kubernetes envtest assets are required")
		}
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true,
		ControlPlaneStartTimeout: time.Minute, ControlPlaneStopTimeout: 30 * time.Second}
	config, err := environment.Start()
	if err != nil {
		_ = environment.Stop()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, corev1.AddToScheme, appsv1.AddToScheme, admissionregistrationv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	cl, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	typed, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	create := func(object client.Object) {
		t.Helper()
		if err := cl.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "migration-example"}, Spec: api.AgentSpec{
		Agent: api.HarnessSpec{Type: api.AgentTypeOpenClaw}, User: api.UserSpec{Alias: "example", DisplayName: "Example"},
		GatewayTokenSecretRef: "runtime", SandboxRef: "example-sandbox", Team: "example-team",
	}}
	create(agent)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-" + agent.Name,
		Labels:          map[string]string{w.ProtectedNamespaceLabel: "true"},
		Annotations:     map[string]string{w.AgentNameAnnotation: agent.Name, w.AgentUIDAnnotation: string(agent.UID)},
		OwnerReferences: []metav1.OwnerReference{w.AgentOwner(agent.Name, agent.UID)}}}
	create(namespace)
	create(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: namespace.Name}})
	workload := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "migration-workload", Namespace: namespace.Name},
		Spec: appsv1.StatefulSetSpec{ServiceName: "migration-workload", Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "migration"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "migration"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent", Image: "example.com/agent:1"}}}}}}
	create(workload)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "migration-target"}, Spec: corev1.NodeSpec{ProviderID: "provider://example.test/target"}}
	create(node)
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if err := cl.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	request := api.PlannedMigrationRequest{RequestID: "migration-00000001", Target: api.MigrationObject{Name: node.Name, UID: node.UID}}
	agent.Spec.PlannedMigration = &request
	if err := cl.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	seedPod := func(name, migrationID string) *corev1.Pod {
		t.Helper()
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace.Name,
			Annotations:     map[string]string{api.AnnotationMigrationRequest: migrationID},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(workload, appsv1.SchemeGroupVersion.WithKind("StatefulSet"))}},
			Spec: corev1.PodSpec{ServiceAccountName: "default", AutomountServiceAccountToken: ptr.To(false),
				Containers: []corev1.Container{{Name: "agent", Image: "example.com/agent:1"}}}}
		create(pod)
		return pod
	}
	source, destination, stale := seedPod("source", request.RequestID), seedPod("destination", request.RequestID), seedPod("stale-destination", request.RequestID)
	recovery := []*corev1.Pod{seedPod("recover-source-current", ""), seedPod("recover-source-legacy", "")}
	completedRecovery := []*corev1.Pod{
		seedPod("recover-completed-current", request.RequestID), seedPod("recover-completed-legacy", request.RequestID),
	}
	ref := func(object client.Object) api.MigrationObject {
		return api.MigrationObject{Name: object.GetName(), UID: object.GetUID()}
	}
	synthetic := func(name string) api.MigrationObject {
		return api.MigrationObject{Name: name, UID: types.UID(name + "-uid")}
	}
	captured := &api.PlannedMigrationStatus{PlannedMigrationRequest: request, Phase: api.MigrationProtecting,
		Namespace: ref(namespace), Workload: ref(workload), WorkloadTemplateHash: strings.Repeat("0", 64), SourcePod: ref(source),
		Source: api.MigrationNode{MigrationObject: synthetic("source-node"), ProviderID: "provider://example.test/source", BootID: "source-boot",
			CSINode: synthetic("source-csi"), CSINodeID: "source-driver-id"},
		Destination: api.MigrationNode{MigrationObject: ref(node), ProviderID: node.Spec.ProviderID, BootID: "target-boot",
			CSINode: synthetic("target-csi"), CSINodeID: "target-driver-id"},
		Home: synthetic("home"), Volume: synthetic("volume"), Attachment: synthetic("attachment"),
		Driver: "csi.example.com", VolumeHandle: "synthetic-volume", StartedAt: metav1.Now()}
	bind := func(pod *corev1.Pod, target string, legacy bool) error {
		binding := &corev1.Binding{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
			Target: corev1.ObjectReference{Kind: "Node", Name: target}}
		if legacy {
			return typed.CoreV1().RESTClient().Post().Namespace(pod.Namespace).Resource("bindings").Body(binding).Do(ctx).Error()
		}
		return typed.CoreV1().Pods(pod.Namespace).Bind(ctx, binding, metav1.CreateOptions{})
	}
	handler, err := NewPodHandler(cl, PodConfig{StateNamespace: "operator-state"})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	webhook := &admission.Webhook{Handler: handler}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		webhook.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	_, _, rules := renderedPodAdmission(t, ca)
	rules.Webhooks[0].ClientConfig.Service = nil
	rules.Webhooks[0].ClientConfig.URL = ptr.To(server.URL + PodValidationPath)
	create(rules)
	if err := wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
		probe := source.DeepCopy()
		probe.Labels = map[string]string{"example.com/probe": "changed"}
		err := cl.Update(ctx, probe, client.DryRunAll)
		return calls.Load() > 0 && err != nil, nil
	}); err != nil {
		t.Fatal("rendered Pod admission did not activate:", err)
	}
	t.Run("both binding APIs allow only ordinary recovery before capture", func(t *testing.T) {
		for index, pod := range recovery {
			legacy := index == 1
			if err := bind(destination, node.Name, legacy); err == nil || !strings.Contains(err.Error(), "MigrationBindingHeld") {
				t.Fatal("an uncaptured destination template was admitted:", err)
			}
			agent.Spec.Suspended = true
			if err := cl.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			if err := bind(pod, node.Name, legacy); err == nil || !strings.Contains(err.Error(), "MigrationBindingHeld") {
				t.Fatal("suspension allowed pre-capture recovery binding:", err)
			}
			agent.Spec.Suspended = false
			agent.Finalizers = []string{sandbox.HoldFinalizer}
			if err := cl.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			if err := bind(pod, node.Name, legacy); err == nil || !strings.Contains(err.Error(), "MigrationBindingHeld") {
				t.Fatal("an active hold allowed pre-capture recovery binding:", err)
			}
			agent.Finalizers = nil
			if err := cl.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			if err := bind(pod, node.Name, legacy); err != nil {
				t.Fatal("ordinary source recovery binding failed:", err)
			}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil || pod.Spec.NodeName != node.Name {
				t.Fatal("source recovery binding did not persist:", err)
			}
		}
	})
	agent.Status.PlannedMigration = captured
	if err := cl.Status().Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	t.Run("accepted request identity is immutable and suspension remains available", func(t *testing.T) {
		changed := agent.DeepCopy()
		changed.Spec.PlannedMigration.RequestID = "migration-00000002"
		if err := cl.Update(ctx, changed); !apierrors.IsInvalid(err) {
			t.Fatal("active request replacement was accepted:", err)
		}
		changed = agent.DeepCopy()
		changed.Spec.PlannedMigration = nil
		if err := cl.Update(ctx, changed); !apierrors.IsInvalid(err) {
			t.Fatal("recorded destination removal was accepted:", err)
		}
		changed = agent.DeepCopy()
		changed.Spec.PlannedMigration.Target.UID = "another-node"
		if err := cl.Update(ctx, changed); !apierrors.IsInvalid(err) {
			t.Fatal("same request changed its destination:", err)
		}
		agent.Spec.Suspended = true
		if err := cl.Update(ctx, agent); err != nil {
			t.Fatal("suspension was blocked:", err)
		}
		agent.Spec.Suspended = false
		if err := cl.Update(ctx, agent); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("source finalizer changes require a durable acknowledgement", func(t *testing.T) {
		source.Finalizers = append(source.Finalizers, api.MigrationPodFinalizer)
		if err := cl.Update(ctx, source); err != nil {
			t.Fatal("recorded source could not be protected:", err)
		}
		early := source.DeepCopy()
		early.Finalizers = nil
		if err := cl.Update(ctx, early); err == nil {
			t.Fatal("unacknowledged finalization was accepted")
		}
		if err := cl.Delete(ctx, source); err != nil {
			t.Fatal(err)
		}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(source), source); err != nil {
			t.Fatal(err)
		}
		agent.Status.PlannedMigration.Phase = api.MigrationSourceTerminated
		if err := cl.Status().Update(ctx, agent); err != nil {
			t.Fatal(err)
		}
		source.Finalizers = slices.DeleteFunc(source.Finalizers, func(s string) bool { return s == api.MigrationPodFinalizer })
		if err := cl.Update(ctx, source); err != nil {
			t.Fatal("acknowledged finalization failed:", err)
		}
	})
	t.Run("both binding APIs require the exact acknowledged destination", func(t *testing.T) {
		if err := bind(destination, node.Name, false); err == nil || !strings.Contains(err.Error(), "MigrationBindingHeld") {
			t.Fatal("binding preceded source release:", err)
		}
		agent.Status.PlannedMigration.Phase = api.MigrationStarting
		if err := cl.Status().Update(ctx, agent); err != nil {
			t.Fatal(err)
		}
		if err := bind(destination, "other-node", false); err == nil || !strings.Contains(err.Error(), "MigrationBindingHeld") {
			t.Fatal("an unrequested destination was accepted:", err)
		}
		if err := bind(destination, node.Name, false); err != nil {
			t.Fatal("current destination binding failed:", err)
		}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(destination), destination); err != nil {
			t.Fatal(err)
		}
		if destination.Spec.NodeName != node.Name {
			t.Fatal("successful binding did not persist the requested node")
		}
		agent.Status.PlannedMigration.Phase = api.MigrationCompleted
		if err := cl.Status().Update(ctx, agent); err != nil {
			t.Fatal(err)
		}
		agent.Spec.PlannedMigration = &api.PlannedMigrationRequest{RequestID: "migration-00000002",
			Target: api.MigrationObject{Name: "next-node", UID: "next-node-uid"}}
		if err := cl.Update(ctx, agent); err != nil {
			t.Fatal(err)
		}
		for index, pod := range completedRecovery {
			legacy := index == 1
			if err := bind(pod, agent.Spec.PlannedMigration.Target.Name, legacy); err == nil || !strings.Contains(err.Error(), "MigrationBindingHeld") {
				t.Fatal("a new uncaptured target replaced the completed destination:", err)
			}
			agent.Finalizers = []string{sandbox.HoldFinalizer}
			if err := cl.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			if err := bind(pod, node.Name, legacy); err == nil || !strings.Contains(err.Error(), "MigrationBindingHeld") {
				t.Fatal("an execution hold allowed recovery of a completed destination:", err)
			}
			agent.Finalizers = nil
			if err := cl.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			if err := bind(pod, node.Name, legacy); err != nil {
				t.Fatal("source recovery did not preserve its completed destination pin:", err)
			}
		}
		if err := cl.Delete(ctx, node); err != nil {
			t.Fatal(err)
		}
		replacement := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node.Name}, Spec: node.Spec}
		create(replacement)
		replacement.Status = node.Status
		if err := cl.Status().Update(ctx, replacement); err != nil {
			t.Fatal(err)
		}
		if replacement.UID == node.UID {
			t.Fatal("test did not replace the Node identity")
		}
		for _, legacy := range []bool{false, true} {
			if err := bind(stale, node.Name, legacy); err == nil || !strings.Contains(err.Error(), "MigrationNodeChanged") {
				t.Fatalf("legacy=%v bound a same-name replacement: %v", legacy, err)
			}
		}
	})
}
