// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestRegistryServiceAccountDistinguishesContainerSetuidFromHostPrivilege(t *testing.T) {
	values := map[string]any{"securityContext": map[string]any{
		"allowPrivilegeEscalation": true, "runAsNonRoot": true, "runAsUser": 1024,
		"capabilities": map[string]any{},
	}}
	if sa, err := registryServiceAccount(values, "example"); err != nil || sa != "default" {
		t.Fatalf("ordinary built-in security context rejected before RBAC checks: %s %v", sa, err)
	}
	security := values["securityContext"].(map[string]any)
	security["privileged"] = true
	if _, err := registryServiceAccount(values, "example"); err == nil {
		t.Fatal("privileged container accepted")
	}
	delete(security, "privileged")
	security["capabilities"] = map[string]any{"add": []any{"SYS_ADMIN"}}
	if _, err := registryServiceAccount(values, "example"); err == nil {
		t.Fatal("unqualified added capabilities accepted")
	}
}

func registryFixture(t *testing.T) (*AgentReconciler, *api.Agent, *corev1.Secret) {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	_ = istiov1beta1.AddToScheme(scheme)
	agent := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "11111111-1111-4111-8111-111111111111", Generation: 1,
		Annotations: map[string]string{registryauth.Operation: "operation-one"}},
		Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: api.AgentTypeHermes, Image: ptr.To("registry.example.com/agent:1"), RegistrySecretRef: &api.RegistrySecretRef{Name: "registry-source-example"}},
			User: api.UserSpec{Alias: "example"}}}
	raw, err := registryauth.Encode(*agent.Spec.Agent.Image, registryauth.Credentials{Username: "example", Password: "synthetic-token"})
	if err != nil {
		t.Fatal(err)
	}
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agent.Spec.Agent.RegistrySecretRef.Name, Namespace: registryauth.DefaultNamespace,
		UID: "source-uid", ResourceVersion: "1", OwnerReferences: []metav1.OwnerReference{registryauth.Owner(agent)}, Labels: registryauth.Labels(agent.UID),
		Annotations: map[string]string{registryauth.Operation: "operation-one"}}, Type: corev1.SecretTypeDockerConfigJson, Data: map[string][]byte{corev1.DockerConfigJsonKey: raw}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(agent).WithObjects(agent, source).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, options ...client.CreateOption) error {
			if review, ok := obj.(*authorizationv1.SubjectAccessReview); ok {
				review.Status.Denied = true
				return nil
			}
			if obj.GetUID() == "" {
				obj.SetUID(types.UID("created-" + obj.GetName()))
			}
			return c.Create(ctx, obj, options...)
		}}).Build()
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(agent), agent)
	return &AgentReconciler{Client: c, APIReader: c, Scheme: scheme}, agent, source
}

func TestRegistrySyncRotationAndRestart(t *testing.T) {
	r, agent, source := registryFixture(t)
	ctx := context.Background()
	name, err := r.syncRegistryCredentials(ctx, agent, "default")
	if err != nil {
		t.Fatal(err)
	}
	target := &corev1.Secret{}
	key := client.ObjectKey{Namespace: "cetirun-example", Name: name}
	if err := r.Get(ctx, key, target); err != nil {
		t.Fatal(err)
	}
	if !registryauth.Owned(target, agent) || !reflect.DeepEqual(target.Data, source.Data) || target.Annotations[registryauth.SourceUID] != string(source.UID) {
		t.Fatal("invalid copy")
	}
	uid := target.UID
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(agent.Name), Namespace: key.Namespace, UID: "workload-uid"}, Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)}}
	if err := r.Create(ctx, sts); err != nil {
		t.Fatal(err)
	}
	before := sts.DeepCopy()
	if err := r.Get(ctx, client.ObjectKeyFromObject(source), source); err != nil {
		t.Fatal(err)
	}
	source.Data[corev1.DockerConfigJsonKey], _ = registryauth.Encode(*agent.Spec.Agent.Image, registryauth.Credentials{Username: "example", Password: "rotated-synthetic-token"})
	if err := r.Update(ctx, source); err != nil {
		t.Fatal(err)
	}
	// New reconciler, no in-memory credential cache.
	restarted := &AgentReconciler{Client: r.Client, Scheme: r.Scheme}
	if _, err := restarted.syncRegistryCredentials(ctx, agent, "default"); err != nil {
		t.Fatal(err)
	}
	_ = r.Get(ctx, key, target)
	_ = r.Get(ctx, client.ObjectKeyFromObject(sts), sts)
	if target.UID != uid || !reflect.DeepEqual(target.Data, source.Data) || !reflect.DeepEqual(before, sts) {
		t.Fatal("rotation replaced the Secret or touched the workload")
	}
	if agent.Status.RegistryCredentials.SourceResourceVersion != source.ResourceVersion || meta.FindStatusCondition(agent.Status.Conditions, api.ConditionRegistryCredentialsReady).Status != metav1.ConditionTrue {
		t.Fatal("missing sync receipt")
	}
}

