// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxcoordination"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestCLIDeleteAndObservationRecovery(t *testing.T) {
	for _, outcome := range []string{"success", "lost reply", "large replacement", "not committed"} {
		t.Run(outcome, func(t *testing.T) {
			args, store, storage, before, _ := cliFixture(t)
			args = args[:len(args)-2] // --after is forbidden for deletion.
			deletes := 0
			writer := interceptor.NewClient(storage, interceptor.Funcs{
				Delete: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.DeleteOption) error {
					deletes++
					if outcome == "not committed" {
						return errors.New(syntheticBody + " sk-dummy")
					}
					if err := c.Delete(ctx, object, opts...); err != nil {
						return err
					}
					if outcome == "lost reply" || outcome == "large replacement" {
						return errors.New(syntheticBody + " sk-dummy")
					}
					return nil
				},
			})
			code, status, message := invoke(t, append(args, "--action", "delete"), injectedClients(writer))
			if deletes != 1 || status.Operation == "" || status.Phase != sandboxcoordination.Changing {
				t.Fatal("deletion was not one-shot with recorded submission intent")
			}
			if outcome == "success" {
				if code != 0 || status.Status != "deletion-accepted" {
					t.Fatal("delete failed:", message)
				}
				if _, err := store.ReadConfigurationDeletion(context.Background(), before); !errors.Is(err, sandboxcoordination.ErrUnavailable) {
					t.Fatal("accepted deletion retained reservation:", err)
				}
				return
			}
			if code != 1 || status.Status != "uncertain" || !strings.Contains(message, deletionObservationInstruction) {
				t.Fatal("unknown Delete did not give the correct recovery instructions")
			}
			var replacement *api.Team
			if outcome == "large replacement" {
				replacement = &api.Team{
					ObjectMeta: metav1.ObjectMeta{Name: before.Name, UID: "replacement-uid"},
					Spec: api.TeamSpec{MemberContext: api.TeamContextSpec{
						Role: strings.Repeat("x", 270000),
					}},
				}
				if err := storage.Create(context.Background(), replacement); err != nil {
					t.Fatal(err)
				}
			}
			writes := 0
			observer := interceptor.NewClient(storage, interceptor.Funcs{
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					writes++
					return errors.New("observation must not write")
				},
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					writes++
					return errors.New("observation must not delete")
				},
			})
			code, observed, _ := invoke(t, append(args, "--action", "observe-delete"), injectedClients(observer))
			if writes != 0 || observed.Operation != status.Operation {
				t.Fatal("observe-delete changed state or selected a different operation")
			}
			if outcome != "not committed" {
				if code != 0 || observed.Status != "deletion-accepted-reservation-retained" {
					t.Fatal("deleted UID not observed")
				}
			} else if code != 1 || observed.Status != "uncertain" {
				t.Fatal("unchanged object declared completed")
			}
			if _, err := store.ReadConfigurationDeletion(context.Background(), before); err != nil {
				t.Fatal("observation released reservation:", err)
			}
			code, recovered, _ := invoke(t, append(args, "--action", "delete"), injectedClients(writer))
			if deletes != 1 || recovered.Operation != status.Operation {
				t.Fatal("recovery replayed or substituted the deletion")
			}
			if (code == 0) != (outcome != "not committed") {
				t.Fatal("unexpected recovery result")
			}
			if replacement != nil {
				current := &api.Team{}
				if err := storage.Get(context.Background(), client.ObjectKeyFromObject(replacement), current); err != nil ||
					current.UID != replacement.UID || current.ResourceVersion != replacement.ResourceVersion ||
					current.Spec.MemberContext.Role != replacement.Spec.MemberContext.Role {
					t.Fatal("recovery modified the replacement:", err)
				}
			}
		})
	}
}

