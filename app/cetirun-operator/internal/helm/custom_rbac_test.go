// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"context"
	"errors"
	"testing"
	"time"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

func waitForRBACGrant(ctx context.Context, interval time.Duration, review func(context.Context) (*authorizationv1.SubjectAccessReview, error)) (*authorizationv1.SubjectAccessReview, error) {
	var last *authorizationv1.SubjectAccessReview
	err := wait.PollUntilContextCancel(ctx, interval, true, func(ctx context.Context) (bool, error) {
		var err error
		last, err = review(ctx)
		if err != nil {
			return false, err
		}
		if last.Status.EvaluationError != "" {
			return false, errors.New(last.Status.EvaluationError)
		}
		return last.Status.Allowed, nil
	})
	return last, err
}

func TestWaitForRBACGrant(t *testing.T) {
	t.Run("waits for the binding to reach the authorizer", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		calls := 0
		grant, err := waitForRBACGrant(ctx, time.Millisecond, func(context.Context) (*authorizationv1.SubjectAccessReview, error) {
			calls++
			return &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{Allowed: calls == 3}}, nil
		})
		if err != nil || !grant.Status.Allowed || calls != 3 {
			t.Fatalf("grant=%+v, err=%v, calls=%d", grant, err, calls)
		}
	})
	t.Run("missing permission still fails within the deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		grant, err := waitForRBACGrant(ctx, time.Millisecond, func(context.Context) (*authorizationv1.SubjectAccessReview, error) {
			return &authorizationv1.SubjectAccessReview{}, nil
		})
		if !errors.Is(err, context.DeadlineExceeded) || grant == nil || grant.Status.Allowed {
			t.Fatalf("grant=%+v, err=%v", grant, err)
		}
	})
	for _, evaluation := range []bool{false, true} {
		name := "API errors are not retried"
		if evaluation {
			name = "evaluation errors are not retried"
		}
		t.Run(name, func(t *testing.T) {
			calls := 0
			_, err := waitForRBACGrant(context.Background(), time.Millisecond, func(context.Context) (*authorizationv1.SubjectAccessReview, error) {
				calls++
				if evaluation {
					return &authorizationv1.SubjectAccessReview{Status: authorizationv1.SubjectAccessReviewStatus{EvaluationError: "synthetic failure"}}, nil
				}
				return nil, errors.New("synthetic failure")
			})
			if err == nil || err.Error() != "synthetic failure" || calls != 1 {
				t.Fatalf("err=%v, calls=%d", err, calls)
			}
		})
	}
}
