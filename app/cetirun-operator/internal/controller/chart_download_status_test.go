// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/helm"
	istiov1beta1 "istio.io/client-go/pkg/apis/networking/v1beta1"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// failingPrepareHelm renders the chart like chartHelm, but fails every chart
// preparation with err while err is set.
type failingPrepareHelm struct {
	chartHelm
	err      error
	prepares int
}

func (h *failingPrepareHelm) Prepare(namespace string, values map[string]any, chartVersion string) (*helm.PreparedRelease, error) {
	h.prepares++
	if h.err != nil {
		return nil, h.err
	}
	return h.chartHelm.Prepare(namespace, values, chartVersion)
}

// The Agent's conditions and Warning event report a chart download failure
// with a fixed message, and other errors with their own text. The phase and
// the reason do not change.
func TestChartDownloadFailureIsReportedWithAFixedMessage(t *testing.T) {
	cause := &url.Error{Op: "Get", URL: "https://storage.example.com/blobs/sha256-0?query=synthetic", Err: errors.New("connection reset by peer")}
	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"chart download": {
			err: &helm.ChartDownloadError{Chart: "oci://registry.example.com/helm/cetirun:1.0.0", Version: "1.0.0",
				Err: fmt.Errorf(`failed to perform "Fetch" on source: %w`, cause)},
			want: "Agent chart 1.0.0 could not be downloaded from the registry; the operator will retry",
		},
		"configuration": {
			err:  errors.New("helm.values.image must use spec.agent.image"),
			want: "helm.values.image must use spec.agent.image",
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, agent, _ := legacyGatewayFixture(t)
			r.HelmClient = &failingPrepareHelm{err: tc.err}
			recorder := record.NewFakeRecorder(100)
			r.EventRecorder = recorder
			ctx := context.Background()
			req := ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}

			_, err := r.Reconcile(ctx, req)
			if err == nil || strings.Contains(err.Error(), "storage.example.com") {
				t.Fatalf("reconcile error = %v", err)
			}
			current := &api.Agent{}
			if err := r.Get(ctx, req.NamespacedName, current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Phase != api.PhaseFailed {
				t.Fatalf("phase = %q, want Failed", current.Status.Phase)
			}
			for _, kind := range []string{api.ConditionAgentConfigReady, api.ConditionReady} {
				condition := meta.FindStatusCondition(current.Status.Conditions, kind)
				if condition == nil || condition.Reason != "AgentConfigInvalid" || condition.Message != tc.want {
					t.Fatalf("%s condition = %+v, want reason AgentConfigInvalid and message %q", kind, condition, tc.want)
				}
			}
			warnings := []string{}
			for len(recorder.Events) > 0 {
				if event := <-recorder.Events; strings.HasPrefix(event, "Warning ") {
					warnings = append(warnings, event)
				}
			}
			if len(warnings) != 1 || warnings[0] != "Warning AgentConfigInvalid "+tc.want {
				t.Fatalf("warning events = %q, want one with message %q", warnings, tc.want)
			}
		})
	}
}

// deployedAgent is a Ready Agent whose chart was installed for its current
// generation, with a chart client whose preparation can be made to fail.
type deployedAgent struct {
	r        *AgentReconciler
	chart    *failingPrepareHelm
	recorder *record.FakeRecorder
	req      ctrl.Request
}

