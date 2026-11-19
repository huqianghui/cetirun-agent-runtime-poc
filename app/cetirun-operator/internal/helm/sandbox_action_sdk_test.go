// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	jsonpatch "github.com/evanphx/json-patch/v5"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/cli-runtime/pkg/resource"
	restfake "k8s.io/client-go/rest/fake"
)

// Only discovery/building and HTTP/storage are synthetic. Mutations go through
// the real Helm install/upgrade actions and kube.Client, not apply(target).
type sandboxSDKKube struct {
	*kube.Client
	api *sandboxSDKAPI
}

func (k *sandboxSDKKube) IsReachable() error { return nil }

func (k *sandboxSDKKube) Build(reader io.Reader, _ bool) (kube.ResourceList, error) {
	decoder := kubeyaml.NewYAMLOrJSONDecoder(reader, 4096)
	var resources kube.ResourceList
	for {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj); err == io.EOF {
			return resources, nil
		} else if err != nil {
			return nil, err
		}
		if len(obj.Object) == 0 {
			continue
		}
		mapping := sandboxTestMapping(&obj)
		apiPath := "/apis/" + mapping.Resource.GroupVersion().String()
		if mapping.Resource.Group == "" {
			apiPath = "/api/" + mapping.Resource.Version
		}
		client := &restfake.RESTClient{
			GroupVersion: mapping.Resource.GroupVersion(), VersionedAPIPath: apiPath,
			NegotiatedSerializer: resource.UnstructuredPlusDefaultContentConfig().NegotiatedSerializer,
			Client:               restfake.CreateHTTPClient(k.api.request),
		}
		resources = append(resources, &resource.Info{
			Client: client, Mapping: mapping, Namespace: obj.GetNamespace(), Name: obj.GetName(), Object: obj.DeepCopy(),
		})
	}
}

type sandboxSDKRequest struct {
	method, path, contentType string
	body                      []byte
}

type sandboxSDKAPI struct {
	mu       sync.Mutex
	objects  map[string]*unstructured.Unstructured
	requests []sandboxSDKRequest
	sequence int
	before   func(sandboxSDKRequest)
	respond  func(sandboxSDKRequest) (*http.Response, error)
	fail     func(sandboxSDKRequest) error
	stored   func(*unstructured.Unstructured)
}

func sandboxSDKConfig() (*action.Configuration, *sandboxSDKAPI, *sandboxActionStorage) {
	cfg := customActionConfig()
	api := &sandboxSDKAPI{objects: map[string]*unstructured.Unstructured{}}
	cfg.KubeClient = &sandboxSDKKube{Client: &kube.Client{Log: func(string, ...any) {}}, api: api}
	mem := driver.NewMemory()
	mem.SetNamespace("cetirun-example")
	store := &sandboxActionStorage{Driver: mem}
	cfg.Releases = storage.Init(store)
	return cfg, api, store
}

func sandboxSDKResponse(code int, value any) (*http.Response, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}},
		Body: io.NopCloser(bytes.NewReader(raw)),
	}, nil
}

func sandboxSDKStatus(code int, reason metav1.StatusReason) (*http.Response, error) {
	return sandboxSDKResponse(code, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure, Reason: reason, Code: int32(code), Message: string(reason),
	})
}

func sandboxSDKMergePatch(current *unstructured.Unstructured, patch []byte, contentType string) (*unstructured.Unstructured, error) {
	raw, err := current.MarshalJSON()
	if err != nil {
		return nil, err
	}
	switch types.PatchType(contentType) {
	case types.MergePatchType:
		raw, err = jsonpatch.MergePatch(raw, patch)
	case types.StrategicMergePatchType:
		versioned := kube.AsVersioned(&resource.Info{Object: current, Mapping: sandboxTestMapping(current)})
		raw, err = strategicpatch.StrategicMergePatch(raw, patch, versioned)
	default:
		return nil, fmt.Errorf("unsupported synthetic API patch type %q", contentType)
	}
	if err != nil {
		return nil, err
	}
	var result unstructured.Unstructured
	if err := result.UnmarshalJSON(raw); err != nil {
		return nil, err
	}
	return &result, nil
}

