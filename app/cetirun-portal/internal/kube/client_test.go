// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"strings"
	"testing"

	cetirunv1alpha1 "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(cetirunv1alpha1.AddToScheme(scheme))
	return scheme
}

func newFakeClient(objs ...client.Object) client.Client {
	return fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetUID() == "" {
				obj.SetUID(types.UID("test-" + obj.GetName()))
			}
			return c.Create(ctx, obj, opts...)
		}}).Build()
}

func sampleInstance() *cetirunv1alpha1.Agent {
	return &cetirunv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "alice",
		},
		Spec: cetirunv1alpha1.AgentSpec{
			Team:                  "example-team",
			SandboxRef:            "standard",
			GatewayTokenSecretRef: "gateway-token",
			User: cetirunv1alpha1.UserSpec{
				Alias:       "alice",
				DisplayName: "Alice Smith",
				Email:       "alice@example.com",
			},
		},
	}
}

func TestGetInstance_Success(t *testing.T) {
	fc := newFakeClient(sampleInstance())
	c := NewClient(fc)

	instance, err := c.GetInstance(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetInstance failed: %v", err)
	}
	if instance.Name != "alice" {
		t.Errorf("Name = %q, want %q", instance.Name, "alice")
	}
	if instance.Spec.User.Alias != "alice" {
		t.Errorf("Alias = %q, want %q", instance.Spec.User.Alias, "alice")
	}
}

func TestGetInstance_NotFound(t *testing.T) {
	fc := newFakeClient()
	c := NewClient(fc)

	_, err := c.GetInstance(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent instance")
	}
	if !IsNotFound(err) {
		t.Errorf("expected NotFound error, got: %v", err)
	}
}

func TestCreateInstance_Success(t *testing.T) {
	fc := newFakeClient()
	c := NewClient(fc)

	instance, err := c.CreateInstance(context.Background(), "bob", "Bob Jones", "other-team", "bob@example.com", "", cetirunv1alpha1.HarnessSpec{}, "", "", "")
	if err != nil {
		t.Fatalf("CreateInstance failed: %v", err)
	}
	if instance.Spec.Agent.Type != cetirunv1alpha1.AgentTypeHermes {
		t.Fatalf("Portal must default spec.agent.type to hermes, got %q", instance.Spec.Agent.Type)
	}
	if instance.Name != "bob" {
		t.Errorf("Name = %q, want %q", instance.Name, "bob")
	}
	if instance.Spec.User.DisplayName != "Bob Jones" {
		t.Errorf("DisplayName = %q, want %q", instance.Spec.User.DisplayName, "Bob Jones")
	}
	if instance.Spec.Team != "other-team" {
		t.Errorf("Team = %q, want %q", instance.Spec.Team, "other-team")
	}
	if instance.Spec.GatewayTokenSecretRef != "gateway-token" {
		t.Errorf("GatewayTokenSecretRef = %q, want %q", instance.Spec.GatewayTokenSecretRef, "gateway-token")
	}
	if instance.Spec.Tailscale != nil {
		t.Errorf("Tailscale should be nil when no auth key provided")
	}

	// Verify it was persisted.
	got, err := c.GetInstance(context.Background(), "bob")
	if err != nil {
		t.Fatalf("GetInstance after create failed: %v", err)
	}
	if got.Spec.User.Email != "bob@example.com" {
		t.Errorf("Email = %q, want %q", got.Spec.User.Email, "bob@example.com")
	}
}

func TestCreateInstanceAgentIdentityRequiresOwner(t *testing.T) {
	c := NewClient(newFakeClient())

	_, err := c.CreateInstance(context.Background(), "bob", "Bob Jones", "other-team", "bob@example.com", "", cetirunv1alpha1.HarnessSpec{}, "", "", "",
		CreateInstanceOptions{AgentIdentityEnabled: true})
	if err == nil || !strings.Contains(err.Error(), "verified owner identity") {
		t.Fatalf("CreateInstance() error = %v, want verified owner identity error", err)
	}
}

