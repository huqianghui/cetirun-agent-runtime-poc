// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package terminal

import (
	"context"
	"fmt"
	"io"
	"net/http"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

type Snapshot struct {
	PodUID      types.UID
	ContainerID string
}

type Streams struct {
	In    io.Reader
	Out   io.Writer
	Sizes remotecommand.TerminalSizeQueue
}

type Backend interface {
	AgentReader
	Resolve(context.Context, Identity) (Snapshot, error)
	DetectShell(context.Context) (string, error)
	Exec(context.Context, string, Streams) error
}

type Kubernetes struct {
	Config Config
	REST   *rest.Config
	Core   kubernetes.Interface
	Agents dynamic.Interface
}

func NewKubernetes(cfg Config, restConfig *rest.Config) (*Kubernetes, error) {
	core, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	agents, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	return &Kubernetes{Config: cfg, REST: restConfig, Core: core, Agents: agents}, nil
}

func (k *Kubernetes) Agent(ctx context.Context) (*api.Agent, error) {
	object, err := k.Agents.Resource(schema.GroupVersionResource{
		Group: api.GroupVersion.Group, Version: api.GroupVersion.Version, Resource: "agents",
	}).Get(ctx, k.Config.AgentName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, ErrDenied
	}
	if err != nil {
		return nil, err
	}
	agent := &api.Agent{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, agent); err != nil {
		return nil, fmt.Errorf("invalid Agent response")
	}
	return agent, nil
}

func controllerIs(refs []metav1.OwnerReference, kind string, uid types.UID) bool {
	for _, ref := range refs {
		version := "apps/v1"
		if kind == "Agent" {
			version = api.GroupVersion.String()
		}
		if ref.Controller != nil && *ref.Controller && ref.Kind == kind && ref.APIVersion == version && ref.UID == uid {
			return true
		}
	}
	return false
}

func (k *Kubernetes) Resolve(ctx context.Context, identity Identity) (Snapshot, error) {
	if err := Authorize(ctx, k, k.Config, identity); err != nil {
		return Snapshot{}, err
	}
	sts, err := k.Core.AppsV1().StatefulSets(k.Config.Namespace).Get(ctx, k.Config.StatefulSet, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Snapshot{}, ErrWaiting
	}
	if err != nil {
		return Snapshot{}, err
	}
	if !controllerIs(sts.OwnerReferences, "Agent", types.UID(k.Config.AgentUID)) ||
		sts.Annotations["cetirun.microsoft.com/agent-uid"] != k.Config.AgentUID {
		return Snapshot{}, ErrDenied
	}
	if !sts.DeletionTimestamp.IsZero() || sts.Spec.Replicas == nil || *sts.Spec.Replicas != 1 {
		return Snapshot{}, ErrWaiting
	}
	pod, err := k.Core.CoreV1().Pods(k.Config.Namespace).Get(ctx, k.Config.PodName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Snapshot{}, ErrWaiting
	}
	if err != nil {
		return Snapshot{}, err
	}
	if !controllerIs(pod.OwnerReferences, "StatefulSet", sts.UID) {
		return Snapshot{}, ErrDenied
	}
	if !pod.DeletionTimestamp.IsZero() || pod.Status.Phase != corev1.PodRunning {
		return Snapshot{}, ErrWaiting
	}
	for _, container := range pod.Status.ContainerStatuses {
		if container.Name == k.Config.Container && container.Ready &&
			container.State.Running != nil && container.ContainerID != "" && pod.UID != "" {
			return Snapshot{PodUID: pod.UID, ContainerID: container.ContainerID}, nil
		}
	}
	return Snapshot{}, ErrWaiting
}

func (k *Kubernetes) DetectShell(ctx context.Context) (string, error) {
	return DetectShell(ctx, func(ctx context.Context, command []string) error {
		return k.execute(ctx, command, Streams{Out: io.Discard}, false)
	})
}

func (k *Kubernetes) Exec(ctx context.Context, shell string, streams Streams) error {
	command, err := terminalpolicy.ShellCommand(shell)
	if err != nil {
		return err
	}
	return k.execute(ctx, command, streams, true)
}

func (k *Kubernetes) execute(ctx context.Context, command []string, streams Streams, interactive bool) error {
	request := k.Core.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(k.Config.Namespace).Name(k.Config.PodName()).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: k.Config.Container,
			Command:   command,
			Stdin:     interactive, Stdout: true, Stderr: !interactive, TTY: interactive,
		}, scheme.ParameterCodec)
	ws, err := remotecommand.NewWebSocketExecutor(k.REST, http.MethodGet, request.URL().String())
	if err != nil {
		return err
	}
	spdy, err := remotecommand.NewSPDYExecutor(k.REST, http.MethodPost, request.URL())
	if err != nil {
		return err
	}
	executor, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return err
	}
	options := remotecommand.StreamOptions{
		Stdin: streams.In, Stdout: streams.Out, Tty: interactive, TerminalSizeQueue: streams.Sizes,
	}
	if !interactive {
		options.Stderr = io.Discard
	}
	return executor.StreamWithContext(ctx, options)
}
