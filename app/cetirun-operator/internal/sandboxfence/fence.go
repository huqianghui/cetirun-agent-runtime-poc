// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxfence

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
)

func Run(ctx context.Context, options Options) (*Journal, error) {
	return run(ctx, options, newARMClient, syncJournalParent)
}

func run(ctx context.Context, options Options, makeClient func(Reservation) (*armClient, error), syncParent func(string) error) (journal *Journal, returnErr error) {
	if options.Action == "" {
		options.Action = Inspect
	}
	if !slices.Contains([]Action{Reserve, Register, Inspect, TestCondition, Delete, Observe}, options.Action) {
		return nil, errors.New("unrecognized action")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.Action == Reserve {
		if err := options.Reservation.validate(); err != nil {
			return nil, err
		}
	} else if options.Reservation != (Reservation{}) {
		return nil, errors.New("target identity may be supplied only when reserving a previously absent pool")
	}
	if options.Action != Delete && options.Action != TestCondition && options.ConfirmDeletePool != "" {
		return nil, errors.New("destruction confirmation is valid only for delete or test-condition")
	}
	store, err := openJournalWithParentSync(options.JournalDirectory, options.Action == Reserve, syncParent)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, store.close()) }()
	journal, err = store.load()
	if options.Action == Reserve {
		if !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				return nil, err
			}
			return journal, errors.New("journal is already reserved; target adoption or reuse is forbidden")
		}
		client, err := makeClient(options.Reservation)
		if err != nil {
			return nil, err
		}
		return reserve(ctx, store, client, options.Reservation)
	}
	if err != nil {
		return nil, fmt.Errorf("load durable registration: %w", err)
	}
	if options.Action == Delete || options.Action == TestCondition {
		if options.ConfirmDeletePool != journal.Reservation.PoolID {
			return journal, errors.New("destruction requires --confirm-delete-pool with the exact registered pool ARM ID")
		}
	}
	client, err := makeClient(journal.Reservation)
	if err != nil {
		return journal, err
	}
	switch options.Action {
	case Register:
		if journal.Phase != Reserved {
			return journal, errors.New("registration cannot be refreshed, replaced or adopted")
		}
		if err := checkCluster(ctx, client, journal); err != nil {
			return journal, err
		}
		pool, err := client.pool(ctx, journal)
		if err != nil {
			return journal, err
		}
		if pool == nil {
			return journal, errors.New("reserved pool has not been created")
		}
		machines, err := client.machines(ctx, journal)
		if err != nil {
			return journal, err
		}
		journal.ETag, journal.Machines = pool.Properties.ETag, machines
		if err := revalidate(ctx, client, journal); err != nil {
			return journal, err
		}
		journal.Phase = Registered
		return journal, store.save(journal)
	case Inspect:
		if journal.Phase == Complete {
			return journal, nil
		}
		if journal.Phase == Reserved {
			return journal, checkCluster(ctx, client, journal)
		}
		if journal.Phase == Registered || journal.Phase == Qualified {
			return journal, revalidate(ctx, client, journal)
		}
		return journal, ErrPending
	case TestCondition:
		return journal, testCondition(ctx, store, client, journal)
	case Delete:
		return journal, deletePool(ctx, store, client, journal)
	case Observe:
		return journal, observe(ctx, store, client, journal)
	default:
		return journal, errors.New("unsupported action")
	}
}

func reserve(ctx context.Context, store *journalStore, client *armClient, r Reservation) (*Journal, error) {
	cluster, err := client.cluster(ctx, r)
	if err != nil {
		return nil, err
	}
	result, err := client.request(ctx, http.MethodGet, client.resourceURL(r.PoolID, APIVersion), "", "")
	if err != nil {
		return nil, err
	}
	if !authoritativeAbsent(result) {
		return nil, errors.New("pool must be authoritatively absent before reservation; existing pools cannot be adopted")
	}
	journal := &Journal{Version: 1, Scope: Scope, API: APIVersion, Reservation: r, ReservedAt: time.Now().UTC(),
		NodeResourceGroup: cluster.Properties.NodeResourceGroup, Phase: Reserved}
	if err := checkCluster(ctx, client, journal); err != nil {
		return nil, err
	}
	return journal, store.save(journal)
}

func checkCluster(ctx context.Context, client *armClient, journal *Journal) error {
	cluster, err := client.cluster(ctx, journal.Reservation)
	if err != nil {
		return err
	}
	if !strings.EqualFold(cluster.Properties.NodeResourceGroup, journal.NodeResourceGroup) {
		return errors.New("cluster node resource group changed")
	}
	return ctx.Err()
}

func revalidate(ctx context.Context, client *armClient, journal *Journal) error {
	if err := checkCluster(ctx, client, journal); err != nil {
		return err
	}
	pool, err := client.pool(ctx, journal)
	if err != nil {
		return err
	}
	if pool == nil || pool.Properties.ETag != journal.ETag {
		return errors.New("registered pool is absent or its ETag changed; no target refresh is permitted")
	}
	machines, err := client.machines(ctx, journal)
	if err != nil {
		return err
	}
	if !slices.Equal(machines, journal.Machines) {
		return errors.New("registered machine/Compute vmId binding changed; no replacement may be selected")
	}
	pool, err = client.pool(ctx, journal)
	if err != nil {
		return err
	}
	if pool == nil || pool.Properties.ETag != journal.ETag {
		return errors.New("pool changed during identity verification")
	}
	return ctx.Err()
}