func TestCreateInstanceAgentIdentityPersistsOwnerBinding(t *testing.T) {
	const tenantID = "11111111-1111-1111-1111-111111111111"
	const objectID = "22222222-2222-2222-2222-222222222222"
	c := NewClient(newFakeClient())

	instance, err := c.CreateInstance(context.Background(), "bob", "Bob Jones", "other-team", "bob@example.com", "", cetirunv1alpha1.HarnessSpec{}, "", "", "",
		CreateInstanceOptions{AgentIdentityEnabled: true, OwnerTenantID: tenantID, OwnerObjectID: objectID})
	if err != nil {
		t.Fatal(err)
	}
	if instance.Spec.AgentIdentity == nil || !instance.Spec.AgentIdentity.Enabled {
		t.Fatal("Agent Identity was not enabled")
	}
	if instance.Annotations[cetirunv1alpha1.AnnotationOwnerTenantID] != tenantID ||
		instance.Annotations[cetirunv1alpha1.AnnotationOwnerObjectID] != objectID {
		t.Fatalf("owner annotations = %#v", instance.Annotations)
	}
}

func TestCreateInstanceRejectsCustomAgentIdentity(t *testing.T) {
	c := NewClient(newFakeClient())
	_, err := c.CreateInstance(context.Background(), "bob", "Bob Jones", "other-team", "bob@example.com", "",
		cetirunv1alpha1.HarnessSpec{Type: cetirunv1alpha1.AgentTypeCustom, Image: ptr.To("registry.example.com/agent:test"), Username: ptr.To("vibe")},
		"", "", "", CreateInstanceOptions{AgentIdentityEnabled: true, OwnerTenantID: "tenant", OwnerObjectID: "owner"})
	if !IsUnsupportedCustomUpdate(err) {
		t.Fatalf("CreateInstance() error = %v, want unsupported Custom identity error", err)
	}
}

func TestCreateAndClearInstanceModel(t *testing.T) {
	fc := newFakeClient()
	c := NewClient(fc)

	instance, err := c.CreateInstance(context.Background(), "bob", "Bob Jones", "other-team", "bob@example.com", "", cetirunv1alpha1.HarnessSpec{}, "", "", "gpt-5.4")
	if err != nil {
		t.Fatalf("CreateInstance failed: %v", err)
	}
	if instance.Spec.Model != "gpt-5.4" {
		t.Errorf("Model = %q, want gpt-5.4", instance.Spec.Model)
	}

	defaultModel := ""
	instance, err = c.UpdateInstance(context.Background(), "bob", UpdateInstanceOpts{Model: &defaultModel})
	if err != nil {
		t.Fatalf("UpdateInstance failed: %v", err)
	}
	if instance.Spec.Model != "" {
		t.Errorf("Model = %q, want empty deployment-default override", instance.Spec.Model)
	}
}

func TestCreateInstance_AlreadyExists(t *testing.T) {
	fc := newFakeClient(sampleInstance())
	c := NewClient(fc)

	_, err := c.CreateInstance(context.Background(), "alice", "Alice Smith", "example-team", "alice@example.com", "", cetirunv1alpha1.HarnessSpec{}, "", "", "")
	if err == nil {
		t.Fatal("expected error for duplicate instance")
	}
	if !IsAlreadyExists(err) {
		t.Errorf("expected AlreadyExists error, got: %v", err)
	}
}

func TestCreateInstance_WithGatewayToken(t *testing.T) {
	fc := newFakeClient()
	c := NewClient(fc)

	instance, err := c.CreateInstance(context.Background(), "bob", "Bob Jones", "other-team", "bob@example.com", "", cetirunv1alpha1.HarnessSpec{}, "my-token", "", "")
	if err != nil {
		t.Fatalf("CreateInstance failed: %v", err)
	}
	if instance.Spec.Tailscale != nil {
		t.Errorf("Tailscale should be nil when no auth key provided")
	}

	// Verify the gateway-token secret was created.
	secret := &corev1.Secret{}
	err = fc.Get(context.Background(), client.ObjectKey{Namespace: "cetirun-bob", Name: "gateway-token"}, secret)
	if err != nil {
		t.Fatalf("expected gateway-token secret to exist: %v", err)
	}
	if string(secret.Data["OPENCLAW_GATEWAY_TOKEN"]) != "my-token" {
		t.Errorf("OPENCLAW_GATEWAY_TOKEN = %q, want %q", string(secret.Data["OPENCLAW_GATEWAY_TOKEN"]), "my-token")
	}
}

