// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxwriter

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestWriterManagerRequiresExplicitDependencies(t *testing.T) {
	tests := map[string]func(*Manager){
		"provider":          func(m *Manager) { m.Provider = nil },
		"clock":             func(m *Manager) { m.Now = nil },
		"zero clock":        func(m *Manager) { m.Now = func() time.Time { return time.Time{} } },
		"nonpositive clock": func(m *Manager) { m.Now = func() time.Time { return time.Unix(0, 0) } },
		"zero age":          func(m *Manager) { m.MaxAge = 0 },
		"negative age":      func(m *Manager) { m.MaxAge = -time.Second },
		"unbounded age":     func(m *Manager) { m.MaxAge = MaxObservationAge + time.Nanosecond },
		"APIReader":         func(m *Manager) { m.Store.APIReader = nil },
		"writer":            func(m *Manager) { m.Store.Client = nil },
		"namespace":         func(m *Manager) { m.Store.Namespace = "" },
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			f := newWriterFixture(t)
			pinned := f.enroll(t)
			change(f.manager)
			if _, err := f.manager.Quiesce(context.Background(), pinned); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("missing dependency was accepted: %v", err)
			}
			if f.writes.updates != 0 || len(f.provider.revokes)+len(f.provider.observes) != 0 {
				t.Fatal("missing dependency allowed an intent or provider call")
			}
		})
	}
	var missing *Manager
	if err := missing.CheckQuiesced(context.Background(), Lifetime{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil manager did not fail closed: %v", err)
	}
}

func TestWriterLostRevokeResponseRestartOnlyObserves(t *testing.T) {
	f := newWriterFixture(t)
	pinned := f.history(t, syntheticWriter(1), syntheticWriter(2))
	priorRevokes, priorObserves := len(f.provider.revokes), len(f.provider.observes)
	f.provider.revoke = func(ctx context.Context, request Request) error {
		stored, err := f.store.Read(ctx, f.lifetime)
		if err != nil || stored.Phase != PhaseRevoking || !reflect.DeepEqual(requestOf(stored), request) ||
			stored.ResourceVersion == pinned.ResourceVersion {
			t.Fatalf("revocation preceded exact durable intent: %+v %v", stored, err)
		}
		return context.DeadlineExceeded
	}
	if _, err := f.manager.Quiesce(context.Background(), pinned); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost response became success: %v", err)
	}
	intent := f.read(t)
	if intent.Phase != PhaseRevoking || len(f.provider.revokes) != priorRevokes+1 || len(f.provider.observes) != priorObserves {
		t.Fatal("lost submission response did not remain an unobserved durable intent")
	}
	restartedProvider := &writerTestProvider{observe: func(_ context.Context, request Request) (Observation, error) {
		if !reflect.DeepEqual(request, requestOf(intent)) {
			t.Fatal("restart changed the durable operation or inventory")
		}
		return syntheticObservation(request, f.now), nil
	}}
	restarted := &Manager{Store: f.store, Provider: restartedProvider, Now: f.manager.Now, MaxAge: time.Minute}
	if err := restarted.CheckQuiesced(context.Background(), f.lifetime); err != nil {
		t.Fatal(err)
	}
	quiescent := f.read(t)
	if quiescent.Phase != PhaseQuiescent || quiescent.OperationID != intent.OperationID ||
		len(restartedProvider.revokes) != 0 || len(restartedProvider.observes) != 1 ||
		!reflect.DeepEqual(quiescent.Writers, pinned.Writers) {
		t.Fatal("restart resubmitted revocation or lost history")
	}
	if err := restarted.CheckQuiesced(context.Background(), f.lifetime); err != nil ||
		len(restartedProvider.revokes) != 0 || len(restartedProvider.observes) != 2 ||
		!reflect.DeepEqual(f.read(t), quiescent) {
		t.Fatalf("quiescent check did not reobserve without rewriting or resubmitting: %v", err)
	}
}

