// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandbox"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxbinding"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxpolicy"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload/testfixture"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type prerequisitesAPIReader struct {
	client.Reader
	afterGet            func(client.ObjectKey, client.Object) error
	reads               map[client.ObjectKey]int
	secretMetadataReads int
	lists               int
}

func (r *prerequisitesAPIReader) Get(ctx context.Context, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
	r.reads[key]++
	if _, fullSecret := object.(*corev1.Secret); fullSecret {
		return fmt.Errorf("prerequisite selector attempted to read Secret contents")
	}
	if metadata, ok := object.(*metav1.PartialObjectMetadata); ok && metadata.Kind == "Secret" {
		if metadata.APIVersion != "v1" {
			return fmt.Errorf("unexpected Secret metadata API version")
		}
		r.secretMetadataReads++
	}
	if err := r.Reader.Get(ctx, key, object, opts...); err != nil {
		return err
	}
	if r.afterGet != nil {
		return r.afterGet(key, object)
	}
	return nil
}

func (r *prerequisitesAPIReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	r.lists++
	return fmt.Errorf("prerequisite selector must not discover resources by listing")
}

type kubernetesPrerequisitesFixture struct {
	fixture testfixture.Fixture
	client  client.Client
	reader  *prerequisitesAPIReader
	source  *controller.KubernetesSandboxPrerequisites
	objects map[string]client.Object
	now     time.Time
}

func newKubernetesPrerequisitesFixture(t *testing.T, typ string) *kubernetesPrerequisitesFixture {
	t.Helper()
	input := testfixture.New(typ)
	input.Agent.Finalizers = []string{sandbox.HoldFinalizer}
	input.Input.Skills.Spec = corev1.PersistentVolumeClaimSpec{
		AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		StorageClassName: ptr.To("published-skills"),
		Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceStorage: resource.MustParse("1Gi"),
		}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: input.Agent.Spec.GatewayTokenSecretRef, Namespace: input.Namespace.Name,
			UID: "secret-uid", ResourceVersion: "1",
			OwnerReferences: []metav1.OwnerReference{sandboxworkload.AgentOwner(input.Agent.Name, input.Agent.UID)},
		},
		Data: map[string][]byte{"TEST_ONLY_KEY": []byte("synthetic-content-must-not-be-read")},
	}
	objects := map[string]client.Object{
		"agent": input.Agent, "team": input.Team, "sandbox": input.Sandbox,
		"namespace": input.Namespace, "home": input.Input.Home, "skills": input.Input.Skills, "secret": secret,
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithStatusSubresource(&api.Agent{}).
		WithObjects(input.Agent, input.Team, input.Sandbox, input.Namespace, input.Input.Home, input.Input.Skills, secret).Build()
	reader := &prerequisitesAPIReader{Reader: c, reads: map[client.ObjectKey]int{}}
	f := &kubernetesPrerequisitesFixture{
		fixture: input, client: c, reader: reader, objects: objects,
		now: time.Date(2026, time.October, 2, 1, 0, 0, 0, time.UTC),
	}
	f.source = &controller.KubernetesSandboxPrerequisites{
		APIReader: reader, HomeClaimName: input.Input.Home.Name, SkillsClaimName: input.Input.Skills.Name,
		ChartVersion: "0.1.0", Helpers: input.Input.Helpers, Now: func() time.Time { return f.now },
	}
	return f
}

func (f *kubernetesPrerequisitesFixture) agent(t *testing.T) *api.Agent {
	t.Helper()
	agent := &api.Agent{}
	if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.fixture.Agent), agent); err != nil {
		t.Fatal(err)
	}
	return agent
}

func (f *kubernetesPrerequisitesFixture) resolve(t *testing.T) (controller.SandboxPrerequisites, error) {
	t.Helper()
	team := &api.Team{}
	if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.fixture.Team), team); err != nil {
		t.Fatal(err)
	}
	return f.source.Resolve(context.Background(), f.agent(t), team)
}

func (f *kubernetesPrerequisitesFixture) editAgent(t *testing.T, mutate func(*api.Agent)) {
	t.Helper()
	agent := f.agent(t)
	mutate(agent)
	if err := f.client.Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
}

func assertPrerequisiteRejection(t *testing.T, got controller.SandboxPrerequisites, err error) {
	t.Helper()
	if err == nil || !reflect.DeepEqual(got, controller.SandboxPrerequisites{}) {
		t.Fatalf("failed selection returned usable prerequisites: %+v, %v", got, err)
	}
}