func TestCreateInstance_WithTailscaleAuthKey(t *testing.T) {
	fc := newFakeClient()
	c := NewClient(fc)

	instance, err := c.CreateInstance(context.Background(), "bob", "Bob Jones", "other-team", "bob@example.com", "", cetirunv1alpha1.HarnessSpec{}, "", "tskey-auth-xxx", "")
	if err != nil {
		t.Fatalf("CreateInstance failed: %v", err)
	}
	if instance.Spec.Tailscale == nil {
		t.Fatal("Tailscale should be set when auth key provided")
	}
	if instance.Spec.Tailscale.AuthKeySecretRef != "tailscale-auth" {
		t.Errorf("AuthKeySecretRef = %q, want %q", instance.Spec.Tailscale.AuthKeySecretRef, "tailscale-auth")
	}

	// Verify the tailscale-auth secret was created.
	secret := &corev1.Secret{}
	err = fc.Get(context.Background(), client.ObjectKey{Namespace: "cetirun-bob", Name: "tailscale-auth"}, secret)
	if err != nil {
		t.Fatalf("expected tailscale-auth secret to exist: %v", err)
	}
	if string(secret.Data["TAILSCALE_AUTH_KEY"]) != "tskey-auth-xxx" {
		t.Errorf("TAILSCALE_AUTH_KEY = %q, want %q", string(secret.Data["TAILSCALE_AUTH_KEY"]), "tskey-auth-xxx")
	}
}

func TestCreateInstance_WithBothSecrets(t *testing.T) {
	fc := newFakeClient()
	c := NewClient(fc)

	instance, err := c.CreateInstance(context.Background(), "bob", "Bob Jones", "other-team", "bob@example.com", "", cetirunv1alpha1.HarnessSpec{}, "my-token", "tskey-auth-xxx", "")
	if err != nil {
		t.Fatalf("CreateInstance failed: %v", err)
	}
	if instance.Spec.Tailscale == nil {
		t.Fatal("Tailscale should be set")
	}

	// Verify both secrets exist.
	gwSecret := &corev1.Secret{}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "cetirun-bob", Name: "gateway-token"}, gwSecret); err != nil {
		t.Fatalf("expected gateway-token secret: %v", err)
	}
	tsSecret := &corev1.Secret{}
	if err := fc.Get(context.Background(), client.ObjectKey{Namespace: "cetirun-bob", Name: "tailscale-auth"}, tsSecret); err != nil {
		t.Fatalf("expected tailscale-auth secret: %v", err)
	}

	// Verify namespace was created with correct labels.
	ns := &corev1.Namespace{}
	if err := fc.Get(context.Background(), client.ObjectKey{Name: "cetirun-bob"}, ns); err != nil {
		t.Fatalf("expected namespace cetirun-bob: %v", err)
	}
	if ns.Labels["app.kubernetes.io/instance"] != "bob" {
		t.Errorf("namespace label app.kubernetes.io/instance = %q, want %q", ns.Labels["app.kubernetes.io/instance"], "bob")
	}
}

func TestDeleteInstance_Success(t *testing.T) {
	fc := newFakeClient(sampleInstance())
	c := NewClient(fc)

	err := c.DeleteInstance(context.Background(), "alice")
	if err != nil {
		t.Fatalf("DeleteInstance failed: %v", err)
	}

	// Verify it was deleted.
	_, err = c.GetInstance(context.Background(), "alice")
	if err == nil {
		t.Fatal("expected error after delete")
	}
	if !IsNotFound(err) {
		t.Errorf("expected NotFound after delete, got: %v", err)
	}
}

func TestDeleteInstance_NotFound(t *testing.T) {
	fc := newFakeClient()
	c := NewClient(fc)

	err := c.DeleteInstance(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent instance")
	}
}

func TestIsNotFound(t *testing.T) {
	if IsNotFound(nil) {
		t.Error("IsNotFound(nil) should be false")
	}
}

func TestIsAlreadyExists(t *testing.T) {
	if IsAlreadyExists(nil) {
		t.Error("IsAlreadyExists(nil) should be false")
	}
}

func TestGetTailscaleAuthKey_Success(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "cetirun-alice",
			Name:      "tailscale-auth",
		},
		Data: map[string][]byte{
			"TAILSCALE_AUTH_KEY": []byte("tskey-auth-test123"),
		},
	}
	fc := newFakeClient(secret)
	c := NewClient(fc)

	key, err := c.GetTailscaleAuthKey(context.Background(), "cetirun-alice", "tailscale-auth")
	if err != nil {
		t.Fatalf("GetTailscaleAuthKey failed: %v", err)
	}
	if key != "tskey-auth-test123" {
		t.Errorf("key = %q, want %q", key, "tskey-auth-test123")
	}
}

