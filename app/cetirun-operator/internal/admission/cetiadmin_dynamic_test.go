// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxadmission

import (
	"context"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	workloadfixture "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	admissionv1 "k8s.io/api/admission/v1"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCetiAdminDynamicAgentManagement(t *testing.T) {
	for _, runtimeType := range []api.AgentType{api.AgentTypeHermes, api.AgentTypeOpenClaw, api.AgentTypeCustom} {
		for _, withNamespace := range []bool{false, true} {
			name := "without-namespace"
			if withNamespace {
				name = "unregistered-other-team"
			}
			t.Run(string(runtimeType)+"/"+name, func(t *testing.T) {
				f := newCetiAdminFixture(t)
				ctx := context.Background()
				team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "new-team", UID: "new-team-uid"}}
				if err := f.kube.Create(ctx, team); err != nil {
					t.Fatal(err)
				}
				agent := f.target.DeepCopy()
				agent.ObjectMeta = metav1.ObjectMeta{Name: "new-runtime", UID: "new-runtime-uid", Generation: 1}
				agent.Spec.Team = team.Name
				agent.Spec.Agent.Type = runtimeType
				agent.Status = api.AgentStatus{}
				if withNamespace {
					agent.Status.Namespace, agent.Status.HelmReleaseName = "cetirun-new-runtime", "openclaw-new-runtime"
					namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: agent.Status.Namespace, UID: "new-namespace-uid",
						OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(agent, api.GroupVersion.WithKind("Agent"))}}}
					if err := f.kube.Create(ctx, namespace); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.kube.Create(ctx, agent); err != nil {
					t.Fatal(err)
				}
				next := agent.DeepCopy()
				next.Spec.Model = "corrected-model"
				request := adminRequest(t, admissionv1.Update, api.GroupVersion.Group, "Agent", "agents", next, agent, f.actor)
				if response := f.guard.Validate(ctx, request); !response.Allowed {
					t.Fatalf("current Agent intent denied: %v", response.Result)
				}
			})
		}
	}
}

func TestCetiAdminCreatesAgentInNewTeam(t *testing.T) {
	f := newCetiAdminFixture(t)
	ctx := context.Background()
	team := &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "new-team", UID: "new-team-uid"}}
	if err := f.kube.Create(ctx, team); err != nil {
		t.Fatal(err)
	}
	agent := f.target.DeepCopy()
	agent.ObjectMeta = metav1.ObjectMeta{Name: "new-runtime", Annotations: map[string]string{
		api.AnnotationOwnerTenantID: f.state.Configuration.Owner.TenantID,
		api.AnnotationOwnerObjectID: f.state.Configuration.Owner.ObjectID,
	}}
	agent.Spec.Team = team.Name
	agent.Spec.User.DisplayName = "Runbook Assistant"
	agent.Status = api.AgentStatus{}
	request := adminRequest(t, admissionv1.Create, api.GroupVersion.Group, "Agent", "agents", agent, nil, f.actor)
	if response := f.guard.Validate(ctx, request); !response.Allowed {
		t.Fatalf("new Team with confirmed owner denied: %v", response.Result)
	}
	for _, name := range []string{"oauth2", "api", "internal", "healthz", "readyz", "assets", "portal", "instances", "connections", "admin", "ui", strings.Repeat("a", 36), "admin-tools"} {
		t.Run(name, func(t *testing.T) {
			next := agent.DeepCopy()
			next.Name = name
			request := adminRequest(t, admissionv1.Create, api.GroupVersion.Group, "Agent", "agents", next, nil, f.actor)
			if result := f.guard.Validate(ctx, request); result.Allowed != (name == "admin-tools") {
				t.Fatalf("instance route name %q: %v", name, result.Result)
			}
		})
	}
	for _, email := range []string{"other@example.com", "", "OWNER@example.com"} {
		t.Run("email="+email, func(t *testing.T) {
			next := agent.DeepCopy()
			next.Spec.User.Email = email
			request := adminRequest(t, admissionv1.Create, api.GroupVersion.Group, "Agent", "agents", next, nil, f.actor)
			if result := f.guard.Validate(ctx, request); result.Allowed != strings.EqualFold(email, agent.Spec.User.Email) {
				t.Fatalf("owner email %q: %v", email, result.Result)
			}
		})
	}
}

