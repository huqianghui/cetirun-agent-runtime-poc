// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cetiadmin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const hookTestNamespace = "test-platform"
const hookTestSource = "cetirun-cetiadmin-config"
const hookTestImage = "registry.example.com/cetirun-operator:1.2.3"

type hookChartLookup struct{ dynamic.Interface }

func (c hookChartLookup) GetClientFor(version, kind string) (dynamic.NamespaceableResourceInterface, bool, error) {
	if version != "v1" || (kind != "ConfigMap" && kind != "Namespace") {
		return nil, false, fmt.Errorf("unexpected chart lookup: %s %s", version, kind)
	}
	resource := strings.ToLower(kind) + "s"
	return c.Resource(schema.GroupVersionResource{Version: version, Resource: resource}), kind == "ConfigMap", nil
}

func TestHookChartDisabledUpgradeUsesBoundInstallation(t *testing.T) {
	for _, scenario := range []string{"source-only", "never-bound", "retained-agent", "invalid-record", "empty-record", "renamed-instance"} {
		t.Run(scenario, func(t *testing.T) {
			chart, err := loader.Load("../../chart")
			if err != nil {
				t.Fatal(err)
			}
			overrides := map[string]any{
				"namespace": hookTestNamespace, "cetiadmin": map[string]any{"enabled": false},
			}
			capabilities := chartutil.DefaultCapabilities.Copy()
			if scenario == "renamed-instance" {
				overrides["cetiadmin"].(map[string]any)["instanceName"] = "replacement-admin"
				overrides["sandboxAdmission"] = map[string]any{"tlsSecretName": "test-tls", "caBundle": base64.StdEncoding.EncodeToString(hookTestCA(t)), "adminUsers": []any{"administrator"}}
				capabilities.KubeVersion.Version = "v1.37.0"
			}
			values, err := chartutil.ToRenderValues(chart, overrides, chartutil.ReleaseOptions{Name: "test-platform", Namespace: hookTestNamespace, IsUpgrade: true}, capabilities)
			if err != nil {
				t.Fatal(err)
			}
			objects := []runtime.Object{&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: hookTestSource, Namespace: hookTestNamespace}}}
			if scenario != "source-only" {
				state := &Installation{Record: Record{SchemaVersion: 1}}
				if scenario == "retained-agent" || scenario == "renamed-instance" {
					state.Agent = ObjectIdentity{Name: "cetiadmin", UID: "retained-agent-uid"}
				}
				state.Source = ObjectIdentity{Name: hookTestSource, Namespace: hookTestNamespace}
				object := hookTestStateObject(t, state)
				if scenario == "invalid-record" {
					object.Data[BindingKey] = "invalid"
				} else if scenario == "empty-record" {
					object.Data[BindingKey] = "{}"
				}
				objects = append(objects, object)
			}
			scheme := runtime.NewScheme()
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			_, err = engine.RenderWithClientProvider(chart, values, hookChartLookup{dynamicfake.NewSimpleDynamicClient(scheme, objects...)})
			wantSuccess := scenario == "source-only" || scenario == "never-bound"
			if (err == nil) != wantSuccess {
				t.Fatalf("disabled upgrade %s: %v", scenario, err)
			}
			if scenario == "retained-agent" && !strings.Contains(err.Error(), "Kubernetes >= 1.37") {
				t.Fatalf("retained installation lost its admission requirement: %v", err)
			}
			if scenario == "renamed-instance" && !strings.Contains(err.Error(), "instanceName") {
				t.Fatalf("retained identity changed before admission could revoke it: %v", err)
			}
		})
	}
}

type hookTestClient struct {
	client.Client
	patches int
	patch   func(context.Context, client.Object, client.Patch) error
	get     func(client.ObjectKey, client.Object) error
	list    func(client.ObjectList) error
}

func (c *hookTestClient) Patch(ctx context.Context, object client.Object, patch client.Patch, _ ...client.PatchOption) error {
	c.patches++
	if c.patch != nil {
		return c.patch(ctx, object, patch)
	}
	return c.Client.Patch(ctx, object, patch)
}

func (c *hookTestClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if c.get != nil {
		if err := c.get(key, object); err != nil {
			return err
		}
	}
	return c.Client.Get(ctx, key, object, options...)
}

func (c *hookTestClient) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if c.list != nil {
		if err := c.list(list); err != nil {
			return err
		}
	}
	return c.Client.List(ctx, list, options...)
}

