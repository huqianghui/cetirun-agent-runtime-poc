// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type terminalRegistryReviewer struct {
	client.Client
	namespace, account string
}

func (c *terminalRegistryReviewer) Create(ctx context.Context, object client.Object, opts ...client.CreateOption) error {
	if review, ok := object.(*authorizationv1.SubjectAccessReview); ok {
		access := review.Spec.ResourceAttributes
		review.Status.Allowed = review.Spec.User == "system:serviceaccount:"+c.namespace+":"+c.account &&
			access.Namespace == c.namespace && access.Group == "" && access.Resource == "pods" &&
			access.Subresource == "exec" && (access.Verb == "get" || access.Verb == "create")
		return nil
	}
	return c.Client.Create(ctx, object, opts...)
}

func terminalRegistryClient(t *testing.T, f *nativeSandboxFixture) *terminalRegistryReviewer {
	t.Helper()
	ctx, agent := context.Background(), f.current(t)
	namespace, workload := instanceNamespace(agent.Name), instanceServiceName(agent.Name)
	account := terminalpolicy.WorkloadName(workload)
	role := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: terminalpolicy.ExecRole},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{"pods/exec"}, Verbs: []string{"get", "create"},
		}},
	}
	if err := f.kube.Create(ctx, role); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: account + "-example", Namespace: namespace},
		Spec:       corev1.PodSpec{ServiceAccountName: account},
	}
	if err := f.kube.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	return &terminalRegistryReviewer{Client: f.kube, namespace: namespace, account: account}
}

func TestCommonCustomTerminalAllowsRegistryIsolation(t *testing.T) {
	for _, backend := range []string{"kata", "container"} {
		t.Run(backend, func(t *testing.T) {
			f := customTerminalFixture(t, backend)
			ctx := context.Background()
			agent := f.current(t)
			namespace, workload := instanceNamespace(agent.Name), instanceServiceName(agent.Name)
			reviewer := terminalRegistryClient(t, f)
			policy := &admissionv1.ValidatingAdmissionPolicy{}
			if err := f.kube.Get(ctx, client.ObjectKey{Name: workload + "-" + namespace + "-deny-exec"}, policy); err != nil {
				t.Fatal(err)
			}
			if len(policy.Spec.MatchConditions) != 0 {
				t.Fatal("protected common terminal must not exempt a legacy administration account")
			}
			if err := registryauth.CheckIsolation(ctx, reviewer, registryauth.DefaultNamespace, namespace, workload, "registry-source-example"); err != nil {
				t.Fatalf("the governed common terminal prevents private Registry repair or rotation: %v", err)
			}
			policy.Spec.Validations[0].Expression = "true"
			if err := f.kube.Update(ctx, policy); err != nil {
				t.Fatal(err)
			}
			if err := registryauth.CheckIsolation(ctx, reviewer, registryauth.DefaultNamespace, namespace, workload); !errors.Is(err, registryauth.ErrIsolation) {
				t.Fatalf("modified terminal admission was accepted: %v", err)
			}
			if err := f.r.ensureCommonTerminalExecPolicy(ctx, agent); err != nil {
				t.Fatal(err)
			}
			if err := registryauth.CheckIsolation(ctx, reviewer, registryauth.DefaultNamespace, namespace, workload); err != nil {
				t.Fatalf("reconciled terminal admission was not recognized: %v", err)
			}
		})
	}
}

