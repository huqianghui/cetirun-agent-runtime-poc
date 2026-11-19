// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"
)

const syntheticBody = "synthetic-input-must-not-be-printed"

func writeObjectFile(t *testing.T, path string, object client.Object) {
	t.Helper()
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func cliFixture(t *testing.T) ([]string, sandboxcoordination.Store, client.WithWatch, *api.Team, *api.Team) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	authority, err := sandboxcoordination.NewState("operator-state", "coordination")
	if err != nil {
		t.Fatal(err)
	}
	authority.UID, authority.ResourceVersion = "authority-uid", "1"
	before := &api.Team{TypeMeta: metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "Team"},
		ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "team-uid", ResourceVersion: "1", Generation: 1,
			CreationTimestamp: metav1.NewTime(time.Unix(1700000000, 0))},
		Spec: api.TeamSpec{AgentPolicyRef: "policy", SandboxRefs: []string{"sandbox", "spare"},
			DefaultSandboxRef: "sandbox", MemberContext: api.TeamContextSpec{Role: syntheticBody}}}
	after := before.DeepCopy()
	after.Spec.DefaultSandboxRef = "spare"
	storage := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&api.Team{}).WithObjects(authority, before).Build()
	store := sandboxcoordination.Store{Client: storage, APIReader: storage, Namespace: authority.Namespace,
		Identity: w.ObjectIdentity{Name: authority.Name, UID: authority.UID}}
	directory := t.TempDir()
	beforePath, afterPath := filepath.Join(directory, "before.json"), filepath.Join(directory, "after.json")
	writeObjectFile(t, beforePath, before)
	writeObjectFile(t, afterPath, after)
	args := []string{"--kubeconfig", "/synthetic-explicit-config-not-read", "--context", "synthetic",
		"--authority-namespace", authority.Namespace, "--authority-name", authority.Name, "--authority-uid", string(authority.UID),
		"--before", beforePath, "--after", afterPath}
	return args, store, storage, before, after
}

func injectedClients(storage client.WithWatch) clientFactory {
	return func(options) (client.Client, client.Reader, error) { return storage, storage, nil }
}

func invoke(t *testing.T, args []string, factory clientFactory) (int, operationStatus, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := runWithClientFactory(context.Background(), args, &stdout, &stderr, factory)
	if stdout.Len() > 4096 || strings.Contains(stdout.String()+stderr.String(), syntheticBody) ||
		strings.Contains(stdout.String()+stderr.String(), "sk-dummy") {
		t.Fatal("CLI output exceeded its bound or exposed input/error content")
	}
	var status operationStatus
	if stdout.Len() != 0 {
		if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
			t.Fatal("status was not one JSON object:", err)
		}
	}
	return code, status, stderr.String()
}

func TestCLIObserveIsDefaultAndNeverWrites(t *testing.T) {
	for _, phase := range []string{"unrecorded", "reserved", "changing unchanged", "changing applied"} {
		t.Run(phase, func(t *testing.T) {
			args, store, storage, before, after := cliFixture(t)
			ctx := context.Background()
			var ticket sandboxcoordination.ChangeTicket
			var err error
			if phase != "unrecorded" {
				ticket, err = store.AcquireConfigurationChange(ctx, before, after)
				if err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(phase, "changing") {
				ticket, err = store.BeginChange(ctx, ticket)
				if err != nil {
					t.Fatal(err)
				}
			}
			if phase == "changing applied" {
				adapter, err := sandboxcoordination.NewKubernetesChangeAdapter(storage, storage, before, after)
				if err != nil {
					t.Fatal(err)
				}
				if err := adapter.Submit(ctx, ticket, func(ctx context.Context) error { return store.CheckChange(ctx, ticket) }); err != nil {
					t.Fatal(err)
				}
			}
			writes := 0
			observed := interceptor.NewClient(storage, interceptor.Funcs{
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					writes++
					return errors.New("observe must not write")
				},
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					writes++
					return errors.New("observe must not create")
				},
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					writes++
					return errors.New("observe must not delete")
				},
			})
			code, status, message := invoke(t, args, injectedClients(observed))
			expectedStatus := map[string]string{"unrecorded": "unavailable", "reserved": "reserved",
				"changing unchanged": "uncertain", "changing applied": "applied-reservation-retained"}[phase]
			expectedCode := 0
			if phase == "unrecorded" || phase == "changing unchanged" {
				expectedCode = 1
				if !strings.Contains(message, observationInstruction) {
					t.Fatal("uncertainty did not explain same-input observation")
				}
			}
			if code != expectedCode || status.Action != "observe" || status.Status != expectedStatus || writes != 0 {
				t.Fatalf("default observe: code=%d status=%+v writes=%d", code, status, writes)
			}
			if phase != "unrecorded" {
				if err := store.CheckChange(ctx, ticket); err != nil {
					t.Fatal("read-only observation released its reservation:", err)
				}
			}
		})
	}
}

