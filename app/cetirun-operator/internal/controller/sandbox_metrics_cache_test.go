// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	"github.com/prometheus/client_golang/prometheus"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	cachetesting "k8s.io/client-go/tools/cache/testing"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

func TestSandboxMetricsMainRegistration(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "../../cmd/main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var body *ast.BlockStmt
	for _, declaration := range file.Decls {
		if function, ok := declaration.(*ast.FuncDecl); ok && function.Name.Name == "main" {
			body = function.Body
		}
	}
	if body == nil {
		t.Fatal("operator main function is missing")
	}
	namedCall := func(node ast.Node, pkg, name string) *ast.CallExpr {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return nil
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != name {
			return nil
		}
		qualifier, ok := selector.X.(*ast.Ident)
		if !ok || qualifier.Name != pkg {
			return nil
		}
		return call
	}
	stringValue := func(expression ast.Expr, want string) bool {
		literal, ok := expression.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return false
		}
		value, err := strconv.Unquote(literal.Value)
		return err == nil && value == want
	}
	var registrations, guardedRegistrations, defaultFlags int
	ast.Inspect(body, func(node ast.Node) bool {
		if namedCall(node, "controller", "RegisterSandboxMetrics") != nil {
			registrations++
		}
		if call := namedCall(node, "flag", "StringVar"); call != nil && len(call.Args) == 4 {
			pointer, ok := call.Args[0].(*ast.UnaryExpr)
			if ok && pointer.Op == token.AND {
				variable, ok := pointer.X.(*ast.Ident)
				if ok && variable.Name == "metricsAddr" &&
					stringValue(call.Args[1], "metrics-bind-address") && stringValue(call.Args[2], ":8080") {
					defaultFlags++
				}
			}
		}
		branch, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		comparison, ok := branch.Cond.(*ast.BinaryExpr)
		if !ok || comparison.Op != token.NEQ || !stringValue(comparison.Y, "0") {
			return true
		}
		variable, ok := comparison.X.(*ast.Ident)
		if !ok || variable.Name != "metricsAddr" {
			return true
		}
		ast.Inspect(branch.Body, func(child ast.Node) bool {
			if namedCall(child, "controller", "RegisterSandboxMetrics") != nil {
				guardedRegistrations++
			}
			return true
		})
		return true
	})
	if registrations != 1 || guardedRegistrations != 1 || defaultFlags != 1 {
		t.Fatalf("metrics startup must honor disabled address 0 and default :8080: %d registrations, %d guarded, %d defaults",
			registrations, guardedRegistrations, defaultFlags)
	}
}

type sandboxMetricsTestManager struct {
	manager.Manager
	source    cache.Cache
	runnables []manager.Runnable
	addErr    error
}

func (m *sandboxMetricsTestManager) GetCache() cache.Cache { return m.source }

func (m *sandboxMetricsTestManager) Add(runnable manager.Runnable) error {
	if m.addErr != nil {
		return m.addErr
	}
	m.runnables = append(m.runnables, runnable)
	return nil
}

func sandboxMetricsRegisteredCollector(t *testing.T, source cache.Cache) (*prometheus.Registry, *sandboxMetricsCollector) {
	t.Helper()
	registry := prometheus.NewPedanticRegistry()
	mgr := &sandboxMetricsTestManager{source: source}
	if err := RegisterSandboxMetrics(registry, mgr); err != nil {
		t.Fatal(err)
	}
	if len(mgr.runnables) != 1 {
		t.Fatalf("registered %d startup initializers, want 1", len(mgr.runnables))
	}
	collector, ok := mgr.runnables[0].(*sandboxMetricsCollector)
	if !ok {
		t.Fatalf("unexpected startup initializer %T", mgr.runnables[0])
	}
	leaderRunnable, ok := mgr.runnables[0].(manager.LeaderElectionRunnable)
	if !ok || leaderRunnable.NeedLeaderElection() {
		t.Fatal("metrics cache initialization must also run on nonleaders")
	}
	return registry, collector
}

type sandboxMetricsTestInformer struct{ cache.Informer }

func sandboxMetricsInformerKind(object runtime.Object) (string, error) {
	switch object.(type) {
	case *api.Agent:
		return "agents", nil
	case *api.Team:
		return "teams", nil
	case *api.Sandbox:
		return "sandboxes", nil
	default:
		return "", fmt.Errorf("unexpected informer object %T", object)
	}
}

