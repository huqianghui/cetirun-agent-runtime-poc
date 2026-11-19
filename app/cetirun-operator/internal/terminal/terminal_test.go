// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package terminal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	"github.com/google/cel-go/cel"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/remotecommand"
	clientexec "k8s.io/client-go/util/exec"
)

func testConfig() Config {
	return Config{AgentName: "example", AgentUID: "example-uid", Namespace: "cetirun-example",
		StatefulSet: "openclaw-example-cetirun", Container: "cetirun", Port: 7681, SessionTTL: time.Minute}
}

func testAgent() *api.Agent {
	return &api.Agent{ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "example-uid"},
		Spec: api.AgentSpec{Agent: api.HarnessSpec{Type: api.AgentTypeCustom},
			User: api.UserSpec{Alias: "alice", Email: "alice@example.com"}}}
}

type fakeBackend struct {
	agent   *api.Agent
	resolve func(context.Context, Identity) (Snapshot, error)
	detect  func(context.Context) (string, error)
	exec    func(context.Context, string, Streams) error
}

func (f *fakeBackend) Agent(context.Context) (*api.Agent, error) { return f.agent, nil }
func (f *fakeBackend) Resolve(ctx context.Context, i Identity) (Snapshot, error) {
	return f.resolve(ctx, i)
}
func (f *fakeBackend) DetectShell(ctx context.Context) (string, error) {
	if f.detect != nil {
		return f.detect(ctx)
	}
	return "sh", nil
}
func (f *fakeBackend) Exec(ctx context.Context, shell string, streams Streams) error {
	return f.exec(ctx, shell, streams)
}

func TestConfigRejectsUnboundOrUnsafeTargets(t *testing.T) {
	for _, edit := range []func(*Config){
		func(c *Config) { c.AgentUID = "" }, func(c *Config) { c.Namespace = "../other" },
		func(c *Config) { c.Container = "" }, func(c *Config) { c.Port = 80 },
		func(c *Config) { c.AgentUID = `"><script>` }, func(c *Config) { c.SessionTTL = 2 * time.Hour },
	} {
		c := testConfig()
		edit(&c)
		if c.Validate() == nil {
			t.Fatal("invalid target was accepted")
		}
	}
	if err := testConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	if len(terminalpolicy.WorkloadName(strings.Repeat("a", 63))) > 63 {
		t.Fatal("terminal service name is too long")
	}
}

func TestShellDetectionOrderAndFailures(t *testing.T) {
	for _, want := range []string{"zsh", "bash", "sh", ""} {
		var calls []string
		got, err := DetectShell(context.Background(), func(_ context.Context, command []string) error {
			calls = append(calls, command[0])
			if command[0] == want {
				return nil
			}
			return fmt.Errorf("exec: %q: executable file not found in $PATH", command[0])
		})
		if want == "" {
			if !errors.Is(err, terminalpolicy.ErrNoShell) || !strings.Contains(err.Error(), "没有找到终端") {
				t.Fatal(err)
			}
		} else if err != nil || got != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
		count := map[string]int{"zsh": 1, "bash": 2, "sh": 3, "": 3}[want]
		if !reflect.DeepEqual(calls, []string{"zsh", "bash", "sh"}[:count]) {
			t.Fatal(calls)
		}
	}
	for _, failure := range []error{
		apierrors.NewForbidden(schema.GroupResource{Resource: "pods/exec"}, "pod", errors.New("denied")),
		context.DeadlineExceeded, errors.New("API connection failed"),
		apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "pod"),
	} {
		calls := 0
		_, err := DetectShell(context.Background(), func(context.Context, []string) error { calls++; return failure })
		if err != failure || calls != 1 {
			t.Fatal("authorization/transport failure caused shell fallback")
		}
	}
}

func TestEntryPointFailsWhenNoShellExists(t *testing.T) {
	f := &fakeBackend{agent: testAgent(),
		resolve: func(context.Context, Identity) (Snapshot, error) {
			return Snapshot{PodUID: "pod", ContainerID: "container"}, nil
		},
		detect: func(context.Context) (string, error) { return "", terminalpolicy.ErrNoShell }}
	var ready bool
	err := watchShell(context.Background(), testConfig(), f, time.Millisecond, func(value bool) { ready = value })
	if !errors.Is(err, terminalpolicy.ErrNoShell) || ready {
		t.Fatalf("entrypoint did not reject a shell-less image: %v", err)
	}
}

func TestSessionHandlesAreBoundedOneUseAndExpire(t *testing.T) {
	store := NewSessionStore()
	now := time.Now()
	store.now = func() time.Time { return now }
	value := Session{Identity: Identity{Value: "alice@example.com"}, AgentUID: "example-uid", Expires: now.Add(time.Minute)}
	key, err := store.Create(value)
	if err != nil || len(key) != 22 {
		t.Fatal(key, err)
	}
	claimed, err := store.Claim(key)
	if err != nil || claimed != value {
		t.Fatal(err)
	}
	if _, err := store.Claim(key); !errors.Is(err, ErrDenied) {
		t.Fatal("handle was reusable")
	}
	key, _ = store.Create(value)
	now = now.Add(time.Hour)
	if _, err := store.Claim(key); !errors.Is(err, ErrDenied) {
		t.Fatal("expired handle accepted")
	}
}