func TestCLIExplicitUpdateAndUncertainRecovery(t *testing.T) {
	for _, outcome := range []string{"success", "lost response", "never committed"} {
		t.Run(outcome, func(t *testing.T) {
			args, store, storage, before, after := cliFixture(t)
			args = append(args, "--action", "update")
			updates := 0
			writer := interceptor.NewClient(storage, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
					if _, ok := object.(*api.Team); ok {
						updates++
						if outcome == "never committed" {
							return errors.New(syntheticBody + " sk-dummy")
						}
					}
					if err := c.Update(ctx, object, opts...); err != nil {
						return err
					}
					if _, ok := object.(*api.Team); ok && outcome == "lost response" {
						return errors.New(syntheticBody + " sk-dummy")
					}
					return nil
				},
			})
			code, status, message := invoke(t, args, injectedClients(writer))
			if updates != 1 || status.Operation == "" || status.Phase != sandboxcoordination.Changing {
				t.Fatalf("no exact one-shot submission: code=%d status=%+v updates=%d", code, status, updates)
			}
			if outcome == "success" {
				if code != 0 || status.Status != "completed" {
					t.Fatal("explicit update failed:", message)
				}
				if _, err := store.ReadConfigurationChange(context.Background(), before, after); err == nil {
					t.Fatal("completed change stayed reserved")
				}
				return
			}
			if code != 1 || status.Status != "uncertain" || !strings.Contains(message, observationInstruction) {
				t.Fatal("lost/failed response was hidden")
			}
			operation := status.Operation
			code, status, message = invoke(t, args, injectedClients(writer))
			if updates != 1 || operation != status.Operation {
				t.Fatal("recovery resubmitted or adopted another operation")
			}
			if outcome == "lost response" {
				if code != 0 || status.Status != "completed" {
					t.Fatal("matching committed write could not be recovered:", message)
				}
			} else {
				if code != 1 || status.Status != "uncertain" {
					t.Fatal("unchanged failed request was inferred aborted")
				}
				if _, err := store.ReadConfigurationChange(context.Background(), before, after); err != nil {
					t.Fatal("uncertain request stopped holding the reservation:", err)
				}
			}
		})
	}
}

