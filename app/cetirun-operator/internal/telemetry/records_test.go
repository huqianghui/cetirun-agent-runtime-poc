// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package telemetry

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func synthAgent() *api.Agent {
	created := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	return &api.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name: "example", UID: types.UID("agent-uid"), CreationTimestamp: created,
			Annotations: map[string]string{
				api.AnnotationOwnerTenantID: "tenant-id", api.AnnotationOwnerObjectID: "object-id",
				"unrelated.example.com/value": "kept out",
			},
		},
		Spec: api.AgentSpec{
			Agent:      api.HarnessSpec{Type: api.AgentType("hermes")},
			User:       api.UserSpec{Alias: "owner", DisplayName: "Owner Example", Email: "owner@example.com"},
			Team:       "team-a",
			SandboxRef: "kata",
			Helm: api.HelmSpec{Values: map[string]runtime.RawExtension{
				"extraEnv": {Raw: []byte(`[{"name":"SYNTHETIC_SECRET","value":"do-not-record"}]`)},
			}},
		},
		Status: api.AgentStatus{
			Phase: "Ready", Namespace: "cetirun-example",
			Policy: &api.PolicyStatus{EffectivePolicyRef: &api.EffectivePolicyReference{
				PolicyObjectReference: api.PolicyObjectReference{Name: "policy-a"}}},
		},
	}
}

func TestAgentRecordHoldsTheMappingAndNothingElse(t *testing.T) {
	record := AgentRecordOf(synthAgent())
	want := AgentRecord{
		Name: "example", UID: "agent-uid", Namespace: "cetirun-example", Type: "hermes", Team: "team-a",
		Sandbox: "kata", Policy: "policy-a", Phase: "Ready", Created: "2026-01-02T03:04:05Z",
		Owner: Owner{Alias: "owner", DisplayName: "Owner Example", Email: "owner@example.com",
			TenantID: "tenant-id", ObjectID: "object-id"},
	}
	if *record != want {
		t.Fatalf("record = %+v, want %+v", *record, want)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"do-not-record", "SYNTHETIC_SECRET", "kept out"} {
		if strings.Contains(string(encoded), leaked) {
			t.Fatalf("record contains %q: %s", leaked, encoded)
		}
	}
}

func TestAgentNamespaceFallsBackToTheNamingRule(t *testing.T) {
	agent := synthAgent()
	agent.Status.Namespace = ""
	if got := AgentNamespace(agent); got != "cetirun-example" {
		t.Fatalf("namespace = %q", got)
	}
}

func TestAgentRecordUsesTheLegacyUserTeam(t *testing.T) {
	agent := synthAgent()
	agent.Spec.Team, agent.Spec.User.Team = "", "legacy-team"
	if got := AgentRecordOf(agent).Team; got != "legacy-team" {
		t.Fatalf("team = %q", got)
	}
}

func TestEventRecordClipsTheMessageAndUsesTheSeriesCount(t *testing.T) {
	first := metav1.NewTime(time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC))
	last := metav1.NewMicroTime(time.Date(2026, 1, 2, 4, 0, 0, 0, time.UTC))
	event := &corev1.Event{
		ObjectMeta:          metav1.ObjectMeta{Name: "example.1", Namespace: "cetirun-example", UID: "event-uid", ResourceVersion: "7"},
		InvolvedObject:      corev1.ObjectReference{APIVersion: "cetirun.microsoft.com/v1alpha1", Kind: "Agent", Name: "example", UID: "agent-uid"},
		Type:                corev1.EventTypeWarning,
		Reason:              "ProvisioningFailed",
		Message:             strings.Repeat("é", MaxTextLength+10),
		Count:               2,
		FirstTimestamp:      first,
		LastTimestamp:       first,
		ReportingController: "cetirun-operator",
		Series:              &corev1.EventSeries{Count: 5, LastObservedTime: last},
	}
	record := EventRecordOf(event)
	if got := len([]rune(record.Message)); got != MaxTextLength {
		t.Fatalf("message has %d characters", got)
	}
	if record.Count != 5 || record.LastTime != "2026-01-02T04:00:00Z" || record.FirstTime != "2026-01-02T03:00:00Z" {
		t.Fatalf("count/time = %d %s %s", record.Count, record.FirstTime, record.LastTime)
	}
	if record.Object.Kind != "Agent" || record.Object.UID != "agent-uid" || record.Source != "cetirun-operator" {
		t.Fatalf("object = %+v source = %q", record.Object, record.Source)
	}
}