func TestGetTailscaleAuthKey_NotFound(t *testing.T) {
	fc := newFakeClient()
	c := NewClient(fc)

	_, err := c.GetTailscaleAuthKey(context.Background(), "cetirun-alice", "tailscale-auth")
	if err == nil {
		t.Fatal("expected error for missing secret")
	}
}

func TestUpdateInstance_DisplayNameAndTeam(t *testing.T) {
	fc := newFakeClient(sampleInstance())
	c := NewClient(fc)

	displayName := "Alice Updated"
	team := "other-team"
	instance, err := c.UpdateInstance(context.Background(), "alice", UpdateInstanceOpts{
		DisplayName: &displayName,
		Team:        &team,
	})
	if err != nil {
		t.Fatalf("UpdateInstance failed: %v", err)
	}
	if instance.Spec.User.DisplayName != "Alice Updated" {
		t.Errorf("DisplayName = %q, want %q", instance.Spec.User.DisplayName, "Alice Updated")
	}
	if instance.Spec.Team != "other-team" {
		t.Errorf("Team = %q, want %q", instance.Spec.Team, "other-team")
	}

	// Verify persisted.
	got, err := c.GetInstance(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetInstance after update failed: %v", err)
	}
	if got.Spec.User.DisplayName != "Alice Updated" {
		t.Errorf("persisted DisplayName = %q, want %q", got.Spec.User.DisplayName, "Alice Updated")
	}
}

func TestUpdateInstance_GatewayToken(t *testing.T) {
	fc := newFakeClient(sampleInstance())
	c := NewClient(fc)

	token := "new-gw-token"
	instance, err := c.UpdateInstance(context.Background(), "alice", UpdateInstanceOpts{
		GatewayToken: &token,
	})
	if err != nil {
		t.Fatalf("UpdateInstance failed: %v", err)
	}

	// Verify annotation was set.
	if instance.Annotations[AnnotationSecretUpdatedAt] == "" {
		t.Error("expected secret-updated-at annotation to be set")
	}

	// Verify the gateway-token secret was created.
	secret := &corev1.Secret{}
	err = fc.Get(context.Background(), client.ObjectKey{Namespace: "cetirun-alice", Name: "gateway-token"}, secret)
	if err != nil {
		t.Fatalf("expected gateway-token secret to exist: %v", err)
	}
	if string(secret.Data["OPENCLAW_GATEWAY_TOKEN"]) != "new-gw-token" {
		t.Errorf("OPENCLAW_GATEWAY_TOKEN = %q, want %q", string(secret.Data["OPENCLAW_GATEWAY_TOKEN"]), "new-gw-token")
	}
}

func TestUpdateInstance_EnableTailscale(t *testing.T) {
	fc := newFakeClient(sampleInstance())
	c := NewClient(fc)

	tsKey := "tskey-auth-new"
	instance, err := c.UpdateInstance(context.Background(), "alice", UpdateInstanceOpts{
		TailscaleAuthKey: &tsKey,
	})
	if err != nil {
		t.Fatalf("UpdateInstance failed: %v", err)
	}
	if instance.Spec.Tailscale == nil {
		t.Fatal("expected Tailscale spec to be set")
	}
	if instance.Spec.Tailscale.AuthKeySecretRef != "tailscale-auth" {
		t.Errorf("AuthKeySecretRef = %q, want %q", instance.Spec.Tailscale.AuthKeySecretRef, "tailscale-auth")
	}

	// Verify the tailscale-auth secret was created.
	secret := &corev1.Secret{}
	err = fc.Get(context.Background(), client.ObjectKey{Namespace: "cetirun-alice", Name: "tailscale-auth"}, secret)
	if err != nil {
		t.Fatalf("expected tailscale-auth secret to exist: %v", err)
	}
	if string(secret.Data["TAILSCALE_AUTH_KEY"]) != "tskey-auth-new" {
		t.Errorf("TAILSCALE_AUTH_KEY = %q, want %q", string(secret.Data["TAILSCALE_AUTH_KEY"]), "tskey-auth-new")
	}
}