func TestCLIUpdateRecoveryAfterStatusRefresh(t *testing.T) {
	for _, change := range []string{"status", "spec", "metadata"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			args, store, storage, before, after := cliFixture(t)
			ticket, err := store.AcquireConfigurationChange(ctx, before, after)
			if err != nil {
				t.Fatal(err)
			}
			ticket, err = store.BeginChange(ctx, ticket)
			if err != nil {
				t.Fatal(err)
			}
			current := before.DeepCopy()
			current.Status.Compatibility = &api.TeamCompatibilityStatus{State: api.CompatibilityCompatible}
			if err := storage.Status().Update(ctx, current); err != nil {
				t.Fatal(err)
			}
			if change != "status" {
				if change == "spec" {
					current.Spec.MemberContext.Role = "Reviewer"
				} else {
					current.Labels = map[string]string{"example.com/changed": "true"}
				}
				if err := storage.Update(ctx, current); err != nil {
					t.Fatal(err)
				}
			}
			if current.ResourceVersion == before.ResourceVersion {
				t.Fatal("fixture did not supersede the original resourceVersion")
			}
			sourceWrites, journalWrites := 0, 0
			writer := interceptor.NewClient(storage, interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
					if _, ok := object.(*corev1.ConfigMap); ok {
						journalWrites++
					} else {
						sourceWrites++
						return errors.New("recovery must not resubmit a source update")
					}
					return c.Update(ctx, object, opts...)
				},
			})
			code, observed, _ := invoke(t, args, injectedClients(writer))
			wantCode, wantStatus := 0, "update-superseded-reservation-retained"
			if change != "status" {
				wantCode, wantStatus = 1, "uncertain"
			}
			if code != wantCode || observed.Status != wantStatus || observed.Operation != ticket.Change.ID ||
				sourceWrites != 0 || journalWrites != 0 {
				t.Fatalf("read-only update recovery: code=%d status=%+v sourceWrites=%d journalWrites=%d", code, observed, sourceWrites, journalWrites)
			}
			if err := store.CheckChange(ctx, ticket); err != nil {
				t.Fatal("observation released the reservation:", err)
			}
			code, recovered, _ := invoke(t, append(args, "--action", "update"), injectedClients(writer))
			if change != "status" {
				if code != 1 || recovered.Status != "uncertain" || sourceWrites != 0 || journalWrites != 0 {
					t.Fatal("configuration drift was treated as a superseded update")
				}
				if err := store.CheckChange(ctx, ticket); err != nil {
					t.Fatal("configuration drift released the reservation:", err)
				}
				return
			}
			if code != 0 || recovered.Status != "update-superseded" || recovered.Operation != ticket.Change.ID ||
				sourceWrites != 0 || journalWrites != 1 {
				t.Fatalf("explicit update recovery: code=%d status=%+v sourceWrites=%d journalWrites=%d", code, recovered, sourceWrites, journalWrites)
			}
			if _, found, err := store.ReadConfigurationUpdate(ctx, current); err != nil || found {
				t.Fatal("superseded update retained its reservation:", err)
			}
			if err := storage.Update(ctx, after.DeepCopy()); !apierrors.IsConflict(err) {
				t.Fatalf("delayed original Update was not fenced by its old resourceVersion: %v", err)
			}
			actual := &api.Team{}
			if err := storage.Get(ctx, client.ObjectKeyFromObject(before), actual); err != nil {
				t.Fatal(err)
			}
			if actual.ResourceVersion != current.ResourceVersion || actual.Spec.DefaultSandboxRef != before.Spec.DefaultSandboxRef ||
				actual.Status.Compatibility == nil {
				t.Fatal("recovery changed the configuration or discarded its status")
			}
		})
	}
}

func TestCLIRejectsMismatchedJournalAndServerFields(t *testing.T) {
	for _, scenario := range []string{"journal mismatch", "generation", "managedFields", "status", "UID", "RV"} {
		t.Run(scenario, func(t *testing.T) {
			args, store, storage, before, after := cliFixture(t)
			if _, err := store.AcquireConfigurationChange(context.Background(), before, after); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "journal mismatch":
				after.Annotations = map[string]string{"example.com/unapproved": "other"}
			case "generation":
				after.Generation++
			case "managedFields":
				after.ManagedFields = []metav1.ManagedFieldsEntry{{Manager: "other"}}
			case "status":
				after.Status.ObservedGeneration++
			case "UID":
				after.UID = "replacement"
			case "RV":
				after.ResourceVersion = "2"
			}
			writeObjectFile(t, args[len(args)-1], after)
			writes := 0
			storage = interceptor.NewClient(storage, interceptor.Funcs{
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					writes++
					return errors.New("unexpected write")
				},
			})
			code, _, _ := invoke(t, append(args, "--action", "update"), injectedClients(storage))
			if code == 0 || writes != 0 {
				t.Fatal("mismatched input authorized an update")
			}
		})
	}
}