type registryAuditClient struct {
	client.Client
	reviews, inventories atomic.Int64
	allow                atomic.Bool
}

func (c *registryAuditClient) Create(ctx context.Context, object client.Object, options ...client.CreateOption) error {
	if review, ok := object.(*authorizationv1.SubjectAccessReview); ok {
		c.reviews.Add(1)
		review.Status.Allowed = c.allow.Load()
		review.Status.Denied = !review.Status.Allowed
		return nil
	}
	return c.Client.Create(ctx, object, options...)
}

func (c *registryAuditClient) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	switch list.(type) {
	case *api.AgentList, *corev1.PodList, *appsv1.StatefulSetList, *rbacv1.RoleList, *rbacv1.ClusterRoleList, *rbacv1.RoleBindingList, *rbacv1.ClusterRoleBindingList:
		c.inventories.Add(1)
	}
	return c.Client.List(ctx, list, options...)
}

func TestRegistryUnchangedSyncSkipsAuditButRotationRechecksPermissions(t *testing.T) {
	r, agent, source := registryFixture(t)
	ctx := context.Background()
	c := &registryAuditClient{Client: r.Client}
	r.Client, r.APIReader = c, c
	name, err := r.syncRegistryCredentials(ctx, agent, "default")
	if err != nil || c.reviews.Load() == 0 || c.inventories.Load() == 0 {
		t.Fatalf("initial copy bypassed authorization: %v", err)
	}
	c.reviews.Store(0)
	c.inventories.Store(0)
	for range 100 {
		if _, err := r.syncRegistryCredentials(ctx, agent, "default"); err != nil {
			t.Fatal(err)
		}
	}
	if c.reviews.Load() != 0 || c.inventories.Load() != 0 {
		t.Fatalf("unchanged sync repeated the cluster audit: reviews=%d lists=%d", c.reviews.Load(), c.inventories.Load())
	}
	key := client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: name}
	target := &corev1.Secret{}
	if err := c.Get(ctx, key, target); err != nil {
		t.Fatal(err)
	}
	before := target.DeepCopy()
	if err := c.Get(ctx, client.ObjectKeyFromObject(source), source); err != nil {
		t.Fatal(err)
	}
	source.Data[corev1.DockerConfigJsonKey], _ = registryauth.Encode(*agent.Spec.Agent.Image,
		registryauth.Credentials{Username: "example", Password: "rotated-synthetic-token"})
	if err := c.Update(ctx, source); err != nil {
		t.Fatal(err)
	}
	c.allow.Store(true)
	if _, err := r.syncRegistryCredentials(ctx, agent, "default"); err == nil || c.reviews.Load() == 0 {
		t.Fatal("rotation reused the previous authorization result")
	}
	if err := c.Get(ctx, key, target); err != nil || !reflect.DeepEqual(before, target) {
		t.Fatal("failed authorization changed the existing pull Secret")
	}
	c.allow.Store(false)
	if _, err := r.syncRegistryCredentials(ctx, agent, "default"); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, target); err != nil || !reflect.DeepEqual(target.Data, source.Data) {
		t.Fatal("rotation did not recover after permissions were corrected")
	}
}