func hookTestCA(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "example.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func hookTestFixture(t *testing.T, enabled bool) (*hookTestClient, HookOptions, *corev1.ConfigMap, *Installation) {
	t.Helper()
	config := DefaultConfig()
	config.Enabled = enabled
	config.Owner = Owner{Alias: "platform-owner", TenantID: "11111111-1111-1111-1111-111111111111", ObjectID: "22222222-2222-2222-2222-222222222222"}
	config.Kubernetes.APIAudience = "https://kubernetes.default.svc.cluster.local"
	config.Kubernetes.APIEgressCIDRs = []string{"192.0.2.10/32"}
	config.Model.InitialMonthlyBudgetUSD = "100"
	config.Runtime.Version = "1.2.3"
	raw, _ := json.Marshal(config)
	source := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: hookTestSource, Namespace: hookTestNamespace, UID: "source-uid", ResourceVersion: "7",
		Annotations: map[string]string{"helm.sh/resource-policy": "keep", "example.com/unrelated": "retain"}}, Data: map[string]string{ConfigKey: string(raw), "unrelated": "retain"}}
	hash, _ := ConfigHash(config)
	state := &Installation{Record: Record{SchemaVersion: 1,
		Source: ObjectIdentity{Name: source.Name, Namespace: source.Namespace, UID: source.UID}, ConfigHash: hash,
		Agent: ObjectIdentity{Name: config.InstanceName, UID: "agent-uid"}, Team: ObjectIdentity{Name: config.InstanceName, UID: "team-uid"},
		Namespace:      ObjectIdentity{Name: "cetirun-cetiadmin", UID: "namespace-uid"},
		ServiceAccount: ObjectIdentity{Name: "openclaw-cetiadmin-cetirun", Namespace: "cetirun-cetiadmin", UID: "sa-uid"}},
		Configuration: config, Phase: PhaseReady, Bootstrapped: true, OperatorImage: hookTestImage, RuntimeVersion: "1.2.3"}
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: state.Agent.Name, UID: state.Agent.UID, Generation: 4,
		Annotations: map[string]string{AnnotationSourceUID: string(source.UID), api.AnnotationOwnerTenantID: config.Owner.TenantID, api.AnnotationOwnerObjectID: config.Owner.ObjectID}},
		Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: api.AgentTypeHermes}, Team: state.Team.Name,
			User: api.UserSpec{Alias: config.Owner.Alias}, Helm: api.HelmSpec{Version: config.Runtime.Version}},
		Status: api.AgentStatus{Namespace: state.Namespace.Name, Phase: api.PhaseReady, ObservedGeneration: 4,
			Conditions: []metav1.Condition{{Type: api.ConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: 4}}}}
	if !enabled {
		state.Phase = PhaseDisabled
		agent.Status.Phase = api.PhaseSuspended
		agent.Status.ObservedGeneration = 0
		agent.Status.Conditions[0].Status = metav1.ConditionFalse
	}
	owner := []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Agent", Name: agent.Name, UID: agent.UID}}
	objects := []client.Object{source, agent,
		&api.Team{ObjectMeta: metav1.ObjectMeta{Name: state.Team.Name, UID: state.Team.UID, Annotations: map[string]string{AnnotationSourceUID: string(source.UID)}}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: state.Namespace.Name, UID: state.Namespace.UID, OwnerReferences: owner}},
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: state.ServiceAccount.Name, Namespace: state.Namespace.Name, UID: state.ServiceAccount.UID,
			OwnerReferences: owner, Annotations: map[string]string{AnnotationSourceUID: string(source.UID)}}, AutomountServiceAccountToken: ptr.To(false)},
	}
	for _, name := range []string{"cetirun-operator", "cetirun-sandbox-admission"} {
		containerName := "manager"
		if name == "cetirun-sandbox-admission" {
			containerName = "admission"
		}
		objects = append(objects, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: hookTestNamespace, UID: types.UID(name), Generation: 2},
			Spec:   appsv1.DeploymentSpec{Replicas: ptr.To(int32(2)), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: containerName, Image: hookTestImage}}}}},
			Status: appsv1.DeploymentStatus{ObservedGeneration: 2, UpdatedReplicas: 2, AvailableReplicas: 2, ReadyReplicas: 2}})
	}
	webhooks := &admissionv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-cetiadmin-admission"}}
	for _, kind := range []string{"configuration", "workloads", "operations"} {
		webhooks.Webhooks = append(webhooks.Webhooks, admissionv1.ValidatingWebhook{
			Name: "cetiadmin-" + kind + ".cetirun.microsoft.com", FailurePolicy: ptr.To(admissionv1.Fail),
			ClientConfig: admissionv1.WebhookClientConfig{CABundle: hookTestCA(t), Service: &admissionv1.ServiceReference{
				Namespace: hookTestNamespace, Name: "cetirun-sandbox-admission", Path: ptr.To("/validate-cetiadmin"), Port: ptr.To(int32(443))}},
		})
		if kind == "workloads" {
			webhooks.Webhooks[len(webhooks.Webhooks)-1].NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "cetirun-operator"}}
		}
	}
	objects = append(objects, webhooks, &admissionv1.MutatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "cetirun-cetiadmin-admission"},
		Webhooks: []admissionv1.MutatingWebhook{{
			Name: "cetiadmin-agent.cetirun.microsoft.com", FailurePolicy: ptr.To(admissionv1.Fail),
			ClientConfig: admissionv1.WebhookClientConfig{CABundle: webhooks.Webhooks[0].ClientConfig.CABundle, Service: &admissionv1.ServiceReference{
				Namespace: hookTestNamespace, Name: "cetirun-sandbox-admission", Path: ptr.To("/mutate-sandbox-agent"), Port: ptr.To(int32(443))}},
			MatchConditions: []admissionv1.MatchCondition{{Name: "cetiadmin-caller", Expression: `request.userInfo.username == "system:serviceaccount:cetirun-cetiadmin:openclaw-cetiadmin-cetirun"`}},
		}},
	}, hookTestStateObject(t, state))
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := &hookTestClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()}
	sum := sha256.Sum256(raw)
	return kube, HookOptions{Namespace: source.Namespace, ConfigMapName: source.Name, ExpectedConfigHash: hex.EncodeToString(sum[:]), Mode: "check", Timeout: 100 * time.Millisecond}, source, state
}

