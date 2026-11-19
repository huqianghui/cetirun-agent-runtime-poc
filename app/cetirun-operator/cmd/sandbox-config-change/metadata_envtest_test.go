// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build envtest

package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	sandboxadmission "github.com/Azure/CetiRun/app/cetirun-operator/internal/admission"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestCLIConfigurationMetadataAPIServer(t *testing.T) {
	if os.Getenv("USE_EXISTING_CLUSTER") == "true" {
		t.Fatal("CLI configuration tests refuse an existing cluster")
	}
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	for _, name := range []string{"kube-apiserver", "etcd", "kubectl"} {
		info, err := os.Stat(filepath.Join(assets, name))
		if assets == "" || err != nil || info.Mode()&0111 == 0 {
			t.Fatal("explicit preinstalled Kubernetes envtest assets are required")
		}
	}
	environment := &envtest.Environment{
		BinaryAssetsDirectory: assets, UseExistingCluster: ptr.To(false),
		CRDDirectoryPaths: []string{"../../config/crd/bases"}, ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	if err != nil {
		if stopErr := environment.Stop(); stopErr != nil {
			t.Error(stopErr)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := admissionregistrationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	storage, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := storage.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "configuration-state"}}); err != nil {
		t.Fatal(err)
	}
	kubeconfig := clientcmdapi.NewConfig()
	kubeconfig.Clusters["local"] = &clientcmdapi.Cluster{Server: config.Host, CertificateAuthorityData: config.CAData}
	kubeconfig.AuthInfos["local"] = &clientcmdapi.AuthInfo{ClientCertificateData: config.CertData, ClientKeyData: config.KeyData}
	kubeconfig.Contexts["local"] = &clientcmdapi.Context{Cluster: "local", AuthInfo: "local"}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*kubeconfig, path); err != nil {
		t.Fatal(err)
	}
	authority, err := sandboxcoordination.NewState("configuration-state", "coordination")
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.Create(ctx, authority); err != nil {
		t.Fatal(err)
	}
	store := sandboxcoordination.Store{Client: storage, APIReader: storage, Namespace: authority.Namespace,
		Identity: w.ObjectIdentity{Name: authority.Name, UID: authority.UID}}
	handler, err := sandboxadmission.NewHandler(storage, sandboxadmission.Config{
		ValidationMode: sandboxadmission.ValidationStrict, AdminGroups: []string{"system:masters"},
		ControllerUsername:        "system:serviceaccount:configuration-state:controller",
		CoordinationConfigMapName: authority.Name, CoordinationConfigMapUID: string(authority.UID),
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(&admission.Webhook{Handler: admission.HandlerFunc(handler.Validate)})
	t.Cleanup(server.Close)
	webhook := &admissionregistrationv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "configuration-coordination"},
		Webhooks: []admissionregistrationv1.ValidatingWebhook{{
			Name: "configuration.example.com", AdmissionReviewVersions: []string{"v1"},
			SideEffects:   ptr.To(admissionregistrationv1.SideEffectClassNone),
			FailurePolicy: ptr.To(admissionregistrationv1.Fail),
			ClientConfig: admissionregistrationv1.WebhookClientConfig{
				URL: ptr.To(server.URL), CABundle: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
			},
			Rules: []admissionregistrationv1.RuleWithOperations{{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
				Rule: admissionregistrationv1.Rule{APIGroups: []string{api.GroupVersion.Group}, APIVersions: []string{api.GroupVersion.Version},
					Resources: []string{"agentpolicies"}, Scope: ptr.To(admissionregistrationv1.ClusterScope)},
			}},
		}},
	}
	if err := storage.Create(ctx, webhook); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"minimal-manifest", "typed-manifest", "lost-response", "pending-submission"} {
		t.Run(scenario, func(t *testing.T) {
			created := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": api.GroupVersion.String(), "kind": "AgentPolicy",
				"metadata": map[string]any{"name": scenario, "labels": map[string]any{"example.com/run": "synthetic"}},
				"spec": map[string]any{
					"harnesses": map[string]any{"allowed": []any{"openclaw", "hermes"}},
					"modelAccess": map[string]any{
						"copilot":       map[string]any{"allowed": true},
						"gatewayBudget": map[string]any{"defaultMonthlyUSD": "5", "maxMonthlyUSD": "5"},
					},
					"network": map[string]any{"ingress": map[string]any{"mode": "disabled"}},
				},
			}}
			var initial client.Object = created
			if scenario == "typed-manifest" {
				typed := &api.AgentPolicy{}
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(created.Object, typed); err != nil {
					t.Fatal(err)
				}
				initial = typed
			}
			if err := storage.Create(ctx, initial, client.FieldOwner("fixture")); err != nil {
				t.Fatal(err)
			}
			policy := &api.AgentPolicy{}
			if err := storage.Get(ctx, client.ObjectKeyFromObject(created), policy); err != nil {
				t.Fatal(err)
			}
			policy.Status.ObservedGeneration = policy.Generation
			policy.Status.Conditions = []metav1.Condition{{
				Type: api.ConditionAgentPolicyAccepted, Status: metav1.ConditionTrue, Reason: "Accepted",
				Message: "synthetic accepted configuration", ObservedGeneration: policy.Generation, LastTransitionTime: metav1.Now(),
			}}
			if err := storage.Status().Update(ctx, policy, client.FieldOwner("fixture-status")); err != nil {
				t.Fatal(err)
			}
			effective, err := w.ExecutionObjectDigest(policy)
			if err != nil {
				t.Fatal(err)
			}
			policySource := w.SourceReference{APIVersion: api.GroupVersion.String(), Kind: "AgentPolicy",
				Name: policy.Name, UID: policy.UID, ResourceVersion: policy.ResourceVersion}
			teamSource := w.SourceReference{APIVersion: api.GroupVersion.String(), Kind: "Team",
				Name: "team", UID: "team-uid", ResourceVersion: "1"}
			boxSource := w.SourceReference{APIVersion: api.GroupVersion.String(), Kind: "Sandbox",
				Name: "box", UID: "box-uid", ResourceVersion: "1"}
			intent := sandboxcoordination.NativeIntent{
				Reconciled: true, Agent: w.ObjectIdentity{Name: scenario, UID: policy.UID + "-agent"}, AgentGeneration: 1,
				Namespace: w.ObjectIdentity{Name: scenario, UID: policy.UID + "-namespace"},
				Home:      w.ObjectIdentity{Name: "home", UID: policy.UID + "-home"},
				Workload:  w.ObjectIdentity{Name: "workload"},
				Binding: sandbox.Binding{AgentUID: policy.UID + "-agent", TeamUID: "team-uid", SandboxUID: "box-uid",
					AgentPolicyUID: policy.UID},
				Revision: "synthetic-revision", Sources: []w.SourceReference{teamSource, boxSource, policySource},
				SourceDigests: map[string]string{w.SourceKey(teamSource): strings.Repeat("a", 64),
					w.SourceKey(boxSource): strings.Repeat("b", 64), w.SourceKey(policySource): effective},
			}
			native, err := store.AcquireNative(ctx, intent)
			if err != nil {
				t.Fatal(err)
			}
			if err := wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 10*time.Second, true, func(ctx context.Context) (bool, error) {
				proposed := policy.DeepCopy()
				proposed.Labels["example.com/change"] = "metadata"
				err := storage.Update(ctx, proposed, client.DryRunAll)
				return err != nil && strings.Contains(err.Error(), "CoordinationRequired"), nil
			}); err != nil {
				t.Fatal("configuration admission never became active:", err)
			}
			raw, err := exec.CommandContext(ctx, filepath.Join(assets, "kubectl"), "--kubeconfig", path, "--context", "local",
				"get", "agentpolicy", policy.Name, "--show-managed-fields", "-o", "json").Output()
			if err != nil {
				t.Fatal("read the complete synthetic API image with kubectl:", err)
			}
			before := &unstructured.Unstructured{}
			if err := json.Unmarshal(raw, before); err != nil {
				t.Fatal(err)
			}
			after := before.DeepCopy()
			after.SetLabels(map[string]string{"example.com/run": "synthetic", "example.com/change": "metadata"})
			directory := t.TempDir()
			for name, object := range map[string]*unstructured.Unstructured{"before.json": before, "after.json": after} {
				raw, err := json.MarshalIndent(object, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, name), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"--kubeconfig", path, "--context", "local",
				"--authority-namespace", authority.Namespace, "--authority-name", authority.Name, "--authority-uid", string(authority.UID),
				"--before", filepath.Join(directory, "before.json"), "--after", filepath.Join(directory, "after.json")}
			updates, sourceUpdates := 0, 0
			var delayed func(context.Context) error
			factory := func(o options) (client.Client, client.Reader, error) {
				writer, reader, err := newClients(o)
				if err != nil {
					return nil, nil, err
				}
				return &metadataCLIWriter{Client: writer, update: func(ctx context.Context, object client.Object, opts ...client.UpdateOption) error {
					updates++
					source := object.GetObjectKind().GroupVersionKind() == api.GroupVersion.WithKind("AgentPolicy")
					if source {
						sourceUpdates++
						if scenario == "pending-submission" {
							pending := object.DeepCopyObject().(client.Object)
							delayed = func(ctx context.Context) error { return writer.Update(ctx, pending, opts...) }
							return errors.New(syntheticBody)
						}
					}
					if err := writer.Update(ctx, object, opts...); err != nil {
						return err
					}
					if source && scenario == "lost-response" {
						return errors.New(syntheticBody)
					}
					return nil
				}}, reader, nil
			}
			code, result, stderr := invoke(t, append(args, "--action", "update"), factory)
			if sourceUpdates != 1 || result.Phase != sandboxcoordination.Changing || result.Operation == "" {
				t.Fatalf("metadata update did not submit once under an exact operation: code=%d status=%+v updates=%d", code, result, sourceUpdates)
			}
			if scenario == "lost-response" || scenario == "pending-submission" {
				if code != 1 || result.Status != "uncertain" || !strings.Contains(stderr, observationInstruction) {
					t.Fatalf("unconfirmed submission was not retained: code=%d status=%+v stderr=%s", code, result, stderr)
				}
				operation, writes := result.Operation, updates
				if scenario == "pending-submission" {
					for _, action := range []string{"observe", "update"} {
						code, result, _ = invoke(t, append(args, "--action", action), factory)
						if code != 1 || result.Status != "uncertain" || result.Operation != operation || updates != writes {
							t.Fatal("unchanged source allowed replay or release of an unknown submission")
						}
					}
					current := &api.AgentPolicy{}
					if err := storage.Get(ctx, client.ObjectKeyFromObject(policy), current); err != nil {
						t.Fatal(err)
					}
					if current.ResourceVersion != before.GetResourceVersion() || current.Labels["example.com/change"] != "" {
						t.Fatal("pending request changed the source before it reached the API")
					}
					if delayed == nil {
						t.Fatal("missing delayed request")
					}
					if err := delayed(ctx); err != nil {
						t.Fatal("original pending request could not complete:", err)
					}
				}
				code, result, _ = invoke(t, append(args, "--action", "observe"), factory)
				if code != 0 || result.Status != "applied-reservation-retained" || result.Operation != operation || updates != writes {
					t.Fatal("read-only observation wrote or failed to confirm the original update")
				}
				code, result, stderr = invoke(t, append(args, "--action", "update"), factory)
				if result.Operation != operation || updates != writes+1 || sourceUpdates != 1 {
					t.Fatal("recovery resubmitted the source or failed to release only the original reservation")
				}
			}
			if code != 0 || result.Status != "completed" {
				t.Fatalf("production CLI metadata update failed: code=%d status=%+v stderr=%s", code, result, stderr)
			}
			observed := &api.AgentPolicy{}
			if err := storage.Get(ctx, client.ObjectKeyFromObject(policy), observed); err != nil {
				t.Fatal(err)
			}
			observedDigest, err := w.ExecutionObjectDigest(observed)
			if err != nil || observedDigest != effective || observed.Generation != before.GetGeneration() ||
				observed.UID != policy.UID || observed.ResourceVersion == before.GetResourceVersion() ||
				observed.Labels["example.com/change"] != "metadata" {
				t.Fatal("metadata change lost its original lifetime, effective inputs or API generation:", err)
			}
			if err := store.CheckNative(ctx, native); err != nil {
				t.Fatal("metadata update invalidated its unaffected native consumer:", err)
			}
			writes := updates
			code, result, _ = invoke(t, append(args, "--action", "update"), factory)
			if code != 1 || result.Status != "unavailable" || updates != writes || sourceUpdates != 1 {
				t.Fatal("completed operation was replayed using its stale before image")
			}
			if err := storage.Get(ctx, client.ObjectKeyFromObject(authority), authority); err != nil {
				t.Fatal(err)
			}
			var state struct {
				Changes []sandboxcoordination.Change `json:"changes"`
			}
			if err := json.Unmarshal([]byte(authority.Data[sandboxcoordination.DataKey]), &state); err != nil {
				t.Fatal(err)
			}
			for _, change := range state.Changes {
				if change.Before.UID == policy.UID {
					t.Fatal("successful CLI update retained its configuration reservation")
				}
			}
		})
	}
}

type metadataCLIWriter struct {
	client.Client
	update func(context.Context, client.Object, ...client.UpdateOption) error
}

func (c *metadataCLIWriter) Update(ctx context.Context, object client.Object, opts ...client.UpdateOption) error {
	return c.update(ctx, object, opts...)
}