func (c *sandboxMetricsTestCache) GetInformer(ctx context.Context, object client.Object, opts ...cache.InformerGetOption) (cache.Informer, error) {
	kind, err := sandboxMetricsInformerKind(object)
	if err != nil {
		return nil, err
	}
	var options cache.InformerGetOptions
	for _, option := range opts {
		option(&options)
	}
	if options.BlockUntilSynced == nil || *options.BlockUntilSynced {
		return nil, fmt.Errorf("startup must initialize informers without waiting for synchronization")
	}
	c.mu.Lock()
	c.informerCalls = append(c.informerCalls, sandboxMetricsCacheCall{kind, ctx, time.Now()})
	failure, block := c.informerFailures[kind], c.informerBlock == kind
	entered, release := c.informerEntered, c.informerRelease
	c.mu.Unlock()
	if block {
		select {
		case <-entered:
		default:
			close(entered)
		}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if failure != nil {
		return nil, failure
	}
	c.mu.Lock()
	c.informers[kind] = true
	c.mu.Unlock()
	return &sandboxMetricsTestInformer{}, nil
}

func startSandboxMetricsForTest(t *testing.T, collector *sandboxMetricsCollector) (func(), <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var runErr error
	go func() {
		runErr = collector.Start(ctx)
		close(done)
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
			if runErr != nil {
				t.Errorf("metrics initializer failed: %v", runErr)
			}
		case <-time.After(5 * time.Second):
			t.Error("metrics initializer did not stop")
		}
	}
	t.Cleanup(stop)
	return stop, done
}

func awaitSandboxMetricsInitialization(t *testing.T, collector *sandboxMetricsCollector) {
	t.Helper()
	if err := wait.PollUntilContextTimeout(context.Background(), time.Millisecond,
		sandboxMetricsInitializationRetryDelay+sandboxMetricsCollectionTimeout+time.Second, true,
		func(context.Context) (bool, error) { return collector.initialized.Load(), nil }); err != nil {
		t.Fatal("metrics initializer did not complete:", err)
	}
}

func TestSandboxMetricsStartupGatesScrapes(t *testing.T) {
	agent, team, box := sandboxMetricsFixture(t, "example", api.SandboxKata)
	source := newSandboxMetricsTestCache(t, agent, team, box)
	source.informerBlock = "teams"
	source.informerEntered, source.informerRelease = make(chan struct{}), make(chan struct{})
	registry, collector := sandboxMetricsRegisteredCollector(t, source)
	for range 3 {
		assertSandboxMetrics(t, registry, nil, 0)
	}
	if len(source.informerCalls) != 0 || len(source.calls) != 0 {
		t.Fatal("registration or cold scrapes initialized/read the cache")
	}
	stop, _ := startSandboxMetricsForTest(t, collector)
	select {
	case <-source.informerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("startup did not begin informer initialization")
	}
	for range 3 {
		assertSandboxMetrics(t, registry, nil, 0)
	}
	source.mu.Lock()
	readCalls := len(source.calls)
	informerCalls := len(source.informerCalls)
	source.mu.Unlock()
	if readCalls != 0 || informerCalls != 2 {
		t.Fatalf("partial startup allowed scrape-driven reads: %d reads, %d informer calls", readCalls, informerCalls)
	}
	close(source.informerRelease)
	awaitSandboxMetricsInitialization(t, collector)
	assertSandboxMetrics(t, registry, map[string]float64{"kata/True/PolicyApplied": 1}, 1)
	stop()
	assertSandboxMetrics(t, registry, nil, 0)
	var kinds []string
	for _, call := range source.informerCalls {
		kinds = append(kinds, call.kind)
		deadline, ok := call.ctx.Deadline()
		if !ok || deadline.Sub(call.at) <= 0 || deadline.Sub(call.at) > sandboxMetricsCollectionTimeout ||
			!errors.Is(call.ctx.Err(), context.Canceled) {
			t.Fatalf("startup initialization context was not bounded and released: %+v", call)
		}
	}
	if !reflect.DeepEqual(kinds, []string{"agents", "teams", "sandboxes"}) || len(source.calls) != 4 {
		t.Fatalf("unexpected startup/scrape calls: %v, %+v", kinds, source.calls)
	}
}

