// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package testfixture supplies synthetic native admission transport fixtures.
package testfixture

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/internal/agenttools"
)

type Native struct {
	Objects   map[string]any
	Server    *httptest.Server
	Transport http.RoundTripper
	Allowed   bool
	Calls     int
	Identity  agenttools.NativePodIdentity
}

func NewNative(t testing.TB) *Native {
	t.Helper()
	f := &Native{Objects: map[string]any{}, Allowed: true, Identity: agenttools.NativePodIdentity{
		AgentUID: "11111111-1111-1111-1111-111111111111", Namespace: "agents",
		PodName: "openclaw-agent-cetirun-0", PodUID: "pod-uid", ServiceAccountName: "default", ServiceAccountUID: "service-account-uid",
	}}
	meta := func(name, namespace, uid string) map[string]any {
		return map[string]any{"name": name, "namespace": namespace, "uid": uid, "resourceVersion": "1", "generation": int64(1)}
	}
	namespace := meta("agents", "", "namespace-uid")
	namespace["labels"] = map[string]string{"cetirun.microsoft.com/sandbox-protected": "true"}
	namespace["ownerReferences"] = []any{map[string]any{
		"apiVersion": "cetirun.microsoft.com/v1alpha1", "kind": "Agent", "name": "agent",
		"uid": f.Identity.AgentUID, "controller": true, "blockOwnerDeletion": true,
	}}
	f.Objects["/api/v1/namespaces/agents"] = map[string]any{"metadata": namespace}
	f.Objects["/api/v1/namespaces/agents/serviceaccounts/default"] = map[string]any{"metadata": meta("default", "agents", f.Identity.ServiceAccountUID)}
	pod := meta(f.Identity.PodName, "agents", f.Identity.PodUID)
	pod["annotations"] = map[string]string{
		"sandbox.cetirun.microsoft.com/reconciled": "true", "cetirun.microsoft.com/agent-uid": f.Identity.AgentUID,
	}
	f.Objects["/api/v1/namespaces/agents/pods/"+f.Identity.PodName] = map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"status":   map[string]any{"phase": "Running", "conditions": []any{map[string]string{"type": "Ready", "status": "True"}}},
		"metadata": pod,
		"spec": map[string]any{
			"serviceAccountName": "default", "automountServiceAccountToken": false,
			"containers": []any{map[string]any{"name": "cetirun", "volumeMounts": []any{map[string]any{
				"name": "connections-identity", "mountPath": "/var/run/cetirun/connections", "readOnly": true,
			}}}},
			"volumes": []any{map[string]any{"name": "connections-identity", "projected": map[string]any{"sources": []any{map[string]any{
				"serviceAccountToken": map[string]any{"audience": "cetirun-connections-mcp", "path": "token", "expirationSeconds": 600},
			}}}}},
		},
	}
	agent := meta("agent", "", f.Identity.AgentUID)
	agent["ownerReferences"] = []any{map[string]any{
		"apiVersion": "cetirun.microsoft.com/v1alpha1", "kind": "Team", "name": "team",
		"uid": "team-uid", "controller": true, "blockOwnerDeletion": true,
	}}
	f.Objects["/apis/cetirun.microsoft.com/v1alpha1/agents/agent"] = map[string]any{
		"metadata": agent, "spec": map[string]any{},
		"status": map[string]any{"namespace": "agents", "phase": "Ready"},
	}
	f.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.Calls++
		var review struct {
			Request struct {
				UID, Operation, SubResource string
				Object, OldObject           json.RawMessage
				DryRun                      bool
			} `json:"request"`
		}
		if json.NewDecoder(r.Body).Decode(&review) != nil || r.Method != http.MethodPost ||
			r.URL.Path != agenttools.NativePodWebhookPath || review.Request.Operation != "UPDATE" ||
			review.Request.SubResource != "" || !review.Request.DryRun || !bytes.Equal(review.Request.Object, review.Request.OldObject) {
			t.Error("native verifier did not use a fixed no-op Pod review")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "admission.k8s.io/v1", "kind": "AdmissionReview",
			"response": map[string]any{"uid": review.Request.UID, "allowed": f.Allowed}})
	}))
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: []string{"cetirun-sandbox-admission.operator.svc"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	f.Server.TLS = &tls.Config{MinVersion: tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	f.Server.StartTLS()
	t.Cleanup(f.Server.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.Server.Certificate().Raw})
	f.Objects["/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations/"+agenttools.NativePodWebhookConfiguration] = map[string]any{
		"metadata": meta(agenttools.NativePodWebhookConfiguration, "", "webhook-uid"),
		"webhooks": []any{map[string]any{
			"name": agenttools.NativePodWebhookName, "admissionReviewVersions": []string{"v1"},
			"sideEffects": "None", "failurePolicy": "Fail",
			"clientConfig": map[string]any{"caBundle": ca, "service": map[string]any{
				"namespace": "operator", "name": "cetirun-sandbox-admission", "path": agenttools.NativePodWebhookPath, "port": 443,
			}},
		}},
	}
	roots := x509.NewCertPool()
	roots.AddCert(f.Server.Certificate())
	transport := &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, f.Server.Listener.Addr().String())
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	f.Transport = transport
	return f
}

func (f *Native) SetPodName(name string) {
	prefix := "/api/v1/namespaces/" + f.Identity.Namespace + "/pods/"
	pod := f.Objects[prefix+f.Identity.PodName].(map[string]any)
	delete(f.Objects, prefix+f.Identity.PodName)
	pod["metadata"].(map[string]any)["name"] = name
	f.Objects[prefix+name] = pod
	f.Identity.PodName = name
}

func (f *Native) SetServiceAccountName(name string) {
	prefix := "/api/v1/namespaces/" + f.Identity.Namespace
	previous := prefix + "/serviceaccounts/" + f.Identity.ServiceAccountName
	account := f.Objects[previous].(map[string]any)
	delete(f.Objects, previous)
	account["metadata"].(map[string]any)["name"] = name
	f.Objects[prefix+"/serviceaccounts/"+name] = account
	f.Objects[prefix+"/pods/"+f.Identity.PodName].(map[string]any)["spec"].(map[string]any)["serviceAccountName"] = name
	f.Identity.ServiceAccountName = name
}

func (f *Native) Get(_ context.Context, path string, out any) error {
	value, exists := f.Objects[path]
	if !exists {
		return errors.New("synthetic source is absent")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (f *Native) Response(path string) (*http.Response, bool) {
	value, exists := f.Objects[path]
	if !exists {
		return nil, false
	}
	data, _ := json.Marshal(value)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(data))}, true
}
