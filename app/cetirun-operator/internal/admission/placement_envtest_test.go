// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package sandboxadmission

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxplacement"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

func TestPlacementServingAndBindingAPIServer(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, name := range []string{"kube-apiserver", "etcd"} {
		info, err := os.Stat(filepath.Join(assets, name))
		if assets == "" || err != nil || info.Mode()&0111 == 0 {
			t.Fatal("preinstalled Kubernetes 1.37 envtest assets are required")
		}
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		ControlPlaneStartTimeout: time.Minute, ControlPlaneStopTimeout: 30 * time.Second,
	}
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
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, api.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
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
	for _, name := range []string{"operator-test", "ordinary-workload", "agent-workload"} {
		create(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
		create(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: name}})
	}
	ordinary := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "ordinary-worker"},
		Spec: corev1.NodeSpec{ProviderID: "example://ordinary-worker"}}
	reserved := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "reserved-worker"},
		Spec: corev1.NodeSpec{ProviderID: "example://reserved-worker"}}
	create(ordinary)
	create(reserved)
	registry, err := json.Marshal(map[string]any{
		"version": "v1",
		"nodes": []any{
			map[string]any{"name": ordinary.Name, "uid": ordinary.UID, "providerID": ordinary.Spec.ProviderID},
			map[string]any{
				"name": reserved.Name, "uid": reserved.UID, "providerID": reserved.Spec.ProviderID,
				"reservation": map[string]any{
					"agent":       map[string]string{"name": "example-agent", "uid": "synthetic-agent"},
					"namespace":   map[string]string{"name": "agent-workload", "uid": "synthetic-namespace"},
					"statefulSet": map[string]string{"name": "example-agent", "uid": "synthetic-statefulset"},
				},
			},
		},
		"platformDaemonSets": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	state := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-placement", Namespace: "operator-test"},
		Data: map[string]string{"placement.json": string(registry)}}
	create(state)
	cert := newServingCertificate(t, 37)
	directory := t.TempDir()
	cert.write(t, directory)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	serverCtx, stopServer := context.WithCancel(ctx)
	done := make(chan error, 1)
	config := testConfig()
	config.PlacementConfigMapName = state.Name
	go func() {
		done <- Run(serverCtx, cfg, config, ServerOptions{
			CertDirectory: directory, Host: "127.0.0.1", Port: port, MetricsAddress: "0", ProbeAddress: "0",
			PodStateNamespace: state.Namespace,
		})
	}()
	stopped := false
	closeServer := func() {
		t.Helper()
		if stopped {
			return
		}
		stopped = true
		stopServer()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("placement admission process did not stop")
		}
	}
	t.Cleanup(closeServer)
	webhook := renderedPlacementWebhook(t, state.Namespace, state.Name, cert.ca)
	webhook.Webhooks[0].ClientConfig.Service = nil
	webhook.Webhooks[0].ClientConfig.URL = ptr.To("https://127.0.0.1:" + strconv.Itoa(port) + PlacementValidationPath)
	webhook.Webhooks[0].TimeoutSeconds = ptr.To(int32(2))
	create(webhook)

	pod := func(name, node string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ordinary-workload"},
			Spec: corev1.PodSpec{NodeName: node, AutomountServiceAccountToken: ptr.To(false),
				Containers: []corev1.Container{{Name: "probe", Image: "registry.example.com/fixture:v1"}}}}
	}
	if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 15*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := cl.Create(ctx, pod("ordinary-activation-check", ordinary.Name), client.DryRunAll); err != nil {
			return false, nil
		}
		err := cl.Create(ctx, pod("activation-check", reserved.Name), client.DryRunAll)
		return err != nil && strings.Contains(err.Error(), "denied the request"), nil
	}); err != nil {
		t.Fatal("placement webhook did not become active:", err)
	}
	t.Run("normal nodes remain available but direct reserved placement is denied", func(t *testing.T) {
		create(pod("ordinary-direct", ordinary.Name))
		if err := cl.Create(ctx, pod("reserved-direct", reserved.Name)); err == nil {
			t.Fatal("direct nodeName bypassed placement checks")
		}
	})
	t.Run("platform defaulted resources create bind and update", func(t *testing.T) {
		identity, err := kube.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
		if err != nil || identity.Status.UserInfo.Username == "" {
			t.Fatal("read authenticated synthetic creator identity:", err)
		}
		namespace := &corev1.Namespace{}
		if err := cl.Get(ctx, client.ObjectKey{Name: "ordinary-workload"}, namespace); err != nil {
			t.Fatal(err)
		}
		for index, preemption := range []corev1.PreemptionPolicy{corev1.PreemptLowerPriority, corev1.PreemptNever} {
			priority := &schedulingv1.PriorityClass{
				ObjectMeta: metav1.ObjectMeta{Name: "synthetic-platform-" + strconv.Itoa(index)},
				Value:      50000, PreemptionPolicy: &preemption,
			}
			create(priority)
			daemon := &appsv1.DaemonSet{
				ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("d", 57) + strconv.Itoa(index), Namespace: namespace.Name},
				Spec: appsv1.DaemonSetSpec{
					Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "platform-fixture"}},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "platform-fixture"}},
						Spec: corev1.PodSpec{
							ServiceAccountName: "default", AutomountServiceAccountToken: ptr.To(false),
							PriorityClassName: priority.Name,
							Containers: []corev1.Container{{
								Name: "platform", Image: "registry.example.com/fixture:v1",
								Resources: corev1.ResourceRequirements{
									Limits: corev1.ResourceList{
										corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("64Mi"),
									},
								},
							}},
							InitContainers: []corev1.Container{{
								Name: "init-platform", Image: "registry.example.com/fixture:v1",
								Resources: corev1.ResourceRequirements{
									Limits: corev1.ResourceList{
										corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("32Mi"),
									},
									Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
								},
							}},
						},
					},
				},
			}
			create(daemon)
			if len(daemon.Spec.Template.Spec.Containers[0].Resources.Requests) != 0 ||
				len(daemon.Spec.Template.Spec.InitContainers[0].Resources.Requests) != 1 {
				t.Fatal("API fixture no longer distinguishes Pod-only requests from the DaemonSet template")
			}
			digest, err := sandboxplacement.CanonicalDaemonSetDigest(daemon)
			if err != nil {
				t.Fatal(err)
			}
			var inventory map[string]json.RawMessage
			if err := json.Unmarshal([]byte(state.Data["placement.json"]), &inventory); err != nil {
				t.Fatal(err)
			}
			var platforms []json.RawMessage
			if err := json.Unmarshal(inventory["platformDaemonSets"], &platforms); err != nil {
				t.Fatal(err)
			}
			entry := map[string]any{
				"name": daemon.Name, "uid": daemon.UID,
				"namespace":      map[string]any{"name": namespace.Name, "uid": namespace.UID},
				"templateDigest": digest, "trustedCreator": identity.Status.UserInfo.Username, "bootstrapPods": []any{},
			}
			registration, err := json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			inventory["platformDaemonSets"], err = json.Marshal(append(platforms, registration))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(inventory)
			if err != nil {
				t.Fatal(err)
			}
			state.Data["placement.json"] = string(raw)
			if err := cl.Update(ctx, state); err != nil {
				t.Fatal(err)
			}
			value := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: daemon.Name + "-", Namespace: namespace.Name,
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(daemon, appsv1.SchemeGroupVersion.WithKind("DaemonSet"))},
					Labels:          map[string]string{"app": "platform-fixture", appsv1.ControllerRevisionHashLabelKey: "synthetic-revision"},
				},
				Spec: *daemon.Spec.Template.Spec.DeepCopy(),
			}
			if generation, found := daemon.Annotations[appsv1.DeprecatedTemplateGeneration]; found {
				value.Labels["pod-template-generation"] = generation
			}
			value.Spec.NodeName = reserved.Name
			value.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{reserved.Name}}},
				}}},
			}}
			for _, taint := range []struct {
				key    string
				effect corev1.TaintEffect
			}{
				{corev1.TaintNodeNotReady, corev1.TaintEffectNoExecute},
				{corev1.TaintNodeUnreachable, corev1.TaintEffectNoExecute},
				{corev1.TaintNodeDiskPressure, corev1.TaintEffectNoSchedule},
				{corev1.TaintNodeMemoryPressure, corev1.TaintEffectNoSchedule},
				{corev1.TaintNodePIDPressure, corev1.TaintEffectNoSchedule},
				{corev1.TaintNodeUnschedulable, corev1.TaintEffectNoSchedule},
			} {
				value.Spec.Tolerations = append(value.Spec.Tolerations, corev1.Toleration{
					Key: taint.key, Operator: corev1.TolerationOpExists, Effect: taint.effect,
				})
			}
			pending := value.DeepCopy()
			pending.Spec.NodeName = ""
			if err := cl.Create(ctx, value); err != nil {
				t.Fatal("API-defaulted registered platform Pod was rejected:", err)
			}
			if len(value.Name) != 63 || !strings.HasPrefix(value.Name, daemon.Name) ||
				ptr.Deref(value.Spec.Priority, 0) != priority.Value || ptr.Deref(value.Spec.PreemptionPolicy, "") != preemption {
				t.Fatal("API fixture did not exercise truncated naming and class-derived Pod fields")
			}
			assertResources := func(pod *corev1.Pod) {
				t.Helper()
				if pod.Spec.Containers[0].Resources.Requests.Cpu().Cmp(resource.MustParse("1")) != 0 ||
					pod.Spec.Containers[0].Resources.Requests.Memory().Cmp(resource.MustParse("64Mi")) != 0 ||
					pod.Spec.InitContainers[0].Resources.Requests.Cpu().Cmp(resource.MustParse("100m")) != 0 ||
					pod.Spec.InitContainers[0].Resources.Requests.Memory().Cmp(resource.MustParse("32Mi")) != 0 {
					t.Fatal("Pod-only request defaults or explicitly declared requests differ from the native API contract")
				}
			}
			assertResources(value)
			if err := cl.Create(ctx, pending); err != nil {
				t.Fatal("unscheduled API-defaulted platform Pod was rejected:", err)
			}
			assertResources(pending)
			binding := &corev1.Binding{
				ObjectMeta: metav1.ObjectMeta{Name: pending.Name, Namespace: pending.Namespace, UID: pending.UID},
				Target:     corev1.ObjectReference{Kind: "Node", Name: reserved.Name},
			}
			if err := kube.CoreV1().Pods(pending.Namespace).Bind(ctx, binding, metav1.CreateOptions{}); err == nil ||
				!strings.Contains(err.Error(), "PlatformPodNotEnrolled") {
				t.Fatal("defaulting bypassed the required persisted Pod-UID enrollment:", err)
			}
			entry["bootstrapPods"] = []map[string]any{{"name": pending.Name, "uid": pending.UID}}
			registration, err = json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			inventory["platformDaemonSets"], err = json.Marshal(append(platforms, registration))
			if err != nil {
				t.Fatal(err)
			}
			raw, err = json.Marshal(inventory)
			if err != nil {
				t.Fatal(err)
			}
			state.Data["placement.json"] = string(raw)
			if err := cl.Update(ctx, state); err != nil {
				t.Fatal(err)
			}
			if err := kube.CoreV1().Pods(pending.Namespace).Bind(ctx, binding, metav1.CreateOptions{}); err != nil {
				t.Fatal("enrolled API-defaulted platform Pod could not bind:", err)
			}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(pending), pending); err != nil {
				t.Fatal(err)
			}
			if pending.Spec.NodeName != reserved.Name {
				t.Fatal("successful binding did not persist the reserved destination")
			}
			assertResources(pending)
			if err := cl.Update(ctx, pending.DeepCopy(), client.DryRunAll); err != nil {
				t.Fatal("bound API-defaulted platform Pod could not pass update validation:", err)
			}
			if err := cl.Delete(ctx, pending, client.GracePeriodSeconds(0)); err != nil {
				t.Fatal(err)
			}
			if err := cl.Delete(ctx, value, client.GracePeriodSeconds(0)); err != nil {
				t.Fatal(err)
			}
			if err := cl.Get(ctx, client.ObjectKeyFromObject(daemon), daemon); err != nil {
				t.Fatal(err)
			}
			currentDigest, err := sandboxplacement.CanonicalDaemonSetDigest(daemon)
			if err != nil || currentDigest != digest {
				t.Fatal("Pod lifecycle changed the registered template:", err)
			}
		}
	})
	bind := func(value *corev1.Pod, node string, uid types.UID) error {
		return kube.CoreV1().Pods(value.Namespace).Bind(ctx, &corev1.Binding{
			ObjectMeta: metav1.ObjectMeta{Name: value.Name, Namespace: value.Namespace, UID: uid},
			Target:     corev1.ObjectReference{Kind: "Node", Name: node},
		}, metav1.CreateOptions{})
	}
	t.Run("scheduler binding cannot bypass reserved nodes", func(t *testing.T) {
		value := pod("binding-check", "")
		create(value)
		if err := bind(value, ordinary.Name, types.UID("synthetic-stale-pod")); err == nil {
			t.Fatal("binding accepted a different persisted Pod UID")
		}
		if err := bind(value, reserved.Name, value.UID); err == nil {
			t.Fatal("binding bypassed reserved-node validation")
		}
		annotated := &corev1.Binding{
			ObjectMeta: metav1.ObjectMeta{Name: value.Name, Namespace: value.Namespace, UID: value.UID,
				Annotations: map[string]string{"example.com/unexpected": "binding-mutation"}},
			Target: corev1.ObjectReference{Kind: "Node", Name: ordinary.Name},
		}
		if err := kube.CoreV1().Pods(value.Namespace).Bind(ctx, annotated, metav1.CreateOptions{}); err == nil {
			t.Fatal("Binding annotations bypassed effective Pod metadata validation")
		}
		if err := bind(value, ordinary.Name, value.UID); err != nil {
			t.Fatal("ordinary binding was blocked:", err)
		}
	})
	t.Run("legacy binding endpoint cannot bypass placement checks", func(t *testing.T) {
		value := pod("legacy-binding-check", "")
		create(value)
		err := kube.CoreV1().RESTClient().Post().Namespace(value.Namespace).Resource("bindings").Body(&corev1.Binding{
			ObjectMeta: metav1.ObjectMeta{Name: value.Name, Namespace: value.Namespace, UID: value.UID},
			Target:     corev1.ObjectReference{Kind: "Node", Name: reserved.Name},
		}).Do(ctx).Error()
		if err == nil || !strings.Contains(err.Error(), "sandbox-placement.cetirun.microsoft.com") {
			t.Fatalf("legacy binding did not reach and fail placement admission: %v", err)
		}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(value), value); err != nil {
			t.Fatal(err)
		}
		if value.Spec.NodeName != "" {
			t.Fatal("legacy binding changed the Pod's placement")
		}
	})
	t.Run("assigned namespace cannot use ordinary capacity by clearing ownership", func(t *testing.T) {
		value := pod("assigned-escape", ordinary.Name)
		value.Namespace = "agent-workload"
		if err := cl.Create(ctx, value); err == nil {
			t.Fatal("assigned namespace escaped to a known ordinary node")
		}
	})
	t.Run("unknown and replacement node identities fail closed", func(t *testing.T) {
		unknown := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "unregistered-worker"},
			Spec: corev1.NodeSpec{ProviderID: "example://unregistered-worker"}}
		create(unknown)
		if err := cl.Create(ctx, pod("unknown-direct", unknown.Name)); err == nil {
			t.Fatal("unknown node was treated as unreserved")
		}
		if err := cl.Delete(ctx, reserved); err == nil || strings.Contains(err.Error(), "UnsupportedRequest") {
			t.Fatalf("reserved node deletion was not checked against its reservation: %v", err)
		}
		base := reserved.DeepCopy()
		reserved.Spec.ProviderID = "example://replacement-worker"
		if err := cl.Update(ctx, reserved); err == nil || strings.Contains(err.Error(), "UnsupportedRequest") {
			t.Fatalf("reserved provider identity drift was not checked against its reservation: %v", err)
		}
		reserved = base
	})
	t.Run("same-name ordinary node replacement must be enrolled", func(t *testing.T) {
		if err := cl.Delete(ctx, ordinary); err != nil {
			t.Fatal("ordinary node cleanup was blocked:", err)
		}
		replacement := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: ordinary.Name},
			Spec: corev1.NodeSpec{ProviderID: ordinary.Spec.ProviderID}}
		create(replacement)
		if replacement.UID == ordinary.UID {
			t.Fatal("API fixture did not create a new Node identity")
		}
		if err := cl.Create(ctx, pod("replacement-direct", replacement.Name)); err == nil {
			t.Fatal("same-name replacement inherited placement authority")
		}
	})
	t.Run("ordinary terminating Pod finalizers remain removable", func(t *testing.T) {
		value := pod("ordinary-finalizer-check", "")
		value.Finalizers = []string{"batch.kubernetes.io/job-tracking"}
		create(value)
		if err := cl.Delete(ctx, value); err != nil {
			t.Fatal(err)
		}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(value), value); err != nil || value.DeletionTimestamp.IsZero() {
			t.Fatal("fixture did not enter termination:", err)
		}
		mutated := value.DeepCopy()
		mutated.Finalizers = nil
		mutated.Annotations = map[string]string{"example.com/changed": "not-cleanup"}
		if err := cl.Update(ctx, mutated); err == nil {
			t.Fatal("terminating cleanup allowed unrelated metadata changes")
		}
		value.Finalizers = nil
		if err := cl.Update(ctx, value); err != nil {
			t.Fatal("ordinary finalizer-only cleanup was blocked:", err)
		}
	})
	t.Run("registry and server failures do not block pod deletion or status", func(t *testing.T) {
		value := pod("cleanup-check", "")
		create(value)
		if err := cl.Delete(ctx, state); err != nil {
			t.Fatal(err)
		}
		if err := cl.Create(ctx, pod("registry-down-direct", ordinary.Name)); err == nil {
			t.Fatal("missing registry authorized direct placement")
		}
		value.Status.Phase = corev1.PodPending
		if err := cl.Status().Update(ctx, value); err != nil {
			t.Fatal("status update depended on placement registry:", err)
		}
		closeServer()
		if err := cl.Create(ctx, pod("server-down-direct", ordinary.Name)); err == nil {
			t.Fatal("unavailable serving process authorized placement")
		}
		if err := cl.Delete(ctx, value); err != nil {
			t.Fatal("Pod deletion depended on placement serving:", err)
		}
	})
}