func TestSandboxMetricsStartupStaticFailures(t *testing.T) {
	t.Run("nil cache", func(t *testing.T) {
		collector := newSandboxMetricsCollector(nil)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := collector.Start(ctx); err == nil {
			t.Fatal("missing cache did not return a static configuration error")
		}
		registry := prometheus.NewPedanticRegistry()
		if err := registry.Register(collector); err != nil {
			t.Fatal(err)
		}
		assertSandboxMetrics(t, registry, nil, 0)
	})
	t.Run("manager registration failure", func(t *testing.T) {
		source := newSandboxMetricsTestCache(t)
		failure := errors.New("synthetic manager registration failure")
		mgr := &sandboxMetricsTestManager{source: source, addErr: failure}
		registry := prometheus.NewPedanticRegistry()
		if err := RegisterSandboxMetrics(registry, mgr); !errors.Is(err, failure) {
			t.Fatalf("manager registration error was not propagated: %v", err)
		}
		assertSandboxMetrics(t, registry, nil, 0)
		if len(source.calls) != 0 || len(source.informerCalls) != 0 {
			t.Fatal("failed registration caused cache activity")
		}
	})
}

func sandboxMetricsInitializationLogs(collector *sandboxMetricsCollector) (<-chan string, *atomic.Int64) {
	logs := make(chan string, 8)
	failures := &atomic.Int64{}
	collector.logger = funcr.New(func(prefix, message string) {
		if strings.Contains(message, "Unable to initialize Sandbox metrics cache; retrying") {
			failures.Add(1)
			select {
			case logs <- prefix + message:
			default:
			}
		}
	}, funcr.Options{})
	return logs, failures
}

func awaitSandboxMetricsInitializationFailure(t *testing.T, logs <-chan string, done <-chan struct{}) string {
	t.Helper()
	select {
	case message := <-logs:
		return message
	case <-done:
		t.Fatal("metrics initialization failure stopped the manager runnable")
	case <-time.After(sandboxMetricsInitializationRetryDelay + sandboxMetricsCollectionTimeout + time.Second):
		t.Fatal("metrics initialization failure was not logged")
	}
	return ""
}

func TestSandboxMetricsStartupRetries(t *testing.T) {
	for index, stage := range []string{"agents", "teams", "sandboxes"} {
		t.Run(stage, func(t *testing.T) {
			t.Parallel()
			agent, team, box := sandboxMetricsFixture(t, "example", api.SandboxKata)
			source := newSandboxMetricsTestCache(t, agent, team, box)
			before := source.objects()
			source.informerFailures = map[string]error{stage: errors.New("synthetic informer initialization failure")}
			registry, collector := sandboxMetricsRegisteredCollector(t, source)
			logs, failures := sandboxMetricsInitializationLogs(collector)
			stop, done := startSandboxMetricsForTest(t, collector)
			wantFailures := 1
			if stage == "teams" {
				wantFailures = 2
			}
			for attempt := 1; attempt <= wantFailures; attempt++ {
				message := awaitSandboxMetricsInitializationFailure(t, logs, done)
				if !strings.Contains(message, "synthetic informer initialization failure") || !strings.Contains(message, "retryAfter") {
					t.Fatalf("initialization failure lacks error or retry information: %s", message)
				}
				for range 3 {
					assertSandboxMetrics(t, registry, nil, 0)
				}
				if body := sandboxMetricsExposition(t, registry); strings.Contains(body, "synthetic informer initialization failure") {
					t.Fatal("initialization error leaked into metrics exposition")
				}
				source.mu.Lock()
				informerCalls, readCalls := len(source.informerCalls), len(source.calls)
				source.mu.Unlock()
				if collector.initialized.Load() || readCalls != 0 || informerCalls != attempt*(index+1) ||
					failures.Load() != int64(attempt) {
					t.Fatalf("failed initialization caused reads or an unbounded retry loop: %d informer calls, %d reads, %d failures",
						informerCalls, readCalls, failures.Load())
				}
				select {
				case <-done:
					t.Fatal("operational metrics failure stopped the runnable")
				default:
				}
			}
			source.mu.Lock()
			source.informerFailures = nil
			source.mu.Unlock()
			awaitSandboxMetricsInitialization(t, collector)
			for range 3 {
				assertSandboxMetrics(t, registry, map[string]float64{"kata/True/PolicyApplied": 1}, 1)
			}
			stop()
			assertSandboxMetrics(t, registry, nil, 0)
			if len(source.informerCalls) != wantFailures*(index+1)+3 || failures.Load() != int64(wantFailures) {
				t.Fatalf("recovery did not reuse the same runnable and registry: %d informer calls, %d failures",
					len(source.informerCalls), failures.Load())
			}
			for attempt := range wantFailures {
				next := (attempt + 1) * (index + 1)
				previous, retry := source.informerCalls[next-1], source.informerCalls[next]
				if retry.at.Sub(previous.at) < sandboxMetricsInitializationRetryDelay || retry.ctx == previous.ctx {
					t.Fatal("retry did not use the bounded delay and a fresh attempt context")
				}
			}
			for _, call := range source.informerCalls {
				deadline, ok := call.ctx.Deadline()
				if !ok || deadline.Sub(call.at) <= 0 || deadline.Sub(call.at) > sandboxMetricsCollectionTimeout ||
					!errors.Is(call.ctx.Err(), context.Canceled) {
					t.Fatalf("initialization attempt context was not bounded and released: %+v", call)
				}
			}
			if !reflect.DeepEqual(before, source.objects()) {
				t.Fatal("initialization retries changed cached objects")
			}
		})
	}
}