func TestCetiAdminMissingAuthorityDoesNotBypassOperations(t *testing.T) {
	for _, missing := range []string{"record", "recorded-account", "source"} {
		t.Run(missing, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			ctx := context.Background()
			checkpoint := &corev1.ConfigMap{}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.source.Namespace, Name: cetiadmin.InstallationName(f.source.Namespace, f.source.Name)}, checkpoint); err != nil {
				t.Fatal(err)
			}
			var err error
			switch missing {
			case "record":
				err = f.kube.Delete(ctx, checkpoint)
			case "source":
				err = f.kube.Delete(ctx, f.source)
			case "recorded-account":
				f.state.ServiceAccount = cetiadmin.ObjectIdentity{}
				checkpoint.Data[cetiadmin.BindingKey] = string(adminJSON(t, f.state))
				err = f.kube.Update(ctx, checkpoint)
			}
			if err != nil {
				t.Fatal(err)
			}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "unrelated-secret", Namespace: f.target.Status.Namespace}}
			request := adminRequest(t, admissionv1.Create, "", "Secret", "secrets", secret, nil, f.actor)
			if f.guard.Validate(ctx, request).Allowed {
				t.Fatal("CetiAdmin bypassed operations without its current authority")
			}
			request.UserInfo = authenticationv1.UserInfo{Username: "unrelated-user", Groups: []string{"system:authenticated"}}
			if result := f.guard.Validate(ctx, request); !result.Allowed {
				t.Fatalf("unrelated caller was classified as CetiAdmin: %v", result.Result)
			}
		})
	}
}

func TestCetiAdminRejectsUnrelatedNamespacePVC(t *testing.T) {
	f := newCetiAdminFixture(t)
	ctx := context.Background()
	old := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "unrelated-home", Namespace: f.target.Status.Namespace, UID: "unrelated-uid"},
		Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}}}}
	if err := f.kube.Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	next := old.DeepCopy()
	next.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("20Gi")
	request := adminRequest(t, admissionv1.Update, "", "PersistentVolumeClaim", "persistentvolumeclaims", next, old, f.actor)
	if response := f.guard.Validate(ctx, request); response.Allowed {
		t.Fatal("same-namespace PVC without runtime ownership was accepted")
	}
}

func (f *cetiAdminFixture) configure(t *testing.T, mutate func(*cetiadmin.Config)) {
	t.Helper()
	mutate(&f.state.Configuration)
	if err := f.state.Configuration.Validate(); err != nil {
		t.Fatal(err)
	}
	f.state.ConfigHash, _ = cetiadmin.ConfigHash(f.state.Configuration)
	f.source.Data[cetiadmin.ConfigKey] = string(adminJSON(t, f.state.Configuration))
	if err := f.kube.Update(context.Background(), f.source); err != nil {
		t.Fatal(err)
	}
	checkpoint := &corev1.ConfigMap{}
	if err := f.kube.Get(context.Background(), client.ObjectKey{Name: cetiadmin.InstallationName(f.source.Namespace, f.source.Name), Namespace: f.source.Namespace}, checkpoint); err != nil {
		t.Fatal(err)
	}
	checkpoint.Data[cetiadmin.BindingKey] = string(adminJSON(t, f.state))
	if err := f.kube.Update(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
}

func TestCetiAdminDiagnosticReadsDoNotExcludeAgent(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.configure(t, func(config *cetiadmin.Config) {
		config.PlatformReadNamespaces = append(config.PlatformReadNamespaces, f.target.Status.Namespace)
	})
	next := f.target.DeepCopy()
	next.Spec.Model = "corrected-model"
	request := adminRequest(t, admissionv1.Update, api.GroupVersion.Group, "Agent", "agents", next, f.target, f.actor)
	if response := f.guard.Validate(context.Background(), request); !response.Allowed {
		t.Fatalf("diagnostic reads excluded a current Agent: %v", response.Result)
	}
}

func TestCetiAdminDynamicAgentBoundaries(t *testing.T) {
	for _, change := range []string{"self", "migration source", "platform namespace redirect", "forged UID", "forged old UID", "stale version", "owner", "Team", "Sandbox binding"} {
		t.Run(change, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			old := f.target.DeepCopy()
			switch change {
			case "self":
				if err := f.kube.Get(context.Background(), client.ObjectKey{Name: f.state.Agent.Name}, old); err != nil {
					t.Fatal(err)
				}
			case "migration source":
				f.configure(t, func(config *cetiadmin.Config) {
					config.MigrationSource = &cetiadmin.MigrationSource{Agent: f.targetIdentity.Agent, Namespace: f.targetIdentity.Namespace}
				})
			case "platform namespace redirect":
				f.target.Status.Namespace = f.source.Namespace
				if err := f.kube.Update(context.Background(), f.target); err != nil {
					t.Fatal(err)
				}
				old = f.target.DeepCopy()
			case "forged old UID":
				old.UID = "forged-agent-uid"
			case "stale version":
				old.ResourceVersion = "stale-version"
			}
			next := old.DeepCopy()
			next.Spec.Model = "corrected-model"
			switch change {
			case "forged UID":
				next.UID = "forged-agent-uid"
			case "owner":
				next.Spec.User.Alias = "another-owner"
			case "Team":
				next.Spec.Team = "another-team"
			case "Sandbox binding":
				next.Spec.SandboxRef = "another-sandbox"
			}
			request := adminRequest(t, admissionv1.Update, api.GroupVersion.Group, "Agent", "agents", next, old, f.actor)
			if f.guard.Validate(context.Background(), request).Allowed {
				t.Fatal("dynamic management bypassed an identity or governance boundary")
			}
		})
	}
}