func TestWriterAmbiguousIntentWriteNeverSubmits(t *testing.T) {
	for _, failure := range []string{"lost response", "missing acknowledgement RV", "changed acknowledgement UID", "changed acknowledgement history", "read-back failure", "read-back RV drift", "read-back UID drift"} {
		t.Run(failure, func(t *testing.T) {
			f := newWriterFixture(t)
			pinned := f.history(t, syntheticWriter(1))
			revokes, observes, updates := len(f.provider.revokes), len(f.provider.observes), f.writes.updates
			f.writes.afterUpdate = func(_ context.Context, object client.Object) error {
				switch failure {
				case "lost response":
					return context.DeadlineExceeded
				case "missing acknowledgement RV":
					object.SetResourceVersion(pinned.ResourceVersion)
				case "changed acknowledgement UID":
					object.SetUID("other-journal")
				case "changed acknowledgement history":
					cm := object.(*corev1.ConfigMap)
					cm.Data[JournalDataKey] = strings.Replace(cm.Data[JournalDataKey], `"phase":"revoking"`, `"phase":"quiescent"`, 1)
				case "read-back failure":
					f.reads.before = func(context.Context, client.ObjectKey, client.Object) error { return context.DeadlineExceeded }
				case "read-back RV drift":
					f.reads.after = func(_ context.Context, _ client.ObjectKey, got client.Object) error {
						got.SetResourceVersion("different")
						return nil
					}
				case "read-back UID drift":
					f.reads.after = func(_ context.Context, _ client.ObjectKey, got client.Object) error {
						got.SetUID("different")
						return nil
					}
				}
				return nil
			}
			if _, err := f.manager.Quiesce(context.Background(), pinned); err == nil {
				t.Fatal("ambiguous intent write was accepted")
			}
			if len(f.provider.revokes) != revokes || len(f.provider.observes) != observes || f.writes.updates != updates+1 {
				t.Fatal("ambiguous intent caused a provider call or write retry")
			}
			f.writes.afterUpdate, f.reads.before, f.reads.after = nil, nil, nil
			intent := f.read(t)
			if intent.Phase != PhaseRevoking || !reflect.DeepEqual(intent.Writers, pinned.Writers) {
				t.Fatal("fixture did not persist the ambiguous intent intact")
			}
			restartedProvider := &writerTestProvider{observe: func(_ context.Context, request Request) (Observation, error) {
				proof := syntheticObservation(request, f.now)
				proof.Barrier = BarrierEvidence{}
				return proof, nil
			}}
			restarted := &Manager{Store: f.store, Provider: restartedProvider, Now: f.manager.Now, MaxAge: time.Minute}
			if err := restarted.CheckQuiesced(context.Background(), f.lifetime); !errors.Is(err, ErrUnproven) ||
				len(restartedProvider.revokes) != 0 || len(restartedProvider.observes) != 1 {
				t.Fatalf("restart replayed an intent that might not have been submitted: %v", err)
			}
			restartedProvider.observe = func(_ context.Context, request Request) (Observation, error) {
				return syntheticObservation(request, f.now), nil
			}
			if err := restarted.CheckQuiesced(context.Background(), f.lifetime); err != nil || len(restartedProvider.revokes) != 0 {
				t.Fatalf("complete external observation did not recover without replay: %v", err)
			}
		})
	}
}

func TestWriterFailedIntentAndCASConflictDoNotRevoke(t *testing.T) {
	for _, failure := range []string{"write rejected", "CAS conflict"} {
		t.Run(failure, func(t *testing.T) {
			f := newWriterFixture(t)
			pinned := f.enroll(t)
			sentinel := errors.New("TEST: persistence unavailable")
			f.writes.beforeUpdate = func(ctx context.Context, _ client.Object) error {
				if failure == "write rejected" {
					return sentinel
				}
				object := f.object(t)
				object.Annotations["example.com/concurrent"] = "change"
				return f.raw.Update(ctx, object)
			}
			_, err := f.manager.Quiesce(context.Background(), pinned)
			if (failure == "write rejected" && !errors.Is(err, sentinel)) ||
				(failure == "CAS conflict" && !apierrors.IsConflict(err)) {
				t.Fatalf("wrong persistence failure: %v", err)
			}
			if f.writes.updates != 1 || len(f.provider.revokes)+len(f.provider.observes) != 0 ||
				f.read(t).Phase != PhaseEnrolled {
				t.Fatal("failed intent was retried or submitted")
			}
		})
	}
}