func TestCLIStrictArgumentsAndCancellation(t *testing.T) {
	args, _, _, _, _ := cliFixture(t)
	never := func(options) (client.Client, client.Reader, error) {
		t.Fatal("invalid arguments read Kubernetes configuration")
		return nil, nil, nil
	}
	for i := 0; i < len(args); i += 2 {
		t.Run("missing "+args[i], func(t *testing.T) {
			missing := append(append([]string{}, args[:i]...), args[i+2:]...)
			if code, _, _ := invoke(t, missing, never); code != 2 {
				t.Fatal("missing required flag accepted")
			}
		})
	}
	for _, extra := range [][]string{
		{"--kubeconfig", "relative"}, {"--context", ""}, {"--context", "line\nbreak"},
		{"--authority-name", "bad/name"}, {"--authority-uid", strings.Repeat("x", 129)},
		{"--action", "apply"}, {"--timeout", "0s"}, {"--timeout", "-1s"}, {"--timeout", "301s"},
		{"--timeout", "invalid"}, {"--unknown", syntheticBody}, {"positional"}, {"--action"},
	} {
		if code, _, _ := invoke(t, append(append([]string{}, args...), extra...), never); code != 2 {
			t.Fatalf("invalid flag combination was accepted: %q", extra[0])
		}
	}
	var stdout, stderr bytes.Buffer
	if code := runWithClientFactory(context.Background(), []string{"--help"}, &stdout, &stderr, never); code != 0 ||
		!strings.Contains(stdout.String(), "observe is the default") {
		t.Fatal("help did not explain read-only default")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := runWithClientFactory(ctx, args, &stdout, &stderr, never); code != 1 {
		t.Fatal("canceled request was not rejected")
	}
}

func TestStrictConfigurationDecoding(t *testing.T) {
	_, _, _, before, _ := cliFixture(t)
	validJSON, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	validYAML, err := yaml.JSONToYAML(validJSON)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{validJSON, validYAML, append([]byte("---\n"), validYAML...)} {
		object, err := decodeConfiguration(raw)
		if err != nil || object.GetUID() != before.UID || object.GetResourceVersion() != before.ResourceVersion {
			t.Fatal("strict valid object did not retain original identity:", err)
		}
	}
	for name, raw := range map[string]string{
		"unknown":          strings.Replace(string(validJSON), `"spec":`, `"unknown":true,"spec":`, 1),
		"unknown spec":     strings.Replace(string(validJSON), `"spec":{`, `"spec":{"unapproved":true,`, 1),
		"unknown status":   strings.Replace(string(validJSON), `"status":{`, `"status":{"unapproved":true,`, 1),
		"unknown metadata": strings.Replace(string(validJSON), `"metadata":{`, `"metadata":{"unapproved":true,`, 1),
		"duplicate":        strings.Replace(string(validJSON), `"kind":"Team"`, `"kind":"Team","kind":"Team"`, 1),
		"nested duplicate": strings.Replace(string(validJSON), `"name":"team"`, `"name":"team","name":"team"`, 1),
		"wrong case":       strings.Replace(string(validJSON), `"spec":`, `"Spec":`, 1),
		"multiple JSON":    string(validJSON) + "\n" + string(validJSON),
		"multiple YAML":    string(validYAML) + "\n---\n" + string(validYAML),
		"extra empty YAML": string(validYAML) + "\n---\n",
		"ended YAML":       string(validYAML) + "\n...\n---\n" + string(validYAML),
		"duplicate YAML":   string(validYAML) + "\nkind: Team\n",
		"kind missing":     strings.Replace(string(validJSON), `"kind":"Team",`, "", 1),
		"version missing":  strings.Replace(string(validJSON), `"apiVersion":"`+api.GroupVersion.String()+`",`, "", 1),
		"version changed":  strings.Replace(string(validJSON), api.GroupVersion.String(), "example.com/v1", 1),
		"Secret":           strings.Replace(string(validJSON), `"kind":"Team"`, `"kind":"Secret"`, 1),
		"ConfigMap":        strings.Replace(string(validJSON), `"kind":"Team"`, `"kind":"ConfigMap"`, 1),
		"Agent":            strings.Replace(string(validJSON), `"kind":"Team"`, `"kind":"Agent"`, 1),
		"UID missing":      strings.Replace(string(validJSON), `"uid":"team-uid",`, "", 1),
		"RV missing":       strings.Replace(string(validJSON), `"resourceVersion":"1",`, "", 1),
		"list":             "[" + string(validJSON) + "]",
		"scalar":           "false",
		"empty":            "",
		"null":             "null",
		"syntax":           "{",
		"oversized":        string(bytes.Repeat([]byte("x"), sandboxcoordination.MaxConfigurationBytes+1)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeConfiguration([]byte(raw)); err == nil {
				t.Fatal("invalid configuration was accepted")
			}
		})
	}
	exactLimit := append(append([]byte{}, validJSON...), bytes.Repeat([]byte(" "), sandboxcoordination.MaxConfigurationBytes-len(validJSON))...)
	if _, err := decodeConfiguration(exactLimit); err != nil {
		t.Fatal("exact byte bound was rejected:", err)
	}
	if _, err := decodeConfiguration(append(exactLimit, ' ')); err == nil {
		t.Fatal("byte bound ignored trailing whitespace")
	}
	for _, kind := range []string{"Sandbox", "AgentPolicy"} {
		raw := `{"apiVersion":"` + api.GroupVersion.String() + `","kind":"` + kind +
			`","metadata":{"name":"example","uid":"example-uid","resourceVersion":"1","generation":1}}`
		if _, err := decodeConfiguration([]byte(raw)); err != nil {
			t.Fatalf("supported %s could not be decoded: %v", kind, err)
		}
	}
}

