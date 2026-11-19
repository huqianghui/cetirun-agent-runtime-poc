// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/aigateway"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestExportNativeUpgradeFixture(t *testing.T) {
	output := os.Getenv("CETIRUN_UPGRADE_FIXTURE_OUT")
	if output == "" {
		t.Skip("explicit output path required")
	}
	source, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(source)) != "6414494154151b2daf74d39d1e989ebeb7ea5020" {
		t.Fatal("export requires the pinned pre-proxy single-policy source revision:", err)
	}
	original := cryptorand.Reader
	cryptorand.Reader = bytes.NewReader(bytes.Repeat([]byte{0x42}, 1<<20))
	t.Cleanup(func() { cryptorand.Reader = original })
	f := newNativeSandboxFixture(t, true)
	f.ready(t)
	f.reconcileOK(t)
	state, expected := f.expectation(t)
	receipt, _, err := f.r.readSandboxPreparationReceipt(context.Background(), f.current(t))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Annotations[nativeStartAnnotation] != expected.Revision || expected.PodUID == "" || expected.ExecutionHeld {
		t.Fatal("fixture did not reach the old native running lifetime")
	}
	objects := []json.RawMessage{}
	lists := []client.ObjectList{
		&api.AgentList{}, &api.TeamList{}, &api.SandboxList{}, &api.AgentPolicyList{},
		&corev1.NamespaceList{}, &corev1.ServiceAccountList{}, &corev1.SecretList{},
		&corev1.ConfigMapList{}, &corev1.PersistentVolumeClaimList{}, &corev1.PodList{},
		&corev1.NodeList{}, &corev1.ServiceList{}, &rbacv1.RoleList{}, &rbacv1.RoleBindingList{},
		&nodev1.RuntimeClassList{}, &storagev1.StorageClassList{}, &appsv1.StatefulSetList{},
		&appsv1.ControllerRevisionList{}, &istiov1beta1.VirtualServiceList{},
	}
	cilium := &unstructured.UnstructuredList{}
	cilium.SetAPIVersion("cilium.io/v2")
	cilium.SetKind("CiliumNetworkPolicyList")
	lists = append(lists, cilium)
	for _, list := range lists {
		if err := f.kube.List(context.Background(), list); err != nil {
			t.Fatal(err)
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			gvks, _, err := f.r.Scheme.ObjectKinds(item)
			if err != nil || len(gvks) == 0 {
				t.Fatalf("object kind: %v", err)
			}
			item.GetObjectKind().SetGroupVersionKind(gvks[0])
			raw, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			objects = append(objects, raw)
		}
	}
	slices.SortFunc(objects, func(a, b json.RawMessage) int { return bytes.Compare(a, b) })
	objectJSON, err := json.Marshal(objects)
	if err != nil {
		t.Fatal(err)
	}
	fixture := struct {
		SourceCommit  string                    `json:"sourceCommit"`
		Description   string                    `json:"description"`
		ObjectsSHA256 string                    `json:"objectsSHA256"`
		Revision      string                    `json:"revision"`
		StateName     string                    `json:"stateName"`
		ReceiptName   string                    `json:"receiptName"`
		StageDigest   string                    `json:"stageDigest"`
		GatewayKeys   map[string]*aigateway.Key `json:"gatewayKeys"`
		Objects       []json.RawMessage         `json:"objects"`
	}{
		strings.TrimSpace(string(source)),
		"Synthetic old native runtime and receipt. The test hydrates only deterministic dummy Secret values and the empty-render checksum; no live cluster or credentials.",
		fmt.Sprintf("%x", sha256.Sum256(objectJSON)),
		expected.Revision, state.Name, receipt.Name, f.installer.digest, f.gateway.keys, objects,
	}
	raw, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	entropy := bytes.Repeat([]byte{0x42}, 32)
	raw = []byte(strings.NewReplacer(
		base64.StdEncoding.EncodeToString([]byte("sk-"+base64.RawURLEncoding.EncodeToString(entropy))), "@SYNTHETIC_GATEWAY_KEY_DATA@",
		base64.StdEncoding.EncodeToString([]byte(base64.StdEncoding.EncodeToString(entropy))), "@SYNTHETIC_RUNTIME_KEY_DATA@",
		fmt.Sprintf("%x", sha256.Sum256([]byte("\n"))), "@EMPTY_RENDER_CHECKSUM@",
	).Replace(string(raw)))
	if err := os.WriteFile(output, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}