func TestAuthorizeRejectsForeignUsersUIDsAndLifecycle(t *testing.T) {
	for _, edit := range []func(*api.Agent){
		func(a *api.Agent) { a.UID = "new-agent" },
		func(a *api.Agent) { a.Spec.Suspended = true },
		func(a *api.Agent) { a.Status.Phase = api.PhaseFailed },
		func(a *api.Agent) { now := metav1.Now(); a.DeletionTimestamp = &now },
		func(a *api.Agent) { a.Spec.Agent.Type = api.AgentTypeHermes },
		func(a *api.Agent) { a.Spec.User.Alias = "bob"; a.Spec.User.Email = "bob@example.com" },
	} {
		agent := testAgent()
		edit(agent)
		if err := Authorize(context.Background(), &fakeBackend{agent: agent}, testConfig(), Identity{Value: "alice@example.com"}); !errors.Is(err, ErrDenied) {
			t.Fatal("foreign or inactive target accepted")
		}
	}
}

func TestAdmissionAllowsOnlyBoundProbesAndInteractiveShells(t *testing.T) {
	env, err := cel.NewEnv(cel.Variable("request", cel.DynType), cel.Variable("object", cel.DynType), cel.Variable("namespaceObject", cel.DynType))
	if err != nil {
		t.Fatal(err)
	}
	expression := terminalpolicy.AdmissionExpression("cetirun-example", "agent-0", "cetirun", "terminal", "agent-uid")
	ast, issues := env.Compile(expression)
	if issues.Err() != nil {
		t.Fatal(issues.Err())
	}
	program, err := env.Program(ast)
	if err != nil {
		t.Fatal(err)
	}
	evaluate := func(command []string, interactive bool, mutate func(map[string]any, map[string]any, map[string]any)) bool {
		t.Helper()
		request := map[string]any{"userInfo": map[string]any{"username": "system:serviceaccount:cetirun-example:terminal"}, "namespace": "cetirun-example", "name": "agent-0"}
		object := map[string]any{"container": "cetirun", "stdout": true, "command": command, "stdin": interactive, "tty": interactive}
		ns := map[string]any{"metadata": map[string]any{"annotations": map[string]any{"cetirun.microsoft.com/agent-uid": "agent-uid"}}}
		if mutate != nil {
			mutate(request, object, ns)
		}
		result, _, err := program.Eval(map[string]any{"request": request, "object": object, "namespaceObject": ns})
		return err == nil && result.Value() == true
	}
	for _, shell := range terminalpolicy.ShellCandidates() {
		if !evaluate(shell.Probe, false, nil) || !evaluate([]string{shell.Name, "-i"}, true, nil) {
			t.Fatal("qualified shell/probe was rejected")
		}
	}
	for _, mutate := range []func(map[string]any, map[string]any, map[string]any){
		func(r, _, _ map[string]any) { r["namespace"] = "other" },
		func(r, _, _ map[string]any) { r["name"] = "terminal-pod" },
		func(r, _, _ map[string]any) { r["userInfo"] = map[string]any{"username": "ordinary-user"} },
		func(_, o, _ map[string]any) { o["container"] = "istio-proxy" },
		func(_, o, _ map[string]any) { o["command"] = []string{"sh", "-c", "arbitrary command"} },
		func(_, o, _ map[string]any) { delete(o, "tty") },
		func(_, _, n map[string]any) {
			n["metadata"] = map[string]any{"annotations": map[string]any{"cetirun.microsoft.com/agent-uid": "replacement"}}
		},
	} {
		if evaluate([]string{"sh", "-i"}, true, mutate) {
			t.Fatal("unbound exec was allowed")
		}
	}
}

type lockedBuffer struct {
	sync.Mutex
	strings.Builder
}

func (b *lockedBuffer) Write(value []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.Builder.Write(value)
}