func hookTestStateObject(t *testing.T, state *Installation) *corev1.ConfigMap {
	t.Helper()
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: InstallationName(state.Source.Namespace, state.Source.Name), Namespace: state.Source.Namespace,
		UID: "record-uid", ResourceVersion: "3", Annotations: map[string]string{AnnotationSourceUID: string(state.Source.UID)}}, Data: map[string]string{BindingKey: string(raw)}}
}

func hookTestUpdate(t *testing.T, kube client.Client, object client.Object, mutate func()) {
	t.Helper()
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(object), object); err != nil {
		t.Fatal(err)
	}
	mutate()
	if err := kube.Update(context.Background(), object); err != nil {
		t.Fatal(err)
	}
}

func TestHookCurrentReadyAndDisabled(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		kube, options, _, _ := hookTestFixture(t, enabled)
		if err := RunHook(context.Background(), kube, options); err != nil {
			t.Fatalf("enabled=%v: %v", enabled, err)
		}
		if kube.patches != 0 {
			t.Fatal("check hook wrote configuration")
		}
	}
}

func TestHookOwnerSuspendedInstallation(t *testing.T) {
	for _, scenario := range []string{"suspended", "not-suspended", "running-writer", "other-disabled-reason"} {
		t.Run(scenario, func(t *testing.T) {
			kube, options, _, state := hookTestFixture(t, true)
			state.Phase, state.Reason, state.Hold = PhaseDisabled, "UserSuspended", true
			if scenario == "other-disabled-reason" {
				state.Reason = "ReconcileFailed"
			}
			object := hookTestStateObject(t, state)
			hookTestUpdate(t, kube, object, func() { object.Data = hookTestStateObject(t, state).Data })
			agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: state.Agent.Name}}
			hookTestUpdate(t, kube, agent, func() {
				agent.Spec.Suspended = scenario != "not-suspended"
				agent.Status.Phase = api.PhaseSuspended
				agent.Status.ObservedGeneration = 0
			})
			if scenario == "running-writer" {
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "writer", Namespace: state.Namespace.Name},
					Status: corev1.PodStatus{Phase: corev1.PodRunning}}
				if err := kube.Create(context.Background(), pod); err != nil {
					t.Fatal(err)
				}
			}
			err := RunHook(context.Background(), kube, options)
			if (err == nil) != (scenario == "suspended") || kube.patches != 0 {
				t.Fatalf("owner suspension check: error=%v writes=%d", err, kube.patches)
			}
		})
	}
}

func TestHookReadsOnlyDisabledBoundLegacySource(t *testing.T) {
	for _, scenario := range []string{"disabled", "enabled", "missing-enabled", "null-enabled", "replaced", "unknown-field"} {
		t.Run(scenario, func(t *testing.T) {
			kube, options, source, _ := hookTestFixture(t, false)
			var value map[string]any
			if err := json.Unmarshal([]byte(source.Data[ConfigKey]), &value); err != nil {
				t.Fatal(err)
			}
			delete(value, "platformReadNamespaces")
			value["scope"] = map[string]any{"mode": "all-managed-agents", "teamNames": []string{}, "platformReadNamespaces": []string{}}
			if scenario == "enabled" {
				value["enabled"] = true
			}
			if scenario == "missing-enabled" {
				delete(value, "enabled")
			}
			if scenario == "null-enabled" {
				value["enabled"] = nil
			}
			if scenario == "unknown-field" {
				value["ignored-secret-setting"] = true
			}
			raw, _ := json.Marshal(value)
			hookTestUpdate(t, kube, source, func() {
				source.Data[ConfigKey] = string(raw)
				if scenario == "replaced" {
					source.UID = "replacement-source"
				}
			})
			options.ExpectedConfigHash = hookRawHash(string(raw))
			err := RunHook(context.Background(), kube, options)
			if (scenario == "disabled") != (err == nil) {
				t.Fatalf("legacy source %s: %v", scenario, err)
			}
			if kube.patches != 0 {
				t.Fatal("legacy source read modified configuration")
			}
		})
	}
}