func TestCommonCustomTerminalRegistryRecoveryRepairsExecGate(t *testing.T) {
	for _, backend := range []string{"kata", "container"} {
		for _, drift := range []string{"policy-missing", "binding-missing", "policy-drift", "binding-drift"} {
			for _, recovery := range []string{"retry", "restart"} {
				t.Run(backend+"/"+drift+"/"+recovery, func(t *testing.T) {
					f := customTerminalFixture(t, backend)
					ctx, agent := context.Background(), f.current(t)
					reviewer := terminalRegistryClient(t, f)
					f.r.Client, f.r.APIReader = reviewer, reviewer
					agent.Spec.Agent.RegistrySecretRef = &api.RegistrySecretRef{Name: "registry-source-example"}
					agent.Annotations[registryauth.Operation] = "synthetic-original-operation"
					agent.Generation++
					if err := f.kube.Update(ctx, agent); err != nil {
						t.Fatal(err)
					}
					raw, err := registryauth.Encode(*agent.Spec.Agent.Image,
						registryauth.Credentials{Username: "example", Password: "synthetic-token"})
					if err != nil {
						t.Fatal(err)
					}
					source := &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{
							Name: agent.Spec.Agent.RegistrySecretRef.Name, Namespace: f.r.registryNamespace(),
							OwnerReferences: []metav1.OwnerReference{registryauth.Owner(agent)}, Labels: registryauth.Labels(agent.UID),
							Annotations: map[string]string{registryauth.Operation: agent.Annotations[registryauth.Operation]},
						},
						Type: corev1.SecretTypeDockerConfigJson, Data: map[string][]byte{corev1.DockerConfigJsonKey: raw},
					}
					if err := f.kube.Create(ctx, source); err != nil {
						t.Fatal(err)
					}
					readyCustomWorkload(t, f)
					terminal := customTerminalDeployment(t, f)
					terminal.Status.ObservedGeneration = terminal.Generation
					terminal.Status.Replicas, terminal.Status.ReadyReplicas = 1, 1
					terminal.Status.UpdatedReplicas, terminal.Status.AvailableReplicas = 1, 1
					if err := f.kube.Status().Update(ctx, terminal); err != nil {
						t.Fatal(err)
					}
					for range 3 {
						f.reconcileOK(t)
					}
					agent = f.current(t)
					if agent.Status.Phase != api.PhaseReady || agent.Status.RegistryCredentials == nil {
						t.Fatal("fixture did not reach Ready with synchronized Registry credentials")
					}
					pullName, err := registryauth.PullSecretName(agent.UID)
					if err != nil {
						t.Fatal(err)
					}
					target := &corev1.Secret{}
					pullKey := client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: pullName}
					if err := f.kube.Get(ctx, pullKey, target); err != nil {
						t.Fatal(err)
					}
					originalTarget := target.DeepCopy()
					main, terminal := f.workload(t), customTerminalDeployment(t, f)
					stages := f.installer.stages
					key := client.ObjectKey{Name: instanceServiceName(agent.Name) + "-" + instanceNamespace(agent.Name) + "-deny-exec"}
					var gate client.Object = &admissionv1.ValidatingAdmissionPolicy{}
					if drift == "binding-missing" || drift == "binding-drift" {
						gate = &admissionv1.ValidatingAdmissionPolicyBinding{}
					}
					if err := f.kube.Get(ctx, key, gate); err != nil {
						t.Fatal(err)
					}
					if drift == "policy-missing" || drift == "binding-missing" {
						if err := f.kube.Delete(ctx, gate); err != nil {
							t.Fatal(err)
						}
					} else {
						switch gate := gate.(type) {
						case *admissionv1.ValidatingAdmissionPolicy:
							gate.Spec.FailurePolicy = ptr.To(admissionv1.Ignore)
						case *admissionv1.ValidatingAdmissionPolicyBinding:
							gate.Spec.ValidationActions = []admissionv1.ValidationAction{admissionv1.Warn}
						}
						if err := f.kube.Update(ctx, gate); err != nil {
							t.Fatal(err)
						}
					}
					// The owner records its new intent before isolation rejects the source write.
					agent.Annotations[registryauth.Operation] = "synthetic-rotated-operation"
					if err := f.kube.Update(ctx, agent); err != nil {
						t.Fatal(err)
					}
					if err := registryauth.CheckIsolation(ctx, reviewer, f.r.registryNamespace(), pullKey.Namespace, "default", source.Name); !errors.Is(err, registryauth.ErrIsolation) {
						t.Fatalf("missing or altered exec gate did not reject credential rotation: %v", err)
					}
					if recovery == "restart" {
						f.r = restartEntraReconciler(f.r)
					}
					for range 2 {
						f.reconcileOK(t)
					}
					if err := registryauth.CheckIsolation(ctx, reviewer, f.r.registryNamespace(), pullKey.Namespace, "default", source.Name); err != nil {
						t.Fatalf("ordinary reconciliation did not repair the exec gate before Registry source rejection: %v", err)
					}
					condition := meta.FindStatusCondition(f.current(t).Status.Conditions, api.ConditionRegistryCredentialsReady)
					if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "RegistrySourceRejected" {
						t.Fatal("gate repair bypassed the unmatched Registry operation")
					}
					currentSource := &corev1.Secret{}
					if err := f.kube.Get(ctx, client.ObjectKeyFromObject(source), currentSource); err != nil {
						t.Fatal(err)
					}
					if err := f.kube.Get(ctx, pullKey, target); err != nil {
						t.Fatal(err)
					}
					if !equality.Semantic.DeepEqual(source, currentSource) || !equality.Semantic.DeepEqual(originalTarget, target) ||
						f.installer.stages != stages || !equality.Semantic.DeepEqual(main, f.workload(t)) ||
						!equality.Semantic.DeepEqual(terminal, customTerminalDeployment(t, f)) {
						t.Fatal("gate repair changed credentials or a retained workload")
					}
					source.Annotations[registryauth.Operation] = agent.Annotations[registryauth.Operation]
					source.Data[corev1.DockerConfigJsonKey], err = registryauth.Encode(*agent.Spec.Agent.Image,
						registryauth.Credentials{Username: "example", Password: "rotated-synthetic-token"})
					if err != nil {
						t.Fatal(err)
					}
					if err := f.kube.Update(ctx, source); err != nil {
						t.Fatal(err)
					}
					f.reconcileOK(t)
					if err := f.kube.Get(ctx, pullKey, target); err != nil {
						t.Fatal(err)
					}
					agent = f.current(t)
					if target.UID != originalTarget.UID || !equality.Semantic.DeepEqual(target.Data, source.Data) ||
						agent.Status.RegistryCredentials.SourceResourceVersion != source.ResourceVersion ||
						!isConditionTrue(agent.Status.Conditions, api.ConditionRegistryCredentialsReady) ||
						!equality.Semantic.DeepEqual(main, f.workload(t)) ||
						!equality.Semantic.DeepEqual(terminal, customTerminalDeployment(t, f)) {
						t.Fatal("owner retry did not rotate credentials in place after gate recovery")
					}
				})
			}
		}
	}
}