func renderedPlacementWebhook(t *testing.T, namespace, registryName string, ca []byte) *admissionregistrationv1.ValidatingWebhookConfiguration {
	t.Helper()
	command := exec.Command("helm", "template", "placement-test", "../../chart", "--kube-version=1.37.0",
		"--set", "cetiadmin.enabled=false",
		"--set", "namespace="+namespace, "--set", "serviceAccount.name=controller",
		"--set", "sandboxAdmission.enabled=true", "--set", "sandboxAdmission.enforce=true",
		"--set", "sandboxAdmission.podsEnabled=true", "--set", "sandboxAdmission.placementConfigMapName="+registryName,
		"--set", "sandboxAdmission.tlsSecretName=placement-test-tls", "--set", "sandboxAdmission.adminUsers[0]=administrator",
		"--set-string", "sandboxAdmission.caBundle="+base64.StdEncoding.EncodeToString(ca))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("render placement webhook: %v\n%s", err, output)
	}
	decoder := kyaml.NewYAMLOrJSONDecoder(strings.NewReader(string(output)), 4096)
	for {
		var object unstructured.Unstructured
		if err := decoder.Decode(&object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if object.GetKind() == "ValidatingWebhookConfiguration" && object.GetName() == "cetirun-sandbox-placement" {
			webhook := &admissionregistrationv1.ValidatingWebhookConfiguration{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, webhook); err != nil {
				t.Fatal(err)
			}
			if len(webhook.Webhooks) != 1 {
				t.Fatal("chart emitted an ambiguous placement webhook")
			}
			return webhook
		}
	}
	t.Fatal("chart did not emit the placement webhook")
	return nil
}