func TestRunnerReconnectsNewContainerButDoesNotReplayExit(t *testing.T) {
	var mu sync.Mutex
	current := Snapshot{PodUID: "pod-1", ContainerID: "container-1"}
	execs, probes := 0, 0
	dimensions := remotecommand.TerminalSize{Width: 120, Height: 40}
	sizes := &SizeQueue{
		Changes: make(chan remotecommand.TerminalSize, 1),
		Current: func() *remotecommand.TerminalSize { return &dimensions },
	}
	f := &fakeBackend{agent: testAgent()}
	f.resolve = func(context.Context, Identity) (Snapshot, error) { mu.Lock(); defer mu.Unlock(); return current, nil }
	f.detect = func(context.Context) (string, error) { probes++; return "bash", nil }
	f.exec = func(ctx context.Context, shell string, streams Streams) error {
		if shell != "bash" {
			t.Errorf("wrong shell %s", shell)
		}
		execs++
		if got := streams.Sizes.Next(); got == nil || *got != dimensions {
			t.Fatalf("exec has incorrect initial dimensions: %v", got)
		}
		sizes.Changes <- remotecommand.TerminalSize{Width: 80, Height: 24}
		if got := streams.Sizes.Next(); got == nil || *got != dimensions {
			t.Fatalf("exec replayed stale dimensions: %v", got)
		}
		if execs == 1 {
			mu.Lock()
			current = Snapshot{PodUID: "pod-2", ContainerID: "container-2"}
			mu.Unlock()
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	out := &lockedBuffer{}
	runner := NewRunner(f, Streams{In: strings.NewReader(""), Out: out, Sizes: sizes})
	runner.CheckInterval = time.Millisecond
	err := runner.Run(context.Background(), Session{Identity: Identity{Value: "alice"}, Expires: time.Now().Add(time.Second)})
	if err != nil || execs != 2 || probes != 2 {
		t.Fatalf("execs=%d probes=%d err=%v", execs, probes, err)
	}
}

func TestRunnerStopsOnDenialAndNormalExit(t *testing.T) {
	for _, result := range []error{nil, clientexec.CodeExitError{Err: errors.New("exit"), Code: 2}, ErrDenied} {
		execs := 0
		f := &fakeBackend{agent: testAgent(),
			resolve: func(context.Context, Identity) (Snapshot, error) {
				return Snapshot{PodUID: "pod", ContainerID: "container"}, nil
			},
			exec: func(context.Context, string, Streams) error { execs++; return result }}
		runner := NewRunner(f, Streams{In: strings.NewReader(""), Out: io.Discard})
		_ = runner.Run(context.Background(), Session{Identity: Identity{Value: "alice"}, Expires: time.Now().Add(time.Second)})
		if execs != 1 {
			t.Fatal("exit started another exec")
		}
	}
}

func TestRunnerRechecksTargetWhenExecEndsBeforeMonitor(t *testing.T) {
	for _, result := range []error{nil, clientexec.CodeExitError{Err: errors.New("terminated"), Code: 137}, io.EOF} {
		t.Run(fmt.Sprint(result), func(t *testing.T) {
			var mu sync.Mutex
			current := Snapshot{PodUID: "pod-1", ContainerID: "container-1"}
			execs := 0
			f := &fakeBackend{agent: testAgent()}
			f.resolve = func(context.Context, Identity) (Snapshot, error) {
				mu.Lock()
				defer mu.Unlock()
				return current, nil
			}
			f.exec = func(context.Context, string, Streams) error {
				execs++
				if execs == 1 {
					mu.Lock()
					current = Snapshot{PodUID: "pod-2", ContainerID: "container-2"}
					mu.Unlock()
					return result
				}
				return nil
			}
			out := &lockedBuffer{}
			runner := NewRunner(f, Streams{In: strings.NewReader(""), Out: out})
			runner.CheckInterval = time.Hour
			err := runner.Run(context.Background(), Session{Identity: Identity{Value: "alice"}, Expires: time.Now().Add(time.Second)})
			if err != nil || execs != 2 {
				t.Fatalf("replacement treated as a user exit: execs=%d err=%v", execs, err)
			}
			if !strings.Contains(out.String(), "Input is discarded") || !strings.Contains(out.String(), "Opening a new terminal") {
				t.Fatal("reconnection did not explain the lost session and input")
			}
		})
	}
}

func TestRunnerDoesNotReconnectWhenExitTargetCannotBeConfirmed(t *testing.T) {
	for _, failure := range []error{ErrDenied, errors.New("API unavailable")} {
		execs := 0
		var mu sync.Mutex
		exited := false
		f := &fakeBackend{agent: testAgent()}
		f.resolve = func(context.Context, Identity) (Snapshot, error) {
			mu.Lock()
			defer mu.Unlock()
			if exited {
				return Snapshot{}, failure
			}
			return Snapshot{PodUID: "pod", ContainerID: "container"}, nil
		}
		f.exec = func(context.Context, string, Streams) error {
			execs++
			mu.Lock()
			exited = true
			mu.Unlock()
			return nil
		}
		runner := NewRunner(f, Streams{In: strings.NewReader(""), Out: io.Discard})
		runner.CheckInterval = time.Hour
		err := runner.Run(context.Background(), Session{Identity: Identity{Value: "alice"}, Expires: time.Now().Add(time.Second)})
		if err == nil || execs != 1 {
			t.Fatalf("unconfirmed target after exit: execs=%d err=%v", execs, err)
		}
	}
}

func TestSizeQueueUsesLatestDimensions(t *testing.T) {
	want := remotecommand.TerminalSize{Width: 120, Height: 40}
	queue := &SizeQueue{
		Changes: make(chan remotecommand.TerminalSize, 1),
		Current: func() *remotecommand.TerminalSize { return &want },
	}
	queue.Changes <- remotecommand.TerminalSize{Width: 80, Height: 24}
	if got := queue.Next(); got == nil || *got != want {
		t.Fatalf("resize replayed stale dimensions: %v, want %v", got, want)
	}
	close(queue.Changes)
	if got := queue.Next(); got != nil {
		t.Fatalf("closed resize queue returned %v", got)
	}
}