func TestHookExplicitRuntimeKeepsDeploymentDefault(t *testing.T) {
	for _, current := range []bool{true, false} {
		t.Run(fmt.Sprintf("installed-runtime-current=%t", current), func(t *testing.T) {
			kube, options, _, state := hookTestFixture(t, true)
			state.RuntimeVersion = "1.0.0"
			object := hookTestStateObject(t, state)
			hookTestUpdate(t, kube, object, func() { object.Data = hookTestStateObject(t, state).Data })
			if !current {
				agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: state.Agent.Name}}
				hookTestUpdate(t, kube, agent, func() { agent.Spec.Helm.Version = "1.0.0" })
			}
			err := RunHook(context.Background(), kube, options)
			if (err == nil) != current {
				t.Fatalf("explicit runtime ready=%t, hook error=%v", current, err)
			}
			if kube.patches != 0 {
				t.Fatal("check hook changed the installation or deployment default")
			}
		})
	}
}

func TestHookUsesOrdinaryConditionAndRetainedSuspensionConventions(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		kube, options, _, state := hookTestFixture(t, enabled)
		agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: state.Agent.Name}}
		hookTestUpdate(t, kube, agent, func() {
			agent.Status.Conditions[0].ObservedGeneration = 0
			if !enabled {
				agent.Spec.Suspended = true
				agent.Status.ObservedGeneration = 0
			}
		})
		before := agent.DeepCopy()
		if err := RunHook(context.Background(), kube, options); err != nil {
			t.Fatal(err)
		}
		if err := kube.Get(context.Background(), client.ObjectKeyFromObject(agent), agent); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, agent) || kube.patches != 0 {
			t.Fatal("hook changed user suspension or retained lifecycle state")
		}
	}
}

func TestHookConsumesActualChartConfigAndWaitsForItsObservation(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("local Helm is required for the source serialization check")
	}
	kube, options, source, state := hookTestFixture(t, true)
	values := map[string]any{
		"namespace": hookTestNamespace, "domainSuffix": "agents.example.com",
		"global": map[string]any{"registry": "registry.example.com", "version": "1.2.3"},
		"cetiadmin": map[string]any{
			"owner": state.Configuration.Owner, "kubernetes": state.Configuration.Kubernetes,
			"model": state.Configuration.Model,
		},
		"sandboxAdmission": map[string]any{"tlsSecretName": "example-tls", "caBundle": base64.StdEncoding.EncodeToString(hookTestCA(t)), "adminUsers": []string{"admin@example.com"}},
	}
	input, _ := json.Marshal(values)
	renderContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(renderContext, "helm", "template", "platform", "../../chart", "--namespace", hookTestNamespace, "--kube-version", "1.37.0", "--values", "-")
	command.Env = append(os.Environ(), "KUBECONFIG="+os.DevNull, "HELM_REGISTRY_CONFIG="+os.DevNull, "HELM_REPOSITORY_CONFIG="+os.DevNull)
	command.Stdin = strings.NewReader(string(input))
	output, err := command.Output()
	if err != nil {
		t.Fatalf("render source configuration: %v", err)
	}
	decoder := yamlutil.NewYAMLOrJSONDecoder(strings.NewReader(string(output)), 4096)
	var rendered string
	readerRoles := 0
	for {
		var object struct {
			Kind     string              `json:"kind"`
			Metadata metav1.ObjectMeta   `json:"metadata"`
			Data     map[string]string   `json:"data"`
			Rules    []rbacv1.PolicyRule `json:"rules"`
		}
		if err := decoder.Decode(&object); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if object.Kind == "ConfigMap" && object.Metadata.Name == source.Name {
			rendered = object.Data[ConfigKey]
		}
		if object.Kind == "ClusterRole" && (object.Metadata.Name == "cetirun-cetiadmin-check" || object.Metadata.Name == "cetirun-cetiadmin-stop") {
			readerRoles++
			agents, namespaces := false, false
			for _, rule := range object.Rules {
				for _, verb := range rule.Verbs {
					if verb != "get" && verb != "list" {
						t.Fatalf("hook ClusterRole grants a write verb: %s", verb)
					}
				}
				if slices.Contains(rule.Resources, "secrets") {
					t.Fatal("hook ClusterRole grants Secret access")
				}
				if len(rule.ResourceNames) == 0 && slices.Contains(rule.Verbs, "list") {
					agents = agents || slices.Contains(rule.APIGroups, api.GroupVersion.Group) && slices.Contains(rule.Resources, "agents")
					namespaces = namespaces || slices.Contains(rule.APIGroups, "") && slices.Contains(rule.Resources, "namespaces")
				}
			}
			if !agents || !namespaces {
				t.Fatalf("%s cannot observe all current handoffs and namespace markers", object.Metadata.Name)
			}
		}
	}
	if rendered == "" || readerRoles != 2 {
		t.Fatal("chart did not render its config.json source and both read-only hook roles")
	}
	hookTestUpdate(t, kube, source, func() { source.Data[ConfigKey] = rendered })
	options.ExpectedConfigHash, options.Timeout = hookRawHash(rendered), 3*time.Second
	config, err := ParseConfig(rendered)
	if err != nil {
		t.Fatalf("rendered source violates the shared Config contract: %v", err)
	}
	reads := 0
	kube.get = func(key client.ObjectKey, _ client.Object) error {
		if key.Name == InstallationName(source.Namespace, source.Name) {
			reads++
			if reads == 2 {
				state.Configuration = config
				state.ConfigHash, _ = ConfigHash(config)
				object := hookTestStateObject(t, state)
				hookTestUpdate(t, kube.Client, object, func() { object.Data = hookTestStateObject(t, state).Data })
			}
		}
		return nil
	}
	if err := RunHook(context.Background(), kube, options); err != nil {
		t.Fatal(err)
	}
	if reads < 2 || kube.patches != 0 {
		t.Fatal("check did not wait for the matching parsed-config observation")
	}
}