func TestRegistryBuiltinSteadyReconcileDoesNotRepeatAudit(t *testing.T) {
	for _, agentType := range []api.AgentType{api.AgentTypeHermes, api.AgentTypeOpenClaw} {
		t.Run(string(agentType), func(t *testing.T) {
			r, agent, _ := registryFixture(t)
			ctx := context.Background()
			ConfigureTestGateway(t, r)
			c := &registryAuditClient{Client: r.Client}
			r.Client, r.APIReader = c, c
			r.HelmClient = &registryNamespaceHelm{}
			r.EventRecorder = record.NewFakeRecorder(10)
			r.ChartVersion = "0.1.1"
			r.DomainSuffix = "agents.example.com"
			agent.Spec.Agent.Type = agentType
			agent.Spec.Agent.Port = ptr.To[int32](18080)
			agent.Spec.GatewayTokenSecretRef = "gateway-token"
			agent.Spec.Helm.Version = r.ChartVersion
			agent.Finalizers = []string{api.Finalizer}
			if err := r.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instanceNamespace(agent.Name),
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(agent, api.GroupVersion.WithKind("Agent"))}}}
			if err := r.Create(ctx, namespace); err != nil {
				t.Fatal(err)
			}
			gatewayToken := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agent.Spec.GatewayTokenSecretRef, Namespace: namespace.Name},
				Data: map[string][]byte{"API_SERVER_KEY": []byte("synthetic-runtime-token"), "CETIRUN_AUTH_KEY": []byte("synthetic-auth-key")}}
			if err := r.Create(ctx, gatewayToken); err != nil {
				t.Fatal(err)
			}
			if _, err := r.syncRegistryCredentials(ctx, agent, "default"); err != nil {
				t.Fatal(err)
			}
			now := metav1.Now()
			agent.Status.Phase = api.PhaseReady
			setCondition(&agent.Status.Conditions, api.ConditionHelmDeployed, metav1.ConditionTrue, "ChartInstalled", "synthetic steady workload")
			agent.Status.AIGateway = &api.AIGatewayStatus{Phase: api.AIGatewayPhaseReady, Routed: true,
				KeyID: "existing-key", KeySecretName: aiGatewayKeySecretName, LastVerifiedTime: &now}
			agent.Status.ObservedGeneration = agent.Generation
			agent.Status.ObservedOperatorHash = r.configHash(agent, nil, secretContentHash(gatewayToken.Data), "")
			if err := r.updateStatus(ctx, agent); err != nil {
				t.Fatal(err)
			}
			c.reviews.Store(0)
			c.inventories.Store(0)
			for range 10 {
				if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(agent)}); err != nil {
					t.Fatal(err)
				}
			}
			if c.reviews.Load() != 0 || c.inventories.Load() != 0 {
				t.Fatalf("steady built-in reconciliation repeated the audit: reviews=%d inventories=%d", c.reviews.Load(), c.inventories.Load())
			}
		})
	}
}

func TestRegistrySourceFailureRetainsExistingCopy(t *testing.T) {
	for _, failure := range []string{"missing", "owner", "type", "host", "data", "operation", "labels-only", "target"} {
		t.Run(failure, func(t *testing.T) {
			r, agent, source := registryFixture(t)
			ctx := context.Background()
			name, err := r.syncRegistryCredentials(ctx, agent, "default")
			if err != nil {
				t.Fatal(err)
			}
			target := &corev1.Secret{}
			key := client.ObjectKey{Namespace: "cetirun-example", Name: name}
			_ = r.Get(ctx, key, target)
			old := target.DeepCopy()
			_ = r.Get(ctx, client.ObjectKeyFromObject(source), source)
			switch failure {
			case "missing":
				_ = r.Delete(ctx, source)
			case "owner":
				source.OwnerReferences[0].UID = "old-instance"
			case "labels-only":
				source.OwnerReferences = nil
			case "type":
				source.Type = corev1.SecretTypeOpaque
			case "host":
				source.Data[corev1.DockerConfigJsonKey] = []byte(strings.ReplaceAll(string(source.Data[corev1.DockerConfigJsonKey]), "registry.example.com", "other.example.com"))
			case "data":
				source.Data[corev1.DockerConfigJsonKey] = []byte("invalid synthetic-token")
			case "operation":
				source.Annotations[registryauth.Operation] = "old-operation"
			case "target":
				target.OwnerReferences[0].UID = "foreign-agent"
				_ = r.Update(ctx, target)
				old = target.DeepCopy()
			}
			if failure != "missing" && failure != "target" {
				_ = r.Update(ctx, source)
			}
			_, err = r.syncRegistryCredentials(ctx, agent, "default")
			if err == nil || strings.Contains(err.Error(), "synthetic-token") {
				t.Fatalf("unsafe sync result: %v", err)
			}
			_ = r.Get(ctx, key, target)
			if !reflect.DeepEqual(old, target) {
				t.Fatal("failed sync modified previous copy")
			}
		})
	}
}