func TestCommonCustomTerminalExecPolicyFences(t *testing.T) {
	for _, scenario := range []string{"steady", "policy-drift", "binding-drift", "policy-owner", "binding-owner", "stale-agent"} {
		t.Run(scenario, func(t *testing.T) {
			f := customTerminalFixture(t, "container")
			ctx, agent := context.Background(), f.current(t)
			key := client.ObjectKey{Name: instanceServiceName(agent.Name) + "-" + instanceNamespace(agent.Name) + "-deny-exec"}
			policy := &admissionv1.ValidatingAdmissionPolicy{}
			binding := &admissionv1.ValidatingAdmissionPolicyBinding{}
			if err := f.kube.Get(ctx, key, policy); err != nil {
				t.Fatal(err)
			}
			if err := f.kube.Get(ctx, key, binding); err != nil {
				t.Fatal(err)
			}
			main, terminal := f.workload(t), customTerminalDeployment(t, f)
			originalPolicy, originalBinding := policy.DeepCopy(), binding.DeepCopy()
			switch scenario {
			case "policy-drift":
				policy.Spec.FailurePolicy = ptr.To(admissionv1.Ignore)
				if err := f.kube.Update(ctx, policy); err != nil {
					t.Fatal(err)
				}
			case "binding-drift":
				binding.Spec.ValidationActions = []admissionv1.ValidationAction{admissionv1.Warn}
				if err := f.kube.Update(ctx, binding); err != nil {
					t.Fatal(err)
				}
			case "policy-owner":
				policy.OwnerReferences[0].UID = "foreign-agent"
				if err := f.kube.Update(ctx, policy); err != nil {
					t.Fatal(err)
				}
			case "binding-owner":
				binding.OwnerReferences[0].UID = "foreign-agent"
				if err := f.kube.Update(ctx, binding); err != nil {
					t.Fatal(err)
				}
			case "stale-agent":
				agent.Generation++
			}
			err := f.r.ensureCommonTerminalExecPolicy(ctx, agent)
			rejected := scenario == "policy-owner" || scenario == "binding-owner" || scenario == "stale-agent"
			if (err != nil) != rejected {
				t.Fatalf("ownership/generation fence: %v", err)
			}
			observedPolicy := &admissionv1.ValidatingAdmissionPolicy{}
			observedBinding := &admissionv1.ValidatingAdmissionPolicyBinding{}
			if err := f.kube.Get(ctx, key, observedPolicy); err != nil {
				t.Fatal(err)
			}
			if err := f.kube.Get(ctx, key, observedBinding); err != nil {
				t.Fatal(err)
			}
			if rejected {
				if !equality.Semantic.DeepEqual(observedPolicy, policy) || !equality.Semantic.DeepEqual(observedBinding, binding) {
					t.Fatal("rejected policy operation changed an object")
				}
			} else if !equality.Semantic.DeepEqual(observedPolicy.Spec, originalPolicy.Spec) ||
				!equality.Semantic.DeepEqual(observedBinding.Spec, originalBinding.Spec) ||
				observedPolicy.UID != originalPolicy.UID || observedBinding.UID != originalBinding.UID {
				t.Fatal("terminal policy did not converge in place")
			}
			if scenario == "steady" && (observedPolicy.ResourceVersion != policy.ResourceVersion ||
				observedBinding.ResourceVersion != binding.ResourceVersion) {
				t.Fatal("steady policy reconciliation wrote unchanged objects")
			}
			if !equality.Semantic.DeepEqual(main, f.workload(t)) || !equality.Semantic.DeepEqual(terminal, customTerminalDeployment(t, f)) {
				t.Fatal("exec policy reconciliation replaced or changed a workload")
			}
		})
	}
}