func TestKubernetesSandboxPrerequisitesNativeEmptySkillsSelection(t *testing.T) {
	for _, tc := range []struct {
		name                                   string
		native, claim, omitName, sharedStorage bool
		base, add, remove                      []string
		catalog                                string
		wantEmpty, wantError                   bool
	}{
		{name: "native absent selection", native: true, wantEmpty: true},
		{name: "native explicit empty selection", native: true, base: []string{}, add: []string{}, wantEmpty: true},
		{name: "native omitted claim name", native: true, omitName: true, wantEmpty: true},
		{name: "native ignores unused existing claim", native: true, claim: true, wantEmpty: true},
		{name: "remove all effective skills", native: true, base: []string{"example-skill"}, add: []string{"npm:example-skill"}, remove: []string{"example-skill", "npm:example-skill"}, wantEmpty: true},
		{name: "remove wins over add", native: true, add: []string{"example-skill"}, remove: []string{"example-skill"}, wantEmpty: true},
		{name: "remaining team skill", native: true, base: []string{"example-skill"}, wantError: true},
		{name: "remaining added skill", native: true, add: []string{"example-skill"}, wantError: true},
		{name: "removal leaves another skill", native: true, base: []string{"example-skill", "another-skill"}, remove: []string{"example-skill"}, wantError: true},
		{name: "selected skill without claim name", native: true, omitName: true, add: []string{"example-skill"}, wantError: true},
		{name: "selected skill retains existing claim", native: true, claim: true, base: []string{"example-skill"}},
		{name: "catalog does not require empty Team publication", native: true, catalog: "skills-catalog", wantEmpty: true},
		{name: "catalog ignores unused existing claim", native: true, claim: true, catalog: "skills-catalog", wantEmpty: true},
		{name: "shared storage still requires claim", native: true, sharedStorage: true, wantError: true},
		{name: "shared storage retains claim", native: true, sharedStorage: true, claim: true},
		{name: "default empty selection requires claim", wantError: true},
		{name: "default omitted name rejected", omitName: true, wantError: true},
		{name: "default empty selection retains claim", claim: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKubernetesPrerequisitesFixture(t, "openclaw")
			f.source.NativeExecution, f.source.SkillsCatalogConfigMapName = tc.native, tc.catalog
			if tc.omitName {
				f.source.SkillsClaimName = ""
			}
			team := f.fixture.Team.DeepCopy()
			team.Spec.Skills = tc.base
			if tc.sharedStorage {
				team.Spec.SkillStorage = &api.SkillStorageSpec{ResourceGroup: "example", StorageAccount: "exampleskills", ContainerName: "skills"}
			}
			if err := f.client.Update(context.Background(), team); err != nil {
				t.Fatal(err)
			}
			f.editAgent(t, func(agent *api.Agent) {
				agent.Spec.SkillOverrides.Add, agent.Spec.SkillOverrides.Remove = tc.add, tc.remove
			})
			if !tc.claim {
				if err := f.client.Delete(context.Background(), f.fixture.Input.Skills); err != nil {
					t.Fatal(err)
				}
			}
			got, err := f.resolve(t)
			if tc.wantError {
				assertPrerequisiteRejection(t, got, err)
				if !strings.Contains(err.Error(), "skills") {
					t.Fatal("missing skills source error was not explicit:", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantEmpty {
				if got.SkillsMode != sandboxworkload.SkillsModeEmpty || got.Skills != (sandboxworkload.ObjectIdentity{}) ||
					len(got.PublicationSources) != 0 || f.reader.reads[client.ObjectKeyFromObject(f.fixture.Input.Skills)] != 0 {
					t.Fatal("empty selection read or invented a skills PVC/publication")
				}
			} else if got.SkillsMode != "" || got.Skills.UID != f.fixture.Input.Skills.UID {
				t.Fatal("existing skills PVC path changed")
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), `"SkillsMode"`) != tc.wantEmpty {
				t.Fatal("default prerequisite serialized shape changed")
			}
		})
	}
}

func TestKubernetesSandboxPrerequisitesAuthoritativeSelection(t *testing.T) {
	for _, typ := range []string{"openclaw", "hermes"} {
		t.Run(typ, func(t *testing.T) {
			f := newKubernetesPrerequisitesFixture(t, typ)
			f.editAgent(t, func(agent *api.Agent) {
				agent.Spec.User.Alias = "not-the-agent-name"
				agent.Spec.Helm.Version = f.source.ChartVersion
				agent.Spec.Helm.Values = map[string]runtime.RawExtension{
					"persistence": {Raw: []byte(`{"existingClaim":"tenant-home"}`)},
					"skills":      {Raw: []byte(`{"existingClaim":"tenant-skills"}`)},
					"secrets":     {Raw: []byte(`{"existingSecret":"tenant-secret"}`)},
					"logger":      {Raw: []byte(`{"enabled":false}`)},
				}
			})
			started := f.now
			f.reader.afterGet = func(_ client.ObjectKey, _ client.Object) error {
				f.now = f.now.Add(time.Millisecond)
				return nil
			}
			got, err := f.resolve(t)
			if err != nil {
				t.Fatal(err)
			}
			if got.AgentUID != f.fixture.Agent.UID || got.AgentGeneration != f.fixture.Agent.Generation ||
				got.NamespaceUID != f.fixture.Namespace.UID ||
				got.Home != (sandboxworkload.ObjectIdentity{Name: f.source.HomeClaimName, UID: f.fixture.Input.Home.UID}) ||
				got.Skills != (sandboxworkload.ObjectIdentity{Name: f.source.SkillsClaimName, UID: f.fixture.Input.Skills.UID}) ||
				got.RuntimeSecret != (sandboxworkload.ObjectIdentity{Name: f.fixture.Agent.Spec.GatewayTokenSecretRef, UID: "secret-uid"}) ||
				got.ChartVersion != f.source.ChartVersion || got.Helpers != f.source.Helpers ||
				len(got.Revision) != 64 || !got.ObservedAt.Equal(started) {
				t.Fatalf("selection did not bind trusted settings and authoritative identities: %+v", got)
			}
			if f.reader.secretMetadataReads != 2 || f.reader.lists != 0 || len(f.reader.reads) != len(f.objects) {
				t.Fatalf("unexpected reads: metadata secrets=%d, lists=%d, identities=%v",
					f.reader.secretMetadataReads, f.reader.lists, f.reader.reads)
			}
			for _, object := range f.objects {
				if reads := f.reader.reads[client.ObjectKeyFromObject(object)]; reads != 2 {
					t.Fatalf("%s was not captured and rechecked: %d reads", object.GetName(), reads)
				}
			}
		})
	}
}

func TestKubernetesSandboxPrerequisitesRejectsInvalidConfigurationAndIntent(t *testing.T) {
	for name, mutate := range map[string]func(*kubernetesPrerequisitesFixture){
		"missing reader":           func(f *kubernetesPrerequisitesFixture) { f.source.APIReader = nil },
		"missing HOME":             func(f *kubernetesPrerequisitesFixture) { f.source.HomeClaimName = "" },
		"template HOME":            func(f *kubernetesPrerequisitesFixture) { f.source.HomeClaimName = "{{.Agent.Name}}" },
		"foreign skills namespace": func(f *kubernetesPrerequisitesFixture) { f.source.SkillsClaimName = "other/skills" },
		"same claims":              func(f *kubernetesPrerequisitesFixture) { f.source.SkillsClaimName = f.source.HomeClaimName },
		"missing chart":            func(f *kubernetesPrerequisitesFixture) { f.source.ChartVersion = "" },
		"chart range":              func(f *kubernetesPrerequisitesFixture) { f.source.ChartVersion = ">=0.1.0" },
		"missing clock":            func(f *kubernetesPrerequisitesFixture) { f.source.Now = func() time.Time { return time.Time{} } },
	} {
		t.Run(name, func(t *testing.T) {
			f := newKubernetesPrerequisitesFixture(t, "hermes")
			mutate(f)
			got, err := f.resolve(t)
			assertPrerequisiteRejection(t, got, err)
			if !errors.Is(err, sandboxpolicy.ErrUnavailable) {
				t.Fatalf("invalid startup settings did not report unavailability: %v", err)
			}
		})
	}
	for name, mutate := range map[string]func(*api.Agent){
		"not held":               func(a *api.Agent) { a.Finalizers = nil },
		"suspended":              func(a *api.Agent) { a.Spec.Suspended = true },
		"Custom":                 func(a *api.Agent) { a.Spec.Agent.Type = api.AgentTypeCustom },
		"tailscale":              func(a *api.Agent) { a.Spec.Tailscale = &api.TailscaleSpec{} },
		"identity":               func(a *api.Agent) { a.Spec.AgentIdentity = &api.AgentIdentitySpec{Enabled: true} },
		"missing authorization":  func(a *api.Agent) { a.Annotations = nil },
		"unapproved chart":       func(a *api.Agent) { a.Spec.Helm.Version = "0.2.0" },
		"missing runtime Secret": func(a *api.Agent) { a.Spec.GatewayTokenSecretRef = "" },
		"foreign runtime Secret": func(a *api.Agent) { a.Spec.GatewayTokenSecretRef = "other/secret" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newKubernetesPrerequisitesFixture(t, "hermes")
			f.editAgent(t, mutate)
			got, err := f.resolve(t)
			assertPrerequisiteRejection(t, got, err)
		})
	}
	t.Run("Hermes doctor", func(t *testing.T) {
		f := newKubernetesPrerequisitesFixture(t, "hermes")
		f.source.Helpers.Doctor = true
		got, err := f.resolve(t)
		assertPrerequisiteRejection(t, got, err)
		if !errors.Is(err, sandboxpolicy.ErrUnsupportedControl) {
			t.Fatal(err)
		}
	})
	for _, target := range []string{"agent", "team"} {
		for _, field := range []string{"nil", "UID", "generation", "intent", "resourceVersion", "deletion"} {
			if target == "agent" && field == "resourceVersion" {
				continue // Agent status-only resource versions do not change intent.
			}
			t.Run("stale requested "+target+"/"+field, func(t *testing.T) {
				f := newKubernetesPrerequisitesFixture(t, "hermes")
				agent, team := f.agent(t), f.fixture.Team.DeepCopy()
				var object client.Object = agent
				if target == "team" {
					object = team
				}
				switch field {
				case "nil":
					if target == "agent" {
						agent = nil
					} else {
						team = nil
					}
				case "UID":
					object.SetUID("stale-uid")
				case "generation":
					object.SetGeneration(object.GetGeneration() + 1)
				case "resourceVersion":
					object.SetResourceVersion("stale-version")
				case "deletion":
					now := metav1.NewTime(time.Now())
					object.SetDeletionTimestamp(&now)
				case "intent":
					if target == "agent" {
						agent.Spec.User.DisplayName = "Stale intent"
					} else {
						team.Spec.DefaultSandboxRef = "stale-selection"
					}
				}
				got, err := f.source.Resolve(context.Background(), agent, team)
				assertPrerequisiteRejection(t, got, err)
			})
		}
	}
}