func TestWriterFailedQuiescentWriteCannotReportSuccess(t *testing.T) {
	for _, failure := range []string{"before write", "lost response", "read-back failure"} {
		t.Run(failure, func(t *testing.T) {
			f := newWriterFixture(t)
			pinned := f.history(t, syntheticWriter(1))
			revokes, observes := len(f.provider.revokes), len(f.provider.observes)
			sentinel := errors.New("TEST: quiescence persistence failed")
			hook := func(_ context.Context, object client.Object) error {
				var record journalRecord
				if err := json.Unmarshal([]byte(object.(*corev1.ConfigMap).Data[JournalDataKey]), &record); err != nil {
					t.Fatal(err)
				}
				if record.Phase != PhaseQuiescent {
					return nil
				}
				if failure == "read-back failure" {
					f.reads.before = func(context.Context, client.ObjectKey, client.Object) error { return sentinel }
					return nil
				}
				return sentinel
			}
			if failure == "before write" {
				f.writes.beforeUpdate = hook
			} else {
				f.writes.afterUpdate = hook
			}
			if _, err := f.manager.Quiesce(context.Background(), pinned); !errors.Is(err, sentinel) {
				t.Fatalf("valid proof bypassed failed durable quiescence: %v", err)
			}
			if len(f.provider.revokes) != revokes+1 || len(f.provider.observes) != observes+1 {
				t.Fatal("unexpected provider retry after quiescence write failure")
			}
			f.writes.beforeUpdate, f.writes.afterUpdate, f.reads.before = nil, nil, nil
			stored := f.read(t)
			if (failure == "before write" && stored.Phase != PhaseRevoking) ||
				(failure != "before write" && stored.Phase != PhaseQuiescent) {
				t.Fatal("unexpected persisted failure state")
			}
			if err := f.manager.CheckQuiesced(context.Background(), f.lifetime); err != nil ||
				len(f.provider.revokes) != revokes+1 || len(f.provider.observes) != observes+2 {
				t.Fatalf("recovery failed to reobserve without resubmission: %v", err)
			}
		})
	}
}

func TestWriterEnrollmentWriteFailures(t *testing.T) {
	for _, failure := range []string{"before write", "lost response", "missing identity", "read-back failure"} {
		t.Run(failure, func(t *testing.T) {
			f := newWriterFixture(t)
			if failure == "before write" {
				f.writes.beforeCreate = func(context.Context, client.Object) error { return context.DeadlineExceeded }
			} else {
				f.writes.afterCreate = func(_ context.Context, object client.Object) error {
					switch failure {
					case "lost response":
						return context.DeadlineExceeded
					case "missing identity":
						object.SetUID("")
					case "read-back failure":
						f.reads.before = func(context.Context, client.ObjectKey, client.Object) error { return context.DeadlineExceeded }
					}
					return nil
				}
			}
			if _, err := f.store.Enroll(context.Background(), f.lifetime, "birth-enrollment"); err == nil {
				t.Fatal("unconfirmed enrollment succeeded")
			}
			if f.writes.creates != 1 || f.writes.updates != 0 || len(f.provider.revokes)+len(f.provider.observes) != 0 {
				t.Fatal("failed enrollment was retried or reached a provider")
			}
			f.reads.before = nil
			snapshot, err := f.store.Read(context.Background(), f.lifetime)
			if failure == "before write" {
				if !apierrors.IsNotFound(err) {
					t.Fatalf("failed create manufactured history: %v", err)
				}
			} else if err != nil || snapshot.EnrollmentID != "birth-enrollment" || snapshot.Phase != PhaseEnrolled {
				t.Fatalf("explicit ambiguous enrollment could not be inspected without adoption: %v", err)
			}
		})
	}
}