func TestCLIDeletionRecoveryByOperationID(testContext *testing.T) {
	for _, scenario := range []string{"committed", "lost reply", "crash", "superseded"} {
		testContext.Run(scenario, func(testContext *testing.T) {
			args, store, storage, before, _ := cliFixture(testContext)
			deleteArgs := args[:len(args)-2]
			deletes := 0
			writer := interceptor.NewClient(storage, interceptor.Funcs{
				Delete: func(ctx context.Context, client client.WithWatch, object client.Object, options ...client.DeleteOption) error {
					deletes++
					if err := client.Delete(ctx, object, options...); err != nil {
						return err
					}
					if scenario == "lost reply" {
						return errors.New("lost reply")
					}
					return nil
				},
			})
			var operation string
			switch scenario {
			case "lost reply":
				code, status, message := invoke(testContext, append(deleteArgs, "--action", "delete"), injectedClients(writer))
				if code != 1 || status.Status != "uncertain" || !strings.Contains(message, deletionObservationInstruction) {
					testContext.Fatal("lost reply did not retain its deletion operation:", message)
				}
				operation = status.Operation
			case "committed":
				ticket, err := store.AcquireConfigurationDeletion(context.Background(), before)
				if err != nil {
					testContext.Fatal(err)
				}
				ticket, err = store.BeginChange(context.Background(), ticket)
				if err != nil {
					testContext.Fatal(err)
				}
				adapter, err := sandboxcoordination.NewKubernetesDeleteAdapter(writer, storage, before)
				if err != nil {
					testContext.Fatal(err)
				}
				if err := adapter.Submit(context.Background(), ticket, func(ctx context.Context) error {
					return store.CheckChange(ctx, ticket)
				}); err != nil {
					testContext.Fatal(err)
				}
				operation = ticket.Change.ID
			case "crash", "superseded":
				ticket, err := store.AcquireConfigurationDeletion(context.Background(), before)
				if err != nil {
					testContext.Fatal(err)
				}
				ticket, err = store.BeginChange(context.Background(), ticket)
				if err != nil {
					testContext.Fatal(err)
				}
				if scenario == "superseded" {
					current := before.DeepCopy()
					current.Annotations = map[string]string{"example.com/observed": "true"}
					if err := storage.Update(context.Background(), current); err != nil {
						testContext.Fatal(err)
					}
				} else {
					if err := writer.Delete(context.Background(), before); err != nil {
						testContext.Fatal(err)
					}
				}
				operation = ticket.Change.ID
			}
			beforePath := deleteArgs[len(deleteArgs)-1]
			if err := os.Remove(beforePath); err != nil {
				testContext.Fatal(err)
			}
			recoveryArgs := append(append([]string{}, deleteArgs[:len(deleteArgs)-2]...), "--operation-id", operation)
			observer := interceptor.NewClient(storage, interceptor.Funcs{
				Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
					return errors.New("operation-id observation must not write")
				},
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					return errors.New("operation-id observation must not delete")
				},
			})
			wantStatus, wantDeletes := "deletion-accepted", 1
			if scenario == "superseded" {
				wantStatus, wantDeletes = "deletion-superseded", 0
			}
			code, observed, message := invoke(testContext, append(recoveryArgs, "--action", "observe-delete"), injectedClients(observer))
			if code != 0 || observed.Status != wantStatus+"-reservation-retained" ||
				observed.Operation != operation || observed.Source.Name != before.Name || observed.Source.UID != before.UID {
				testContext.Fatal("operation-id observation did not retain its recorded deletion:", message)
			}
			if _, err := store.ReadConfigurationDeletion(context.Background(), before); err != nil {
				testContext.Fatal("operation-id observation released the deletion reservation:", err)
			}
			code, recovered, message := invoke(testContext, append(recoveryArgs, "--action", "delete"), injectedClients(writer))
			if code != 0 || recovered.Status != wantStatus || recovered.Operation != operation || deletes != wantDeletes {
				testContext.Fatal("operation-id recovery resubmitted or did not release the deletion:", message)
			}
			if _, err := store.ReadConfigurationDeletion(context.Background(), before); !errors.Is(err, sandboxcoordination.ErrUnavailable) {
				testContext.Fatal("operation-id recovery retained the released deletion:", err)
			}
		})
	}
}