type registryRaceReader struct {
	client.Reader
	sourceKey   client.ObjectKey
	sourceReads int
	mutate      func()
}

func (r *registryRaceReader) Get(ctx context.Context, key client.ObjectKey, obj client.Object, options ...client.GetOption) error {
	if key == r.sourceKey {
		r.sourceReads++
		if r.sourceReads == 2 {
			r.mutate()
		}
	}
	return r.Reader.Get(ctx, key, obj, options...)
}

func TestRegistryRechecksSourceAfterCopy(t *testing.T) {
	r, agent, source := registryFixture(t)
	ctx := context.Background()
	r.APIReader = &registryRaceReader{Reader: r.Client, sourceKey: client.ObjectKeyFromObject(source), mutate: func() {
		_ = r.Client.Get(ctx, client.ObjectKeyFromObject(source), source)
		source.Labels["changed"] = "true"
		if err := r.Update(ctx, source); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := r.syncRegistryCredentials(ctx, agent, "default"); err == nil || err.Error() != "RegistryInputChanged" {
		t.Fatalf("stale source accepted: %v", err)
	}
	if agent.Status.RegistryCredentials != nil {
		t.Fatal("stale sync recorded as successful")
	}
	r.APIReader = r.Client
	if _, err := r.syncRegistryCredentials(ctx, agent, "default"); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryRemovalAndUIDCleanup(t *testing.T) {
	r, agent, source := registryFixture(t)
	ctx := context.Background()
	name, err := r.syncRegistryCredentials(ctx, agent, "default")
	if err != nil {
		t.Fatal(err)
	}
	foreign := source.DeepCopy()
	foreign.Name = "registry-source-foreign"
	foreign.UID = ""
	foreign.ResourceVersion = ""
	foreign.OwnerReferences[0].UID = "another-agent"
	if err := r.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(agent.Name), Namespace: "cetirun-example"},
		Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{ImagePullSecrets: []corev1.LocalObjectReference{{Name: name}}}}}}
	_ = r.Create(ctx, sts)
	agent.Spec.Agent.RegistrySecretRef = nil
	agent.Generation++
	if err := r.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if r.cleanupRegistryCredentials(ctx, agent, false) == nil {
		t.Fatal("cleaned credentials before removing the future Pod reference")
	}
	sts.Spec.Template.Spec.ImagePullSecrets = nil
	_ = r.Update(ctx, sts)
	if err := r.cleanupRegistryCredentials(ctx, agent, false); err != nil {
		t.Fatal(err)
	}
	for _, key := range []client.ObjectKey{client.ObjectKeyFromObject(source), {Namespace: "cetirun-example", Name: name}} {
		if err := r.Get(ctx, key, &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Fatal("owned secret was not removed")
		}
	}
	if r.Get(ctx, client.ObjectKeyFromObject(foreign), &corev1.Secret{}) != nil {
		t.Fatal("forged label authorized deletion")
	}
	if agent.Status.RegistryCredentials != nil {
		t.Fatal("sync receipt retained after removal")
	}
}

func TestRegistryPermissionErrorsAreRedacted(t *testing.T) {
	r, agent, _ := registryFixture(t)
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.CreateOption) error {
		if _, ok := object.(*authorizationv1.SubjectAccessReview); ok {
			return errors.New("upstream synthetic-secret")
		}
		return c.Create(ctx, object, options...)
	}})
	_, err := r.syncRegistryCredentials(context.Background(), agent, "default")
	if err == nil || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatalf("unsafe error %v", err)
	}
}

func TestMissingRegistrySourceIsHeldBeforeChartResolution(t *testing.T) {
	r, agent, source := registryFixture(t)
	ConfigureTestGateway(t, r)
	ctx := context.Background()
	agent.Spec.Helm.Version = "0.1.1"
	agent.Finalizers = []string{api.Finalizer}
	if err := r.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, source); err != nil {
		t.Fatal(err)
	}
	// The intentionally absent Helm client catches any chart call before the
	// Portal has saved its source, including default-port resolution.
	result, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: agent.Name}})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("unexpected missing-source result: %v %v", result, err)
	}
	_ = r.Get(ctx, client.ObjectKeyFromObject(agent), agent)
	condition := meta.FindStatusCondition(agent.Status.Conditions, api.ConditionRegistryCredentialsReady)
	if agent.Status.Policy != nil || condition == nil || condition.Reason != "RegistrySourcePending" || condition.Status != metav1.ConditionFalse {
		t.Fatalf("legacy Agent did not wait for its registry source: policy=%+v condition=%+v", agent.Status.Policy, condition)
	}
	workloads := &appsv1.StatefulSetList{}
	_ = r.List(ctx, workloads)
	if len(workloads.Items) != 0 {
		t.Fatal("workload started without credentials")
	}
}