func TestWriterEvidenceRejectsIncompleteAndPointInTimeProof(t *testing.T) {
	tests := map[string]func(*Observation, time.Time){
		"journal UID":       func(o *Observation, _ time.Time) { o.Request.JournalUID = "other" },
		"operation":         func(o *Observation, _ time.Time) { o.Request.OperationID = strings.Repeat("a", 32) },
		"enrollment":        func(o *Observation, _ time.Time) { o.Request.EnrollmentID = "other" },
		"Agent UID":         func(o *Observation, _ time.Time) { o.Request.Lifetime.Agent.UID = "other" },
		"namespace UID":     func(o *Observation, _ time.Time) { o.Request.Lifetime.Namespace.UID = "other" },
		"HOME UID":          func(o *Observation, _ time.Time) { o.Request.Lifetime.Home.UID = "other" },
		"request history":   func(o *Observation, _ time.Time) { o.Request.Writers = o.Request.Writers[1:] },
		"request reordered": func(o *Observation, _ time.Time) { slices.Reverse(o.Request.Writers) },
		"request storage":   func(o *Observation, _ time.Time) { o.Request.Writers[0].Storage.Handle = "other" },
		"coverage":          func(o *Observation, _ time.Time) { o.Coverage = "" },
		"zero time":         func(o *Observation, _ time.Time) { o.ObservedAt = time.Time{} },
		"nonpositive time":  func(o *Observation, _ time.Time) { o.ObservedAt = time.Unix(0, 0) },
		"negative time":     func(o *Observation, _ time.Time) { o.ObservedAt = time.Unix(-1, 0) },
		"stale time":        func(o *Observation, now time.Time) { o.ObservedAt = now.Add(-time.Minute - time.Nanosecond) },
		"future time":       func(o *Observation, now time.Time) { o.ObservedAt = now.Add(time.Nanosecond) },
		"barrier absent":    func(o *Observation, _ time.Time) { o.Barrier = BarrierEvidence{} },
		"barrier ID":        func(o *Observation, _ time.Time) { o.Barrier.ID = "" },
		"barrier too long":  func(o *Observation, _ time.Time) { o.Barrier.ID = strings.Repeat("b", 257) },
		"barrier stop only": func(o *Observation, _ time.Time) { o.Barrier.Method = "one-shot-stop" },
		"old writer missing": func(o *Observation, _ time.Time) {
			o.Writers = o.Writers[1:]
		},
		"unreachable node missing": func(o *Observation, _ time.Time) {
			o.Writers = o.Writers[:1]
		},
		"duplicate writer": func(o *Observation, _ time.Time) { o.Writers[0] = o.Writers[1] },
		"extra writer":     func(o *Observation, _ time.Time) { o.Writers = append(o.Writers, o.Writers[0]) },
		"workload UID":     func(o *Observation, _ time.Time) { o.Writers[0].Writer.Workload.UID = "other" },
		"Pod UID":          func(o *Observation, _ time.Time) { o.Writers[0].Writer.Pod.UID = "other" },
		"Pod name":         func(o *Observation, _ time.Time) { o.Writers[0].Writer.Pod.Name = "other" },
		"epoch":            func(o *Observation, _ time.Time) { o.Writers[0].Writer.Epoch = "other" },
		"Node UID":         func(o *Observation, _ time.Time) { o.Writers[0].Writer.Node.UID = "other" },
		"Node name":        func(o *Observation, _ time.Time) { o.Writers[0].Writer.Node.Name = "other" },
		"provider ID":      func(o *Observation, _ time.Time) { o.Writers[0].Writer.Node.ProviderID = "example://other" },
		"machine changed":  func(o *Observation, _ time.Time) { o.Writers[0].Writer.MachineID = "other" },
		"machine omitted":  func(o *Observation, _ time.Time) { o.Writers[0].Writer.MachineID = "" },
		"machines swapped": func(o *Observation, _ time.Time) {
			o.Writers[0].Writer.MachineID, o.Writers[1].Writer.MachineID = o.Writers[1].Writer.MachineID, o.Writers[0].Writer.MachineID
		},
		"CRI receipt":       func(o *Observation, _ time.Time) { o.Writers[0].Method = "observed_stopped" },
		"empty pool":        func(o *Observation, _ time.Time) { o.Writers[0].Method = "empty-pool-deletion" },
		"Pod absence":       func(o *Observation, _ time.Time) { o.Writers[0].Method = "pod-not-found" },
		"unknown method":    func(o *Observation, _ time.Time) { o.Writers[0].Method = "future-method" },
		"missing receipt":   func(o *Observation, _ time.Time) { o.Writers[0].EvidenceID = "" },
		"ambiguous receipt": func(o *Observation, _ time.Time) { o.Writers[0].EvidenceID = "receipt\nother" },
		"writer PV drift":   func(o *Observation, _ time.Time) { o.Writers[0].Writer.Storage.Volume.UID = "other" },
		"HOME absent":       func(o *Observation, _ time.Time) { o.Home = HomeRelease{} },
		"HOME rebind":       func(o *Observation, _ time.Time) { o.Home.Home.UID = "other" },
		"HOME rename":       func(o *Observation, _ time.Time) { o.Home.Home.Name = "other" },
		"HOME pending":      func(o *Observation, _ time.Time) { o.Home.State = "pending" },
		"HOME receipt":      func(o *Observation, _ time.Time) { o.Home.EvidenceID = "" },
		"storage absent":    func(o *Observation, _ time.Time) { o.Home.Volumes = nil },
		"old PV missing":    func(o *Observation, _ time.Time) { o.Home.Volumes = o.Home.Volumes[1:] },
		"storage duplicate": func(o *Observation, _ time.Time) { o.Home.Volumes[0] = o.Home.Volumes[1] },
		"storage extra":     func(o *Observation, _ time.Time) { o.Home.Volumes = append(o.Home.Volumes, o.Home.Volumes[0]) },
		"storage pending":   func(o *Observation, _ time.Time) { o.Home.Volumes[0].State = "pending" },
		"storage receipt":   func(o *Observation, _ time.Time) { o.Home.Volumes[0].EvidenceID = "" },
		"PV UID drift":      func(o *Observation, _ time.Time) { o.Home.Volumes[0].Storage.Volume.UID = "other" },
		"PV name drift":     func(o *Observation, _ time.Time) { o.Home.Volumes[0].Storage.Volume.Name = "other" },
		"CSI driver drift":  func(o *Observation, _ time.Time) { o.Home.Volumes[0].Storage.Driver = "other.example.com" },
		"CSI handle drift":  func(o *Observation, _ time.Time) { o.Home.Volumes[0].Storage.Handle = "other" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			f := newWriterFixture(t)
			pinned := f.history(t, syntheticWriter(1), syntheticWriter(2))
			f.provider.observe = func(_ context.Context, request Request) (Observation, error) {
				observation := syntheticObservation(request, f.now)
				mutate(&observation, f.now)
				return observation, nil
			}
			updates := f.writes.updates
			if _, err := f.manager.Quiesce(context.Background(), pinned); !errors.Is(err, ErrUnproven) {
				t.Fatalf("unqualified evidence passed: %v", err)
			}
			stored := f.read(t)
			if stored.Phase != PhaseRevoking || f.writes.updates != updates+1 ||
				!reflect.DeepEqual(stored.Writers, pinned.Writers) {
				t.Fatal("invalid evidence persisted quiescence or changed history")
			}
		})
	}
}

