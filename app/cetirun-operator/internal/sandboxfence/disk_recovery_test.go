// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxfence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxwriter"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// diskRecoveryProbe composes the test-only disk reader with independently
// supplied synthetic provider evidence. It never derives a barrier, exclusion
// method or storage-release assertion from an Unattached response.
type diskRecoveryProbe struct {
	t         *testing.T
	store     sandboxwriter.Store
	arm       *armClient
	now       func() time.Time
	storage   sandboxwriter.StorageIdentity
	prove     func(sandboxwriter.Request) sandboxwriter.Observation
	revokeErr error
	revokes   []sandboxwriter.Request
	observes  []sandboxwriter.Request
}

var _ sandboxwriter.Provider = (*diskRecoveryProbe)(nil)

func (p *diskRecoveryProbe) Revoke(ctx context.Context, request sandboxwriter.Request) error {
	p.revokes = append(p.revokes, request.DeepCopy())
	stored, err := p.store.Read(ctx, request.Lifetime)
	if err != nil {
		return err
	}
	if stored.Phase != sandboxwriter.PhaseRevoking || !reflect.DeepEqual(diskRecoveryRequest(stored), request) {
		p.t.Fatal("provider submission preceded the exact serialized writer intent")
	}
	return p.revokeErr
}

func (p *diskRecoveryProbe) Observe(ctx context.Context, request sandboxwriter.Request) (sandboxwriter.Observation, error) {
	p.observes = append(p.observes, request.DeepCopy())
	// Empty birth enrollment is synthetic too, but has no historical disk.
	if len(request.Writers) == 0 {
		return p.prove(request), nil
	}
	for _, writer := range request.Writers {
		if writer.Node.ProviderID != fixtureVM || writer.MachineID != fixtureVMID || writer.Storage != p.storage {
			return sandboxwriter.Observation{}, fmt.Errorf("%w: writer is outside the pinned probe incarnations", sandboxwriter.ErrUnproven)
		}
	}
	diskObservedAt := p.now()
	// The synthetic driver's opaque handle is the disk incarnation, not the
	// reusable ARM path or a claim about the production Azure CSI handle.
	if err := probeUnattachedDisk(ctx, p.arm, diskProbeIdentity{fixtureDisk, p.storage.Handle}); err != nil {
		return sandboxwriter.Observation{}, fmt.Errorf("%w: independent disk observation: %v", sandboxwriter.ErrUnproven, err)
	}
	proof := p.prove(request)
	now := p.now()
	if !proof.ObservedAt.After(time.Unix(0, 0)) || proof.ObservedAt.After(now) || diskObservedAt.After(now) {
		return sandboxwriter.Observation{}, fmt.Errorf("%w: inconsistent component observation times", sandboxwriter.ErrUnproven)
	}
	// Later evidence cannot refresh an earlier disk or exclusion observation.
	// The manager rechecks this oldest time after its journal reads and writes.
	if diskObservedAt.Before(proof.ObservedAt) {
		proof.ObservedAt = diskObservedAt
	}
	return proof, nil
}

func diskRecoveryRequest(snapshot sandboxwriter.Snapshot) sandboxwriter.Request {
	return sandboxwriter.Request{
		JournalUID: snapshot.UID, OperationID: snapshot.OperationID, EnrollmentID: snapshot.EnrollmentID,
		Lifetime: snapshot.Lifetime, Writers: snapshot.DeepCopy().Writers,
	}
}

func syntheticDiskRecoveryProof(request sandboxwriter.Request, now time.Time) sandboxwriter.Observation {
	proof := sandboxwriter.Observation{
		Request: request.DeepCopy(), ObservedAt: now, Coverage: sandboxwriter.CompleteHistory,
		Barrier: sandboxwriter.BarrierEvidence{ID: "synthetic-independent-barrier", Method: sandboxwriter.DurableStartBarrier},
		Home: sandboxwriter.HomeRelease{
			Home: request.Lifetime.Home, State: sandboxwriter.Released, EvidenceID: "synthetic-independent-home-release",
		},
	}
	for _, writer := range request.Writers {
		proof.Writers = append(proof.Writers, sandboxwriter.WriterEvidence{
			Writer: writer, Method: sandboxwriter.RuntimeRevocation, EvidenceID: "synthetic-independent-exclusion-" + writer.Epoch,
		})
	}
	if len(request.Writers) != 0 {
		proof.Home.Volumes = []sandboxwriter.StorageRelease{{
			Storage: request.Writers[0].Storage, State: sandboxwriter.Released, EvidenceID: "synthetic-independent-volume-release",
		}}
	}
	return proof
}