func testCondition(ctx context.Context, store *journalStore, client *armClient, journal *Journal) error {
	if journal.Phase != Registered {
		return errors.New("condition probe may be submitted only once from registered state; use observe after uncertain outcomes")
	}
	if err := revalidate(ctx, client, journal); err != nil {
		return err
	}
	etag, err := differentETag(journal.ETag)
	if err != nil {
		return err
	}
	journal.Probe = &Attempt{RequestID: newRequestID(), IfMatch: etag, IntentAt: time.Now().UTC()}
	journal.Phase = ProbePending
	if err := store.save(journal); err != nil {
		return err
	}
	submitErr := client.submit(ctx, journal, journal.Probe)
	if journal.Probe.HTTPStatus == http.StatusPreconditionFailed && submitErr == nil {
		journal.Phase = ProbeRejected
	} else if journal.Probe.HTTPStatus == http.StatusAccepted || journal.Probe.HTTPStatus == http.StatusNoContent {
		journal.Phase = ProbeUnexpected
	} else if journal.Probe.HTTPStatus >= 400 && journal.Probe.HTTPStatus < 500 &&
		journal.Probe.HTTPStatus != http.StatusRequestTimeout && journal.Probe.HTTPStatus != http.StatusTooManyRequests {
		journal.Phase = Failed
	}
	if err := store.save(journal); err != nil {
		return errors.Join(submitErr, err)
	}
	if submitErr != nil {
		return submitErr
	}
	if journal.Phase != ProbeRejected {
		return fmt.Errorf("conditional DELETE qualification did not establish 412 rejection: %w", ErrPending)
	}
	return finishProbe(ctx, store, client, journal)
}

func finishProbe(ctx context.Context, store *journalStore, client *armClient, journal *Journal) error {
	if err := revalidate(ctx, client, journal); err != nil {
		return err
	}
	journal.Phase = Qualified
	return store.save(journal)
}

func deletePool(ctx context.Context, store *journalStore, client *armClient, journal *Journal) error {
	if journal.Phase != Qualified {
		return errors.New("deletion requires the qualified condition probe and can be submitted only once; use observe")
	}
	if err := revalidate(ctx, client, journal); err != nil {
		return err
	}
	journal.Deletion = &Attempt{RequestID: newRequestID(), IfMatch: journal.ETag, IntentAt: time.Now().UTC()}
	journal.Phase = DeletePending
	if err := store.save(journal); err != nil {
		return err
	}
	submitErr := client.submit(ctx, journal, journal.Deletion)
	if journal.Deletion.HTTPStatus == http.StatusAccepted && submitErr == nil {
		journal.Phase = Deleting
	} else if journal.Deletion.HTTPStatus >= 400 && journal.Deletion.HTTPStatus < 500 &&
		journal.Deletion.HTTPStatus != http.StatusRequestTimeout && journal.Deletion.HTTPStatus != http.StatusTooManyRequests {
		journal.Phase = Failed
	}
	if err := store.save(journal); err != nil {
		return errors.Join(submitErr, err)
	}
	if submitErr != nil {
		return submitErr
	}
	if journal.Phase == Failed {
		return unexpectedHTTP(journal.Deletion.HTTPStatus)
	}
	return ErrPending
}

func observe(ctx context.Context, store *journalStore, client *armClient, journal *Journal) error {
	if journal.Phase == Complete {
		return nil
	}
	if journal.Phase == ProbeRejected {
		return finishProbe(ctx, store, client, journal)
	}
	if journal.Phase != DeletePending && journal.Phase != Deleting && journal.Phase != OperationDone {
		return ErrPending
	}
	if err := checkCluster(ctx, client, journal); err != nil {
		return err
	}
	if journal.Phase == DeletePending {
		if journal.Deletion == nil || journal.Deletion.HTTPStatus != http.StatusAccepted || journal.Deletion.OperationURL == "" ||
			!slices.Contains([]string{"azure-async-operation", "location"}, journal.Deletion.PollKind) {
			return ErrPending
		}
		if err := client.operationURL(journal.Deletion.OperationURL, journal.Reservation); err != nil {
			return err
		}
		resumed := *journal
		resumed.Phase = Deleting
		if err := store.save(&resumed); err != nil {
			return err
		}
		*journal = resumed
	}
	if journal.Phase == Deleting {
		status, err := client.poll(ctx, journal)
		if err != nil {
			return err
		}
		journal.Deletion.Status = status
		switch status {
		case "Succeeded":
			journal.Phase = OperationDone
		case "Failed", "Canceled", "Cancelled":
			journal.Phase = Failed
		}
		if err := store.save(journal); err != nil {
			return err
		}
		if journal.Phase != OperationDone {
			return fmt.Errorf("deletion operation status is %s: %w", status, ErrPending)
		}
	}
	result, err := client.request(ctx, http.MethodGet, client.resourceURL(journal.Reservation.PoolID, APIVersion), "", "")
	if err != nil {
		return err
	}
	if !authoritativeAbsent(result) {
		return fmt.Errorf("registered pool is still present, replaced, or its absence is unconfirmed: %w", ErrPending)
	}
	for _, vm := range journal.Machines {
		_, absent, err := client.vm(ctx, vm)
		if err != nil {
			return err
		}
		if !absent {
			return fmt.Errorf("old Compute resource is still present or replaced; no VM is changed: %w", ErrPending)
		}
	}
	if err := checkCluster(ctx, client, journal); err != nil {
		return err
	}
	completed := *journal
	completed.Completion = &Completion{OperationURL: journal.Deletion.OperationURL, ObservedAt: time.Now().UTC(),
		AbsentVMs: slices.Clone(journal.Machines)}
	completed.Phase = Complete
	if err := store.save(&completed); err != nil {
		return err
	}
	*journal = completed
	return nil
}