func TestWriterCompleteProofAcrossEpochsAndAgeBoundary(t *testing.T) {
	f := newWriterFixture(t)
	pinned := f.history(t, syntheticWriter(1), syntheticWriter(2))
	f.provider.revoke = func(ctx context.Context, request Request) error {
		stored, err := f.store.Read(ctx, f.lifetime)
		if err != nil || stored.Phase != PhaseRevoking || !reflect.DeepEqual(requestOf(stored), request) {
			t.Fatal("provider request was not exactly the acknowledged durable intent")
		}
		request.Writers[0].MachineID = "provider-mutated-input"
		return nil
	}
	f.provider.observe = func(_ context.Context, request Request) (Observation, error) {
		if !reflect.DeepEqual(request.Writers, pinned.Writers) {
			t.Fatal("provider mutated the request used by another call")
		}
		observation := syntheticObservation(request, f.now.Add(-f.manager.MaxAge))
		observation.Writers[1].Method = InfrastructureDestruction
		slices.Reverse(observation.Writers)
		slices.Reverse(observation.Home.Volumes)
		return observation, nil
	}
	quiescent := f.quiesce(t, pinned)
	if quiescent.Phase != PhaseQuiescent || quiescent.OperationID == "" ||
		quiescent.ResourceVersion == pinned.ResourceVersion || !reflect.DeepEqual(quiescent.Writers, pinned.Writers) {
		t.Fatal("complete exact-history proof did not persist quiescence")
	}
	revokes, observes, updates := len(f.provider.revokes), len(f.provider.observes), f.writes.updates
	if err := f.manager.CheckQuiesced(context.Background(), f.lifetime); err != nil ||
		len(f.provider.revokes) != revokes || len(f.provider.observes) != observes+1 || f.writes.updates != updates {
		t.Fatalf("later check did not reobserve the durable barrier: %v", err)
	}
	if _, err := f.manager.Quiesce(context.Background(), pinned); !apierrors.IsConflict(err) || len(f.provider.revokes) != revokes {
		t.Fatalf("a second manager reused the old transition to submit: %v", err)
	}
}

