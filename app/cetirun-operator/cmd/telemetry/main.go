// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Command telemetry writes CetiRun inventory and Kubernetes event records to
// standard output, one JSON object per line, for a log collector. The
// cetirun-observability chart runs it with read-only access to events and to
// the CetiRun resources; ADX-Mon stores its lines in the Telemetry table.
//
// Inventory records map each Agent to its namespace, owner, Team and policy,
// which the Agent's Pods do not carry. Logs and metrics are attributed through
// them by namespace.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/buildinfo"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/telemetry"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

type options struct {
	events           telemetry.EventFilter
	snapshotInterval time.Duration
	healthAddress    string
	metricsAddress   string
}

func parse(args []string) (options, error) {
	var o options
	var events string
	fs := flag.NewFlagSet("telemetry", flag.ContinueOnError)
	fs.StringVar(&events, "events", string(telemetry.EventsWarningsAndCetiRun),
		"events to write: all, warnings-and-cetirun (warnings, and events about CetiRun resources or in cetirun- namespaces) or none")
	fs.DurationVar(&o.snapshotInterval, "snapshot-interval", time.Hour,
		"how often every Agent, Team, AgentPolicy and Sandbox is written again")
	fs.StringVar(&o.healthAddress, "health-address", ":8081", "health probe address")
	fs.StringVar(&o.metricsAddress, "metrics-address", "0", "metrics address; 0 disables")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() != 0 {
		return o, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	o.events = telemetry.EventFilter(events)
	if !o.events.Valid() {
		return o, fmt.Errorf("--events must be all, warnings-and-cetirun or none")
	}
	if o.snapshotInterval < time.Minute {
		return o, fmt.Errorf("--snapshot-interval must be at least 1m")
	}
	return o, nil
}

func main() {
	// Diagnostics go to standard error; standard output holds only records.
	ctrl.SetLogger(zap.New(zap.WriteTo(os.Stderr)))
	logger := ctrl.Log.WithName("telemetry")
	o, err := parse(os.Args[1:])
	if err != nil {
		logger.Error(err, "invalid configuration")
		os.Exit(2)
	}
	if err := run(ctrl.SetupSignalHandler(), o); err != nil {
		logger.Error(err, "the telemetry program stopped")
		os.Exit(1)
	}
}

func run(ctx context.Context, o options) error {
	logger := ctrl.Log.WithName("telemetry")
	logger.Info("starting", "version", buildinfo.Version, "commit", buildinfo.Commit,
		"events", o.events, "snapshotInterval", o.snapshotInterval)
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := api.AddToScheme(scheme); err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: o.healthAddress,
		Metrics:                metricsserver.Options{BindAddress: o.metricsAddress},
		// Records need only metadata and a few spec and status fields.
		Cache: cache.Options{DefaultTransform: cache.TransformStripManagedFields()},
	})
	if err != nil {
		return err
	}
	recorder := &telemetry.Recorder{Writer: telemetry.NewWriter(os.Stdout), Events: o.events, Log: logger}
	if err := recorder.Register(ctx, mgr.GetCache()); err != nil {
		return err
	}
	if err := mgr.Add(runnableFunc(func(ctx context.Context) error {
		return recorder.RunRegistration(ctx, mgr.GetCache(), time.Minute)
	})); err != nil {
		return err
	}
	if err := mgr.Add(runnableFunc(func(ctx context.Context) error {
		return recorder.RunSnapshots(ctx, mgr.GetClient(), o.snapshotInterval)
	})); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	return mgr.Start(ctx)
}

type runnableFunc func(context.Context) error

func (f runnableFunc) Start(ctx context.Context) error { return f(ctx) }