func TestSandboxMetricsStartupAttemptDeadline(t *testing.T) {
	t.Parallel()
	agent, team, box := sandboxMetricsFixture(t, "example", api.SandboxKata)
	source := newSandboxMetricsTestCache(t, agent, team, box)
	source.informerBlock = "teams"
	source.informerEntered, source.informerRelease = make(chan struct{}), make(chan struct{})
	registry, collector := sandboxMetricsRegisteredCollector(t, source)
	logs, failures := sandboxMetricsInitializationLogs(collector)
	stop, done := startSandboxMetricsForTest(t, collector)
	awaitSandboxMetricsInitializationFailure(t, logs, done)
	assertSandboxMetrics(t, registry, nil, 0)
	source.mu.Lock()
	firstAttempt := append([]sandboxMetricsCacheCall(nil), source.informerCalls...)
	source.mu.Unlock()
	if len(firstAttempt) != 2 || !errors.Is(firstAttempt[0].ctx.Err(), context.DeadlineExceeded) ||
		firstAttempt[0].ctx != firstAttempt[1].ctx {
		t.Fatal("informer initialization did not share and expire its attempt context")
	}
	close(source.informerRelease)
	awaitSandboxMetricsInitialization(t, collector)
	assertSandboxMetrics(t, registry, map[string]float64{"kata/True/PolicyApplied": 1}, 1)
	stop()
	if failures.Load() != 1 {
		t.Fatalf("timeout recovery logged %d failures, want 1", failures.Load())
	}
}

func TestSandboxMetricsStartupCancellation(t *testing.T) {
	t.Run("before startup", func(t *testing.T) {
		source := newSandboxMetricsTestCache(t)
		registry, collector := sandboxMetricsRegisteredCollector(t, source)
		_, failures := sandboxMetricsInitializationLogs(collector)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := collector.Start(ctx); err != nil {
			t.Fatalf("shutdown cancellation returned a runnable error: %v", err)
		}
		assertSandboxMetrics(t, registry, nil, 0)
		if len(source.informerCalls) != 0 || len(source.calls) != 0 || failures.Load() != 0 {
			t.Fatal("cancelled startup performed work or logged a failure")
		}
	})
	for _, phase := range []string{"informer initialization", "retry delay"} {
		t.Run(phase, func(t *testing.T) {
			source := newSandboxMetricsTestCache(t)
			source.informerEntered, source.informerRelease = make(chan struct{}), make(chan struct{})
			if phase == "informer initialization" {
				source.informerBlock = "teams"
			} else {
				source.informerFailures = map[string]error{"agents": errors.New("synthetic initialization failure")}
			}
			registry, collector := sandboxMetricsRegisteredCollector(t, source)
			logs, failures := sandboxMetricsInitializationLogs(collector)
			stop, done := startSandboxMetricsForTest(t, collector)
			wantFailures := int64(0)
			if phase == "informer initialization" {
				select {
				case <-source.informerEntered:
				case <-time.After(5 * time.Second):
					t.Fatal("initialization did not reach the blocked informer")
				}
			} else {
				awaitSandboxMetricsInitializationFailure(t, logs, done)
				wantFailures = 1
			}
			start := time.Now()
			stop()
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("shutdown waited for initialization or retry delay: %v", elapsed)
			}
			assertSandboxMetrics(t, registry, nil, 0)
			if collector.initialized.Load() || len(source.calls) != 0 || failures.Load() != wantFailures {
				t.Fatal("shutdown cancellation caused reads, readiness or an extra failure")
			}
		})
	}
}

type sandboxMetricsInformerSource struct {
	*cachetesting.FakeControllerSource
	lists   atomic.Int64
	watches atomic.Int64
}

func (s *sandboxMetricsInformerSource) List(options metav1.ListOptions) (runtime.Object, error) {
	s.lists.Add(1)
	return s.FakeControllerSource.List(options)
}

func (s *sandboxMetricsInformerSource) Watch(options metav1.ListOptions) (watch.Interface, error) {
	s.watches.Add(1)
	return s.FakeControllerSource.Watch(options)
}