type diskRecoveryFixture struct {
	api                client.Client
	store              sandboxwriter.Store
	now                time.Time
	reserved           sandboxwriter.Snapshot
	writer             sandboxwriter.Writer
	lostQuiescentReply error
}

func newDiskRecoveryFixture(t *testing.T) *diskRecoveryFixture {
	t.Helper()
	f := &diskRecoveryFixture{now: time.Date(2026, 10, 23, 8, 0, 0, 0, time.UTC)}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	// Retain the existing ConfigMap JSON/RV protocol in a fake API. This is
	// process-restart simulation, not API-server or physical durability proof.
	f.api = fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			object.SetUID("synthetic-disk-recovery-journal")
			return c.Create(ctx, object, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
			if err := c.Update(ctx, object, opts...); err != nil {
				return err
			}
			var record struct {
				Phase sandboxwriter.Phase `json:"phase"`
			}
			if err := json.Unmarshal([]byte(object.(*corev1.ConfigMap).Data[sandboxwriter.JournalDataKey]), &record); err != nil {
				return err
			}
			if record.Phase == sandboxwriter.PhaseQuiescent && f.lostQuiescentReply != nil {
				err := f.lostQuiescentReply
				f.lostQuiescentReply = nil
				return err
			}
			return nil
		},
	}).Build()
	f.store = sandboxwriter.Store{Client: f.api, APIReader: f.api, Namespace: "operator-state"}
	lifetime := sandboxwriter.Lifetime{
		Agent:     sandboxworkload.ObjectIdentity{Name: "demo", UID: "synthetic-agent"},
		Namespace: sandboxworkload.ObjectIdentity{Name: "cetirun-demo", UID: "synthetic-namespace"},
		Home:      sandboxworkload.ObjectIdentity{Name: "home", UID: "synthetic-home"},
	}
	f.writer = sandboxwriter.Writer{
		Workload: sandboxworkload.ObjectIdentity{Name: "demo", UID: "synthetic-workload"},
		Pod:      sandboxworkload.ObjectIdentity{Name: "demo-0", UID: "synthetic-pod-1"},
		Node: sandboxworkload.PlacementNode{
			Name: "worker", UID: "synthetic-node", ProviderID: fixtureVM,
		},
		MachineID: fixtureVMID, Epoch: "epoch-1",
		Storage: sandboxwriter.StorageIdentity{
			Volume: sandboxworkload.ObjectIdentity{Name: "home-volume", UID: "synthetic-pv"},
			Driver: "storage.example.com", Handle: fixtureDiskUID,
		},
	}
	enrolled, err := f.store.Enroll(context.Background(), lifetime, "synthetic-complete-birth-enrollment")
	if err != nil {
		t.Fatal(err)
	}
	manager, _ := f.restart(t, nil)
	quiescent, err := manager.Quiesce(context.Background(), enrolled)
	if err != nil {
		t.Fatal(err)
	}
	f.reserved, err = f.store.Reserve(context.Background(), quiescent, f.writer)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *diskRecoveryFixture) restart(t *testing.T, arm *armClient) (*sandboxwriter.Manager, *diskRecoveryProbe) {
	t.Helper()
	store := sandboxwriter.Store{Client: f.api, APIReader: f.api, Namespace: f.store.Namespace}
	p := &diskRecoveryProbe{t: t, store: store, arm: arm, now: func() time.Time { return f.now }, storage: f.writer.Storage}
	p.prove = func(request sandboxwriter.Request) sandboxwriter.Observation {
		return syntheticDiskRecoveryProof(request, f.now)
	}
	return &sandboxwriter.Manager{Store: store, Provider: p, Now: p.now, MaxAge: time.Minute}, p
}