func TestCLIDeletionOperationIDRejectsInvalidTicket(testContext *testing.T) {
	for _, scenario := range []string{"missing", "update", "reserved", "wrong authority"} {
		testContext.Run(scenario, func(testContext *testing.T) {
			args, store, storage, before, after := cliFixture(testContext)
			var operation string
			switch scenario {
			case "update":
				ticket, err := store.AcquireConfigurationChange(context.Background(), before, after)
				if err != nil {
					testContext.Fatal(err)
				}
				ticket, err = store.BeginChange(context.Background(), ticket)
				if err != nil {
					testContext.Fatal(err)
				}
				operation = ticket.Change.ID
			case "reserved", "wrong authority":
				ticket, err := store.AcquireConfigurationDeletion(context.Background(), before)
				if err != nil {
					testContext.Fatal(err)
				}
				if scenario == "wrong authority" {
					ticket, err = store.BeginChange(context.Background(), ticket)
					if err != nil {
						testContext.Fatal(err)
					}
				}
				operation = ticket.Change.ID
			default:
				operation = "missing-operation"
			}
			operationArgs := append(append([]string{}, args[:len(args)-4]...), "--operation-id", operation, "--action", "observe-delete")
			if scenario == "wrong authority" {
				for index, argument := range operationArgs {
					if argument == "--authority-uid" {
						operationArgs[index+1] = "wrong-authority-uid"
					}
				}
			}
			deletes := 0
			writer := interceptor.NewClient(storage, interceptor.Funcs{
				Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
					deletes++
					return errors.New("invalid operation must not delete")
				},
			})
			code, status, message := invoke(testContext, operationArgs, injectedClients(writer))
			if code != 1 || status.Status != "unavailable" || deletes != 0 || !strings.Contains(message, deletionObservationInstruction) {
				testContext.Fatal("invalid operation-id ticket was accepted:", message)
			}
		})
	}
}

func TestCLIDeleteRequiresExplicitActionAndForbidsAfter(t *testing.T) {
	args, store, storage, before, _ := cliFixture(t)
	deleteArgs := args[:len(args)-2]
	operationArgs := append(append([]string{}, deleteArgs[:len(deleteArgs)-2]...), "--operation-id", "recorded-operation")
	for _, invalid := range [][]string{
		deleteArgs,
		append(append([]string{}, args...), "--action", "delete"),
		append(append([]string{}, args...), "--action", "observe-delete"),
		append(append([]string{}, deleteArgs...), "--operation-id", "recorded-operation", "--action", "delete"),
		append(append([]string{}, operationArgs...), "--after", args[len(args)-1], "--action", "delete"),
		append(append([]string{}, operationArgs...), "--after", args[len(args)-1], "--action", "observe"),
	} {
		calls := 0
		factory := func(options) (client.Client, client.Reader, error) {
			calls++
			return storage, storage, nil
		}
		code, _, _ := invoke(t, invalid, factory)
		if code != 2 || calls != 0 {
			t.Fatal("ambiguous deletion flags reached client setup")
		}
	}
	code, _, message := invoke(t, append(args[:len(args)-2], "--action", "observe-delete"), injectedClients(storage))
	if code != 1 || !strings.Contains(message, deletionObservationInstruction) {
		t.Fatal("unrecorded deletion was not reported explicitly")
	}
	if _, err := store.ReadConfigurationDeletion(context.Background(), before); !errors.Is(err, sandboxcoordination.ErrUnavailable) {
		t.Fatal("observe-delete created a reservation")
	}
}

func TestCLIDeleteAcceptancePreservesFinalizers(t *testing.T) {
	args, _, storage, before, _ := cliFixture(t)
	before.Finalizers = []string{"example.com/retain"}
	if err := storage.Update(context.Background(), before); err != nil {
		t.Fatal(err)
	}
	before.TypeMeta = metav1.TypeMeta{APIVersion: api.GroupVersion.String(), Kind: "Team"}
	for i, arg := range args {
		if arg == "--before" {
			writeObjectFile(t, args[i+1], before)
		}
	}
	code, status, message := invoke(t, append(args[:len(args)-2], "--action", "delete"), injectedClients(storage))
	if code != 0 || status.Status != "deletion-accepted" {
		t.Fatal("terminating deletion was not accepted:", message)
	}
	current := &api.Team{}
	if err := storage.Get(context.Background(), client.ObjectKeyFromObject(before), current); err != nil ||
		current.DeletionTimestamp.IsZero() || len(current.Finalizers) != 1 {
		t.Fatal("CLI deleted/finalized a retained object:", err)
	}
}

func TestCLITransportDoesNotRetryADelete(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.Method != http.MethodDelete {
			t.Error("unexpected method:", request.Method)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Retry-After", "1")
		writer.WriteHeader(http.StatusTooManyRequests)
		if err := json.NewEncoder(writer).Encode(metav1.Status{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure,
			Reason: metav1.StatusReasonTooManyRequests, Code: http.StatusTooManyRequests,
		}); err != nil {
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
	err = writer.Delete(ctx, &api.Team{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "team-uid", ResourceVersion: "1"}})
	if err == nil || requests.Load() != 1 || apierrors.IsNotFound(err) {
		t.Fatalf("automatic Delete retry reached HTTP: requests=%d error=%v", requests.Load(), err)
	}
}
