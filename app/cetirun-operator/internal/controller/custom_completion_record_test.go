// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestValidCustomCompletion(t *testing.T) {
	instance := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "agent-uid", Generation: 2}}
	tests := []struct {
		name   string
		change func(*customCompletion)
		valid  bool
	}{
		{"not-started", func(*customCompletion) {}, true},
		{"unknown", func(c *customCompletion) { c.Outcome = CustomApplyUnknown }, true},
		{"applied", func(c *customCompletion) { c.Outcome = CustomApplySucceeded }, true},
		{"older-generation", func(c *customCompletion) { c.Operation.Generation = 1 }, true},
		{"maximum-id", func(c *customCompletion) { c.Operation.ID = strings.Repeat("a", 128) }, true},
		{"maximum-plan-key", func(c *customCompletion) { c.Operation.PlanKey = strings.Repeat("a", 512) }, true},
		{"wrong-version", func(c *customCompletion) { c.Version = "v2" }, false},
		{"wrong-name", func(c *customCompletion) { c.AgentName = "other" }, false},
		{"wrong-uid", func(c *customCompletion) { c.AgentUID = "other" }, false},
		{"empty-id", func(c *customCompletion) { c.Operation.ID = "" }, false},
		{"long-id", func(c *customCompletion) { c.Operation.ID = strings.Repeat("a", 129) }, false},
		{"empty-plan-key", func(c *customCompletion) { c.Operation.PlanKey = "" }, false},
		{"long-plan-key", func(c *customCompletion) { c.Operation.PlanKey = strings.Repeat("a", 513) }, false},
		{"zero-generation", func(c *customCompletion) { c.Operation.Generation = 0 }, false},
		{"negative-generation", func(c *customCompletion) { c.Operation.Generation = -1 }, false},
		{"carriage-return", func(c *customCompletion) { c.Operation.ID += "\r" }, false},
		{"newline", func(c *customCompletion) { c.Operation.PlanKey += "\n" }, false},
		{"tab", func(c *customCompletion) { c.Operation.ID += "\t" }, false},
		{"null-byte", func(c *customCompletion) { c.Operation.PlanKey += "\x00" }, false},
		{"empty-outcome", func(c *customCompletion) { c.Outcome = "" }, false},
		{"invalid-outcome", func(c *customCompletion) { c.Outcome = "failed" }, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			completion := customCompletion{
				Version: "v1", AgentName: instance.Name, AgentUID: instance.UID,
				Operation: CustomApplyOperation{ID: "operation-id", PlanKey: "plan-key", Generation: instance.Generation},
				Outcome:   CustomApplyNotStarted,
			}
			test.change(&completion)
			if got := validCompletion(completion, instance); got != test.valid {
				t.Fatalf("validCompletion = %v, want %v", got, test.valid)
			}
		})
	}
}

type completionRecordLease struct {
	complete func(CustomApplyOutcome) error
}

func (l completionRecordLease) Check(context.Context) error { return nil }

func (l completionRecordLease) Complete(_ context.Context, outcome CustomApplyOutcome) error {
	return l.complete(outcome)
}

func TestFinishCustomCompletionPreservesRecordBoundaries(t *testing.T) {
	notFound := apierrors.NewNotFound(corev1.Resource("configmaps"), "completion")
	writeFailure := errors.New("synthetic write failure")
	readFailure := errors.New("synthetic read failure")
	tests := []struct {
		name           string
		alreadyApplied bool
		secondRead     string
		finalizerError error
		deleteError    error
		wantError      string
		wantEvents     []string
	}{
		{name: "success", wantEvents: []string{"read", "persist", "read", "complete", "finalize", "delete"}},
		{name: "same-outcome", alreadyApplied: true, wantEvents: []string{"read", "read", "complete", "finalize", "delete"}},
		{name: "missing-before-delivery", secondRead: "missing", wantError: "Custom completion identity changed",
			wantEvents: []string{"read", "persist", "read"}},
		{name: "changed-before-delivery", secondRead: "changed", wantError: "Custom completion identity changed",
			wantEvents: []string{"read", "persist", "read"}},
		{name: "invalid-before-delivery", secondRead: "invalid", wantError: "Custom completion record is invalid",
			wantEvents: []string{"read", "persist", "read"}},
		{name: "read-failure", secondRead: "error", wantError: readFailure.Error(),
			wantEvents: []string{"read", "persist", "read"}},
		{name: "gone-during-finalization", finalizerError: notFound,
			wantEvents: []string{"read", "persist", "read", "complete", "finalize"}},
		{name: "finalization-failure", finalizerError: writeFailure, wantError: writeFailure.Error(),
			wantEvents: []string{"read", "persist", "read", "complete", "finalize"}},
		{name: "already-deleted", deleteError: notFound,
			wantEvents: []string{"read", "persist", "read", "complete", "finalize", "delete"}},
		{name: "delete-failure", deleteError: writeFailure, wantError: writeFailure.Error(),
			wantEvents: []string{"read", "persist", "read", "complete", "finalize", "delete"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			instance := &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "agent-uid", Generation: 2}}
			base := fake.NewClientBuilder().Build()
			r := &AgentReconciler{Client: base}
			operation, err := r.beginCustomCompletion(ctx, instance, "plan-key")
			if err != nil {
				t.Fatal(err)
			}
			if test.alreadyApplied {
				if err := r.recordCustomOutcome(ctx, instance, operation, CustomApplySucceeded); err != nil {
					t.Fatal(err)
				}
			}
			var events []string
			reads := 0
			r.Client = interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					events = append(events, "read")
					reads++
					if err := c.Get(ctx, key, obj, opts...); err != nil {
						return err
					}
					if reads == 2 {
						record := obj.(*corev1.ConfigMap)
						switch test.secondRead {
						case "missing":
							return notFound
						case "changed":
							var completion customCompletion
							if err := json.Unmarshal([]byte(record.Data["completion"]), &completion); err != nil {
								t.Fatal(err)
							}
							completion.Operation.ID = "replacement-operation"
							data, err := json.Marshal(completion)
							if err != nil {
								t.Fatal(err)
							}
							record.Data["completion"] = string(data)
						case "invalid":
							record.Data["completion"] = "{"
						case "error":
							return readFailure
						}
					}
					return nil
				},
				Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if slices.Contains(obj.GetFinalizers(), customCompletionFinalizer) {
						events = append(events, "persist")
					} else {
						events = append(events, "finalize")
						if test.finalizerError != nil {
							return test.finalizerError
						}
					}
					return c.Update(ctx, obj, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					events = append(events, "delete")
					options := (&client.DeleteOptions{}).ApplyOptions(opts)
					preconditions := options.Preconditions
					if preconditions == nil || preconditions.UID == nil || *preconditions.UID != obj.GetUID() ||
						preconditions.ResourceVersion == nil || *preconditions.ResourceVersion != obj.GetResourceVersion() {
						t.Fatal("outbox deletion must match both UID and resource version")
					}
					if test.deleteError != nil {
						return test.deleteError
					}
					return c.Delete(ctx, obj, opts...)
				},
			})
			lease := completionRecordLease{complete: func(outcome CustomApplyOutcome) error {
				events = append(events, "complete")
				if outcome != CustomApplySucceeded {
					t.Fatalf("completion outcome = %q, want %q", outcome, CustomApplySucceeded)
				}
				return nil
			}}
			err = r.finishCustomCompletion(ctx, instance, operation, CustomApplySucceeded, lease)
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || err.Error() != test.wantError {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
			if !slices.Equal(events, test.wantEvents) {
				t.Fatalf("events = %v, want %v", events, test.wantEvents)
			}
		})
	}
}