func TestUpdateInstance_DisableTailscale(t *testing.T) {
	inst := sampleInstance()
	inst.Spec.Tailscale = &cetirunv1alpha1.TailscaleSpec{AuthKeySecretRef: "tailscale-auth"}
	fc := newFakeClient(inst)
	c := NewClient(fc)

	empty := ""
	instance, err := c.UpdateInstance(context.Background(), "alice", UpdateInstanceOpts{
		TailscaleAuthKey: &empty,
	})
	if err != nil {
		t.Fatalf("UpdateInstance failed: %v", err)
	}
	if instance.Spec.Tailscale != nil {
		t.Error("expected Tailscale spec to be nil after disabling")
	}
}

func TestUpdateInstance_NotFound(t *testing.T) {
	fc := newFakeClient()
	c := NewClient(fc)

	displayName := "X"
	_, err := c.UpdateInstance(context.Background(), "nonexistent", UpdateInstanceOpts{
		DisplayName: &displayName,
	})
	if err == nil {
		t.Fatal("expected error for nonexistent instance")
	}
	if !IsNotFound(err) {
		t.Errorf("expected NotFound error, got: %v", err)
	}
}

func TestUpdateInstance_NoChanges(t *testing.T) {
	fc := newFakeClient(sampleInstance())
	c := NewClient(fc)

	// No fields set - should succeed without updating.
	instance, err := c.UpdateInstance(context.Background(), "alice", UpdateInstanceOpts{})
	if err != nil {
		t.Fatalf("UpdateInstance failed: %v", err)
	}
	if instance.Spec.User.DisplayName != "Alice Smith" {
		t.Errorf("DisplayName = %q, want %q", instance.Spec.User.DisplayName, "Alice Smith")
	}
}

// --- GetOperatorChartVersion tests ---

func operatorDeployment(chartVersion string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "cetirun-system",
			Name:      "cetirun-operator",
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "operator"},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "operator"},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "manager",
							Image: "cetirun-operator:latest",
							Args:  []string{"--leader-elect", "--chart-version=" + chartVersion},
						},
					},
				},
			},
		},
	}
}

func TestGetOperatorChartVersion_Success(t *testing.T) {
	deploy := operatorDeployment("0.1.18")
	fc := newFakeClient(deploy)
	c := NewClient(fc)

	version, err := c.GetOperatorChartVersion(context.Background())
	if err != nil {
		t.Fatalf("GetOperatorChartVersion failed: %v", err)
	}
	if version != "0.1.18" {
		t.Errorf("version = %q, want %q", version, "0.1.18")
	}
}

func TestGetOperatorChartVersion_DeploymentNotFound(t *testing.T) {
	fc := newFakeClient()
	c := NewClient(fc)

	_, err := c.GetOperatorChartVersion(context.Background())
	if err == nil {
		t.Fatal("expected error when operator deployment is missing")
	}
}

func TestGetOperatorChartVersion_MissingArg(t *testing.T) {
	deploy := operatorDeployment("0.1.18")
	// Remove --chart-version arg
	deploy.Spec.Template.Spec.Containers[0].Args = []string{"--leader-elect"}
	fc := newFakeClient(deploy)
	c := NewClient(fc)

	_, err := c.GetOperatorChartVersion(context.Background())
	if err == nil {
		t.Fatal("expected error when --chart-version arg is missing")
	}
}

func TestGetOperatorChartVersion_Cached(t *testing.T) {
	deploy := operatorDeployment("0.1.18")
	fc := newFakeClient(deploy)
	c := NewClient(fc)

	// First call populates cache.
	v1, err := c.GetOperatorChartVersion(context.Background())
	if err != nil {
		t.Fatalf("first call failed: %v", err)
	}

	// Delete the deployment to prove the second call uses the cache.
	if err := fc.Delete(context.Background(), deploy); err != nil {
		t.Fatalf("failed to delete deployment: %v", err)
	}

	v2, err := c.GetOperatorChartVersion(context.Background())
	if err != nil {
		t.Fatalf("second call (cached) failed: %v", err)
	}
	if v1 != v2 {
		t.Errorf("cached version %q != first version %q", v2, v1)
	}
}