func TestCetiAdminDynamicProtectedExec(t *testing.T) {
	for _, protection := range []string{"Team", "namespace", "Sandbox", "hold finalizer"} {
		t.Run(protection, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			pod := f.runtimePod(t)
			request := adminRequest(t, admissionv1.Connect, "", "Pod", "pods", pod, nil, f.actor)
			request.SubResource = "exec"
			if response := f.guard.Validate(context.Background(), request); !response.Allowed {
				t.Fatalf("ordinary runtime exec denied: %v", response.Result)
			}
			switch protection {
			case "Team":
				team := &api.Team{}
				if err := f.kube.Get(context.Background(), client.ObjectKey{Name: f.target.Spec.Team}, team); err != nil {
					t.Fatal(err)
				}
				team.Spec.AgentPolicyRef = "protected-policy"
				if err := f.kube.Update(context.Background(), team); err != nil {
					t.Fatal(err)
				}
			case "namespace":
				namespace := &corev1.Namespace{}
				if err := f.kube.Get(context.Background(), client.ObjectKey{Name: pod.Namespace}, namespace); err != nil {
					t.Fatal(err)
				}
				namespace.Labels = map[string]string{"cetirun.microsoft.com/sandbox-protected": "true"}
				if err := f.kube.Update(context.Background(), namespace); err != nil {
					t.Fatal(err)
				}
			case "Sandbox":
				f.updateTarget(t, func(agent *api.Agent) { agent.Spec.SandboxRef = "protected-sandbox" })
			case "hold finalizer":
				f.updateTarget(t, func(agent *api.Agent) { agent.Finalizers = []string{sandbox.HoldFinalizer} })
			}
			if f.guard.Validate(context.Background(), request).Allowed {
				t.Fatal("newly protected runtime retained exec access")
			}
		})
	}
}

func TestCetiAdminRuntimePVCGrowth(t *testing.T) {
	for _, test := range []struct {
		name string
		want bool
	}{{"Agent owner", true}, {"claim template", true}, {"volume reference", true}, {"unrelated claim", false}, {"foreign workload", false}, {"stale UID", false}, {"stale version", false}} {
		t.Run(test.name, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			ctx := context.Background()
			pod := f.runtimePod(t)
			workload := &appsv1.StatefulSet{}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: pod.OwnerReferences[0].Name}, workload); err != nil {
				t.Fatal(err)
			}
			old := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "existing-home", Namespace: f.target.Status.Namespace, UID: "home-uid"},
				Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}}}}
			switch test.name {
			case "Agent owner":
				old.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(f.target, api.GroupVersion.WithKind("Agent"))}
			case "claim template":
				old.Name = "data-" + workload.Name + "-0"
				workload.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "data"}}}
			case "foreign workload":
				workload.Annotations["meta.helm.sh/release-name"] = "foreign-release"
			}
			if test.name != "unrelated claim" && test.name != "claim template" {
				workload.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: old.Name}}}}
			}
			if err := f.kube.Update(ctx, workload); err != nil {
				t.Fatal(err)
			}
			if err := f.kube.Create(ctx, old); err != nil {
				t.Fatal(err)
			}
			if test.name == "stale UID" {
				old.UID = "previous-claim-uid"
			} else if test.name == "stale version" {
				old.ResourceVersion = "previous-version"
			}
			next := old.DeepCopy()
			next.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("20Gi")
			request := adminRequest(t, admissionv1.Update, "", "PersistentVolumeClaim", "persistentvolumeclaims", next, old, f.actor)
			if response := f.guard.Validate(ctx, request); response.Allowed != test.want {
				t.Fatalf("allowed=%v want=%v: %v", response.Allowed, test.want, response.Result)
			}
		})
	}
}

