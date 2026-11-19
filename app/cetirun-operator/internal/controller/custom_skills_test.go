// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCustomSkillConfiguration(t *testing.T) {
	global := map[string]any{"skills": map[string]any{
		"shared": true, "resourceGroup": "platform", "storageAccount": "globalskills",
		"containerName": "approved", "protocol": "nfs", "npm": []any{},
	}}
	team := &api.Team{Spec: api.TeamSpec{SkillStorage: &api.SkillStorageSpec{
		ResourceGroup: "team", StorageAccount: "teamskills", ContainerName: "team-skills",
		StorageSize: "200Gi", MountPath: "/native-discovery",
	}}}
	instance := &api.Agent{Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: api.AgentTypeCustom}}}
	r := &AgentReconciler{CetiRunHelmValues: global}
	values, err := r.customOverrides(instance, team)
	if err != nil {
		t.Fatal(err)
	}
	skills := values["skills"].(map[string]any)
	if skills["resourceGroup"] != "team" || skills["storageAccount"] != "teamskills" ||
		skills["protocol"] != "nfs" || skills["storageSize"] != "200Gi" {
		t.Fatal("Custom lost shared Team/global skill settings")
	}
	if global["skills"].(map[string]any)["storageAccount"] != "globalskills" {
		t.Fatal("Custom mutated global settings")
	}
	native := map[string]any{"skills": deepMerge(nil, global["skills"])}
	if err := applySkillValues(native, team, instance.Spec.SkillOverrides, "", false); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(skills, native["skills"]) {
		t.Fatal("native and Custom skill selection diverged")
	}
	r.SkillsStorageAccount = "legacyoverride"
	values, err = r.customOverrides(instance, team)
	if err != nil || values["skills"].(map[string]any)["storageAccount"] != "legacyoverride" {
		t.Fatal("explicit platform storage-account override was lost")
	}
	instance.Spec.Helm.Values = map[string]runtime.RawExtension{"skills": {Raw: []byte(`{"storageAccount":"foreign"}`)}}
	if _, err := r.customOverrides(instance, team); err == nil {
		t.Fatal("tenant redirected platform storage through raw Helm values")
	}
}

func TestCustomSkillSelectionCannotBeSilentlyIgnored(t *testing.T) {
	instance := &api.Agent{Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: api.AgentTypeCustom}}}
	r := &AgentReconciler{}
	team := &api.Team{Spec: api.TeamSpec{Skills: []string{"npm:example-skill"}}}
	if _, err := r.customOverrides(instance, team); err == nil || !strings.Contains(err.Error(), "SkillsMaterializationPending") {
		t.Fatal("unmaterialized Team skills were silently ignored")
	}
	instance.Spec.SkillOverrides.Remove = []string{"npm:example-skill"}
	if _, err := r.customOverrides(instance, team); err != nil {
		t.Fatalf("removed Team skill still blocked shared context: %v", err)
	}
	instance.Spec.SkillOverrides.Add = []string{"another-skill"}
	if _, err := r.customOverrides(instance, team); err == nil || !strings.Contains(err.Error(), "SkillsMaterializationPending") {
		t.Fatal("unmaterialized per-Agent skills were silently ignored")
	}
}