func (*sandboxMetricsInformerSource) IsWatchListSemanticsUnSupported() bool { return true }

type sandboxMetricsRejectHTTP struct{ calls atomic.Int64 }

func (s *sandboxMetricsRejectHTTP) RoundTrip(*http.Request) (*http.Response, error) {
	s.calls.Add(1)
	return nil, errors.New("unexpected HTTP request from offline cache test")
}

func TestSandboxMetricsRealCacheWithoutLeaderControllers(t *testing.T) {
	for _, preexistingAgentInformer := range []bool{false, true} {
		t.Run(fmt.Sprintf("preexisting-agent-informer=%t", preexistingAgentInformer), func(t *testing.T) {
			agent, team, box := sandboxMetricsFixture(t, "example", api.SandboxKata)
			sources := make(map[string]*sandboxMetricsInformerSource)
			scheme := runtime.NewScheme()
			if err := api.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{api.GroupVersion})
			for _, object := range []client.Object{agent, team, box} {
				kind, err := sandboxMetricsInformerKind(object)
				if err != nil {
					t.Fatal(err)
				}
				source := &sandboxMetricsInformerSource{FakeControllerSource: cachetesting.NewFakeControllerSource()}
				source.Add(object.DeepCopyObject())
				t.Cleanup(source.Shutdown)
				sources[kind] = source
				gvks, _, err := scheme.ObjectKinds(object)
				if err != nil || len(gvks) != 1 {
					t.Fatalf("fixture object kind: %v, %v", gvks, err)
				}
				mapper.Add(gvks[0], apimeta.RESTScopeRoot)
			}
			var constructions atomic.Int64
			noHTTP := &sandboxMetricsRejectHTTP{}
			source, err := cache.New(&rest.Config{Host: "https://api.example.com"}, cache.Options{
				Scheme: scheme, Mapper: mapper, HTTPClient: &http.Client{Transport: noHTTP},
				NewInformer: func(_ toolscache.ListerWatcher, object runtime.Object, resync time.Duration, indexers toolscache.Indexers) toolscache.SharedIndexInformer {
					kind, err := sandboxMetricsInformerKind(object)
					if err != nil {
						panic(err)
					}
					constructions.Add(1)
					return toolscache.NewSharedIndexInformer(sources[kind], object, resync, indexers)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if preexistingAgentInformer {
				if _, err := source.GetInformer(context.Background(), &api.Agent{}, cache.BlockUntilSynced(false)); err != nil {
					t.Fatal(err)
				}
			}
			beforeStartup := constructions.Load()
			registry, collector := sandboxMetricsRegisteredCollector(t, source)
			ctx, cancel := context.WithCancel(context.Background())
			cacheDone := make(chan error, 1)
			go func() { cacheDone <- source.Start(ctx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-cacheDone:
					if err != nil {
						t.Errorf("cache failed: %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("cache did not stop")
				}
			})
			for range 3 {
				assertSandboxMetrics(t, registry, nil, 0)
			}
			if constructions.Load() != beforeStartup {
				t.Fatal("cold scrapes created an informer in the real cache")
			}
			// No leader controllers or watches are installed in this test.
			// Only the registered nonleader initializer can create missing types.
			startSandboxMetricsForTest(t, collector)
			awaitSandboxMetricsInitialization(t, collector)
			syncCtx, syncCancel := context.WithTimeout(ctx, 5*time.Second)
			defer syncCancel()
			if !source.WaitForCacheSync(syncCtx) {
				t.Fatal("initialized cache did not synchronize")
			}
			if err := wait.PollUntilContextTimeout(ctx, time.Millisecond, 5*time.Second, true,
				func(context.Context) (bool, error) {
					for _, source := range sources {
						if source.watches.Load() != 1 {
							return false, nil
						}
					}
					return true, nil
				}); err != nil {
				t.Fatal("startup list/watch did not complete:", err)
			}
			for range 10 {
				assertSandboxMetrics(t, registry, map[string]float64{"kata/True/PolicyApplied": 1}, 1)
				sandboxMetricsExposition(t, registry)
			}
			if constructions.Load() != 3 || noHTTP.calls.Load() != 0 {
				t.Fatalf("unexpected informer or HTTP activity: %d constructions, %d HTTP requests", constructions.Load(), noHTTP.calls.Load())
			}
			for kind, source := range sources {
				if source.lists.Load() != 1 || source.watches.Load() != 1 {
					t.Errorf("scrapes triggered new list/watch work for %s: %d lists, %d watches", kind, source.lists.Load(), source.watches.Load())
				}
			}
		})
	}
}
