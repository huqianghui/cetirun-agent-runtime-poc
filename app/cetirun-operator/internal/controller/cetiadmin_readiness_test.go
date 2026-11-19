// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

func cetiAdminWebhookFixture() *admissionv1.ValidatingWebhookConfiguration {
	object := &admissionv1.ValidatingWebhookConfiguration{ObjectMeta: metav1.ObjectMeta{Name: cetiAdminAdmissionName}}
	for kind, rules := range cetiAdminRequiredRules() {
		object.Webhooks = append(object.Webhooks, admissionv1.ValidatingWebhook{Name: "cetiadmin-" + kind + ".cetirun.microsoft.com", Rules: rules,
			ClientConfig:  admissionv1.WebhookClientConfig{CABundle: []byte("synthetic-CA"), Service: &admissionv1.ServiceReference{Name: "cetirun-sandbox-admission", Namespace: "platform-test", Path: ptr.To(cetiadmin.AdmissionValidationPath), Port: ptr.To(int32(443))}},
			FailurePolicy: ptr.To(admissionv1.Fail), MatchPolicy: ptr.To(admissionv1.Equivalent), SideEffects: ptr.To(admissionv1.SideEffectClassNone), MatchConditions: cetiAdminWebhookConditions(kind, "platform-test", "cetiadmin", "cetirun-cetiadmin-config")})
		if kind == "workloads" {
			object.Webhooks[len(object.Webhooks)-1].NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/managed-by": "cetirun-operator"}}
		}
	}
	return object
}

func TestCetiAdminAdmissionContractRejectsBypasses(t *testing.T) {
	if err := checkCetiAdminWebhookContract(cetiAdminWebhookFixture(), "platform-test", "cetiadmin", "cetirun-cetiadmin-config"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*admissionv1.ValidatingWebhookConfiguration)
	}{
		{"missing handler", func(o *admissionv1.ValidatingWebhookConfiguration) { o.Webhooks = o.Webhooks[:2] }},
		{"fail open", func(o *admissionv1.ValidatingWebhookConfiguration) {
			o.Webhooks[0].FailurePolicy = ptr.To(admissionv1.Ignore)
		}},
		{"wrong server", func(o *admissionv1.ValidatingWebhookConfiguration) { o.Webhooks[0].ClientConfig.Service.Name = "other" }},
		{"CA absent", func(o *admissionv1.ValidatingWebhookConfiguration) { o.Webhooks[0].ClientConfig.CABundle = nil }},
		{"bypass selector", func(o *admissionv1.ValidatingWebhookConfiguration) {
			o.Webhooks[0].ObjectSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"optional": "true"}}
		}},
		{"false condition", func(o *admissionv1.ValidatingWebhookConfiguration) {
			o.Webhooks[0].MatchConditions = []admissionv1.MatchCondition{{Name: "never", Expression: "false"}}
		}},
		{"missing rules", func(o *admissionv1.ValidatingWebhookConfiguration) { o.Webhooks[0].Rules = nil }},
		{"unguarded ConfigMap restoration", func(o *admissionv1.ValidatingWebhookConfiguration) {
			for i := range o.Webhooks {
				if o.Webhooks[i].Name == "cetiadmin-workloads.cetirun.microsoft.com" {
					for j := range o.Webhooks[i].Rules {
						o.Webhooks[i].Rules[j].Resources = slices.DeleteFunc(o.Webhooks[i].Rules[j].Resources, func(resource string) bool { return resource == "configmaps" })
					}
				}
			}
		}},
		{"cluster only", func(o *admissionv1.ValidatingWebhookConfiguration) {
			for i := range o.Webhooks {
				for j := range o.Webhooks[i].Rules {
					o.Webhooks[i].Rules[j].Scope = ptr.To(admissionv1.ClusterScope)
				}
			}
		}},
		{"namespaced only", func(o *admissionv1.ValidatingWebhookConfiguration) {
			for i := range o.Webhooks {
				for j := range o.Webhooks[i].Rules {
					o.Webhooks[i].Rules[j].Scope = ptr.To(admissionv1.NamespacedScope)
				}
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			object := cetiAdminWebhookFixture()
			test.change(object)
			if checkCetiAdminWebhookContract(object, "platform-test", "cetiadmin", "cetirun-cetiadmin-config") == nil {
				t.Fatal("incomplete or bypassable admission was accepted")
			}
		})
	}
}