func TestKubernetesSandboxPrerequisitesRejectsMissingOrUnpersistedResources(t *testing.T) {
	for _, target := range []string{"agent", "team", "sandbox", "namespace", "home", "skills", "secret"} {
		for _, change := range []string{"missing", "UID", "resourceVersion", "deleting", "wrong name", "wrong namespace"} {
			t.Run(target+"/"+change, func(t *testing.T) {
				f := newKubernetesPrerequisitesFixture(t, "hermes")
				key := client.ObjectKeyFromObject(f.objects[target])
				f.reader.afterGet = func(actual client.ObjectKey, object client.Object) error {
					if actual != key {
						return nil
					}
					switch change {
					case "missing":
						return apierrors.NewNotFound(schema.GroupResource{Resource: target}, key.Name)
					case "UID":
						object.SetUID("")
					case "resourceVersion":
						object.SetResourceVersion("")
					case "deleting":
						now := metav1.NewTime(f.now)
						object.SetDeletionTimestamp(&now)
					case "wrong name":
						object.SetName("foreign-name")
					case "wrong namespace":
						object.SetNamespace("foreign-namespace")
					}
					return nil
				}
				got, err := f.resolve(t)
				assertPrerequisiteRejection(t, got, err)
				if change == "missing" && !apierrors.IsNotFound(err) {
					t.Fatalf("lost API not-found error: %v", err)
				}
			})
		}
	}
}