func TestCLIFileClientAndOutputFailuresAreRedacted(t *testing.T) {
	args, _, storage, _, _ := cliFixture(t)
	for _, path := range []string{t.TempDir(), filepath.Join(t.TempDir(), "missing")} {
		bad := append([]string{}, args...)
		bad[len(bad)-1] = path
		if code, _, _ := invoke(t, bad, injectedClients(storage)); code != 2 {
			t.Fatal("invalid file accepted")
		}
	}
	large := filepath.Join(t.TempDir(), "large.json")
	if err := os.WriteFile(large, bytes.Repeat([]byte(" "), sandboxcoordination.MaxConfigurationBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfiguration(large); err == nil {
		t.Fatal("oversized file accepted")
	}
	if code, _, message := invoke(t, args, func(options) (client.Client, client.Reader, error) {
		return nil, nil, errors.New(syntheticBody + " sk-dummy")
	}); code != 1 || !strings.Contains(message, "client setup failed") {
		t.Fatal("client failure was not surfaced safely")
	}
	var stderr bytes.Buffer
	if code := runWithClientFactory(context.Background(), args, failingWriter{}, &stderr, injectedClients(storage)); code != 1 ||
		!strings.Contains(stderr.String(), observationInstruction) || strings.Contains(stderr.String(), syntheticBody) {
		t.Fatal("status output failure was hidden or leaked error content")
	}
	stderr.Reset()
	if code := runWithClientFactory(context.Background(), []string{"--help"}, failingWriter{}, &stderr, injectedClients(storage)); code != 1 {
		t.Fatal("help output failure was hidden")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New(syntheticBody) }

func TestExplicitSyntheticKubeconfig(t *testing.T) {
	for _, scenario := range []string{"valid", "missing context", "missing cluster", "missing user", "no server", "http", "insecure", "exec", "auth provider"} {
		t.Run(scenario, func(t *testing.T) {
			config := clientcmdapi.NewConfig()
			config.CurrentContext = "ambient"
			config.Clusters["explicit"] = &clientcmdapi.Cluster{Server: "https://cluster.example.com"}
			config.Clusters["ambient"] = &clientcmdapi.Cluster{Server: "https://ambient.example.com"}
			config.AuthInfos["synthetic"] = &clientcmdapi.AuthInfo{}
			config.Contexts["selected"] = &clientcmdapi.Context{Cluster: "explicit", AuthInfo: "synthetic"}
			config.Contexts["ambient"] = &clientcmdapi.Context{Cluster: "ambient", AuthInfo: "synthetic"}
			switch scenario {
			case "missing context":
				delete(config.Contexts, "selected")
			case "missing cluster":
				delete(config.Clusters, "explicit")
			case "missing user":
				delete(config.AuthInfos, "synthetic")
			case "no server":
				config.Clusters["explicit"].Server = ""
			case "http":
				config.Clusters["explicit"].Server = "http://cluster.example.com"
			case "insecure":
				config.Clusters["explicit"].InsecureSkipTLSVerify = true
			case "exec":
				config.AuthInfos["synthetic"].Exec = &clientcmdapi.ExecConfig{Command: "must-not-run"}
			case "auth provider":
				config.AuthInfos["synthetic"].AuthProvider = &clientcmdapi.AuthProviderConfig{Name: "azure"}
			}
			raw, err := clientcmd.Write(*config)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "generated-synthetic-config")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "must-not-read"))
			t.Setenv("KUBERNETES_SERVICE_HOST", "ambient.example.com")
			t.Setenv("KUBERNETES_SERVICE_PORT", "443")
			selected, err := loadRESTConfig(options{kubeconfig: path, contextName: "selected", timeout: 30 * time.Second})
			if scenario == "valid" {
				if err != nil || selected.Host != "https://cluster.example.com" || selected.Timeout != 30*time.Second {
					t.Fatal("explicit context was not selected without fallback:", err)
				}
			} else if err == nil {
				t.Fatal("invalid explicit configuration silently fell back")
			}
		})
	}
}