func TestRegistrySuspensionDoesNotNeedCredentialsOrHelm(t *testing.T) {
	for _, agentType := range []api.AgentType{api.AgentTypeHermes, api.AgentTypeOpenClaw} {
		t.Run(string(agentType), func(t *testing.T) {
			r, agent, source := registryFixture(t)
			ctx := context.Background()
			agent.Spec.Agent.Type = agentType
			if err := r.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			if _, err := r.syncRegistryCredentials(ctx, agent, "default"); err != nil {
				t.Fatal(err)
			}
			owner := registryauth.Owner(agent)
			sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
				Name: instanceServiceName(agent.Name), Namespace: instanceNamespace(agent.Name), OwnerReferences: []metav1.OwnerReference{owner},
				Annotations: map[string]string{
					"cetirun.microsoft.com/agent-uid": string(agent.UID),
					"meta.helm.sh/release-name":       helmReleaseName(agent.Name),
					"meta.helm.sh/release-namespace":  instanceNamespace(agent.Name),
				},
			}, Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)}}
			if err := r.Create(ctx, sts); err != nil {
				t.Fatal(err)
			}
			uid := sts.UID
			route := buildVirtualService(agent, "agents.example.com", "istio-system/example-gateway")
			route.OwnerReferences = []metav1.OwnerReference{owner}
			route.Annotations = map[string]string{
				"cetirun.microsoft.com/agent-uid": string(agent.UID),
				"meta.helm.sh/release-name":       helmReleaseName(agent.Name),
				"meta.helm.sh/release-namespace":  instanceNamespace(agent.Name),
			}
			if err := r.Create(ctx, route); err != nil {
				t.Fatal(err)
			}
			_ = r.Delete(ctx, source)
			agent.Spec.Suspended = true
			agent.Finalizers = []string{api.Finalizer}
			_ = r.Update(ctx, agent)
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKey{Name: agent.Name}}); err != nil {
				t.Fatal(err)
			}
			_ = r.Get(ctx, client.ObjectKeyFromObject(sts), sts)
			_ = r.Get(ctx, client.ObjectKeyFromObject(agent), agent)
			if sts.UID != uid || *sts.Spec.Replicas != 0 || agent.Status.Phase != api.PhaseSuspended || sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
				t.Fatal("suspension depended on credentials or changed storage ownership")
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(route), route); !apierrors.IsNotFound(err) {
				t.Fatal("native ingress remained after suspension")
			}
		})
	}
}

// Copilot proxy ownership is enforced by the dedicated shutdown primitive.
// A denied policy binding reaches Sandbox hold before legacy registry
// suspension, so this test invokes that primitive directly.
func TestRegistrySuspensionStopsTheCopilotProxy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		own       bool
		suspended bool
	}{
		{name: "the Agent's own proxy", own: true, suspended: true},
		{name: "another release's Deployment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, agent, _ := registryFixture(t)
			ctx := context.Background()
			agent.Spec.Agent.Type = api.AgentTypeOpenClaw
			if err := r.Update(ctx, agent); err != nil {
				t.Fatal(err)
			}
			if _, err := r.syncRegistryCredentials(ctx, agent, "default"); err != nil {
				t.Fatal(err)
			}
			proxy := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name: copilotProxyServiceName(agent), Namespace: instanceNamespace(agent.Name),
					Annotations: map[string]string{"meta.helm.sh/release-name": "another-release"},
				},
				Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1)},
			}
			if tc.own {
				proxy.Annotations["meta.helm.sh/release-name"] = helmReleaseName(agent.Name)
			}
			if err := r.Create(ctx, proxy); err != nil {
				t.Fatal(err)
			}
			namespace := &corev1.Namespace{}
			if err := r.Get(ctx, client.ObjectKey{Name: instanceNamespace(agent.Name)}, namespace); err != nil {
				t.Fatal(err)
			}
			if tc.own {
				if err := r.suspendCopilotProxy(ctx, agent); err != nil {
					t.Fatal(err)
				}
			} else if err := r.suspendCopilotProxy(ctx, agent); err == nil {
				t.Fatal("foreign proxy was accepted for suspension")
			}
			_ = r.Get(ctx, client.ObjectKeyFromObject(proxy), proxy)
			if stopped := *proxy.Spec.Replicas == 0; stopped != tc.own {
				t.Fatalf("proxy replicas %d", *proxy.Spec.Replicas)
			}
		})
	}
}