func TestKubernetesSandboxPrerequisitesRejectsForeignOwnershipAndStorage(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		mutate       func(client.Object)
		storageError bool
	}{
		{"Team replacement", "team", func(o client.Object) { o.SetUID("replacement-team") }, false},
		{"Sandbox replacement", "sandbox", func(o client.Object) { o.SetUID("replacement-sandbox") }, false},
		{"Team removed Sandbox", "team", func(o client.Object) { o.(*api.Team).Spec.SandboxRefs = nil }, false},
		{"unprotected namespace", "namespace", func(o client.Object) { o.SetLabels(nil) }, false},
		{"namespace without markers", "namespace", func(o client.Object) { o.SetAnnotations(nil) }, false},
		{"namespace wrong Agent name", "namespace", func(o client.Object) { o.GetAnnotations()[sandboxworkload.AgentNameAnnotation] = "other" }, false},
		{"namespace wrong Agent UID", "namespace", func(o client.Object) { o.GetAnnotations()[sandboxworkload.AgentUIDAnnotation] = "old-uid" }, false},
		{"namespace without owner", "namespace", func(o client.Object) { o.SetOwnerReferences(nil) }, false},
		{"namespace old owner", "namespace", func(o client.Object) { o.GetOwnerReferences()[0].UID = "old-uid" }, false},
		{"namespace incomplete owner", "namespace", func(o client.Object) { o.GetOwnerReferences()[0].BlockOwnerDeletion = nil }, false},
		{"HOME without Agent marker", "home", func(o client.Object) { o.SetAnnotations(nil) }, false},
		{"HOME stale marker", "home", func(o client.Object) { o.GetAnnotations()[sandboxworkload.AgentUIDAnnotation] = "old-uid" }, false},
		{"HOME workload lifetime", "home", func(o client.Object) {
			o.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "example", UID: "workload"}})
		}, false},
		{"HOME stale owner", "home", func(o client.Object) {
			o.SetOwnerReferences([]metav1.OwnerReference{sandboxworkload.AgentOwner("example", "old-uid")})
		}, false},
		{"HOME lost", "home", func(o client.Object) { o.(*corev1.PersistentVolumeClaim).Status.Phase = corev1.ClaimLost }, false},
		{"HOME class", "home", func(o client.Object) { o.(*corev1.PersistentVolumeClaim).Spec.StorageClassName = ptr.To("foreign") }, true},
		{"HOME resize", "home", func(o client.Object) {
			o.(*corev1.PersistentVolumeClaim).Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("11Gi")
		}, true},
		{"HOME RWX", "home", func(o client.Object) {
			o.(*corev1.PersistentVolumeClaim).Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
		}, true},
		{"HOME extra access modes", "home", func(o client.Object) {
			o.(*corev1.PersistentVolumeClaim).Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadOnlyMany}
		}, true},
		{"HOME block", "home", func(o client.Object) {
			o.(*corev1.PersistentVolumeClaim).Spec.VolumeMode = ptr.To(corev1.PersistentVolumeBlock)
		}, true},
		{"skills lost", "skills", func(o client.Object) { o.(*corev1.PersistentVolumeClaim).Status.Phase = corev1.ClaimLost }, false},
		{"skills block", "skills", func(o client.Object) {
			o.(*corev1.PersistentVolumeClaim).Spec.VolumeMode = ptr.To(corev1.PersistentVolumeBlock)
		}, false},
		{"skills HOME alias", "skills", func(o client.Object) { o.SetUID("home-uid") }, false},
		{"skills stale owner", "skills", func(o client.Object) {
			o.SetOwnerReferences([]metav1.OwnerReference{sandboxworkload.AgentOwner("example", "old-uid")})
		}, false},
		{"skills stale Team", "skills", func(o client.Object) {
			o.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "Team", Name: "team-example", UID: "old-team"}})
		}, false},
		{"skills foreign marker", "skills", func(o client.Object) {
			o.SetAnnotations(map[string]string{sandboxworkload.AgentUIDAnnotation: "foreign"})
		}, false},
		{"Secret without owner", "secret", func(o client.Object) { o.SetOwnerReferences(nil) }, false},
		{"Secret stale owner", "secret", func(o client.Object) { o.GetOwnerReferences()[0].UID = "old-uid" }, false},
		{"Secret foreign name", "secret", func(o client.Object) { o.GetOwnerReferences()[0].Name = "other" }, false},
		{"Secret conflicting release", "secret", func(o client.Object) {
			o.SetAnnotations(map[string]string{"meta.helm.sh/release-name": "other-release"})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newKubernetesPrerequisitesFixture(t, "hermes")
			key := client.ObjectKeyFromObject(f.objects[tc.target])
			f.reader.afterGet = func(actual client.ObjectKey, object client.Object) error {
				if actual == key {
					tc.mutate(object)
				}
				return nil
			}
			got, err := f.resolve(t)
			assertPrerequisiteRejection(t, got, err)
			if tc.storageError && !errors.Is(err, sandboxworkload.ErrStorageTransitionRequired) {
				t.Fatalf("HOME transition lost its classification: %v", err)
			}
		})
	}
}

func TestKubernetesSandboxPrerequisitesPreservesSupportedStorageShapes(t *testing.T) {
	for _, mode := range []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteOncePod} {
		for _, owner := range []string{"retained", "Agent", "Team"} {
			t.Run(string(mode)+"/"+owner, func(t *testing.T) {
				f := newKubernetesPrerequisitesFixture(t, "hermes")
				box := f.fixture.Sandbox.DeepCopy()
				box.Spec.Storage.Home.AccessMode = mode
				if err := f.client.Update(context.Background(), box); err != nil {
					t.Fatal(err)
				}
				home := f.fixture.Input.Home.DeepCopy()
				home.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{mode}
				if owner != "retained" {
					home.OwnerReferences = []metav1.OwnerReference{sandboxworkload.AgentOwner(f.fixture.Agent.Name, f.fixture.Agent.UID)}
				}
				if err := f.client.Update(context.Background(), home); err != nil {
					t.Fatal(err)
				}
				skills := f.fixture.Input.Skills.DeepCopy()
				switch owner {
				case "Agent":
					skills.OwnerReferences = []metav1.OwnerReference{sandboxworkload.AgentOwner(f.fixture.Agent.Name, f.fixture.Agent.UID)}
				case "Team":
					skills.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(f.fixture.Team, api.GroupVersion.WithKind("Team"))}
				}
				if err := f.client.Update(context.Background(), skills); err != nil {
					t.Fatal(err)
				}
				if _, err := f.resolve(t); err != nil {
					t.Fatalf("retained/pending HOME or shared RWX skills were rejected: %v", err)
				}
			})
		}
	}
}

func TestKubernetesPrerequisiteRevisionIgnoresUnselectedTeamChanges(t *testing.T) {
	ctx := context.Background()
	f := newKubernetesPrerequisitesFixture(t, "hermes")
	first, err := f.resolve(t)
	if err != nil {
		t.Fatal(err)
	}
	team := &api.Team{}
	if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Team), team); err != nil {
		t.Fatal(err)
	}
	team.Spec.SandboxRefs = append(team.Spec.SandboxRefs, "spare-sandbox")
	team.Spec.DefaultSandboxRef = "spare-sandbox"
	if err := f.client.Update(ctx, team); err != nil {
		t.Fatal(err)
	}
	next, err := f.resolve(t)
	if err != nil || next.Revision != first.Revision {
		t.Fatal("unselected Team changes invalidated whole-generation effective approval:", err)
	}
	team.Spec.AgentPolicyRef = "new-policy"
	if err := f.client.Update(ctx, team); err != nil {
		t.Fatal(err)
	}
	next, err = f.resolve(t)
	if err != nil || next.Revision == first.Revision {
		t.Fatal("Team policy change did not invalidate prerequisite approval:", err)
	}
	previous := next.Revision
	team.Spec.MemberContext.Role = "changed-shared-input"
	if err := f.client.Update(ctx, team); err != nil {
		t.Fatal(err)
	}
	next, err = f.resolve(t)
	if err != nil || next.Revision == previous {
		t.Fatal("shared Team runtime input did not invalidate prerequisite approval:", err)
	}
}