func TestHookDisabledWithoutAnAgentStillNeedsOperatorConfirmation(t *testing.T) {
	kube, options, _, state := hookTestFixture(t, false)
	state.Agent, state.Team, state.Namespace, state.ServiceAccount = ObjectIdentity{}, ObjectIdentity{}, ObjectIdentity{}, ObjectIdentity{}
	state.Bootstrapped = false
	object := hookTestStateObject(t, state)
	hookTestUpdate(t, kube, object, func() { object.Data = hookTestStateObject(t, state).Data })
	if err := RunHook(context.Background(), kube, options); err != nil {
		t.Fatal(err)
	}
	if err := kube.Delete(context.Background(), object); err != nil {
		t.Fatal(err)
	}
	if err := RunHook(context.Background(), kube, options); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing checkpoint was not left unconfirmed: %v", err)
	}
}

func TestHookStopDoesNotWriteAnUnboundOrReplacedSource(t *testing.T) {
	for _, change := range []string{"unbound", "replaced"} {
		t.Run(change, func(t *testing.T) {
			kube, options, source, state := hookTestFixture(t, true)
			options.Mode = "stop"
			if change == "unbound" {
				if err := kube.Delete(context.Background(), hookTestStateObject(t, state)); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := kube.Delete(context.Background(), source); err != nil {
					t.Fatal(err)
				}
				source.UID, source.ResourceVersion = "replacement-uid", ""
				if err := kube.Create(context.Background(), source); err != nil {
					t.Fatal(err)
				}
			}
			if err := RunHook(context.Background(), kube, options); err == nil || kube.patches != 0 {
				t.Fatalf("unattributed source was modified: patches=%d, error=%v", kube.patches, err)
			}
		})
	}
}

func TestHookStopPreservesJSONBytesAndRefusesOtherChanges(t *testing.T) {
	raw := "{\n \"enabled\" : true, \"monitor\": {\"enabled\": true}, \"label\": \"enabled:true\"\n}"
	disabled, err := hookEnabledJSON(raw, false)
	if err != nil || disabled != strings.Replace(raw, ": true,", ": false,", 1) {
		t.Fatalf("stop changed bytes other than the top-level boolean: %v", err)
	}
	restored, err := hookEnabledJSON(disabled, true)
	if err != nil || restored != raw {
		t.Fatal("retry cannot recover the original exact hash")
	}
	for _, invalid := range []string{`{}`, `{"enabled":null}`, `{"enabled":"false"}`, `{"enabled":true,"enabled":false}`} {
		if _, err := hookEnabledJSON(invalid, false); err == nil {
			t.Fatal("ambiguous enabled field accepted")
		}
	}
}

func TestHookDoesNotTreatResolvedResumedTargetsOrOtherSubjectsAsPending(t *testing.T) {
	kube, options, _, state := hookTestFixture(t, false)
	for _, extra := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-namespace", UID: "target-namespace-uid"}},
		&api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "not-installed-yet", UID: "not-installed-uid"}},
		&api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "target", UID: "target-uid", Generation: 4}, Status: api.AgentStatus{
			HelmMaintenance: &api.HelmMaintenanceStatus{Phase: api.HelmMaintenancePhaseReleased, ObservedGeneration: 3}}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: state.Namespace.Name},
			Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: "someone-else", Namespace: state.Namespace.Name}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "retained-completed", Namespace: state.Namespace.Name}, Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
	} {
		if err := kube.Create(context.Background(), extra); err != nil {
			t.Fatal(err)
		}
	}
	if err := RunHook(context.Background(), kube, options); err != nil {
		t.Fatal(err)
	}
}