func TestEventFilter(t *testing.T) {
	cases := []struct {
		name  string
		event corev1.Event
		want  bool
	}{
		{"platform namespace", corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "cetirun-system"}, Type: "Normal"}, true},
		{"agent namespace", corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "cetirun-example"}, Type: "Normal"}, true},
		{"cetirun resource", corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}, Type: "Normal",
			InvolvedObject: corev1.ObjectReference{APIVersion: "cetirun.microsoft.com/v1alpha1", Kind: "Agent"}}, true},
		{"warning elsewhere", corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system"}, Type: "Warning"}, true},
		{"normal elsewhere", corev1.Event{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system"}, Type: "Normal",
			InvolvedObject: corev1.ObjectReference{APIVersion: "v1", Kind: "Pod"}}, false},
	}
	for _, c := range cases {
		if got := EventsWarningsAndCetiRun.Keep(&c.event); got != c.want {
			t.Errorf("%s: Keep = %v", c.name, got)
		}
		if !EventsAll.Keep(&c.event) || EventsNone.Keep(&c.event) {
			t.Errorf("%s: all/none filters are wrong", c.name)
		}
	}
	if EventFilter("everything").Valid() {
		t.Fatal("unknown filter accepted")
	}
}

func TestClipKeepsWholeCharacters(t *testing.T) {
	for _, c := range []struct {
		text  string
		limit int
		want  string
	}{{"abc", 5, "abc"}, {"abcdef", 3, "abc"}, {"日本語テキスト", 3, "日本語"}, {"", 0, ""}} {
		if got := Clip(c.text, c.limit); got != c.want {
			t.Errorf("Clip(%q, %d) = %q", c.text, c.limit, got)
		}
	}
}

func TestWriterWritesOneLinePerRecord(t *testing.T) {
	var out bytes.Buffer
	writer := NewWriter(&out)
	writer.now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC) }
	if err := writer.Write(LogTypeAgent, Record{Action: ActionSnapshot, Agent: AgentRecordOf(synthAgent())}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines", len(lines))
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["log_type"] != LogTypeAgent || decoded["schema_version"] != float64(SchemaVersion) ||
		decoded["time"] != "2026-01-02T03:04:05.000000006Z" || decoded["action"] != ActionSnapshot {
		t.Fatalf("record = %v", decoded)
	}
	if _, present := decoded["event"]; present {
		t.Fatal("empty sections must be left out")
	}
}

func TestRecorderWritesChangesOnceAndSnapshotsAlways(t *testing.T) {
	var out bytes.Buffer
	recorder := &Recorder{Writer: NewWriter(&out), Events: EventsNone, Log: logr.Discard()}
	agent := synthAgent()
	kind := kinds[0]
	recorder.inventory(kind, agent, ActionUpsert)
	recorder.inventory(kind, agent, ActionUpsert)
	changed := agent.DeepCopy()
	changed.Status.Phase = "Suspended"
	recorder.inventory(kind, changed, ActionUpsert)
	recorder.inventory(kind, changed, ActionSnapshot)
	recorder.inventory(kind, changed, ActionDelete)
	var actions []string
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		var record Record
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, record.Action+":"+record.Agent.Phase)
	}
	want := []string{"upsert:Ready", "upsert:Suspended", "snapshot:Suspended", "delete:Suspended"}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Fatalf("actions = %v, want %v", actions, want)
	}
}
