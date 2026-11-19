// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-operator/internal/cetiadmin"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestCetiAdminDisabledInstallPublishesHookCheckpoint(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.config.Enabled = false
	f.updateConfig()
	for range 3 {
		if err := f.run(); err != nil {
			t.Fatal(err)
		}
	}
	state := f.state()
	wanted, _ := cetiadmin.ConfigHash(f.config)
	if state.Phase != cetiadmin.PhaseDisabled || state.ConfigHash != wanted || state.Configuration.Enabled || state.Agent.UID != "" || state.Team.UID != "" || len(state.Resources) != 0 {
		t.Fatal("disabled installation lacks a current zero-instance checkpoint")
	}
	agents := &api.AgentList{}
	if err := f.kube.List(f.ctx, agents); err != nil || len(agents.Items) != 0 {
		t.Fatalf("disabled install created Agents: %v", err)
	}
	op := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cetirun-operator", Namespace: f.source.Namespace}, Spec: appsv1.DeploymentSpec{Replicas: ptr.To(int32(1)), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "manager", Image: f.r.OperatorImage}}}}}}
	f.seed(op)
	op.Status = appsv1.DeploymentStatus{ObservedGeneration: op.Generation, Replicas: 1, UpdatedReplicas: 1, ReadyReplicas: 1, AvailableReplicas: 1}
	if err := f.kube.Status().Update(f.ctx, op); err != nil {
		t.Fatal(err)
	}
	source := &corev1.ConfigMap{}
	if err := f.kube.Get(f.ctx, client.ObjectKeyFromObject(f.source), source); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(source.Data[cetiadmin.ConfigKey]))
	if err := cetiadmin.RunHook(f.ctx, f.kube, cetiadmin.HookOptions{Namespace: source.Namespace, ConfigMapName: source.Name, ExpectedConfigHash: hex.EncodeToString(hash[:]), Mode: cetiadmin.HookModeCheck, Timeout: 100 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
}

func TestCetiAdminDisableRecordsCurrentIntentAfterStop(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.converge()
	before := f.state().Record
	f.config.Enabled = false
	f.updateConfig()
	for range 3 {
		if err := f.run(); err != nil {
			t.Fatal(err)
		}
	}
	state := f.state()
	want, _ := cetiadmin.ConfigHash(f.config)
	if state.Phase != cetiadmin.PhaseDisabled || state.ConfigHash != want || state.Configuration.Enabled || state.Record.Agent != before.Agent || state.Namespace != before.Namespace {
		t.Fatal("disable checkpoint is stale or changed retained identities")
	}
}

func TestCetiAdminReenableAfterMinimalDisabledValuesKeepsIdentity(t *testing.T) {
	f := newCetiAdminFixture(t)
	f.converge()
	enabled := f.config
	identity := f.state().Agent
	f.config = cetiadmin.DefaultConfig()
	f.config.Enabled = false
	f.updateConfig()
	for range 3 {
		if err := f.run(); err != nil {
			t.Fatal(err)
		}
	}
	f.config = enabled
	f.updateConfig()
	f.converge()
	if f.state().Agent != identity {
		t.Fatal("reenabling replaced the retained Agent identity")
	}
}