func (a *sandboxSDKAPI) request(request *http.Request) (*http.Response, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	call := sandboxSDKRequest{method: request.Method, path: request.URL.Path, contentType: request.Header.Get("Content-Type")}
	if request.Body != nil {
		var err error
		call.body, err = io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
	}
	a.requests = append(a.requests, call)
	if a.before != nil {
		a.before(call)
	}
	if a.respond != nil {
		response, err := a.respond(call)
		if response != nil || err != nil {
			return response, err
		}
	}
	if a.fail != nil {
		if err := a.fail(call); err != nil {
			return nil, err
		}
	}
	current := a.objects[call.path]
	switch call.method {
	case http.MethodGet:
		if current == nil {
			return sandboxSDKStatus(http.StatusNotFound, metav1.StatusReasonNotFound)
		}
		return sandboxSDKResponse(http.StatusOK, current)
	case http.MethodPost:
		var created unstructured.Unstructured
		if err := created.UnmarshalJSON(call.body); err != nil {
			return nil, err
		}
		path := call.path + "/" + created.GetName()
		if path != sandboxTestPath(&created) {
			return nil, fmt.Errorf("incorrect synthetic create URL %s", path)
		}
		if a.objects[path] != nil {
			return sandboxSDKStatus(http.StatusConflict, metav1.StatusReasonAlreadyExists)
		}
		if created.GetUID() != "" || created.GetResourceVersion() != "" {
			return sandboxSDKStatus(http.StatusBadRequest, metav1.StatusReasonBadRequest)
		}
		a.sequence++
		created.SetUID(types.UID(fmt.Sprintf("sdk-object-%d", a.sequence)))
		created.SetResourceVersion(strconv.Itoa(a.sequence))
		a.objects[path] = created.DeepCopy()
		if a.stored != nil {
			a.stored(a.objects[path])
		}
		return sandboxSDKResponse(http.StatusCreated, &created)
	case http.MethodPatch:
		if current == nil {
			return sandboxSDKStatus(http.StatusNotFound, metav1.StatusReasonNotFound)
		}
		updated, err := sandboxSDKMergePatch(current, call.body, call.contentType)
		if err != nil {
			return nil, err
		}
		if updated.GetUID() != current.GetUID() || updated.GetResourceVersion() != current.GetResourceVersion() {
			return sandboxSDKStatus(http.StatusConflict, metav1.StatusReasonConflict)
		}
		a.sequence++
		updated.SetResourceVersion(strconv.Itoa(a.sequence))
		a.objects[call.path] = updated.DeepCopy()
		if a.stored != nil {
			a.stored(a.objects[call.path])
		}
		return sandboxSDKResponse(http.StatusOK, updated)
	case http.MethodDelete:
		if current == nil {
			return sandboxSDKStatus(http.StatusNotFound, metav1.StatusReasonNotFound)
		}
		var options metav1.DeleteOptions
		if err := json.Unmarshal(call.body, &options); err != nil {
			return nil, err
		}
		if options.Preconditions != nil &&
			(options.Preconditions.UID != nil && *options.Preconditions.UID != current.GetUID() ||
				options.Preconditions.ResourceVersion != nil && *options.Preconditions.ResourceVersion != current.GetResourceVersion()) {
			return sandboxSDKStatus(http.StatusConflict, metav1.StatusReasonConflict)
		}
		delete(a.objects, call.path)
		return sandboxSDKResponse(http.StatusOK, current)
	default:
		return nil, fmt.Errorf("unexpected synthetic API method %s", call.method)
	}
}

func (a *sandboxSDKAPI) object(t *testing.T, kind string) *unstructured.Unstructured {
	t.Helper()
	for _, object := range a.objects {
		if object.GetKind() == kind {
			return object
		}
	}
	t.Fatalf("missing synthetic %s", kind)
	return nil
}