func TestCetiAdminAdmissionContractRecognizesExactAKSExclusions(t *testing.T) {
	object := cetiAdminWebhookFixture()
	for i := range object.Webhooks {
		if object.Webhooks[i].NamespaceSelector == nil {
			object.Webhooks[i].NamespaceSelector = &metav1.LabelSelector{}
		}
		object.Webhooks[i].NamespaceSelector.MatchExpressions = []metav1.LabelSelectorRequirement{
			{Key: "control-plane", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"true"}},
			{Key: "kubernetes.azure.com/managedby", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"aks"}},
		}
	}
	if err := checkCetiAdminWebhookContract(object, "platform-test", "cetiadmin", "cetirun-cetiadmin-config"); err != nil {
		t.Fatal(err)
	}
	object.Webhooks[0].NamespaceSelector.MatchExpressions = append(object.Webhooks[0].NamespaceSelector.MatchExpressions,
		metav1.LabelSelectorRequirement{Key: "tenant-controlled", Operator: metav1.LabelSelectorOpDoesNotExist})
	if checkCetiAdminWebhookContract(object, "platform-test", "cetiadmin", "cetirun-cetiadmin-config") == nil {
		t.Fatal("an unrelated namespace bypass was accepted")
	}
}

func TestCetiAdminAdmissionChecksActualServerVersion(t *testing.T) {
	for _, version := range []string{"v1.36.9", "unrecognized"} {
		t.Run(version, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/version" {
					t.Error("unexpected API operation")
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"gitVersion":"` + version + `"}`))
			}))
			defer server.Close()
			r := &CetiAdminReconciler{}
			probe, err := r.NewAdmissionProbe(&rest.Config{Host: server.URL})
			if err != nil {
				t.Fatal(err)
			}
			if err = probe(context.Background()); err == nil || !strings.Contains(err.Error(), ">= 1.37") {
				t.Fatalf("old/unknown API server accepted: %v", err)
			}
		})
	}
}

func TestCetiAdminRenderedAdmissionMeetsControllerContract(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	chart, err := loader.Load("../../chart")
	if err != nil {
		t.Fatal(err)
	}
	for _, enforce := range []bool{false, true} {
		values := map[string]any{"namespace": "platform-test", "domainSuffix": "agents.example.com",
			"cetiadmin":        map[string]any{"owner": map[string]any{"alias": "owner", "tenantID": "11111111-1111-1111-1111-111111111111", "objectID": "22222222-2222-2222-2222-222222222222"}, "model": map[string]any{"initialMonthlyBudgetUSD": "100"}, "kubernetes": map[string]any{"apiAudience": "kubernetes-api", "apiEgressCIDRs": []any{"192.0.2.10/32"}}},
			"sandboxAdmission": map[string]any{"enabled": enforce, "enforce": enforce, "tlsSecretName": "fixture-tls", "caBundle": ca, "adminUsers": []any{"administrator"}}}
		capabilities := chartutil.DefaultCapabilities.Copy()
		capabilities.KubeVersion.Version = "v1.37.0"
		input, err := chartutil.ToRenderValues(chart, values, chartutil.ReleaseOptions{Name: "operator", Namespace: "platform-test", IsInstall: true}, capabilities)
		if err != nil {
			t.Fatal(err)
		}
		manifests, err := engine.Render(chart, input)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		var guard *admissionv1.ValidatingAdmissionPolicy
		var guardBinding *admissionv1.ValidatingAdmissionPolicyBinding
		for _, document := range strings.Split(manifests["cetirun-operator/templates/cetiadmin-admission.yaml"], "\n---") {
			var kind struct {
				Kind string `json:"kind"`
			}
			if yaml.Unmarshal([]byte(document), &kind) != nil {
				t.Fatal("rendered document is not YAML")
			}
			if kind.Kind == "ValidatingWebhookConfiguration" {
				object := &admissionv1.ValidatingWebhookConfiguration{}
				if err := yaml.Unmarshal([]byte(document), object); err != nil {
					t.Fatal(err)
				}
				if err := checkCetiAdminWebhookContract(object, "platform-test", "cetiadmin", "cetirun-cetiadmin-config"); err != nil {
					t.Fatalf("enforce=%v: %v", enforce, err)
				}
				found = true
			}
			if kind.Kind == "ValidatingAdmissionPolicy" {
				guard = &admissionv1.ValidatingAdmissionPolicy{}
				if err := yaml.Unmarshal([]byte(document), guard); err != nil {
					t.Fatal(err)
				}
			}
			if kind.Kind == "ValidatingAdmissionPolicyBinding" {
				guardBinding = &admissionv1.ValidatingAdmissionPolicyBinding{}
				if err := yaml.Unmarshal([]byte(document), guardBinding); err != nil {
					t.Fatal(err)
				}
			}
		}
		if !found {
			t.Fatal("CetiAdmin validation rules were not rendered")
		}
		if guard == nil || guardBinding == nil {
			t.Fatal("independent namespace label protection was not rendered")
		}
		if err := cetiadmin.ValidateNamespaceAdmissionGuard(guard, guardBinding, "platform-test", "cetirun-cetiadmin"); err != nil {
			t.Fatal(err)
		}
	}
}