func TestWriterEvidenceFreshnessSurvivesPersistenceLatency(t *testing.T) {
	for _, stage := range []string{"post-observation read", "quiescent write", "later quiescent read", "clock moves backward"} {
		t.Run(stage, func(t *testing.T) {
			f := newWriterFixture(t)
			pinned := f.history(t, syntheticWriter(1))
			if stage == "later quiescent read" {
				pinned = f.quiesce(t, pinned)
			}
			moveClock := func() {
				if stage == "clock moves backward" {
					f.now = f.now.Add(-time.Nanosecond)
				} else {
					f.now = f.now.Add(f.manager.MaxAge + time.Nanosecond)
				}
			}
			if stage == "quiescent write" {
				f.writes.afterUpdate = func(_ context.Context, object client.Object) error {
					var record journalRecord
					if err := json.Unmarshal([]byte(object.(*corev1.ConfigMap).Data[JournalDataKey]), &record); err != nil {
						t.Fatal(err)
					}
					if record.Phase == PhaseQuiescent {
						moveClock()
					}
					return nil
				}
			} else {
				f.provider.observe = func(_ context.Context, request Request) (Observation, error) {
					f.reads.after = func(context.Context, client.ObjectKey, client.Object) error {
						f.reads.after = nil
						moveClock()
						return nil
					}
					return syntheticObservation(request, f.now), nil
				}
			}
			result, err := f.manager.Quiesce(context.Background(), pinned)
			if !errors.Is(err, ErrUnproven) || result.UID != "" {
				t.Fatalf("proof aged during persistence but returned success: %+v %v", result, err)
			}
			if stage == "post-observation read" || stage == "clock moves backward" {
				if f.read(t).Phase != PhaseRevoking {
					t.Fatal("expired proof was used for a quiescent transition")
				}
			}
		})
	}
}

func TestWriterQuiescentPhaseIsNotCachedEvidence(t *testing.T) {
	for _, failure := range []string{"unreachable", "stale", "missing barrier", "missing HOME", "missing provider"} {
		t.Run(failure, func(t *testing.T) {
			f := newWriterFixture(t)
			quiescent := f.quiesce(t, f.history(t, syntheticWriter(1)))
			revokes, updates := len(f.provider.revokes), f.writes.updates
			sentinel := errors.New("TEST: node or external verifier unreachable")
			f.provider.observe = func(_ context.Context, request Request) (Observation, error) {
				proof := syntheticObservation(request, f.now)
				switch failure {
				case "unreachable":
					return Observation{}, sentinel
				case "stale":
					proof.ObservedAt = f.now.Add(-f.manager.MaxAge - time.Nanosecond)
				case "missing barrier":
					proof.Barrier = BarrierEvidence{}
				case "missing HOME":
					proof.Home = HomeRelease{}
				}
				return proof, nil
			}
			if failure == "missing provider" {
				f.manager.Provider = nil
			}
			err := f.manager.CheckQuiesced(context.Background(), f.lifetime)
			if err == nil || (failure == "unreachable" && !errors.Is(err, sentinel)) {
				t.Fatalf("old phase replaced fresh evidence: %v", err)
			}
			if len(f.provider.revokes) != revokes || f.writes.updates != updates || !reflect.DeepEqual(f.read(t), quiescent) {
				t.Fatal("failed reobservation resubmitted or discarded history")
			}
		})
	}
}