func TestCLIUsesExactConfigurationResourcePaths(t *testing.T) {
	config := clientcmdapi.NewConfig()
	config.Clusters["synthetic"] = &clientcmdapi.Cluster{Server: "https://cluster.example.com"}
	config.AuthInfos["synthetic"] = &clientcmdapi.AuthInfo{}
	config.Contexts["synthetic"] = &clientcmdapi.Context{Cluster: "synthetic", AuthInfo: "synthetic"}
	raw, err := clientcmd.Write(*config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "generated-synthetic-config")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	writer, reader, err := newClients(options{kubeconfig: path, contextName: "synthetic", timeout: 30 * time.Second})
	if err != nil || writer == nil || reader == nil {
		t.Fatal("explicit client construction failed:", err)
	}
	for kind, resource := range map[string]string{"Team": "teams", "Sandbox": "sandboxes", "AgentPolicy": "agentpolicies"} {
		mapping, err := writer.RESTMapper().RESTMapping(api.GroupVersion.WithKind(kind).GroupKind(), api.GroupVersion.Version)
		if err != nil || mapping.Resource.Resource != resource || mapping.Scope != meta.RESTScopeRoot {
			t.Fatalf("incorrect endpoint for %s: mapping=%v error=%v", kind, mapping, err)
		}
	}
}

func TestCLITransportDoesNotRetryAnUpdate(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Method != http.MethodPut {
			t.Error("unexpected request method:", request.Method)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Retry-After", "1")
		writer.WriteHeader(http.StatusTooManyRequests)
		if err := json.NewEncoder(writer).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
			Status: metav1.StatusFailure, Reason: metav1.StatusReasonTooManyRequests, Code: http.StatusTooManyRequests}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{api.GroupVersion})
	mapper.Add(api.GroupVersion.WithKind("Team"), meta.RESTScopeRoot)
	httpClient := &http.Client{Transport: &singleMutationTransport{next: http.DefaultTransport}}
	base, err := client.New(&rest.Config{Host: server.URL}, client.Options{Scheme: scheme, Mapper: mapper, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	writer := &singleMutationClient{Client: base}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = writer.Update(ctx, &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "uid", ResourceVersion: "1"}})
	if err == nil || requests.Load() != 1 {
		t.Fatalf("automatic retry reached HTTP: requests=%d error=%v", requests.Load(), err)
	}
}

func TestCLITransportRejectsUnapprovedWrites(t *testing.T) {
	calls := 0
	transport := &singleMutationTransport{next: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		request, err := http.NewRequest(method, "https://cluster.example.com", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := transport.RoundTrip(request); err == nil {
			t.Fatal("unapproved request reached transport")
		}
	}
	if calls != 0 {
		t.Fatal("unapproved request reached network")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