func newDeployedAgent(t *testing.T) *deployedAgent {
	t.Helper()
	r, agent, _ := legacyGatewayFixture(t)
	f := &deployedAgent{r: r, chart: &failingPrepareHelm{}, recorder: record.NewFakeRecorder(100),
		req: ctrl.Request{NamespacedName: types.NamespacedName{Name: agent.Name}}}
	r.HelmClient, r.EventRecorder = f.chart, f.recorder
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(agent.Name), Namespace: instanceNamespace(agent.Name), Generation: 1},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1, UpdatedReplicas: 1, ObservedGeneration: 1},
	}
	if err := r.Create(context.Background(), sts); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4 && f.agent(t).Status.Phase != api.PhaseReady; i++ {
		if _, err := r.Reconcile(context.Background(), f.req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	if phase := f.agent(t).Status.Phase; phase != api.PhaseReady || f.chart.installs != 1 {
		t.Fatalf("phase = %q, installs = %d; want a Ready Agent after one install", phase, f.chart.installs)
	}
	f.warnings()
	return f
}

func (f *deployedAgent) agent(t *testing.T) *api.Agent {
	t.Helper()
	current := &api.Agent{}
	if err := f.r.Get(context.Background(), f.req.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	return current
}

// verificationDue makes the next reconcile take the full path, as the
// periodic AI Gateway verification does every ten minutes.
func (f *deployedAgent) verificationDue(t *testing.T) {
	t.Helper()
	current := f.agent(t)
	current.Status.AIGateway.LastVerifiedTime = &metav1.Time{Time: time.Now().Add(-2 * aiGatewayVerificationInterval)}
	if err := f.r.Status().Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
}

// warnings drains the recorder and returns its Warning events.
func (f *deployedAgent) warnings() []string {
	var warnings []string
	for len(f.recorder.Events) > 0 {
		if event := <-f.recorder.Events; strings.HasPrefix(event, "Warning ") {
			warnings = append(warnings, event)
		}
	}
	return warnings
}

// A periodic full reconcile downloads the Agent's chart again. When that
// download fails while the Agent runs the chart of its current generation,
// the Agent stays Ready and its release untouched, AgentConfigReady reports
// the temporary failure once, and the reconcile is retried until the
// download works again.
func TestChartDownloadFailureKeepsADeployedAgent(t *testing.T) {
	f := newDeployedAgent(t)
	ctx := context.Background()
	f.verificationDue(t)
	f.chart.err = &helm.ChartDownloadError{Chart: "oci://registry.example.com/helm/cetirun:0.1.28", Version: "0.1.28",
		Err: errors.New("synthetic registry outage")}
	want := "Agent chart 0.1.28 could not be downloaded from the registry; the deployed release is unchanged and the operator will retry"

	for pass := 1; pass <= 2; pass++ {
		before := f.agent(t)
		result, err := f.r.Reconcile(ctx, f.req)
		if err != nil || result.RequeueAfter != chartUnavailableRetry {
			t.Fatalf("pass %d: result = %+v, err = %v; want a retry after %s", pass, result, err, chartUnavailableRetry)
		}
		current := f.agent(t)
		if current.Status.Phase != api.PhaseReady || !meta.IsStatusConditionTrue(current.Status.Conditions, api.ConditionReady) {
			t.Fatalf("pass %d: phase = %q, Ready = %+v; want the Agent to stay Ready", pass, current.Status.Phase,
				meta.FindStatusCondition(current.Status.Conditions, api.ConditionReady))
		}
		condition := meta.FindStatusCondition(current.Status.Conditions, api.ConditionAgentConfigReady)
		if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != chartUnavailableReason || condition.Message != want {
			t.Fatalf("pass %d: AgentConfigReady = %+v, want reason %s and message %q", pass, condition, chartUnavailableReason, want)
		}
		warnings := f.warnings()
		if pass == 1 && (len(warnings) != 1 || warnings[0] != "Warning "+chartUnavailableReason+" "+want) {
			t.Fatalf("first failure: warning events = %q, want one %s event", warnings, chartUnavailableReason)
		}
		if pass == 2 && (len(warnings) != 0 || current.ResourceVersion != before.ResourceVersion) {
			t.Fatalf("a repeated failure wrote the status or recorded events again: %q", warnings)
		}
	}
	if f.chart.installs != 1 {
		t.Fatalf("installs = %d; a download failure must not touch the release", f.chart.installs)
	}

	// The download works again: the pass completes, verifies the Agent's
	// Gateway key and reports the configuration as resolved, without
	// reinstalling the chart.
	verified := f.agent(t).Status.AIGateway.LastVerifiedTime.Time
	f.chart.err = nil
	if _, err := f.r.Reconcile(ctx, f.req); err != nil {
		t.Fatal(err)
	}
	current := f.agent(t)
	condition := meta.FindStatusCondition(current.Status.Conditions, api.ConditionAgentConfigReady)
	if current.Status.Phase != api.PhaseReady || condition == nil || condition.Status != metav1.ConditionTrue ||
		!current.Status.AIGateway.LastVerifiedTime.After(verified) || f.chart.installs != 1 {
		t.Fatalf("recovery: phase = %q, AgentConfigReady = %+v, installs = %d", current.Status.Phase, condition, f.chart.installs)
	}
	if warnings := f.warnings(); len(warnings) != 0 {
		t.Fatalf("recovery recorded warnings: %q", warnings)
	}
}

// Registry availability must not freeze the workload's health or suppress retries.
func TestChartUnavailableObservesWorkload(t *testing.T) {
	f := newDeployedAgent(t)
	ctx := context.Background()
	f.verificationDue(t)
	f.chart.err = &helm.ChartDownloadError{Version: "0.1.28", Err: errors.New("synthetic outage")}
	reconcile := func() {
		t.Helper()
		before := f.chart.prepares
		result, err := f.r.Reconcile(ctx, f.req)
		if err != nil || result.RequeueAfter != chartUnavailableRetry || f.chart.prepares <= before {
			t.Fatalf("result=%+v err=%v prepares=%d (before %d)", result, err, f.chart.prepares, before)
		}
		if f.chart.installs != 1 || !chartUnavailable(f.agent(t)) {
			t.Fatal("download failure changed the release or lost its condition")
		}
	}
	reconcile()
	f.warnings()
	// Make verification fresh: the chart-unavailable condition alone must keep
	// retries on the full path, including after the Pod recovers.
	a := f.agent(t)
	a.Status.AIGateway.LastVerifiedTime = &metav1.Time{Time: time.Now()}
	if err := f.r.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	sts := &appsv1.StatefulSet{}
	key := types.NamespacedName{Namespace: instanceNamespace(a.Name), Name: instanceServiceName(a.Name)}
	for _, replicas := range []int32{0, 1} {
		if err := f.r.Get(ctx, key, sts); err != nil {
			t.Fatal(err)
		}
		sts.Status.ReadyReplicas = replicas
		if err := f.r.Status().Update(ctx, sts); err != nil {
			t.Fatal(err)
		}
		reconcile()
		current := f.agent(t)
		wantPhase := api.PhaseProvisioning
		if replicas == 1 {
			wantPhase = api.PhaseReady
		}
		if current.Status.Phase != wantPhase {
			t.Fatalf("phase=%s want %s", current.Status.Phase, wantPhase)
		}
		for _, kind := range []string{api.ConditionPodReady, api.ConditionReady} {
			if meta.IsStatusConditionTrue(current.Status.Conditions, kind) != (replicas == 1) {
				t.Fatalf("%s did not follow ready replicas: %+v", kind, current.Status.Conditions)
			}
		}
		var events []string
		for len(f.recorder.Events) > 0 {
			events = append(events, <-f.recorder.Events)
		}
		if replicas == 0 && len(events) != 0 || replicas == 1 && (len(events) != 1 || !strings.Contains(events[0], "Normal InstanceReady ")) {
			t.Fatalf("health transition events=%v", events)
		}
		reconcile()
		if f.agent(t).ResourceVersion != current.ResourceVersion || len(f.recorder.Events) != 0 {
			t.Fatal("unchanged health wrote status or emitted events")
		}
	}
	f.chart.err = nil
	if _, err := f.r.Reconcile(ctx, f.req); err != nil {
		t.Fatal(err)
	}
	if !meta.IsStatusConditionTrue(f.agent(t).Status.Conditions, api.ConditionAgentConfigReady) || f.chart.installs != 1 {
		t.Fatal("registry recovery did not clear the condition without reinstalling")
	}
}

type unavailableStatefulSetClient struct{ client.Client }

// Count requests, not just resource versions: even a no-op status patch must
// not be used as the polling mechanism while the workload stays unready.
type countingAgentStatusClient struct {
	client.Client
	writes int
}

func (c *countingAgentStatusClient) Status() client.SubResourceWriter {
	return &countingAgentStatusWriter{SubResourceWriter: c.Client.Status(), owner: c}
}

type countingAgentStatusWriter struct {
	client.SubResourceWriter
	owner *countingAgentStatusClient
}

func (w *countingAgentStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	if _, ok := obj.(*api.Agent); ok {
		w.owner.writes++
	}
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

func (w *countingAgentStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if _, ok := obj.(*api.Agent); ok {
		w.owner.writes++
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func TestUnreadyWorkloadKeepsPrerequisitesStable(t *testing.T) {
	f := newDeployedAgent(t)
	ctx := context.Background()
	f.verificationDue(t)
	sts := &appsv1.StatefulSet{}
	key := types.NamespacedName{Namespace: instanceNamespace(f.req.Name), Name: instanceServiceName(f.req.Name)}
	if err := f.r.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	sts.Status.ReadyReplicas = 0
	if err := f.r.Status().Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Reconcile(ctx, f.req); err != nil {
		t.Fatal(err)
	}
	f.warnings()
	before := f.agent(t)
	condition := *meta.FindStatusCondition(before.Status.Conditions, builtInPrerequisitesReady)
	if condition.Status != metav1.ConditionTrue || before.Status.Phase != api.PhaseProvisioning {
		t.Fatalf("not a workload-only wait: %+v", before.Status)
	}
	counter := &countingAgentStatusClient{Client: f.r.Client}
	f.r.Client = counter
	for pass := 0; pass < 4; pass++ {
		result, err := f.r.Reconcile(ctx, f.req)
		if err != nil || result.RequeueAfter != 15*time.Second {
			t.Fatalf("pass %d: result=%+v err=%v", pass, result, err)
		}
		current := f.agent(t)
		if counter.writes != 0 || current.ResourceVersion != before.ResourceVersion ||
			*meta.FindStatusCondition(current.Status.Conditions, builtInPrerequisitesReady) != condition || len(f.recorder.Events) != 0 {
			t.Fatalf("pass %d changed steady wait: writes=%d RV=%s (was %s)", pass, counter.writes, current.ResourceVersion, before.ResourceVersion)
		}
	}
	if f.chart.installs != 1 {
		t.Fatal("workload wait reinstalled the release")
	}
}

func (c unavailableStatefulSetClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*appsv1.StatefulSet); ok {
		return errors.New("synthetic StatefulSet read failure")
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestChartUnavailableCannotReadWorkload(t *testing.T) {
	for _, missing := range []bool{true, false} {
		t.Run(fmt.Sprintf("missing=%t", missing), func(t *testing.T) {
			f := newDeployedAgent(t)
			ctx := context.Background()
			f.verificationDue(t)
			f.chart.err = &helm.ChartDownloadError{Version: "0.1.28", Err: errors.New("synthetic outage")}
			a := f.agent(t)
			if missing {
				sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: instanceServiceName(a.Name), Namespace: instanceNamespace(a.Name)}}
				if err := f.r.Delete(ctx, sts); err != nil {
					t.Fatal(err)
				}
			} else {
				// A healthy steady-state Agent does not need to read its workload.
				// Invalidate the apply cache so this case exercises the read that a
				// chart recovery would require.
				a.Status.ObservedOperatorHash = ""
				if err := f.r.Status().Update(ctx, a); err != nil {
					t.Fatal(err)
				}
				unavailable := unavailableStatefulSetClient{f.r.Client}
				f.r.Client, f.r.APIReader = unavailable, unavailable
			}
			_, err := f.r.Reconcile(ctx, f.req)
			if (err != nil) == missing {
				t.Fatalf("missing=%t err=%v", missing, err)
			}
			a = f.agent(t)
			if a.Status.Phase != api.PhaseProvisioning || !meta.IsStatusConditionFalse(a.Status.Conditions, api.ConditionReady) ||
				!meta.IsStatusConditionFalse(a.Status.Conditions, api.ConditionPodReady) || f.chart.installs != 1 {
				t.Fatalf("unreadable workload retained readiness: %+v", a.Status)
			}
		})
	}
}

func TestChartUnavailablePreservesOtherLifecycleStates(t *testing.T) {
	for _, phase := range []string{api.PhaseSuspended, api.PhaseFailed, api.PhaseProvisioning} {
		t.Run(string(phase), func(t *testing.T) {
			f := newDeployedAgent(t)
			ctx := context.Background()
			a := f.agent(t)
			a.Status.Phase = phase
			setCondition(&a.Status.Conditions, api.ConditionReady, metav1.ConditionFalse, "OtherPrerequisite", "Waiting for another prerequisite")
			if err := f.r.Status().Update(ctx, a); err != nil {
				t.Fatal(err)
			}
			_, err := f.r.preparationFailed(ctx, a, &helm.ChartDownloadError{Version: "0.1.28", Err: errors.New("synthetic outage")})
			if err != nil {
				t.Fatal(err)
			}
			current := f.agent(t)
			if current.Status.Phase != phase || meta.FindStatusCondition(current.Status.Conditions, api.ConditionReady).Reason != "OtherPrerequisite" {
				t.Fatalf("healthy Pod overrode lifecycle state: %+v", current.Status)
			}
		})
	}
}

func TestChartUnavailableKeepsDeployedURL(t *testing.T) {
	f := newDeployedAgent(t)
	ctx := context.Background()
	a := f.agent(t)
	oldURL := a.Status.AccessURL
	key := types.NamespacedName{Namespace: instanceNamespace(a.Name), Name: virtualServiceName(a.Name)}
	before := &istiov1beta1.VirtualService{}
	if err := f.r.Get(ctx, key, before); err != nil {
		t.Fatal(err)
	}
	f.r.DomainSuffix = "changed.example.com"
	f.chart.err = &helm.ChartDownloadError{Version: "0.1.28", Err: errors.New("synthetic outage")}
	for range 2 {
		if _, err := f.r.Reconcile(ctx, f.req); err != nil {
			t.Fatal(err)
		}
		if f.agent(t).Status.AccessURL != oldURL {
			t.Fatal("outage changed the deployed URL")
		}
		after := &istiov1beta1.VirtualService{}
		if err := f.r.Get(ctx, key, after); err != nil {
			t.Fatal(err)
		}
		if after.ResourceVersion != before.ResourceVersion {
			t.Fatal("outage changed the route")
		}
	}
	f.chart.err = nil
	if _, err := f.r.Reconcile(ctx, f.req); err != nil {
		t.Fatal(err)
	}
	after := &istiov1beta1.VirtualService{}
	if err := f.r.Get(ctx, key, after); err != nil {
		t.Fatal(err)
	}
	if f.agent(t).Status.AccessURL == oldURL || after.ResourceVersion == before.ResourceVersion {
		t.Fatal("route recovery did not update the URL and route")
	}
}

func TestChartUnavailableOlderStatusNeedsPrerequisitePass(t *testing.T) {
	f := newDeployedAgent(t)
	ctx := context.Background()
	f.verificationDue(t)
	a := f.agent(t)
	meta.RemoveStatusCondition(&a.Status.Conditions, builtInPrerequisitesReady)
	if err := f.r.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	f.chart.err = &helm.ChartDownloadError{Version: "0.1.28", Err: errors.New("synthetic outage")}
	sts := &appsv1.StatefulSet{}
	key := types.NamespacedName{Namespace: instanceNamespace(a.Name), Name: instanceServiceName(a.Name)}
	for _, replicas := range []int32{0, 1} {
		if err := f.r.Get(ctx, key, sts); err != nil {
			t.Fatal(err)
		}
		sts.Status.ReadyReplicas = replicas
		if err := f.r.Status().Update(ctx, sts); err != nil {
			t.Fatal(err)
		}
		if _, err := f.r.Reconcile(ctx, f.req); err != nil {
			t.Fatal(err)
		}
		if f.agent(t).Status.Phase == api.PhaseReady {
			t.Fatal("older status recovered without prerequisite evidence")
		}
	}
	f.chart.err = nil
	if _, err := f.r.Reconcile(ctx, f.req); err != nil {
		t.Fatal(err)
	}
	if f.agent(t).Status.Phase != api.PhaseReady {
		t.Fatal("successful full pass did not restore readiness")
	}
}

func TestChartUnavailableDoesNotHideLaterIdentityFailure(t *testing.T) {
	f := newDeployedAgent(t)
	ctx := context.Background()
	f.verificationDue(t)
	sts := &appsv1.StatefulSet{}
	key := types.NamespacedName{Namespace: instanceNamespace(f.req.Name), Name: instanceServiceName(f.req.Name)}
	if err := f.r.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	sts.Status.ReadyReplicas = 0
	if err := f.r.Status().Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	outage := &helm.ChartDownloadError{Version: "0.1.28", Err: errors.New("synthetic outage")}
	f.chart.err = outage
	if _, err := f.r.Reconcile(ctx, f.req); err != nil {
		t.Fatal(err)
	}
	if meta.FindStatusCondition(f.agent(t).Status.Conditions, api.ConditionReady).Reason != "PodNotReady" {
		t.Fatal("missing initial workload wait")
	}
	// Exercise the actual identity cleanup lookup after chart recovery. A
	// canonical existing binding requires a directory read even with no spec.
	a := f.agent(t)
	a.Status.AgentIdentity = &api.AgentIdentityStatus{ClientID: testAgentIdentityClientID, ObjectID: testAgentIdentityObjectID}
	if err := f.r.Status().Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	f.r.agentIdentityGraphClientFactory = func() (agentIdentityDirectoryClient, error) { return nil, errors.New("synthetic directory outage") }
	f.chart.err = nil
	if _, err := f.r.Reconcile(ctx, f.req); err != nil {
		t.Fatal(err)
	}
	a = f.agent(t)
	if !meta.IsStatusConditionFalse(a.Status.Conditions, api.ConditionAgentIdentityReady) || !meta.IsStatusConditionFalse(a.Status.Conditions, builtInPrerequisitesReady) {
		t.Fatalf("identity failure not recorded: %+v", a.Status)
	}
	f.warnings()
	if err := f.r.Get(ctx, key, sts); err != nil {
		t.Fatal(err)
	}
	sts.Status.ReadyReplicas = 1
	if err := f.r.Status().Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	f.chart.err = outage
	for range 2 {
		if _, err := f.r.Reconcile(ctx, f.req); err != nil {
			t.Fatal(err)
		}
		a = f.agent(t)
		if a.Status.Phase == api.PhaseReady || !meta.IsStatusConditionFalse(a.Status.Conditions, api.ConditionReady) || !meta.IsStatusConditionTrue(a.Status.Conditions, api.ConditionPodReady) {
			t.Fatalf("Pod recovery hid identity failure: %+v", a.Status)
		}
		for len(f.recorder.Events) > 0 {
			if e := <-f.recorder.Events; strings.Contains(e, "Normal InstanceReady ") {
				t.Fatal(e)
			}
		}
	}
}

// A download failure is temporary only for an Agent whose chart is
// installed for its current generation. Other preparation errors, and a
// download failure after a spec change, still reject the configuration.
func TestDeployedAgentStillRejectsOtherPreparationFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		err        error
		specChange bool
		want       string
	}{
		"configuration error": {
			err:  errors.New("helm.values.image must use spec.agent.image"),
			want: "helm.values.image must use spec.agent.image",
		},
		"download after a spec change": {
			err: &helm.ChartDownloadError{Chart: "oci://registry.example.com/helm/cetirun:0.1.29", Version: "0.1.29",
				Err: errors.New("synthetic registry outage")},
			specChange: true,
			want:       "Agent chart 0.1.29 could not be downloaded from the registry; the operator will retry",
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDeployedAgent(t)
			ctx := context.Background()
			f.verificationDue(t)
			if tc.specChange {
				current := f.agent(t)
				current.Spec.Helm.Version = "0.1.29"
				current.Generation++ // the fake client does not maintain the generation
				if err := f.r.Update(ctx, current); err != nil {
					t.Fatal(err)
				}
			}
			f.chart.err = tc.err
			if _, err := f.r.Reconcile(ctx, f.req); err == nil {
				t.Fatal("the preparation failure was not reported")
			}
			current := f.agent(t)
			condition := meta.FindStatusCondition(current.Status.Conditions, api.ConditionAgentConfigReady)
			if current.Status.Phase != api.PhaseFailed || condition == nil || condition.Reason != "AgentConfigInvalid" || condition.Message != tc.want {
				t.Fatalf("phase = %q, AgentConfigReady = %+v; want Failed with AgentConfigInvalid and message %q",
					current.Status.Phase, condition, tc.want)
			}
		})
	}
}

func TestChartInstalledForGeneration(t *testing.T) {
	installed := metav1.Condition{Type: api.ConditionHelmDeployed, Status: metav1.ConditionTrue, Reason: "ChartInstalled"}
	notInstalled := metav1.Condition{Type: api.ConditionHelmDeployed, Status: metav1.ConditionFalse, Reason: "ChartNotInstalled"}
	for name, tc := range map[string]struct {
		conditions []metav1.Condition
		observed   int64
		want       bool
	}{
		"installed for the current generation": {[]metav1.Condition{installed}, 3, true},
		"installed for an earlier generation":  {[]metav1.Condition{installed}, 2, false},
		"chart not installed":                  {[]metav1.Condition{notInstalled}, 3, false},
		"no HelmDeployed condition":            {nil, 3, false},
	} {
		agent := &api.Agent{
			ObjectMeta: metav1.ObjectMeta{Generation: 3},
			Status:     api.AgentStatus{Conditions: tc.conditions, ObservedGeneration: tc.observed},
		}
		if got := chartInstalledForGeneration(agent); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}