func TestHookRejectsStaleOrIncompleteReadiness(t *testing.T) {
	for _, change := range []string{"source hash", "source UID", "record marker", "state hash", "state config", "active", "pending create", "Agent UID", "Agent generation", "Ready generation", "suspended", "operator absent", "operator old generation", "old image", "admission absent", "fake CA", "mutator absent", "wrong caller"} {
		t.Run(change, func(t *testing.T) {
			kube, options, source, state := hookTestFixture(t, true)
			switch change {
			case "source hash":
				options.ExpectedConfigHash = strings.Repeat("0", 64)
			case "source UID", "state hash", "state config", "active", "pending create", "Agent UID", "old image":
				switch change {
				case "source UID":
					state.Source.UID = "foreign"
				case "state hash":
					state.ConfigHash = "old"
				case "state config":
					state.Configuration.Model.Default = "other"
				case "active":
					state.Phase = PhaseActive
				case "pending create":
					state.PendingCreate = "Agent"
				case "Agent UID":
					state.Agent.UID = "foreign"
				case "old image":
					state.OperatorImage = "registry.example.com/operator:old"
				}
				object := hookTestStateObject(t, state)
				hookTestUpdate(t, kube, object, func() { object.Data = hookTestStateObject(t, state).Data })
			case "record marker":
				object := hookTestStateObject(t, state)
				hookTestUpdate(t, kube, object, func() { object.Annotations[AnnotationSourceUID] = "foreign" })
			case "Agent generation", "Ready generation", "suspended":
				agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: state.Agent.Name}}
				hookTestUpdate(t, kube, agent, func() {
					if change == "suspended" {
						agent.Spec.Suspended = true
					} else if change == "Agent generation" {
						agent.Status.ObservedGeneration--
					} else {
						agent.Status.Conditions[0].ObservedGeneration--
					}
				})
			case "operator absent", "admission absent":
				name := "cetirun-operator"
				if change == "admission absent" {
					name = "cetirun-sandbox-admission"
				}
				if err := kube.Delete(context.Background(), &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: source.Namespace}}); err != nil {
					t.Fatal(err)
				}
			case "operator old generation":
				object := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-operator", Namespace: source.Namespace}}
				if err := kube.Get(context.Background(), client.ObjectKeyFromObject(object), object); err != nil {
					t.Fatal(err)
				}
				object.Status.ObservedGeneration--
				if err := kube.Status().Update(context.Background(), object); err != nil {
					t.Fatal(err)
				}
			case "fake CA":
				object := &admissionv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-cetiadmin-admission"}}
				hookTestUpdate(t, kube, object, func() {
					object.Webhooks[0].ClientConfig.CABundle = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("not a certificate")})
				})
			case "mutator absent", "wrong caller":
				object := &admissionv1.MutatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-cetiadmin-admission"}}
				if change == "mutator absent" {
					if err := kube.Delete(context.Background(), object); err != nil {
						t.Fatal(err)
					}
				} else {
					hookTestUpdate(t, kube, object, func() { object.Webhooks[0].MatchConditions[0].Expression = "true" })
				}
			}
			if err := RunHook(context.Background(), kube, options); err == nil {
				t.Fatal("incomplete readiness reported success")
			}
			if kube.patches != 0 {
				t.Fatal("check hook wrote an object")
			}
		})
	}
}