func TestRegistryRemovalBeforeFirstSync(t *testing.T) {
	r, agent, source := registryFixture(t)
	ctx := context.Background()
	agent.Spec.Agent.RegistrySecretRef = nil
	if err := r.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}
	source.Labels = nil
	if err := r.Update(ctx, source); err != nil {
		t.Fatal(err)
	}
	if !registryManaged(agent) {
		t.Fatal("unobserved removal intent was lost")
	}
	if err := r.removeRegistryCredentials(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(source), &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatal("receiptless source survived removal")
	}
}

func TestRegistrySuspensionRejectsForeignNativeRoute(t *testing.T) {
	for _, scenario := range []string{"namespace-owner", "route-owner", "route-labels", "newer-intent"} {
		t.Run(scenario, func(t *testing.T) {
			r, agent, _ := registryFixture(t)
			ctx := context.Background()
			if _, err := r.syncRegistryCredentials(ctx, agent, "default"); err != nil {
				t.Fatal(err)
			}
			route := buildVirtualService(agent, "agents.example.com", "istio-system/example-gateway")
			switch scenario {
			case "namespace-owner":
				ns := &corev1.Namespace{}
				if err := r.Get(ctx, client.ObjectKey{Name: instanceNamespace(agent.Name)}, ns); err != nil {
					t.Fatal(err)
				}
				ns.OwnerReferences[0].UID = "another-agent"
				if err := r.Update(ctx, ns); err != nil {
					t.Fatal(err)
				}
			case "route-owner":
				owner := registryauth.Owner(agent)
				owner.UID = "another-agent"
				route.OwnerReferences = []metav1.OwnerReference{owner}
			case "route-labels":
				route.Labels["app.kubernetes.io/managed-by"] = "another-controller"
			case "newer-intent":
				current := agent.DeepCopy()
				current.Generation++
				if err := r.Update(ctx, current); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.Create(ctx, route); err != nil {
				t.Fatal(err)
			}
			if err := r.removeRegistryBuiltinRoute(ctx, agent); err == nil {
				t.Fatal("foreign native route accepted for deletion")
			}
			if err := r.Get(ctx, client.ObjectKeyFromObject(route), route); err != nil {
				t.Fatal("foreign native route was removed")
			}
		})
	}
}

func TestRegistryRemovalPreservesRunningOrSuspendedWorkload(t *testing.T) {
	for _, replicas := range []int32{0, 1} {
		r, agent, source := registryFixture(t)
		ctx := context.Background()
		name, err := r.syncRegistryCredentials(ctx, agent, "default")
		if err != nil {
			t.Fatal(err)
		}
		sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(agent.Name), Namespace: instanceNamespace(agent.Name)},
			Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(replicas), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers:       []corev1.Container{{Name: "agent", Image: *agent.Spec.Agent.Image}},
				ImagePullSecrets: []corev1.LocalObjectReference{{Name: name}},
			}}}}
		if err := r.Create(ctx, sts); err != nil {
			t.Fatal(err)
		}
		uid := sts.UID
		agent.Spec.Agent.RegistrySecretRef = nil
		agent.Spec.Agent.Image = ptr.To("invalid desired image")
		if err := r.Update(ctx, agent); err != nil {
			t.Fatal(err)
		}
		if err := r.removeRegistryCredentials(ctx, agent); err != nil {
			t.Fatal(err)
		}
		_ = r.Get(ctx, client.ObjectKeyFromObject(sts), sts)
		if sts.UID != uid || *sts.Spec.Replicas != replicas || sts.Spec.Template.Spec.Containers[0].Image != "registry.example.com/agent:1" ||
			len(sts.Spec.Template.Spec.ImagePullSecrets) != 0 || !registryOnlyRollout(sts) {
			t.Fatal("removal changed writer inputs or replicas")
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(source), &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Fatal("source was not removed")
		}
		if err := r.Get(ctx, client.ObjectKey{Namespace: sts.Namespace, Name: name}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Fatal("local copy was not removed")
		}
	}
}
