// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxcoordination

import (
	"context"
	"errors"
	"strings"
	"testing"

	w "github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestReconciledReservationsRefreshOnlyTheSameSources(t *testing.T) {
	ctx := context.Background()
	store, _ := fixture(t)
	initial := testNativeIntent("one")
	initial.Reconciled, initial.Workload.UID = true, ""
	first := acquireNative(t, store, initial)
	peer := testNativeIntent("two")
	peer.Reconciled, peer.Workload.UID = true, ""
	acquireNative(t, store, peer)
	next := first.DeepCopy().Reservation.Intent
	next.AgentGeneration++
	next.Revision = "updated-render"
	next.Sources[0].ResourceVersion = "new-status-version"
	updated := acquireNative(t, store, next)
	if updated.Reservation.ID != first.Reservation.ID {
		t.Fatal("an ordinary template update replaced the HOME source reservation")
	}

	if err := store.AuthorizeNativeStart(ctx, initial); !errors.Is(err, ErrConflict) {
		t.Fatal("the prior desired generation still authorized new Pods")
	}
	if err := store.AuthorizeNativeStart(ctx, next); err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"home", "source", "generation", "legacy"} {
		desired := updated.DeepCopy().Reservation.Intent
		switch change {
		case "home":
			desired.Home.UID = "another-home"
		case "source":
			desired.SourceDigests[w.SourceKey(desired.Sources[0])] = strings.Repeat("f", 64)
		case "generation":
			desired.AgentGeneration--
		case "legacy":
			desired.Reconciled = false
			desired.Workload.UID = "a-physical-workload"
		}
		if _, err := store.AcquireNative(ctx, desired); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s crossed the same-source refresh boundary: %v", change, err)
		}
	}
	stopping, err := store.BeginNativeStop(ctx, updated)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcquireNative(ctx, next); !errors.Is(err, ErrConflict) {
		t.Fatal("reconciliation reopened a stopping reservation")
	}
	if err := store.ReleaseNative(ctx, stopping); err != nil {
		t.Fatal(err)
	}
	if err := store.AuthorizeNativeStart(ctx, peer); err != nil {
		t.Fatal("stopping one HOME interrupted an independent peer:", err)
	}
}

func TestReconciledReservationRefreshRetriesOnlyDefinitiveCASConflicts(t *testing.T) {
	for _, mode := range []string{"conflict", "stopping", "uncertain"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store, _ := fixture(t)
			initial := testNativeIntent("one")
			initial.Reconciled, initial.Workload.UID = true, ""
			first := acquireNative(t, store, initial)
			desired := first.DeepCopy().Reservation.Intent
			desired.Sources[0].ResourceVersion = "changed-status-only"
			original := store
			writes := 0
			lostResponse := errors.New("synthetic update response lost")
			store.Client = interceptor.NewClient(store.Client.(client.WithWatch), interceptor.Funcs{
				Update: func(ctx context.Context, c client.WithWatch, object client.Object, options ...client.UpdateOption) error {
					writes++
					if writes == 1 {
						if mode == "uncertain" {
							if err := c.Update(ctx, object, options...); err != nil {
								return err
							}
							return lostResponse
						}
						if mode == "stopping" {
							if _, err := original.BeginNativeStop(ctx, first); err != nil {
								t.Fatal(err)
							}
						}
						return apierrors.NewConflict(corev1.Resource("configmaps"), object.GetName(), errors.New("synthetic peer update"))
					}
					return c.Update(ctx, object, options...)
				},
			})
			ticket, err := store.AcquireNative(ctx, desired)
			switch mode {
			case "conflict":
				if err != nil || writes != 2 || ticket.Reservation.ID != first.Reservation.ID {
					t.Fatal("a peer CAS conflict replaced or stopped the same-source reservation:", err)
				}
				if err := store.AuthorizeNativeStart(ctx, desired); err != nil {
					t.Fatal("refreshed reservation lost its current authorization:", err)
				}
			case "stopping":
				if !errors.Is(err, ErrConflict) || writes != 1 {
					t.Fatal("retry reopened a concurrently stopped reservation:", err)
				}
			case "uncertain":
				if !errors.Is(err, lostResponse) || writes != 1 {
					t.Fatal("retry replayed an uncertain committed write:", err)
				}
			}
		})
	}
}