func TestHookStopConditionalPatchAndRetryAfterLostAcknowledgement(t *testing.T) {
	kube, options, source, state := hookTestFixture(t, true)
	options.Mode = "stop"
	original := source.DeepCopy()
	kube.patch = func(ctx context.Context, object client.Object, patch client.Patch) error {
		if client.ObjectKeyFromObject(object) != client.ObjectKeyFromObject(source) || patch.Type() != types.JSONPatchType {
			t.Fatal("stop patched an unrelated object or used a nonconditional patch")
		}
		data, err := patch.Data(object)
		if err != nil {
			t.Fatal(err)
		}
		var operations []struct {
			Op, Path string
			Value    any
		}
		if err := json.Unmarshal(data, &operations); err != nil {
			t.Fatal(err)
		}
		if len(operations) != 4 || operations[0].Op != "test" || operations[0].Path != "/metadata/uid" || operations[0].Value != string(source.UID) ||
			operations[1].Op != "test" || operations[1].Path != "/metadata/resourceVersion" || operations[1].Value != source.ResourceVersion ||
			operations[2].Op != "test" || operations[2].Path != "/data/config.json" || operations[3].Op != "replace" || operations[3].Path != "/data/config.json" {
			t.Fatal("stop did not test source UID/RV/data before replacing only config.json")
		}
		if err := kube.Client.Patch(ctx, object, patch); err != nil {
			return err
		}
		return errors.New("synthetic lost patch acknowledgement")
	}
	if err := RunHook(context.Background(), kube, options); err == nil {
		t.Fatal("unknown write outcome reported success")
	}
	if kube.patches != 1 {
		t.Fatal("stop retried an unknown write")
	}
	current := &corev1.ConfigMap{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(source), current); err != nil {
		t.Fatal(err)
	}
	config, err := ParseConfig(current.Data[ConfigKey])
	if err != nil || config.Enabled {
		t.Fatal("disable did not persist")
	}
	if current.Data["unrelated"] != original.Data["unrelated"] || !reflect.DeepEqual(current.Annotations, original.Annotations) {
		t.Fatal("stop overwrote unrelated source content")
	}
	state.Configuration, state.Phase = config, PhaseDisabled
	state.ConfigHash, _ = ConfigHash(config)
	object := hookTestStateObject(t, state)
	hookTestUpdate(t, kube, object, func() { object.Data = hookTestStateObject(t, state).Data })
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: state.Agent.Name}}
	hookTestUpdate(t, kube, agent, func() {
		agent.Status.Phase = api.PhaseSuspended
		agent.Status.Conditions[0].Status = metav1.ConditionFalse
	})
	if err := RunHook(context.Background(), kube, options); err != nil {
		t.Fatalf("same-intent stop did not resume: %v", err)
	}
	if kube.patches != 1 {
		t.Fatal("resumed stop issued another patch")
	}
	if err := RunHook(context.Background(), kube, options); err != nil {
		t.Fatal(err)
	}
}

func TestHookStopRejectsConflictAndConcurrentSourceChange(t *testing.T) {
	for _, mode := range []string{"conflict", "source replaced", "already false with another model"} {
		t.Run(mode, func(t *testing.T) {
			kube, options, source, _ := hookTestFixture(t, true)
			options.Mode = "stop"
			if mode == "already false with another model" {
				hookTestUpdate(t, kube, source, func() {
					source.Data[ConfigKey] = strings.ReplaceAll(strings.ReplaceAll(source.Data[ConfigKey], `"enabled":true`, `"enabled":false`), `"default":"auto"`, `"default":"other"`)
				})
			} else {
				kube.patch = func(context.Context, client.Object, client.Patch) error {
					return apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, source.Name, errors.New(mode))
				}
			}
			if err := RunHook(context.Background(), kube, options); err == nil {
				t.Fatal("conflicting stop succeeded")
			}
			if kube.patches > 1 {
				t.Fatal("stop retried a conflict")
			}
		})
	}
}