func TestWriterEmptyHistoryStillRequiresBirthBarrierAndHomeProof(t *testing.T) {
	for _, failure := range []string{"empty observation", "missing coverage", "missing barrier", "HOME pending"} {
		t.Run(failure, func(t *testing.T) {
			f := newWriterFixture(t)
			pinned := f.enroll(t)
			f.provider.observe = func(_ context.Context, request Request) (Observation, error) {
				proof := syntheticObservation(request, f.now)
				switch failure {
				case "empty observation":
					return Observation{}, nil
				case "missing coverage":
					proof.Coverage = ""
				case "missing barrier":
					proof.Barrier = BarrierEvidence{}
				case "HOME pending":
					proof.Home.State = "pending"
				}
				return proof, nil
			}
			if _, err := f.manager.Quiesce(context.Background(), pinned); !errors.Is(err, ErrUnproven) {
				t.Fatalf("empty inventory substituted for proof: %v", err)
			}
		})
	}
}

func TestWriterObservationDetectsConcurrentReservation(t *testing.T) {
	f := newWriterFixture(t)
	quiescent := f.quiesce(t, f.history(t, syntheticWriter(1)))
	revokes := len(f.provider.revokes)
	f.provider.observe = func(ctx context.Context, request Request) (Observation, error) {
		if _, err := f.store.Reserve(ctx, quiescent, syntheticWriter(2)); err != nil {
			t.Fatal(err)
		}
		return syntheticObservation(request, f.now), nil
	}
	if _, err := f.manager.Quiesce(context.Background(), quiescent); !apierrors.IsConflict(err) {
		t.Fatalf("observation raced with a new reservation but still succeeded: %v", err)
	}
	stored := f.read(t)
	if stored.Phase != PhaseReserved || len(stored.Writers) != 2 || len(f.provider.revokes) != revokes {
		t.Fatal("raced check lost the new reservation or resubmitted old revocation")
	}
}