func TestKubernetesSandboxPrerequisitesRevisionStabilityAndInvalidation(t *testing.T) {
	f := newKubernetesPrerequisitesFixture(t, "hermes")
	before, err := f.resolve(t)
	if err != nil {
		t.Fatal(err)
	}
	staleStatusAgent := f.agent(t)
	agent := staleStatusAgent.DeepCopy()
	agent.Status.Phase = api.PhasePending
	if err := f.client.Status().Update(context.Background(), agent); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	after, err := f.source.Resolve(context.Background(), staleStatusAgent, f.fixture.Team)
	if err != nil || after.Revision != before.Revision || after.ObservedAt.Equal(before.ObservedAt) {
		t.Fatalf("status churn or time invalidated unchanged prerequisites: %+v, %v", after, err)
	}
	f.editAgent(t, func(a *api.Agent) {
		a.Finalizers = append(a.Finalizers, sandbox.PreparationFinalizer, "example.com/unrelated")
		a.Annotations["example.com/unrelated"] = "metadata"
	})
	after, err = f.resolve(t)
	if err != nil || after.Revision != before.Revision {
		t.Fatalf("preparation bookkeeping invalidated its own inputs: %+v, %v", after, err)
	}
	for _, target := range []string{"team", "sandbox", "namespace", "home", "skills", "secret"} {
		for _, field := range []string{"resourceVersion", "UID"} {
			if field == "UID" && (target == "team" || target == "sandbox") {
				continue // UID replacements require explicit binding reauthorization.
			}
			t.Run(target+"/"+field, func(t *testing.T) {
				f := newKubernetesPrerequisitesFixture(t, "hermes")
				first, err := f.resolve(t)
				if err != nil {
					t.Fatal(err)
				}
				key := client.ObjectKeyFromObject(f.objects[target])
				f.reader.afterGet = func(actual client.ObjectKey, object client.Object) error {
					if actual == key {
						if field == "UID" {
							object.SetUID("replacement-uid")
						} else {
							object.SetResourceVersion("next-version")
						}
					}
					return nil
				}
				team := f.fixture.Team.DeepCopy()
				if target == "team" {
					team.ResourceVersion = "next-version"
				}
				second, err := f.source.Resolve(context.Background(), f.agent(t), team)
				mustChange := field == "UID" || target == "secret"
				if err != nil || (second.Revision != first.Revision) != mustChange {
					t.Fatalf("revision did not distinguish effective inputs from observation metadata: %+v, %v", second, err)
				}
				third, err := f.source.Resolve(context.Background(), f.agent(t), team)
				if err != nil || third.Revision != second.Revision {
					t.Fatalf("unchanged captured identity is not stable: %+v, %v", third, err)
				}
			})
		}
	}
	for _, change := range []string{"generation", "intent", "reauthorization", "chart", "helpers", "HOME name", "skills name"} {
		t.Run(change, func(t *testing.T) {
			f := newKubernetesPrerequisitesFixture(t, "hermes")
			first, err := f.resolve(t)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "generation":
				f.editAgent(t, func(a *api.Agent) { a.Generation++ })
			case "intent":
				f.editAgent(t, func(a *api.Agent) { a.Spec.User.DisplayName = "New intent" })
			case "reauthorization":
				binding, err := sandboxbinding.AuthorizedBinding(f.agent(t))
				if err != nil {
					t.Fatal(err)
				}
				request, err := sandboxbinding.FormatReauthorization("new-test-request", binding)
				if err != nil {
					t.Fatal(err)
				}
				f.editAgent(t, func(a *api.Agent) { a.Annotations[sandboxbinding.ReauthorizeAnnotation] = request })
			case "chart":
				f.source.ChartVersion = "0.2.0"
			case "helpers":
				f.source.Helpers.Logger = !f.source.Helpers.Logger
			case "HOME name", "skills name":
				claim := f.fixture.Input.Home.DeepCopy()
				if change == "skills name" {
					claim = f.fixture.Input.Skills.DeepCopy()
				}
				claim.Name, claim.UID, claim.ResourceVersion = "another-configured-claim", "another-claim-uid", ""
				if err := f.client.Create(context.Background(), claim); err != nil {
					t.Fatal(err)
				}
				if change == "HOME name" {
					f.source.HomeClaimName = claim.Name
				} else {
					f.source.SkillsClaimName = claim.Name
				}
			}
			second, err := f.resolve(t)
			if err != nil || second.Revision == first.Revision {
				t.Fatalf("changed trusted inputs did not invalidate revision: %+v, %v", second, err)
			}
		})
	}
}

func TestKubernetesSandboxPrerequisitesRejectsConcurrentReadbackChanges(t *testing.T) {
	for _, target := range []string{"agent", "team", "sandbox", "namespace", "home", "skills", "secret"} {
		for _, change := range []string{"replacement", "update", "deleting", "read error"} {
			t.Run(target+"/"+change, func(t *testing.T) {
				f := newKubernetesPrerequisitesFixture(t, "hermes")
				key := client.ObjectKeyFromObject(f.objects[target])
				failure := errors.New("synthetic authoritative read failure")
				f.reader.afterGet = func(actual client.ObjectKey, object client.Object) error {
					if actual != key || f.reader.reads[key] != 2 {
						return nil
					}
					switch change {
					case "replacement":
						object.SetUID("replacement-with-the-same-resource-version")
					case "update":
						if target == "agent" {
							object.(*api.Agent).Finalizers = append(object.GetFinalizers(), sandbox.PreparationFinalizer)
						} else {
							object.SetResourceVersion("updated-version")
						}
					case "deleting":
						now := metav1.NewTime(f.now)
						object.SetDeletionTimestamp(&now)
					case "read error":
						return failure
					}
					return nil
				}
				got, err := f.resolve(t)
				assertPrerequisiteRejection(t, got, err)
				if f.reader.reads[key] != 2 || !strings.Contains(err.Error(), "recheck") {
					t.Fatalf("did not reject the readback race: %v", err)
				}
				if change == "read error" && !errors.Is(err, failure) {
					t.Fatalf("lost the underlying read error: %v", err)
				}
			})
		}
	}
	t.Run("cancellation after reads", func(t *testing.T) {
		f := newKubernetesPrerequisitesFixture(t, "hermes")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		key := client.ObjectKeyFromObject(f.objects["secret"])
		f.reader.afterGet = func(actual client.ObjectKey, _ client.Object) error {
			if actual == key && f.reader.reads[key] == 2 {
				cancel()
			}
			return nil
		}
		got, err := f.source.Resolve(ctx, f.agent(t), f.fixture.Team)
		assertPrerequisiteRejection(t, got, err)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lost cancellation: %v", err)
		}
	})
}