type terminalPolicyFailClient struct {
	client.Client
	failure error
}

func (c *terminalPolicyFailClient) Create(ctx context.Context, object client.Object, opts ...client.CreateOption) error {
	if _, ok := object.(*admissionv1.ValidatingAdmissionPolicyBinding); ok {
		return c.failure
	}
	return c.Client.Create(ctx, object, opts...)
}

func TestCommonCustomTerminalExecPolicySurfacesWriteFailure(t *testing.T) {
	f := customTerminalFixture(t, "container")
	ctx, agent := context.Background(), f.current(t)
	binding := &admissionv1.ValidatingAdmissionPolicyBinding{}
	key := client.ObjectKey{Name: instanceServiceName(agent.Name) + "-" + instanceNamespace(agent.Name) + "-deny-exec"}
	if err := f.kube.Get(ctx, key, binding); err != nil {
		t.Fatal(err)
	}
	if err := f.kube.Delete(ctx, binding); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("synthetic policy binding write failure")
	f.r.Client = &terminalPolicyFailClient{Client: f.kube, failure: failure}
	if err := f.r.ensureCommonTerminalExecPolicy(ctx, agent); !errors.Is(err, failure) {
		t.Fatalf("binding write failure was hidden: %v", err)
	}
}

func TestCommonCustomTerminalRegistryGateFailurePreventsPermissions(t *testing.T) {
	for _, backend := range []string{"kata", "container"} {
		t.Run(backend, func(t *testing.T) {
			setup := []func(*AgentReconciler, client.Client){withCommonCustom(t, "chat"), withCommonTerminalTopology(t)}
			if backend == "container" {
				setup = append(setup, withContainerRuntime(t))
			}
			f, source := reconciledRegistryFixture(t, "custom", setup...)
			f.r.CetiRunHelmValues["ttyd"].(map[string]any)["enabled"] = true
			failure := errors.New("synthetic policy binding write failure")
			f.r.Client = &terminalPolicyFailClient{Client: f.r.Client, failure: failure}
			var err error
			for range 4 {
				if _, err = f.reconcile(); err != nil {
					break
				}
			}
			if !errors.Is(err, failure) {
				t.Fatalf("early gate write failure was not surfaced: %v", err)
			}
			ctx, agent := context.Background(), f.current(t)
			condition := meta.FindStatusCondition(agent.Status.Conditions, api.ConditionRegistryCredentialsReady)
			if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "RegistryIsolationUnverified" {
				t.Fatal("gate write failure did not block Registry synchronization")
			}
			pullName, err := registryauth.PullSecretName(agent.UID)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.kube.Get(ctx, client.ObjectKey{Namespace: instanceNamespace(agent.Name), Name: pullName}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
				t.Fatalf("credentials were projected before the exec gate was installed: %v", err)
			}
			bindings := &rbacv1.RoleBindingList{}
			if err := f.kube.List(ctx, bindings, client.InNamespace(instanceNamespace(agent.Name))); err != nil {
				t.Fatal(err)
			}
			for _, binding := range bindings.Items {
				if binding.RoleRef.Name == terminalpolicy.ExecRole {
					t.Fatal("terminal exec permissions were installed before the exec gate")
				}
			}
			currentSource := &corev1.Secret{}
			if err := f.kube.Get(ctx, client.ObjectKeyFromObject(source), currentSource); err != nil {
				t.Fatal(err)
			}
			if !equality.Semantic.DeepEqual(source, currentSource) || f.installer.stages != 0 {
				t.Fatal("failed gate preparation changed source credentials or submitted a workload")
			}
		})
	}
}