func sandboxSDKKubernetesAccessPlans(t *testing.T) (*PreparedRelease, *PreparedRelease, *unstructured.Unstructured) {
	t.Helper()
	fixture := gatewayHeldFixture("openclaw")
	fixture.Input.Helpers.Desktop = false
	fixture.Policy.Spec.KubernetesAccess = &api.AgentPolicyAdminKubernetesAccess{
		ServiceAccountName: "sandbox-example",
		ClusterRoleBindings: []api.AgentPolicyClusterRoleBinding{
			{ClusterRoleName: "example-reader-a"}, {ClusterRoleName: "example-reader-b"},
		},
	}
	resources := sandbox.Resources{
		Sandboxes:     map[string]*api.Sandbox{fixture.Sandbox.Name: fixture.Sandbox},
		AgentPolicies: map[string]*api.AgentPolicy{fixture.Policy.Name: fixture.Policy},
	}
	caps := fixture.Input.Resolution.Capabilities
	caps.KubernetesAccess = true
	platform := sandboxpolicy.Platform{
		Adapter: sandboxpolicy.AdapterRegistration{
			Name: sandboxpolicy.AzureCNIOverlayCilium, Revision: "synthetic-only", Qualified: true, Available: true,
		},
		Topology: sandboxpolicy.Topology{
			Revision: "synthetic-only", ManagedPeersComplete: true,
			DNSResolverIPs: []string{"10.96.0.53"},
			DNS: []sandboxpolicy.Peer{{
				Name: "dns", Namespace: "platform-dns", UID: "dns-uid", IPs: []string{"10.96.0.53", "10.244.0.11"},
			}},
			Gateway: []sandboxpolicy.Destination{{
				Peer:  sandboxpolicy.Peer{Name: "gateway", Namespace: "platform-gateway", UID: "gateway-uid", IPs: []string{"10.96.0.20"}},
				Ports: []api.AgentPolicyPort{{Port: 8443, Protocol: corev1.ProtocolTCP}},
			}},
			KubernetesAPI: []sandboxpolicy.Destination{{
				Peer:  sandboxpolicy.Peer{Name: "api", UID: "api-uid", IPs: []string{"10.96.0.1"}},
				Ports: []api.AgentPolicyPort{{Port: 443, Protocol: corev1.ProtocolTCP}},
			}},
			PodCIDRs: []string{"10.244.0.0/16"}, ServiceCIDRs: []string{"10.96.0.0/12"},
			NodeCIDRs: []string{"10.0.0.0/24"}, ManagementCIDRs: []string{"192.168.0.0/24"},
			MetadataCIDRs: []string{"169.254.169.254/32"},
		},
		KubernetesAudience: "https://kubernetes.default.svc", TokenExpirationSeconds: 600,
		Identities: []sandboxpolicy.ApprovedIdentity{{
			AgentUID: fixture.Agent.UID, Namespace: fixture.Namespace.Name, ServiceAccountName: "sandbox-example",
			Revision: "synthetic-only", Kubernetes: true,
		}},
	}
	for _, grant := range fixture.Policy.Spec.KubernetesAccess.ClusterRoleBindings {
		platform.Grants = append(platform.Grants, sandboxpolicy.ApprovedGrant{
			PolicyUID: fixture.Policy.UID, RoleUID: types.UID(grant.ClusterRoleName + "-uid"),
			RoleResourceVersion: "1", Revision: "synthetic-only",
			RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: grant.ClusterRoleName},
			Rules:   []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}}},
		})
	}
	var prepared []*PreparedRelease
	for revision := 0; revision < 2; revision++ {
		if revision == 1 {
			fixture.Policy.Generation++
			fixture.Policy.Spec.KubernetesAccess.ClusterRoleBindings = fixture.Policy.Spec.KubernetesAccess.ClusterRoleBindings[:1]
		}
		record, err := sandboxbinding.Snapshot(fixture.Agent, fixture.Team, resources)
		if err != nil {
			t.Fatal(err)
		}
		fixture.Agent.Annotations[sandboxbinding.AuthorizationAnnotation] = record
		binding, err := sandboxbinding.AuthorizedBinding(fixture.Agent)
		if err != nil {
			t.Fatal(err)
		}
		resolution, err := sandbox.Resolve(sandbox.ResolveInput{
			Agent: fixture.Agent, Team: fixture.Team, Resources: resources, AuthorizedBinding: binding,
			Registrations: map[api.SandboxType]sandbox.Registration{api.SandboxKata: {Capabilities: caps, Available: true}},
		})
		if err != nil {
			t.Fatal(err)
		}
		plan, err := sandboxpolicy.Compile(sandboxpolicy.CompileInput{
			Resolution: *resolution, AgentName: fixture.Agent.Name, Namespace: fixture.Namespace.Name, Platform: platform,
		})
		if err != nil {
			t.Fatal(err)
		}
		fixture.Input.Resolution = resolution
		fixture.Input.Controls, err = sandboxworkload.ControlsFromPlan(*plan)
		if err != nil {
			t.Fatal(err)
		}
		bundle, err := sandboxworkload.Build(fixture.Input)
		if err != nil {
			t.Fatal(err)
		}
		input := sampleInput("openclaw", "openai", false)
		object(input["secrets"])["existingSecret"] = "runtime-secret"
		p, err := PrepareSandboxChart(testChart(t), input, nil, bundle)
		if err != nil {
			t.Fatal(err)
		}
		prepared = append(prepared, p)
	}
	old, err := sandboxStageObjects(prepared[0].manifest, sandboxSealIdentity(fixture.Input))
	if err != nil {
		t.Fatal(err)
	}
	next, err := sandboxStageObjects(prepared[1].manifest, sandboxSealIdentity(fixture.Input))
	if err != nil {
		t.Fatal(err)
	}
	for key, obj := range old {
		if _, retained := next[key]; !retained && obj.GetKind() == "ClusterRoleBinding" {
			return prepared[0], prepared[1], obj.DeepCopy()
		}
	}
	t.Fatal("compiler transition did not retire a ClusterRoleBinding")
	return nil, nil, nil
}