func TestWriterQuiescentReadBackDetectsConcurrentReservation(t *testing.T) {
	f := newWriterFixture(t)
	pinned := f.history(t, syntheticWriter(1))
	f.writes.afterUpdate = func(ctx context.Context, object client.Object) error {
		var record journalRecord
		if err := json.Unmarshal([]byte(object.(*corev1.ConfigMap).Data[JournalDataKey]), &record); err != nil {
			t.Fatal(err)
		}
		if record.Phase == PhaseQuiescent {
			quiescent := f.read(t)
			if _, err := f.store.Reserve(ctx, quiescent, syntheticWriter(2)); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	if _, err := f.manager.Quiesce(context.Background(), pinned); !apierrors.IsConflict(err) {
		t.Fatalf("quiescent write accepted a read-back of a different reservation: %v", err)
	}
	if stored := f.read(t); stored.Phase != PhaseReserved || len(stored.Writers) != 2 {
		t.Fatal("read-back conflict lost concurrent history")
	}
}

func TestWriterObservationCannotCommitAfterJournalMutation(t *testing.T) {
	f := newWriterFixture(t)
	pinned := f.history(t, syntheticWriter(1))
	f.provider.observe = func(ctx context.Context, request Request) (Observation, error) {
		object := f.object(t)
		object.Annotations["example.com/concurrent"] = "change"
		if err := f.raw.Update(ctx, object); err != nil {
			t.Fatal(err)
		}
		return syntheticObservation(request, f.now), nil
	}
	updates := f.writes.updates
	if _, err := f.manager.Quiesce(context.Background(), pinned); !apierrors.IsConflict(err) {
		t.Fatalf("journal mutation during observation was ignored: %v", err)
	}
	if f.writes.updates != updates+1 || f.read(t).Phase != PhaseRevoking {
		t.Fatal("observation retried or persisted quiescence over changed state")
	}
}

func TestWriterCancellationBlocksEveryTransition(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		f := newWriterFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := f.store.Enroll(ctx, f.lifetime, "birth-enrollment"); !errors.Is(err, context.Canceled) {
			t.Fatalf("enroll: %v", err)
		}
		if _, err := f.store.Read(ctx, f.lifetime); !errors.Is(err, context.Canceled) {
			t.Fatalf("read: %v", err)
		}
		if _, err := f.store.Reserve(ctx, Snapshot{}, syntheticWriter(1)); !errors.Is(err, context.Canceled) {
			t.Fatalf("reserve: %v", err)
		}
		if err := f.manager.CheckQuiesced(ctx, f.lifetime); !errors.Is(err, context.Canceled) {
			t.Fatalf("check: %v", err)
		}
		if _, err := f.manager.Quiesce(ctx, Snapshot{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("quiesce: %v", err)
		}
		if f.writes.creates+f.writes.updates+f.reads.gets+len(f.provider.revokes)+len(f.provider.observes) != 0 {
			t.Fatal("already-canceled call had side effects")
		}
	})
	for _, stage := range []string{"intent write", "revoke", "observe", "quiescent write", "read-back"} {
		t.Run(stage, func(t *testing.T) {
			f := newWriterFixture(t)
			pinned := f.history(t, syntheticWriter(1))
			revokes, observes := len(f.provider.revokes), len(f.provider.observes)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch stage {
			case "intent write", "quiescent write":
				f.writes.afterUpdate = func(_ context.Context, object client.Object) error {
					var record journalRecord
					if err := json.Unmarshal([]byte(object.(*corev1.ConfigMap).Data[JournalDataKey]), &record); err != nil {
						t.Fatal(err)
					}
					if (stage == "intent write" && record.Phase == PhaseRevoking) ||
						(stage == "quiescent write" && record.Phase == PhaseQuiescent) {
						cancel()
					}
					return nil
				}
			case "revoke":
				f.provider.revoke = func(context.Context, Request) error { cancel(); return nil }
			case "observe":
				f.provider.observe = func(_ context.Context, request Request) (Observation, error) {
					cancel()
					return syntheticObservation(request, f.now), nil
				}
			case "read-back":
				f.writes.afterUpdate = func(context.Context, client.Object) error {
					f.reads.after = func(context.Context, client.ObjectKey, client.Object) error { cancel(); return nil }
					return nil
				}
			}
			if _, err := f.manager.Quiesce(ctx, pinned); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation was hidden by provider/persistence success: %v", err)
			}
			if (stage == "intent write" || stage == "read-back") && len(f.provider.revokes) != revokes {
				t.Fatal("canceled write submitted revocation")
			}
			if (stage == "intent write" || stage == "read-back" || stage == "revoke") && len(f.provider.observes) != observes {
				t.Fatal("cancellation proceeded to Observe")
			}
			f.writes.afterUpdate, f.reads.after = nil, nil
			revokes = len(f.provider.revokes)
			f.provider.observe = func(_ context.Context, request Request) (Observation, error) {
				return syntheticObservation(request, f.now), nil
			}
			if err := f.manager.CheckQuiesced(context.Background(), f.lifetime); err != nil || len(f.provider.revokes) != revokes {
				t.Fatalf("restart after cancellation resubmitted or could not observe: %v", err)
			}
		})
	}
}

func TestWriterProviderObserveErrorsPreserveCancellation(t *testing.T) {
	f := newWriterFixture(t)
	pinned := f.history(t, syntheticWriter(1))
	f.provider.observe = func(context.Context, Request) (Observation, error) {
		return Observation{}, context.Canceled
	}
	if _, err := f.manager.Quiesce(context.Background(), pinned); !errors.Is(err, context.Canceled) ||
		f.read(t).Phase != PhaseRevoking {
		t.Fatalf("provider cancellation was hidden or quiescence persisted: %v", err)
	}
}