func TestKubernetesSandboxPrerequisitesReconciledTeamReadbackIgnoresCompatibilityOnly(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(*api.Team)
		accept bool
	}{
		{
			name: "compatibility", accept: true,
			mutate: func(team *api.Team) {
				team.ResourceVersion = "compatibility-status-version"
				team.Status.Compatibility = &api.TeamCompatibilityStatus{
					State: api.CompatibilityCompatible,
					Team:  api.CompatibilityReference{Name: team.Name, UID: string(team.UID), Generation: team.Generation},
				}
			},
		},
		{
			name: "spec", mutate: func(team *api.Team) { team.Spec.Skills = append(team.Spec.Skills, "npm:changed") },
		},
		{
			name: "generation", mutate: func(team *api.Team) { team.Generation++ },
		},
		{
			name: "UID", mutate: func(team *api.Team) { team.UID = "replacement-team-uid" },
		},
		{
			name: "deletion", mutate: func(team *api.Team) { now := metav1.NewTime(time.Now()); team.DeletionTimestamp = &now },
		},
		{
			name: "accepted status", mutate: func(team *api.Team) {
				team.Status.Conditions = append(team.Status.Conditions, metav1.Condition{Type: api.ConditionTeamAccepted, Status: metav1.ConditionFalse})
			},
		},
	} {
		t.Run(change.name, func(t *testing.T) {
			f := newKubernetesPrerequisitesFixture(t, "hermes")
			f.source.NativeExecution = true
			f.editAgent(t, func(agent *api.Agent) { agent.Spec.Helm.Version = f.source.ChartVersion })
			team := f.fixture.Team.DeepCopy()
			if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(team), team); err != nil {
				t.Fatal(err)
			}
			key := client.ObjectKeyFromObject(team)
			changed := false
			f.reader.afterGet = func(actual client.ObjectKey, object client.Object) error {
				if actual == key && f.reader.reads[key] == 2 {
					changed = true
					if change.name != "compatibility" {
						change.mutate(object.(*api.Team))
					} else {
						// The current common path reads a full Team while the baseline
						// raw-RV verifier reads only metadata. Both observe the version
						// change produced by a compatibility status update.
						object.SetResourceVersion("compatibility-status-version")
						if team, ok := object.(*api.Team); ok {
							team.Status.Compatibility = &api.TeamCompatibilityStatus{
								State: api.CompatibilityCompatible,
								Team:  api.CompatibilityReference{Name: team.Name, UID: string(team.UID), Generation: team.Generation},
							}
						}
					}
				}
				return nil
			}
			got, err := f.source.ResolveCurrent(context.Background(), f.agent(t), team)
			if !changed {
				t.Fatal("fixture did not update Team between capture and final readback")
			}
			if change.accept {
				if err != nil || got.Revision == "" {
					t.Fatalf("compatibility-only Team status update fenced common reconciliation: %+v, %v", got, err)
				}
				return
			}
			assertPrerequisiteRejection(t, got, err)
			if !strings.Contains(err.Error(), "Sandbox source identity changed") {
				t.Fatalf("meaningful Team update bypassed the source fence: %v", err)
			}
		})
	}
}

func TestKubernetesSandboxPrerequisitesReconciledTeamInitialReadIgnoresCompatibilityOnly(t *testing.T) {
	for _, change := range []struct {
		name   string
		mutate func(*api.Team)
		accept bool
	}{
		{
			name: "compatibility", accept: true,
			mutate: func(team *api.Team) {
				team.ResourceVersion = "compatibility-status-version"
				team.Status.Compatibility = &api.TeamCompatibilityStatus{
					State: api.CompatibilityCompatible,
					Team:  api.CompatibilityReference{Name: team.Name, UID: string(team.UID), Generation: team.Generation},
				}
			},
		},
		{"spec", func(team *api.Team) { team.Spec.Skills = append(team.Spec.Skills, "npm:changed") }, false},
		{"generation", func(team *api.Team) { team.Generation++ }, false},
		{"UID", func(team *api.Team) { team.UID = "replacement-team-uid" }, false},
		{"deletion", func(team *api.Team) { now := metav1.NewTime(time.Now()); team.DeletionTimestamp = &now }, false},
		{"accepted status", func(team *api.Team) {
			team.Status.Conditions = append(team.Status.Conditions, metav1.Condition{Type: api.ConditionTeamAccepted, Status: metav1.ConditionFalse})
		}, false},
	} {
		t.Run(change.name, func(t *testing.T) {
			f := newKubernetesPrerequisitesFixture(t, "hermes")
			f.source.NativeExecution = true
			f.editAgent(t, func(agent *api.Agent) { agent.Spec.Helm.Version = f.source.ChartVersion })
			team := f.fixture.Team.DeepCopy()
			if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(team), team); err != nil {
				t.Fatal(err)
			}
			key := client.ObjectKeyFromObject(team)
			changed := false
			f.reader.afterGet = func(actual client.ObjectKey, object client.Object) error {
				if actual == key && f.reader.reads[key] == 1 {
					changed = true
					change.mutate(object.(*api.Team))
				}
				return nil
			}
			got, err := f.source.ResolveCurrent(context.Background(), f.agent(t), team)
			if !changed {
				t.Fatal("fixture did not update Team at the initial authoritative read")
			}
			if change.accept {
				if err != nil || got.Revision == "" {
					t.Fatalf("compatibility-only Team status update fenced common reconciliation: %+v, %v", got, err)
				}
				return
			}
			assertPrerequisiteRejection(t, got, err)
			if !strings.Contains(err.Error(), "Sandbox source identity changed") {
				t.Fatalf("meaningful Team update bypassed the source fence: %v", err)
			}
		})
	}
}

// This records injected test calls only; it is not a fencing implementation.
type testSandboxWriterExclusion struct {
	calls         int
	agent         *api.Agent
	prerequisites controller.SandboxPrerequisites
	err           error
	check         func(context.Context, *api.Agent, controller.SandboxPrerequisites)
}