func TestSandboxSDKRetiredBindingReplacementIsNeverPruned(t *testing.T) {
	previous, next, retired := sandboxSDKKubernetesAccessPlans(t)
	cfg, api, store := sandboxSDKConfig()
	if err := stageSandboxTest(previous, cfg, sandboxStageCheck); err != nil {
		t.Fatal(err)
	}
	before := store.writes
	if err := stageSandboxTest(next, cfg, sandboxStageCheck); err == nil || store.writes != before {
		t.Fatal("unrevoked compiler grant was accepted")
	}
	path := sandboxTestPath(retired)
	delete(api.objects, path)
	replacement := retired.DeepCopy()
	replacement.SetUID("foreign-replacement")
	replacement.SetResourceVersion("foreign-version")
	replacement.SetOwnerReferences(nil)
	fired := false
	api.before = func(call sandboxSDKRequest) {
		if !fired && (call.method == http.MethodPatch || call.method == http.MethodPost) {
			fired = true
			api.objects[path] = replacement.DeepCopy()
		}
	}
	err := stageSandboxTest(next, cfg, sandboxStageCheck)
	if !fired {
		t.Fatal("retired-name replacement race was not exercised")
	}
	if got := api.objects[path]; got == nil || sandboxJSONHash(got) != sandboxJSONHash(replacement) {
		t.Fatal("Helm changed/deleted the foreign replacement of a journal-revoked grant")
	}
	for _, request := range api.requests {
		if request.method == http.MethodDelete {
			t.Fatalf("stage invoked implicit Helm pruning: %s", request.path)
		}
	}
	if err == nil {
		t.Fatal("retired inventory replacement escaped postconditions")
	}
	last, _ := cfg.Releases.Last(next.releaseName)
	if last.Info.Status != release.StatusFailed {
		t.Fatalf("retired-object race recorded %s instead of a failed held stage", last.Info.Status)
	}
	base, err := cfg.Releases.Get(previous.releaseName, 1)
	if err != nil || base.Info.Status != release.StatusDeployed {
		t.Fatalf("failed postconditions superseded the last deployed baseline: %v", err)
	}
	// Simulate external collision recovery, not a Helm deletion, then retry
	// the exact held plan against its still-deployed original inventory.
	api.before = nil
	delete(api.objects, path)
	requests := len(api.requests)
	if err := stageSandboxTest(next, cfg, sandboxStageCheck); err != nil {
		t.Fatalf("compiler grant retirement could not complete after recovery: %v", err)
	}
	last, err = cfg.Releases.Last(next.releaseName)
	if err != nil || last.Info.Status != release.StatusDeployed || last.Manifest != string(next.manifest) {
		t.Fatalf("recovered compiler transition was not durably deployed: %v", err)
	}
	for _, call := range api.requests[requests:] {
		if call.method == http.MethodDelete || call.path == path {
			if call.method != http.MethodGet {
				t.Fatalf("retired binding reached a write path during recovery: %s %s", call.method, call.path)
			}
		}
	}
}