func (f *diskRecoveryFixture) read(t *testing.T) sandboxwriter.Snapshot {
	t.Helper()
	snapshot, err := f.store.Read(context.Background(), f.reserved.Lifetime)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestDiskWriterRecoveryAfterLostRevokeReply(t *testing.T) {
	ctx := context.Background()
	f := newDiskRecoveryFixture(t)
	arm, reads := newDiskProbeClient(t, http.StatusOK, fixtureDiskBody)
	manager, provider := f.restart(t, arm)
	lost := errors.New("synthetic accepted revoke reply lost")
	provider.revokeErr = lost
	if _, err := manager.Quiesce(ctx, f.reserved); !errors.Is(err, lost) {
		t.Fatalf("lost revoke reply became success: %v", err)
	}
	intent := f.read(t)
	if intent.Phase != sandboxwriter.PhaseRevoking || intent.OperationID == "" ||
		!reflect.DeepEqual(intent.Writers, f.reserved.Writers) ||
		len(provider.revokes) != 1 || len(provider.observes) != 0 || reads.Load() != 0 {
		t.Fatal("response loss changed history or skipped the observation-only recovery boundary")
	}
	restarted, observer := f.restart(t, arm)
	if err := restarted.CheckQuiesced(ctx, intent.Lifetime); err != nil {
		t.Fatal(err)
	}
	quiescent := f.read(t)
	if quiescent.Phase != sandboxwriter.PhaseQuiescent ||
		!reflect.DeepEqual(diskRecoveryRequest(quiescent), diskRecoveryRequest(intent)) ||
		len(observer.revokes) != 0 || len(observer.observes) != 1 || reads.Load() != 1 {
		t.Fatal("restart resubmitted, retargeted, or skipped independent disk observation")
	}
	again, freshObserver := f.restart(t, arm)
	if err := again.CheckQuiesced(ctx, intent.Lifetime); err != nil {
		t.Fatal(err)
	}
	if len(freshObserver.revokes) != 0 || reads.Load() != 2 || !reflect.DeepEqual(f.read(t), quiescent) {
		t.Fatal("quiescent restart cached its disk observation or rewrote history")
	}
}

func TestDiskWriterRecoveryDoesNotRefreshEarlyEvidence(t *testing.T) {
	for _, boundary := range []string{"disk ages while exclusion is read", "exclusion predates fresh disk", "future exclusion", "clock moves backward"} {
		t.Run(boundary, func(t *testing.T) {
			f := newDiskRecoveryFixture(t)
			arm, reads := newDiskProbeClient(t, http.StatusOK, fixtureDiskBody)
			manager, provider := f.restart(t, arm)
			provider.prove = func(request sandboxwriter.Request) sandboxwriter.Observation {
				switch boundary {
				case "disk ages while exclusion is read":
					f.now = f.now.Add(time.Minute + time.Nanosecond)
					return syntheticDiskRecoveryProof(request, f.now)
				case "exclusion predates fresh disk":
					return syntheticDiskRecoveryProof(request, f.now.Add(-time.Minute-time.Nanosecond))
				case "future exclusion":
					return syntheticDiskRecoveryProof(request, f.now.Add(time.Second))
				default:
					f.now = f.now.Add(-time.Second)
					return syntheticDiskRecoveryProof(request, f.now)
				}
			}
			if _, err := manager.Quiesce(context.Background(), f.reserved); !errors.Is(err, sandboxwriter.ErrUnproven) {
				t.Fatalf("component evidence was refreshed or its invalid time hidden: %v", err)
			}
			intent := f.read(t)
			if intent.Phase != sandboxwriter.PhaseRevoking || reads.Load() != 1 || len(provider.revokes) != 1 {
				t.Fatal("invalid evidence bypassed a real disk read or persisted quiescence")
			}
			restarted, observer := f.restart(t, arm)
			if err := restarted.CheckQuiesced(context.Background(), intent.Lifetime); err != nil {
				t.Fatal(err)
			}
			if len(observer.revokes) != 0 || reads.Load() != 2 ||
				!reflect.DeepEqual(diskRecoveryRequest(f.read(t)), diskRecoveryRequest(intent)) {
				t.Fatal("fresh recovery changed or resubmitted the durable operation")
			}
		})
	}
}

func TestDiskWriterRecoveryRejectsPreviousOperationReplay(t *testing.T) {
	ctx := context.Background()
	f := newDiskRecoveryFixture(t)
	arm, reads := newDiskProbeClient(t, http.StatusOK, fixtureDiskBody)
	manager, provider := f.restart(t, arm)
	var previous sandboxwriter.Observation
	provider.prove = func(request sandboxwriter.Request) sandboxwriter.Observation {
		previous = syntheticDiskRecoveryProof(request, f.now)
		return previous
	}
	quiescent, err := manager.Quiesce(ctx, f.reserved)
	if err != nil {
		t.Fatal(err)
	}
	nextWriter := f.writer
	nextWriter.Pod.UID, nextWriter.Epoch = "synthetic-pod-2", "epoch-2"
	reserved, err := f.store.Reserve(ctx, quiescent, nextWriter)
	if err != nil {
		t.Fatal(err)
	}
	submitter, submission := f.restart(t, arm)
	lost := errors.New("synthetic second revoke reply lost")
	submission.revokeErr = lost
	if _, err := submitter.Quiesce(ctx, reserved); !errors.Is(err, lost) {
		t.Fatal(err)
	}
	intent := f.read(t)
	if intent.OperationID == quiescent.OperationID || len(intent.Writers) != 2 {
		t.Fatal("new writer did not invalidate the prior operation while retaining history")
	}
	f.now = f.now.Add(time.Second)
	restarted, observer := f.restart(t, arm)
	observer.prove = func(sandboxwriter.Request) sandboxwriter.Observation {
		previous.ObservedAt = f.now
		return previous
	}
	if err := restarted.CheckQuiesced(ctx, intent.Lifetime); !errors.Is(err, sandboxwriter.ErrUnproven) {
		t.Fatalf("fresh disk and restamped old proof authorized a later writer epoch: %v", err)
	}
	if len(observer.revokes) != 0 || reads.Load() != 2 || !reflect.DeepEqual(f.read(t), intent) {
		t.Fatal("replayed proof rewrote history, skipped disk observation or resubmitted")
	}
	recovered, current := f.restart(t, arm)
	if err := recovered.CheckQuiesced(ctx, intent.Lifetime); err != nil {
		t.Fatal(err)
	}
	if len(current.revokes) != 0 || reads.Load() != 3 ||
		!reflect.DeepEqual(diskRecoveryRequest(f.read(t)), diskRecoveryRequest(intent)) {
		t.Fatal("current complete-history proof failed observation-only recovery")
	}
}

func TestDiskWriterRecoveryAfterLostQuiescentWriteReply(t *testing.T) {
	ctx := context.Background()
	f := newDiskRecoveryFixture(t)
	arm, reads := newDiskProbeClient(t, http.StatusOK, fixtureDiskBody)
	manager, provider := f.restart(t, arm)
	lost := errors.New("synthetic persisted quiescence reply lost")
	f.lostQuiescentReply = lost
	if _, err := manager.Quiesce(ctx, f.reserved); !errors.Is(err, lost) {
		t.Fatalf("lost journal acknowledgement became success: %v", err)
	}
	quiescent := f.read(t)
	if quiescent.Phase != sandboxwriter.PhaseQuiescent || len(provider.revokes) != 1 || reads.Load() != 1 {
		t.Fatal("fixture did not persist quiescence before losing the reply")
	}
	tests := []struct {
		name   string
		body   string
		change func(*sandboxwriter.Observation)
	}{
		{"reattached disk", strings.Replace(fixtureDiskBody, `"Unattached"`, `"Reserved"`, 1), nil},
		{"recreated disk at same path", strings.Replace(fixtureDiskBody, fixtureDiskUID, fixtureReplacement, 1), nil},
		{"replaced machine at same path", fixtureDiskBody, func(o *sandboxwriter.Observation) {
			o.Writers[0].Writer.MachineID = fixtureReplacement
		}},
		{"rebound PV at same name", fixtureDiskBody, func(o *sandboxwriter.Observation) {
			o.Home.Volumes[0].Storage.Volume.UID = "synthetic-replacement-pv"
		}},
		{"missing independent storage release", fixtureDiskBody, func(o *sandboxwriter.Observation) {
			o.Home.Volumes = nil
		}},
		{"accepted stop", fixtureDiskBody, func(o *sandboxwriter.Observation) {
			o.Writers[0].Method = "accepted-stop"
		}},
		{"NotReady node", fixtureDiskBody, func(o *sandboxwriter.Observation) {
			o.Writers[0].Method = "node-not-ready"
		}},
		{"absent Pod", fixtureDiskBody, func(o *sandboxwriter.Observation) {
			o.Writers[0].Method = "pod-not-found"
		}},
		{"missing durable barrier", fixtureDiskBody, func(o *sandboxwriter.Observation) {
			o.Barrier = sandboxwriter.BarrierEvidence{}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			disk, diskReads := newDiskProbeClient(t, http.StatusOK, test.body)
			restarted, observer := f.restart(t, disk)
			observer.prove = func(request sandboxwriter.Request) sandboxwriter.Observation {
				proof := syntheticDiskRecoveryProof(request, f.now)
				if test.change != nil {
					test.change(&proof)
				}
				return proof
			}
			if err := restarted.CheckQuiesced(ctx, quiescent.Lifetime); !errors.Is(err, sandboxwriter.ErrUnproven) {
				t.Fatalf("historical quiescence or disk read replaced current complete proof: %v", err)
			}
			if len(observer.revokes) != 0 || len(observer.observes) != 1 || diskReads.Load() != 1 ||
				!reflect.DeepEqual(f.read(t), quiescent) {
				t.Fatal("restart skipped disk verification, changed history or resubmitted")
			}
		})
	}
	restarted, observer := f.restart(t, arm)
	if err := restarted.CheckQuiesced(ctx, quiescent.Lifetime); err != nil {
		t.Fatal(err)
	}
	if len(observer.revokes) != 0 || reads.Load() != 2 || !reflect.DeepEqual(f.read(t), quiescent) {
		t.Fatal("fresh independent evidence did not recover without rewriting or resubmitting")
	}
}