func (f *testSandboxWriterExclusion) CheckQuiesced(ctx context.Context, agent *api.Agent, prerequisites controller.SandboxPrerequisites) error {
	f.calls++
	f.agent, f.prerequisites = agent.DeepCopy(), prerequisites
	if f.check != nil {
		f.check(ctx, agent, prerequisites)
	}
	return f.err
}

func TestKubernetesSandboxPrerequisitesWriterExclusion(t *testing.T) {
	f := newKubernetesPrerequisitesFixture(t, "hermes")
	tuple, err := f.resolve(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.source.CheckQuiesced(context.Background(), f.agent(t), tuple); !errors.Is(err, sandbox.ErrObservationUnavailable) {
		t.Fatalf("Pod absence and valid PVCs were treated as fencing: %v", err)
	}
	var nilSource *controller.KubernetesSandboxPrerequisites
	if err := nilSource.CheckQuiesced(context.Background(), f.agent(t), tuple); !errors.Is(err, sandbox.ErrObservationUnavailable) {
		t.Fatalf("nil source did not fail closed: %v", err)
	}
	failure := errors.New("synthetic TEST-only fence failure")
	fence := &testSandboxWriterExclusion{err: failure}
	f.source.WriterExclusion = fence
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "test-context")
	fence.check = func(gotCtx context.Context, agent *api.Agent, got controller.SandboxPrerequisites) {
		if gotCtx != ctx || !reflect.DeepEqual(agent, f.agent(t)) || !reflect.DeepEqual(got, tuple) {
			t.Fatal("fencing received a different context, Agent or prerequisite tuple")
		}
		agent.Name = "test-provider-cannot-mutate-caller"
	}
	agent := f.agent(t)
	if err := f.source.CheckQuiesced(ctx, agent, tuple); err != failure || agent.Name != f.fixture.Agent.Name {
		t.Fatalf("fence error or caller ownership was lost: %v", err)
	}
	fence.check, fence.err = nil, nil
	for _, change := range []string{"nil Agent", "namespace", "name", "UID", "generation", "tuple UID", "tuple generation", "future generation", "namespace UID", "HOME name", "HOME UID"} {
		t.Run(change, func(t *testing.T) {
			agent, requested := f.agent(t), tuple
			switch change {
			case "nil Agent":
				agent = nil
			case "namespace":
				agent.Namespace = "foreign"
			case "name":
				agent.Name = "other/name"
			case "UID":
				agent.UID = ""
			case "generation":
				agent.Generation = 0
			case "tuple UID":
				requested.AgentUID = "other-agent"
			case "tuple generation":
				requested.AgentGeneration = 0
			case "future generation":
				requested.AgentGeneration = agent.Generation + 1
			case "namespace UID":
				requested.NamespaceUID = ""
			case "HOME name":
				requested.Home.Name = "foreign/home"
			case "HOME UID":
				requested.Home.UID = ""
			}
			calls := fence.calls
			err := f.source.CheckQuiesced(ctx, agent, requested)
			if !errors.Is(err, sandbox.ErrObservationUnavailable) || fence.calls != calls {
				t.Fatalf("invalid tuple reached the fence: calls=%d, %v", fence.calls, err)
			}
		})
	}
	agent = f.agent(t)
	agent.Generation += 5
	agent.Spec.Suspended, agent.Spec.Agent.Type = true, api.AgentTypeCustom
	agent.Spec.Team, agent.Spec.SandboxRef = "removed-team", "removed-sandbox"
	agent.Finalizers, agent.Annotations = nil, nil
	now := metav1.NewTime(f.now.Add(24 * time.Hour))
	agent.DeletionTimestamp = &now
	f.source.APIReader, f.source.HomeClaimName, f.source.SkillsClaimName, f.source.ChartVersion = nil, "", "", ""
	if err := f.source.CheckQuiesced(ctx, agent, tuple); err != nil || !reflect.DeepEqual(fence.prerequisites, tuple) ||
		!reflect.DeepEqual(fence.agent, agent) {
		t.Fatalf("historical cleanup reauthorized current preparation or changed the tuple: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	fence.check = func(context.Context, *api.Agent, controller.SandboxPrerequisites) { cancel() }
	if err := f.source.CheckQuiesced(canceled, agent, tuple); !errors.Is(err, context.Canceled) {
		t.Fatalf("successful test adapter hid cancellation: %v", err)
	}
	calls := fence.calls
	if err := f.source.CheckQuiesced(canceled, agent, tuple); !errors.Is(err, context.Canceled) || fence.calls != calls {
		t.Fatalf("already canceled check reached the adapter: %v", err)
	}
}

func installKubernetesPrerequisites(t *testing.T, f *preparationFixture, fence controller.SandboxWriterExclusionSource) *controller.KubernetesSandboxPrerequisites {
	t.Helper()
	secret := &corev1.Secret{}
	if err := f.client.Get(context.Background(), client.ObjectKey{
		Namespace: f.fixture.Namespace.Name, Name: f.fixture.Agent.Spec.GatewayTokenSecretRef,
	}, secret); err != nil {
		t.Fatal(err)
	}
	secret.OwnerReferences = []metav1.OwnerReference{sandboxworkload.AgentOwner(f.fixture.Agent.Name, f.fixture.Agent.UID)}
	if err := f.client.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	source := &controller.KubernetesSandboxPrerequisites{
		APIReader: f.client, HomeClaimName: f.source.inputs.Home.Name, SkillsClaimName: f.source.inputs.Skills.Name,
		ChartVersion: f.source.inputs.ChartVersion, Helpers: f.source.inputs.Helpers,
		Now: func() time.Time { return f.now }, WriterExclusion: fence,
	}
	f.reconciler.SandboxPreparation.Prerequisites = source
	return source
}

func TestKubernetesSandboxPrerequisitesUnavailableFenceDoesNotPersistPreparation(t *testing.T) {
	for _, typ := range []string{"openclaw", "hermes"} {
		for _, disableSources := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/disable-sources=%v", typ, disableSources), func(t *testing.T) {
				ctx := context.Background()
				f := newPreparationFixture(t, typ)
				installKubernetesPrerequisites(t, f, nil)
				if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) {
					t.Fatal("missing writer-exclusion adapter did not stop preparation:", err)
				}
				agent := f.current(t)
				if f.installer.renders != 0 || f.installer.stages != 0 ||
					slices.Contains(agent.Finalizers, sandbox.PreparationFinalizer) ||
					!slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) || agent.Status.AccessURL != "" {
					t.Fatal("unavailable fencing rendered a workload, persisted a cleanup obligation or released the hold")
				}
				var state corev1.ConfigMapList
				if err := f.client.List(ctx, &state, client.InNamespace(f.reconciler.OperatorNamespace)); err != nil {
					t.Fatal(err)
				}
				if len(state.Items) != 0 {
					t.Fatal("unavailable fencing persisted a preparation receipt, journal or expectation")
				}
				if disableSources {
					f.reconciler.SandboxPreparation = nil
				}
				if err := f.client.Delete(ctx, agent); err != nil {
					t.Fatal(err)
				}
				for range 2 {
					if err := f.reconcile(); err != nil {
						t.Fatal("an unstaged Agent required unavailable fencing to complete deletion:", err)
					}
				}
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
					t.Fatal("failed preparation stranded Agent deletion:", err)
				}
				if err := f.client.Get(ctx, client.ObjectKeyFromObject(f.fixture.Namespace), &corev1.Namespace{}); !apierrors.IsNotFound(err) {
					t.Fatal("failed preparation stranded the owned namespace:", err)
				}
			})
		}
	}
}

