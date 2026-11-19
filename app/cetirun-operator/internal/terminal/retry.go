// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package terminal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/remotecommand"
	clientexec "k8s.io/client-go/util/exec"
)

type Runner struct {
	Backend        Backend
	Gate           *InputGate
	Streams        Streams
	CheckInterval  time.Duration
	RetryMin       time.Duration
	RetryMax       time.Duration
	RequestTimeout time.Duration
}

func NewRunner(backend Backend, streams Streams) *Runner {
	return &Runner{
		Backend: backend, Streams: streams, Gate: &InputGate{},
		CheckInterval: 3 * time.Second, RetryMin: time.Second, RetryMax: 10 * time.Second,
		RequestTimeout: 10 * time.Second,
	}
}

func denied(err error) bool {
	return errors.Is(err, ErrDenied) || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err)
}

func (r *Runner) resolve(ctx context.Context, identity Identity) (Snapshot, error) {
	request, cancel := context.WithTimeout(ctx, r.RequestTimeout)
	defer cancel()
	return r.Backend.Resolve(request, identity)
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (r *Runner) Run(ctx context.Context, session Session) error {
	ctx, cancel := context.WithDeadline(ctx, session.Expires)
	defer cancel()
	defer r.Gate.Close()
	r.Gate.Bind(r.Streams.In)
	go r.Gate.Pump(r.Streams.In)
	delay := r.RetryMin
	waiting := false
	markWaiting := func() {
		if !waiting {
			fmt.Fprintln(r.Streams.Out, "\r\nWaiting for the agent. Input is discarded while reconnecting.")
			waiting = true
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		target, err := r.resolve(ctx, session.Identity)
		if denied(err) {
			return ErrDenied
		}
		if err != nil {
			markWaiting()
			if err := wait(ctx, delay); err != nil {
				return err
			}
			delay = min(delay*2, r.RetryMax)
			continue
		}
		probe, cancelProbe := context.WithTimeout(ctx, r.RequestTimeout)
		shell, err := r.Backend.DetectShell(probe)
		cancelProbe()
		if errors.Is(err, terminalpolicy.ErrNoShell) || denied(err) {
			return err
		}
		if err != nil {
			if err := wait(ctx, delay); err != nil {
				return err
			}
			delay = min(delay*2, r.RetryMax)
			continue
		}
		// A container may be replaced during shell probing.
		current, err := r.resolve(ctx, session.Identity)
		if denied(err) {
			return ErrDenied
		}
		if err != nil || current != target {
			continue
		}
		if waiting {
			fmt.Fprintln(r.Streams.Out, "\r\nAgent ready. Opening a new terminal; previous commands are not replayed.")
		}
		waiting = false
		delay = r.RetryMin
		execContext, stop := context.WithCancelCause(ctx)
		monitorDone := make(chan struct{})
		go r.monitor(execContext, stop, target, session.Identity, monitorDone)
		reader := r.Gate.Start()
		sizes := r.Streams.Sizes
		if queue, ok := sizes.(*SizeQueue); ok {
			next := &SizeQueue{Changes: queue.Changes, Done: execContext.Done(), Current: queue.Current}
			if queue.Current != nil {
				next.Initial = queue.Current()
			}
			sizes = next
		}
		err = r.Backend.Exec(execContext, shell, Streams{In: reader, Out: r.Streams.Out, Sizes: sizes})
		cause := context.Cause(execContext)
		stop(context.Canceled)
		_ = reader.Close()
		r.Gate.Stop()
		<-monitorDone
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if denied(err) || denied(cause) {
			return ErrDenied
		}
		if errors.Is(cause, ErrRestarted) {
			markWaiting()
			continue
		}
		// Pod termination can return an exit code before the monitor observes it.
		// Recheck the target before distinguishing replacement from a user exit.
		current, resolveErr := r.resolve(ctx, session.Identity)
		if denied(resolveErr) {
			return ErrDenied
		}
		if errors.Is(resolveErr, ErrWaiting) || (resolveErr == nil && current != target) {
			markWaiting()
			continue
		}
		var exit clientexec.ExitError
		if cause == nil && resolveErr == nil && (err == nil || errors.As(err, &exit)) {
			return err
		}
		// An ambiguous transport failure must not duplicate a live exec.
		return fmt.Errorf("terminal connection ended; reconnect to open a new terminal")
	}
}

func (r *Runner) monitor(ctx context.Context, cancel context.CancelCauseFunc, target Snapshot, identity Identity, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(r.CheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current, err := r.resolve(ctx, identity)
			switch {
			case denied(err):
				cancel(ErrDenied)
			case errors.Is(err, ErrWaiting), err == nil && current != target:
				cancel(ErrRestarted)
			case err != nil:
				cancel(err) // Authorization cannot be assumed during an API outage.
			default:
				continue
			}
			return
		}
	}
}

type SizeQueue struct {
	Changes chan remotecommand.TerminalSize
	Done    <-chan struct{}
	Current func() *remotecommand.TerminalSize
	Initial *remotecommand.TerminalSize
}

func (q *SizeQueue) Next() *remotecommand.TerminalSize {
	if q.Initial != nil {
		value := q.Initial
		q.Initial = nil
		return value
	}
	select {
	case <-q.Done:
		return nil
	case size, ok := <-q.Changes:
		if !ok {
			return nil
		}
		// The queue coalesces resize events; read the current PTY dimensions
		// instead of replaying a size buffered before a reconnect or resize.
		if q.Current != nil {
			if current := q.Current(); current != nil {
				return current
			}
		}
		return &size
	}
}