func TestCommonCustomTerminalRestoresExecGateWithoutRollout(t *testing.T) {
	for _, backend := range []string{"kata", "container"} {
		t.Run(backend, func(t *testing.T) {
			f := customTerminalFixture(t, backend)
			ctx := context.Background()
			readyCustomWorkload(t, f)
			terminal := customTerminalDeployment(t, f)
			terminal.Status.ObservedGeneration = terminal.Generation
			terminal.Status.Replicas, terminal.Status.ReadyReplicas = 1, 1
			terminal.Status.UpdatedReplicas, terminal.Status.AvailableReplicas = 1, 1
			if err := f.kube.Status().Update(ctx, terminal); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				f.reconcileOK(t)
			}
			agent := f.current(t)
			main, terminal := f.workload(t), customTerminalDeployment(t, f)
			stages := f.installer.stages
			policy := &admissionv1.ValidatingAdmissionPolicy{}
			key := client.ObjectKey{Name: instanceServiceName(agent.Name) + "-" + instanceNamespace(agent.Name) + "-deny-exec"}
			if err := f.kube.Get(ctx, key, policy); err != nil {
				t.Fatal(err)
			}
			if err := f.kube.Delete(ctx, policy); err != nil {
				t.Fatal(err)
			}
			f.reconcileOK(t)
			if err := f.kube.Get(ctx, key, &admissionv1.ValidatingAdmissionPolicy{}); err != nil {
				t.Fatal("steady reconciliation did not restore the exact terminal gate:", err)
			}
			if f.installer.stages != stages || !equality.Semantic.DeepEqual(main, f.workload(t)) ||
				!equality.Semantic.DeepEqual(terminal, customTerminalDeployment(t, f)) {
				t.Fatal("adding the missing exec gate rolled a retained workload")
			}
		})
	}
}