func TestGetOperatorChartVersion_ErrorCached(t *testing.T) {
	fc := newFakeClient() // no deployment
	c := NewClient(fc)

	// First call fails and caches the error.
	_, err1 := c.GetOperatorChartVersion(context.Background())
	if err1 == nil {
		t.Fatal("expected error on first call")
	}

	// Seed the deployment after first failure.
	deploy := operatorDeployment("0.1.18")
	if err := fc.Create(context.Background(), deploy); err != nil {
		t.Fatalf("failed to create deployment: %v", err)
	}

	// Second call should still return cached error (not re-fetch).
	_, err2 := c.GetOperatorChartVersion(context.Background())
	if err2 == nil {
		t.Fatal("expected cached error on second call")
	}
}

// --- UpgradeInstance tests ---

func TestUpgradeInstance_Success(t *testing.T) {
	inst := sampleInstance()
	inst.Spec.Helm.Version = "0.1.17"
	deploy := operatorDeployment("0.1.18")
	fc := newFakeClient(inst, deploy)
	c := NewClient(fc)

	result, err := c.UpgradeInstance(context.Background(), "alice")
	if err != nil {
		t.Fatalf("UpgradeInstance failed: %v", err)
	}
	if result.Spec.Helm.Version != "0.1.18" {
		t.Errorf("version = %q, want %q", result.Spec.Helm.Version, "0.1.18")
	}

	// Verify persisted.
	got, err := c.GetInstance(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetInstance after upgrade failed: %v", err)
	}
	if got.Spec.Helm.Version != "0.1.18" {
		t.Errorf("persisted version = %q, want %q", got.Spec.Helm.Version, "0.1.18")
	}
}

func TestUpgradeInstance_AlreadyLatest(t *testing.T) {
	inst := sampleInstance()
	inst.Spec.Helm.Version = "0.1.18"
	deploy := operatorDeployment("0.1.18")
	fc := newFakeClient(inst, deploy)
	c := NewClient(fc)

	result, err := c.UpgradeInstance(context.Background(), "alice")
	if err != nil {
		t.Fatalf("UpgradeInstance failed: %v", err)
	}
	if result.Spec.Helm.Version != "0.1.18" {
		t.Errorf("version = %q, want %q", result.Spec.Helm.Version, "0.1.18")
	}
}

func TestUpgradeInstance_NotFound(t *testing.T) {
	deploy := operatorDeployment("0.1.18")
	fc := newFakeClient(deploy)
	c := NewClient(fc)

	_, err := c.UpgradeInstance(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent instance")
	}
}

func TestUpgradeInstance_NoOperator(t *testing.T) {
	inst := sampleInstance()
	inst.Spec.Helm.Version = "0.1.17"
	fc := newFakeClient(inst)
	c := NewClient(fc)

	_, err := c.UpgradeInstance(context.Background(), "alice")
	if err == nil {
		t.Fatal("expected error when operator deployment is missing")
	}
}

func TestTeamCRUD(t *testing.T) {
	fc := newFakeClient()
	c := NewClient(fc)
	ctx := context.Background()

	// Create
	created, err := c.CreateTeam(ctx, &cetirunv1alpha1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "aks", UID: "team-uid"},
		Spec:       cetirunv1alpha1.TeamSpec{},
	})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if created.Name != "aks" {
		t.Errorf("created name = %q", created.Name)
	}

	// Get
	got, err := c.GetTeam(ctx, "aks")
	if err != nil {
		t.Fatalf("GetTeam: %v", err)
	}
	// List
	list, err := c.ListTeams(ctx)
	if err != nil {
		t.Fatalf("ListTeams: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("list len = %d, want 1", len(list))
	}

	// Update
	updated, err := c.UpdateTeamSpec(ctx, "aks", cetirunv1alpha1.TeamSpec{
		MemberContext: cetirunv1alpha1.TeamContextSpec{Role: "AKS Engineer"},
	}, ConfigurationVersion{UID: got.UID, ResourceVersion: got.ResourceVersion})
	if err != nil {
		t.Fatalf("UpdateTeamSpec: %v", err)
	}
	if updated.Spec.MemberContext.Role != "AKS Engineer" {
		t.Errorf("role = %q", updated.Spec.MemberContext.Role)
	}
	// Delete
	if err := c.DeleteTeam(ctx, "aks", ConfigurationVersion{UID: updated.UID, ResourceVersion: updated.ResourceVersion}); err != nil {
		t.Fatalf("DeleteTeam: %v", err)
	}
	if _, err := c.GetTeam(ctx, "aks"); err == nil {
		t.Errorf("expected GetTeam after delete to fail")
	}
}