func TestSandboxSDKRepairsLivePolicyDrift(t *testing.T) {
	p, _, _ := sandboxRender(t, "openclaw", true, false)
	cfg, api, store := sandboxSDKConfig()
	if err := stageSandboxTest(p, cfg, sandboxStageCheck); err != nil {
		t.Fatal(err)
	}
	addSandboxTestAPIDefaults(t, &sandboxActionKube{objects: api.objects})
	policy := api.object(t, "CiliumNetworkPolicy")
	path, uid := sandboxTestPath(policy), policy.GetUID()
	if err := unstructured.SetNestedField(policy.Object, false, "spec", "enableDefaultDeny", "egress"); err != nil {
		t.Fatal(err)
	}
	policy.SetResourceVersion("drift-version")
	if err := stageSandboxTest(p, cfg, sandboxStageCheck); err != nil {
		t.Fatal(err)
	}
	policy = api.objects[path]
	egress, _, err := unstructured.NestedBool(policy.Object, "spec", "enableDefaultDeny", "egress")
	if err != nil || !egress || policy.GetUID() != uid {
		t.Fatalf("successful Helm stage left live CNP drift or recreated the policy: egress=%v uid=%s err=%v", egress, policy.GetUID(), err)
	}
	patched := false
	for _, call := range api.requests {
		if call.method != http.MethodPatch {
			continue
		}
		var patch unstructured.Unstructured
		if err := json.Unmarshal(call.body, &patch.Object); err != nil {
			t.Fatal(err)
		}
		if patch.GetUID() == "" || patch.GetResourceVersion() == "" {
			t.Fatalf("unconditional %s write: %s", call.path, call.body)
		}
		if call.path == path {
			patched = true
			if call.contentType != string(types.MergePatchType) || patch.GetUID() != uid || patch.GetResourceVersion() != "drift-version" {
				t.Fatalf("policy repair lost its observed identity: %s", call.body)
			}
		}
	}
	if !patched {
		t.Fatal("live CNP drift did not reach the real HTTP patch boundary")
	}
	service := api.object(t, "Service")
	if address, _, _ := unstructured.NestedString(service.Object, "spec", "clusterIP"); address != "10.96.0.20" {
		t.Fatal("live-aware reconciliation removed the allocated Service address")
	}
	writes, requests := store.writes, len(api.requests)
	if err := stageSandboxTest(p, cfg, sandboxStageCheck); err != nil {
		t.Fatal(err)
	}
	if store.writes != writes {
		t.Fatal("converged, defaulted resources produced another Helm revision")
	}
	for _, call := range api.requests[requests:] {
		if call.method != http.MethodGet {
			t.Fatal("converged stage was not read-only")
		}
	}
}