func TestCommonCustomMaterializationHoldPreservesIssuedKey(t *testing.T) {
	for _, backend := range []string{"kata", "container"} {
		for _, selected := range []string{"npm:example-skill", "example-skill", "not-allowed"} {
			t.Run(backend+"/"+selected, func(t *testing.T) {
				ctx := context.Background()
				setup := []func(*AgentReconciler, client.Client){
					withCommonCustom(t, "headless"), withNativePublishedSkills(t),
				}
				if backend == "container" {
					setup = append(setup, withContainerRuntime(t))
				}
				f, _ := newUnreconciledSandboxRuntimeFixture(t, true, nil, true, "custom", true, false, setup...)
				policy := &api.AgentPolicy{}
				if err := f.kube.Get(ctx, client.ObjectKey{Name: "policy-example"}, policy); err != nil {
					t.Fatal(err)
				}
				policy.Spec.Skills.Allowed = []string{"npm:example-skill", "example-skill"}
				if err := f.kube.Update(ctx, policy); err != nil {
					t.Fatal(err)
				}
				for range 5 {
					f.reconcileOK(t)
				}
				agent := f.current(t)
				keyID, workloadUID := agent.Status.AIGateway.KeyID, f.workload(t).UID
				namespace := &corev1.Namespace{}
				if err := f.kube.Get(ctx, client.ObjectKey{Name: instanceNamespace(agent.Name)}, namespace); err != nil {
					t.Fatal(err)
				}
				claims := &corev1.PersistentVolumeClaimList{}
				if err := f.kube.List(ctx, claims, client.InNamespace(namespace.Name)); err != nil {
					t.Fatal(err)
				}
				if len(claims.Items) != 2 {
					t.Fatal("fixture must have both HOME and published-skills claims")
				}
				secret, err := f.r.aiGatewayKeySecret(ctx, agent)
				if err != nil || secret == nil {
					t.Fatalf("initial key Secret is unavailable: %v", err)
				}
				token := aigateway.Token(string(secret.Data[aiGatewayKeySecretField]))
				key := f.gateway.keys[token]
				if key == nil || key.MaxBudget == nil {
					t.Fatal("initial virtual key is unavailable")
				}
				key.Spend = ptr.To(1.25)
				reset := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
				key.BudgetResetAt = &reset
				budget, duration := *key.MaxBudget, key.BudgetDuration
				generated := len(f.gateway.generated)
				agent.Spec.SkillOverrides.Add = []string{selected}
				agent.Generation++
				if err := f.kube.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				f.reconcileOK(t)
				if selected == "not-allowed" {
					if f.gateway.keys[token] != nil || len(f.gateway.deletedTokens) == 0 {
						t.Fatal("an actual policy denial must still revoke the key")
					}
					return
				}
				held := f.gateway.keys[token]
				if held == nil || !held.Blocked || len(f.gateway.deletedTokens) != 0 || len(f.gateway.deletedAliases) != 0 {
					t.Fatal("materialization failure destroyed or failed to block the issued key")
				}
				if f.current(t).Status.Policy.Phase != "Pending" || *f.workload(t).Spec.Replicas != 0 {
					t.Fatal("missing materialization did not hold policy and compute")
				}
				agent = f.current(t)
				agent.Spec.SkillOverrides.Add = nil
				agent.Generation++
				if err := f.kube.Update(ctx, agent); err != nil {
					t.Fatal(err)
				}
				for range 5 {
					f.reconcileOK(t)
				}
				recovered := f.gateway.keys[token]
				if recovered == nil || recovered.Blocked || recovered.Spend == nil || *recovered.Spend != 1.25 ||
					recovered.BudgetResetAt == nil || !recovered.BudgetResetAt.Equal(reset) ||
					recovered.MaxBudget == nil || *recovered.MaxBudget != budget || recovered.BudgetDuration != duration ||
					len(f.gateway.generated) != generated || len(f.gateway.deletedTokens) != 0 ||
					f.current(t).Status.AIGateway.KeyID != keyID || f.workload(t).UID != workloadUID {
					t.Fatal("clearing the unsupported selection did not retain workload and accounting identity")
				}
				currentNamespace := &corev1.Namespace{}
				if err := f.kube.Get(ctx, client.ObjectKey{Name: namespace.Name}, currentNamespace); err != nil || currentNamespace.UID != namespace.UID {
					t.Fatal("materialization recovery replaced the retained namespace")
				}
				currentSecret, err := f.r.aiGatewayKeySecret(ctx, f.current(t))
				if err != nil || currentSecret == nil || currentSecret.UID != secret.UID {
					t.Fatal("materialization recovery replaced the retained key Secret")
				}
				for _, before := range claims.Items {
					current := &corev1.PersistentVolumeClaim{}
					if err := f.kube.Get(ctx, client.ObjectKeyFromObject(&before), current); err != nil ||
						current.UID != before.UID || current.Spec.VolumeName != before.Spec.VolumeName {
						t.Fatal("materialization recovery replaced a retained volume binding")
					}
				}
			})
		}
	}
}