func TestKubernetesSandboxPrerequisitesPreparationAndCleanupCallers(t *testing.T) {
	for _, typ := range []string{"openclaw", "hermes"} {
		t.Run(typ, func(t *testing.T) {
			f := newPreparationFixture(t, typ)
			source := installKubernetesPrerequisites(t, f, nil)
			if err := f.reconcile(); !errors.Is(err, sandbox.ErrObservationUnavailable) || f.installer.stages != 0 {
				t.Fatalf("native preparation bypassed unavailable fencing: stages=%d, %v", f.installer.stages, err)
			}
			agent := f.current(t)
			if agent.Status.AccessURL != "" || !slices.Contains(agent.Finalizers, sandbox.HoldFinalizer) {
				t.Fatal("unavailable fencing lost the execution hold")
			}
			failure := errors.New("TEST-only writer exclusion failure")
			fence := &testSandboxWriterExclusion{err: failure}
			source.WriterExclusion = fence
			if err := f.reconcile(); !errors.Is(err, failure) || f.installer.stages != 0 || fence.calls == 0 {
				t.Fatalf("preparation lost the injected fence error: %v", err)
			}
			fence.err = nil
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			assertSandboxPrepared(t, f)
			tuple := fence.prerequisites
			agent = f.current(t)
			agent.Generation++
			agent.Spec.Suspended = true
			if err := f.client.Update(context.Background(), agent); err != nil {
				t.Fatal(err)
			}
			if err := f.client.Delete(context.Background(), agent); err != nil {
				t.Fatal(err)
			}
			for _, object := range []client.Object{f.fixture.Team, f.fixture.Sandbox, f.fixture.Policy, f.fixture.Input.Home, f.fixture.Input.Skills} {
				if err := f.client.Delete(context.Background(), object); err != nil {
					t.Fatal(err)
				}
			}
			source.APIReader, source.HomeClaimName, source.SkillsClaimName, source.ChartVersion = nil, "", "", ""
			f.reconciler.SandboxPreparation.Configuration = controller.SandboxConfigurationSourceFunc(
				func(context.Context) (controller.SandboxConfigurationSnapshot, error) {
					t.Fatal("cleanup attempted to renew preparation authorization")
					return controller.SandboxConfigurationSnapshot{}, sandboxpolicy.ErrUnavailable
				})
			f.now = f.now.Add(24 * time.Hour)
			preparation := f.reconciler.SandboxPreparation
			source.WriterExclusion = nil
			for _, disableSources := range []bool{false, true} {
				wantError := sandbox.ErrObservationUnavailable
				if disableSources {
					f.reconciler.SandboxPreparation = nil
					wantError = sandboxpolicy.ErrUnavailable
				}
				if err := f.reconcile(); !errors.Is(err, wantError) ||
					!slices.Contains(f.current(t).Finalizers, sandbox.PreparationFinalizer) {
					t.Fatal("existing preparation bypassed unavailable cleanup fencing:", err)
				}
				if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.fixture.Namespace), &corev1.Namespace{}); err != nil {
					t.Fatal("existing preparation lost its namespace without cleanup fencing:", err)
				}
				f.reconciler.SandboxPreparation = preparation
			}
			source.WriterExclusion = fence
			fence.err = failure
			if err := f.reconcile(); !errors.Is(err, failure) {
				t.Fatalf("cleanup lost the injected historical fence error: %v", err)
			}
			if !reflect.DeepEqual(fence.prerequisites, tuple) || fence.agent.Generation != agent.Generation ||
				fence.agent.DeletionTimestamp.IsZero() || tuple.AgentGeneration >= fence.agent.Generation {
				t.Fatal("cleanup did not pass the exact prior tuple for the current terminating Agent")
			}
			if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.fixture.Namespace), &corev1.Namespace{}); err != nil {
				t.Fatalf("namespace removed before confirming historical fencing: %v", err)
			}
			fence.err = nil
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			if err := f.reconcile(); err != nil {
				t.Fatal(err)
			}
			if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(agent), &api.Agent{}); !apierrors.IsNotFound(err) {
				t.Fatalf("historical tuple validation stranded cleanup: %v", err)
			}
		})
	}
}

func TestKubernetesSandboxPrerequisitesFirstPreparationDoesNotInvalidateItself(t *testing.T) {
	f := newPreparationFixture(t, "hermes")
	installKubernetesPrerequisites(t, f, &testSandboxWriterExclusion{})
	if err := f.reconcile(); err != nil {
		t.Fatalf("first preparation invalidated prerequisites after recording its cleanup finalizer: %v", err)
	}
	assertSandboxPrepared(t, f)
	stages, renders := f.installer.stages, f.installer.renders
	if err := f.reconcile(); err != nil || f.installer.stages != stages || f.installer.renders != renders {
		t.Fatalf("unchanged authoritative prerequisites prevented read-only reuse: %v", err)
	}
}