func TestSandboxSDKRefusesPolicyWriteRaces(t *testing.T) {
	for _, mode := range []string{"replacement", "concurrent change", "disappeared"} {
		t.Run(mode, func(t *testing.T) {
			p, _, _ := sandboxRender(t, "openclaw", true, false)
			cfg, api, _ := sandboxSDKConfig()
			if err := stageSandboxTest(p, cfg, sandboxStageCheck); err != nil {
				t.Fatal(err)
			}
			policy := api.object(t, "CiliumNetworkPolicy")
			path := sandboxTestPath(policy)
			object(object(policy.Object["spec"])["enableDefaultDeny"])["egress"] = false
			policy.SetResourceVersion("drift-version")
			fired := false
			var changed *unstructured.Unstructured
			api.before = func(call sandboxSDKRequest) {
				if fired || call.method != http.MethodPatch || call.path != path {
					return
				}
				fired = true
				changed = api.objects[path].DeepCopy()
				switch mode {
				case "replacement":
					changed.SetUID("foreign-replacement")
				case "concurrent change":
					changed.SetResourceVersion("concurrent-version")
					changed.SetAnnotations(map[string]string{"example.com/concurrent": "must survive"})
				case "disappeared":
					delete(api.objects, path)
					return
				}
				api.objects[path] = changed
			}
			err := stageSandboxTest(p, cfg, sandboxStageCheck)
			if !fired || err == nil {
				t.Fatalf("write race was not refused: fired=%v err=%v", fired, err)
			}
			if mode == "disappeared" {
				if api.objects[path] != nil {
					t.Fatal("disappeared policy was recreated")
				}
			} else if sandboxJSONHash(api.objects[path]) != sandboxJSONHash(changed) {
				t.Fatal("conditional patch overwrote a replacement or concurrent change")
			}
			last, _ := cfg.Releases.Last(p.releaseName)
			if last.Info.Status != release.StatusFailed {
				t.Fatalf("conflict recorded as %s", last.Info.Status)
			}
		})
	}
}

func TestSandboxSDKRejectsFalseSuccessAndUnownedFields(t *testing.T) {
	for _, mode := range []string{"install lost policy content", "upgrade lost policy content", "unowned policy field"} {
		t.Run(mode, func(t *testing.T) {
			p, _, _ := sandboxRender(t, "openclaw", true, false)
			cfg, api, _ := sandboxSDKConfig()
			if !strings.HasPrefix(mode, "install") {
				if err := stageSandboxTest(p, cfg, sandboxStageCheck); err != nil {
					t.Fatal(err)
				}
				policy := api.object(t, "CiliumNetworkPolicy")
				object(object(policy.Object["spec"])["enableDefaultDeny"])["egress"] = false
				policy.SetResourceVersion("drift-version")
				if mode == "unowned policy field" {
					policy.Object["unapprovedField"] = "must-not-be-blindly-erased"
				}
			}
			if mode != "unowned policy field" {
				api.stored = func(obj *unstructured.Unstructured) {
					if obj.GetKind() == "CiliumNetworkPolicy" {
						object(object(obj.Object["spec"])["enableDefaultDeny"])["egress"] = false
					}
				}
			}
			if err := stageSandboxTest(p, cfg, sandboxStageCheck); err == nil {
				t.Fatal("HTTP success and valid metadata were mistaken for converged live content")
			}
			last, _ := cfg.Releases.Last(p.releaseName)
			if last.Info.Status != release.StatusFailed {
				t.Fatalf("nonconverged stage was durably marked %s", last.Info.Status)
			}
			if mode == "unowned policy field" && api.object(t, "CiliumNetworkPolicy").Object["unapprovedField"] != "must-not-be-blindly-erased" {
				t.Fatal("stage blindly erased an unowned field to force convergence")
			}
		})
	}
}