func TestCetiAdminMaintenanceObserverDeletingCleanup(t *testing.T) {
	for _, kind := range []string{"Pod", "StatefulSet"} {
		for _, caller := range []string{testController, "system:serviceaccount:kube-system:statefulset-controller"} {
			t.Run(kind+"/"+caller, func(t *testing.T) {
				f := newCetiAdminFixture(t)
				ctx := context.Background()
				pod := f.runtimePod(t)
				f.updateTarget(t, func(agent *api.Agent) { agent.Finalizers = []string{"example.com/cleanup"} })
				if err := f.kube.Delete(ctx, f.target); err != nil {
					t.Fatal(err)
				}
				if err := f.kube.Get(ctx, client.ObjectKeyFromObject(f.target), f.target); err != nil || f.target.DeletionTimestamp == nil || cetiadmin.MaintenanceUnfinished(f.target) {
					t.Fatalf("expected a retained deleting Agent without maintenance: %v", err)
				}
				var old client.Object = pod
				group, resourceName := "", "pods"
				if kind == "StatefulSet" {
					old = &appsv1.StatefulSet{}
					group, resourceName = "apps", "statefulsets"
					if err := f.kube.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: pod.OwnerReferences[0].Name}, old); err != nil {
						t.Fatal(err)
					}
				}
				old.SetFinalizers([]string{"example.com/cleanup"})
				if err := f.kube.Update(ctx, old); err != nil {
					t.Fatal(err)
				}
				next := old.DeepCopyObject().(client.Object)
				next.SetFinalizers(nil)
				actor := authenticationv1.UserInfo{Username: caller, Groups: []string{"system:authenticated"}}
				request := adminRequest(t, admissionv1.Update, group, kind, resourceName, next, old, actor)
				if response := f.guard.Validate(ctx, request); !response.Allowed {
					t.Fatalf("ordinary deleting-Agent metadata cleanup denied: %v", response.Result)
				}
			})
		}
	}
}

func TestCetiAdminMaintenanceObserverCommonCustomBootstrap(t *testing.T) {
	f := newCetiAdminFixture(t)
	ctx := context.Background()
	common := workloadfixture.New("custom")
	oldNamespace := &corev1.Namespace{}
	if err := f.kube.Get(ctx, client.ObjectKey{Name: f.target.Status.Namespace}, oldNamespace); err != nil {
		t.Fatal(err)
	}
	for _, old := range []client.Object{f.target, oldNamespace} {
		if err := f.kube.Delete(ctx, old); err != nil {
			t.Fatal(err)
		}
	}
	common.Namespace.Labels["app.kubernetes.io/managed-by"] = "cetirun-operator"
	for _, object := range []client.Object{common.Agent, common.Team, common.Namespace, common.Sandbox, common.Policy} {
		object.SetResourceVersion("")
		if err := f.kube.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	if _, target, err := cetiadmin.ResolveRuntimeTarget(ctx, f.kube, f.state.Configuration, f.state.Record, common.Agent.Name, common.Namespace.Name); err != nil || !target.Protected {
		t.Fatalf("native ownership must resolve only as a protected target without a legacy receipt: %v", err)
	}
	for _, caller := range []string{testController, "system:serviceaccount:kube-system:statefulset-controller"} {
		t.Run(caller, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "common-runtime-0", Namespace: common.Namespace.Name},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime", Image: "registry.example.com/runtime:synthetic"}}}}
			actor := authenticationv1.UserInfo{Username: caller, Groups: []string{"system:authenticated"}}
			request := adminRequest(t, admissionv1.Create, "", "Pod", "pods", pod, nil, actor)
			if response := f.guard.Validate(ctx, request); !response.Allowed {
				t.Fatalf("common bootstrap-owned namespace was treated as an unauthorized Admin target: %v", response.Result)
			}
			request.UserInfo = f.actor
			if f.guard.Validate(ctx, request).Allowed {
				t.Fatal("maintenance observation granted CetiAdmin workload creation")
			}
		})
	}
}