func TestHookStopRequiresRevocationStoppedWritersAndResolvedMaintenance(t *testing.T) {
	for _, kind := range []string{"Pod", "TerminatingPod", "StatefulSet", "Deployment", "ReplicaSet", "DaemonSet", "Job", "CronJob", "RoleBinding", "DefaultedSubject", "ClusterRoleBinding", "Maintenance", "StaleReleased", "NamespaceMarker", "OperatorMissing"} {
		t.Run(kind, func(t *testing.T) {
			kube, options, _, state := hookTestFixture(t, false)
			options.Mode = "stop"
			metadata := metav1.ObjectMeta{Name: "remaining", Namespace: state.Namespace.Name}
			var object client.Object
			switch kind {
			case "Pod", "TerminatingPod":
				pod := &corev1.Pod{ObjectMeta: metadata, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
				if kind == "TerminatingPod" {
					pod.DeletionTimestamp = &metav1.Time{Time: time.Now()}
					pod.Finalizers = []string{"example.com/writer"}
				}
				object = pod
			case "StatefulSet":
				object = &appsv1.StatefulSet{ObjectMeta: metadata, Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))}}
			case "Deployment":
				object = &appsv1.Deployment{ObjectMeta: metadata, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))}}
			case "ReplicaSet":
				object = &appsv1.ReplicaSet{ObjectMeta: metadata, Spec: appsv1.ReplicaSetSpec{Replicas: ptr.To(int32(1))}}
			case "DaemonSet":
				object = &appsv1.DaemonSet{ObjectMeta: metadata, Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 1}}
			case "Job":
				object = &batchv1.Job{ObjectMeta: metadata}
			case "CronJob":
				object = &batchv1.CronJob{ObjectMeta: metadata}
			case "RoleBinding", "DefaultedSubject":
				object = &rbacv1.RoleBinding{ObjectMeta: metadata, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: state.ServiceAccount.Name, Namespace: state.ServiceAccount.Namespace}}}
				if kind == "DefaultedSubject" {
					object.(*rbacv1.RoleBinding).Subjects[0].Namespace = ""
				}
			case "ClusterRoleBinding":
				object = &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "remaining"}, Subjects: []rbacv1.Subject{{Kind: "User", Name: "system:serviceaccount:" + state.ServiceAccount.Namespace + ":" + state.ServiceAccount.Name}}}
			case "Maintenance", "StaleReleased", "NamespaceMarker":
				target := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "target", UID: "target-uid", Generation: 3},
					Status: api.AgentStatus{HelmMaintenance: &api.HelmMaintenanceStatus{RequestID: "maintenance-example-0001", Phase: api.HelmMaintenancePhaseHeld}}}
				if kind == "StaleReleased" {
					target.Spec.Suspended = true
					target.Status.HelmMaintenance.Phase = api.HelmMaintenancePhaseReleased
				}
				namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "target-namespace", UID: "target-namespace-uid"}}
				if kind == "NamespaceMarker" {
					target.Status.HelmMaintenance.Phase = api.HelmMaintenancePhaseReleased
					target.Status.HelmMaintenance.ObservedGeneration = target.Generation
					namespace.Annotations = map[string]string{MaintenanceAgentName: target.Name, MaintenanceAgentUID: string(target.UID), MaintenanceRequestID: "maintenance-example-0001"}
				}
				if err := kube.Create(context.Background(), namespace); err != nil {
					t.Fatal(err)
				}
				object = target
			case "OperatorMissing":
				if err := kube.Delete(context.Background(), &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-operator", Namespace: hookTestNamespace}}); err != nil {
					t.Fatal(err)
				}
			}
			if object != nil {
				if err := kube.Create(context.Background(), object); err != nil {
					t.Fatal(err)
				}
			}
			if err := RunHook(context.Background(), kube, options); err == nil {
				t.Fatal("unconfirmed stop reported success")
			}
			if kube.patches != 0 {
				t.Fatal("already disabled source was rewritten")
			}
		})
	}
}

func TestHookStopFindsOrphanedMaintenanceMarkers(t *testing.T) {
	for _, marker := range []string{MaintenanceAgentName, MaintenanceAgentUID, MaintenanceRequestID} {
		t.Run(marker, func(t *testing.T) {
			kube, options, _, _ := hookTestFixture(t, false)
			options.Mode = HookModeStop
			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orphaned-maintenance", UID: "orphaned-namespace-uid",
				Annotations: map[string]string{marker: "retained-maintenance-identity"}}}
			if err := kube.Create(context.Background(), namespace); err != nil {
				t.Fatal(err)
			}
			if err := RunHook(context.Background(), kube, options); err == nil {
				t.Fatal("stop ignored a maintenance marker whose Agent no longer exists")
			}
			if kube.patches != 0 {
				t.Fatal("already disabled source was rewritten")
			}
		})
	}
}

func TestHookStopRequiresCurrentMaintenanceLists(t *testing.T) {
	for _, kind := range []string{"agents", "namespaces"} {
		t.Run(kind, func(t *testing.T) {
			kube, options, _, _ := hookTestFixture(t, false)
			options.Mode = HookModeStop
			unavailable := errors.New("synthetic maintenance list failure")
			kube.list = func(list client.ObjectList) error {
				switch list.(type) {
				case *api.AgentList:
					if kind == "agents" {
						return unavailable
					}
				case *corev1.NamespaceList:
					if kind == "namespaces" {
						return unavailable
					}
				}
				return nil
			}
			if err := RunHook(context.Background(), kube, options); !errors.Is(err, unavailable) {
				t.Fatalf("stop lost an incomplete %s observation: %v", kind, err)
			}
			if kube.patches != 0 {
				t.Fatal("already disabled source was rewritten")
			}
		})
	}
}

func TestHookInputValidationAndDeadline(t *testing.T) {
	kube, options, _, _ := hookTestFixture(t, true)
	for _, mutate := range []func(*HookOptions){
		func(o *HookOptions) { o.Namespace = "" }, func(o *HookOptions) { o.ConfigMapName = "../other" },
		func(o *HookOptions) { o.Mode = "delete" }, func(o *HookOptions) { o.ExpectedConfigHash = "missing" },
		func(o *HookOptions) { o.Timeout = -time.Second }, func(o *HookOptions) { o.Timeout = 2 * time.Hour },
	} {
		invalid := options
		mutate(&invalid)
		if err := RunHook(context.Background(), kube, invalid); err == nil {
			t.Fatal("invalid hook options accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunHook(ctx, kube, options); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if kube.patches != 0 {
		t.Fatal("invalid invocation wrote configuration")
	}
}