func TestCetiAdminMaintenanceObserverKeepsStartupFences(t *testing.T) {
	for _, condition := range []string{"no handoff", "intent", "status only", "deleting intent", "marker without intent", "orphan marker", "wrong marker name", "wrong marker UID", "request-only marker", "wrong owner UID", "missing namespace owner", "missing installation"} {
		for _, caller := range []string{testController, "system:serviceaccount:kube-system:statefulset-controller", "administrator", "cetiadmin"} {
			t.Run(condition+"/"+caller, func(t *testing.T) {
				f := newCetiAdminFixture(t)
				ctx := context.Background()
				f.updateTarget(t, func(agent *api.Agent) {
					if condition == "intent" || condition == "deleting intent" || condition == "missing installation" {
						agent.Spec.Suspended = true
						agent.Spec.Helm.Maintenance = &api.HelmMaintenanceRequest{RequestID: "maintenance-observer-one", ExpectedRevision: 1}
					}
					if condition == "status only" {
						agent.Status.HelmMaintenance = &api.HelmMaintenanceStatus{RequestID: "maintenance-observer-one", Phase: api.HelmMaintenancePhaseReapplying}
					}
					if condition == "deleting intent" {
						agent.Finalizers = []string{"example.com/cleanup"}
					}
				})
				namespace := &corev1.Namespace{}
				if err := f.kube.Get(ctx, client.ObjectKey{Name: f.target.Status.Namespace}, namespace); err != nil {
					t.Fatal(err)
				}
				switch condition {
				case "marker without intent", "orphan marker", "wrong marker name", "wrong marker UID", "request-only marker":
					namespace.Annotations = map[string]string{cetiadmin.MaintenanceAgentName: f.target.Name, cetiadmin.MaintenanceAgentUID: string(f.target.UID), cetiadmin.MaintenanceRequestID: "maintenance-observer-one"}
					if condition == "wrong marker name" {
						namespace.Annotations[cetiadmin.MaintenanceAgentName] = "another-agent"
					} else if condition == "wrong marker UID" {
						namespace.Annotations[cetiadmin.MaintenanceAgentUID] = "another-agent-uid"
					} else if condition == "request-only marker" {
						delete(namespace.Annotations, cetiadmin.MaintenanceAgentName)
						delete(namespace.Annotations, cetiadmin.MaintenanceAgentUID)
					}
				case "wrong owner UID":
					namespace.OwnerReferences[0].UID = "previous-agent-uid"
				case "missing namespace owner":
					namespace.OwnerReferences = nil
				case "missing installation":
					checkpoint := &corev1.ConfigMap{}
					if err := f.kube.Get(ctx, client.ObjectKey{Namespace: f.source.Namespace, Name: cetiadmin.InstallationName(f.source.Namespace, f.source.Name)}, checkpoint); err != nil {
						t.Fatal(err)
					}
					if err := f.kube.Delete(ctx, checkpoint); err != nil {
						t.Fatal(err)
					}
				}
				if err := f.kube.Update(ctx, namespace); err != nil {
					t.Fatal(err)
				}
				if condition == "deleting intent" || condition == "orphan marker" {
					if err := f.kube.Delete(ctx, f.target); err != nil {
						t.Fatal(err)
					}
				}
				actor := authenticationv1.UserInfo{Username: caller, Groups: []string{"system:authenticated"}}
				if caller == "cetiadmin" {
					actor = f.actor
				}
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "new-runtime", Namespace: namespace.Name}}
				request := adminRequest(t, admissionv1.Create, "", "Pod", "pods", pod, nil, actor)
				want := condition == "no handoff" && caller != "cetiadmin"
				if response := f.guard.Validate(ctx, request); response.Allowed != want {
					t.Fatalf("allowed=%v want=%v: %v", response.Allowed, want, response.Result)
				}
			})
		}
	}
}

func TestCetiAdminMaintenanceObserverLegacyCustomNamespace(t *testing.T) {
	for _, staleUID := range []bool{false, true} {
		name := "current Agent UID annotation"
		if staleUID {
			name = "stale Agent UID annotation"
		}
		t.Run(name, func(t *testing.T) {
			f := newCetiAdminFixture(t)
			ctx := context.Background()
			f.updateTarget(t, func(agent *api.Agent) { agent.Spec.Agent.Type = api.AgentTypeCustom })
			namespace := &corev1.Namespace{}
			if err := f.kube.Get(ctx, client.ObjectKey{Name: f.target.Status.Namespace}, namespace); err != nil {
				t.Fatal(err)
			}
			namespace.OwnerReferences = nil
			namespace.Annotations = map[string]string{w.AgentUIDAnnotation: string(f.target.UID)}
			if staleUID {
				namespace.Annotations[w.AgentUIDAnnotation] = "previous-agent-uid"
			}
			if err := f.kube.Update(ctx, namespace); err != nil {
				t.Fatal(err)
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "custom-runtime", Namespace: namespace.Name}}
			actor := authenticationv1.UserInfo{Username: testController, Groups: []string{"system:authenticated"}}
			request := adminRequest(t, admissionv1.Create, "", "Pod", "pods", pod, nil, actor)
			if response := f.guard.Validate(ctx, request); response.Allowed == staleUID {
				t.Fatalf("allowed=%v with stale UID=%v: %v", response.Allowed, staleUID, response.Result)
			}
		})
	}
}
