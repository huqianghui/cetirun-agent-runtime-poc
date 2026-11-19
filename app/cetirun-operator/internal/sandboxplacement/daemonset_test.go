// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func TestDaemonSetCreatesRequireTrustedCreatorBeforeScheduling(t *testing.T) {
	for _, node := range []string{"", "reserved-worker", "ordinary-worker"} {
		for _, creator := range []string{"system:kube-controller-manager", "system:kube-scheduler", "example-user", ""} {
			t.Run(node+"/"+creator, func(t *testing.T) {
				f := newFixture(t)
				pod := f.platformPod.DeepCopy()
				pod.Spec.NodeName = node
				if node != "" {
					pod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = targetAffinity(node)
				}
				request := podRequest(t, pod, nil)
				request.UserInfo.Username = creator
				response := f.handler.Validate(context.Background(), request)
				if creator == f.registry.PlatformDaemonSets[0].TrustedCreator {
					expectAllowed(t, response)
				} else {
					expectDenied(t, response, "DaemonSetCreatorMismatch")
				}
			})
		}
	}
	f := newFixture(t)
	request := podRequest(t, f.platformPod, nil)
	request.UserInfo.Username = f.registry.PlatformDaemonSets[0].TrustedCreator
	request.UserInfo.Groups = []string{"system:unauthenticated"}
	expectDenied(t, f.handler.Validate(context.Background(), request), "DaemonSetCreatorMismatch")
}

func TestReplacementDaemonPodNeedsManualUIDEnrollment(t *testing.T) {
	f := newFixture(t)
	replacement := f.platformPod.DeepCopy()
	replacement.Name, replacement.UID, replacement.ResourceVersion = f.daemon.Name+"-hjkmp", "", ""
	request := podRequest(t, replacement, nil)
	request.UserInfo.Username = f.registry.PlatformDaemonSets[0].TrustedCreator
	expectAllowed(t, f.handler.Validate(context.Background(), request))
	replacement.UID = "replacement-pod-uid"
	if err := f.client.Create(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	expectDenied(t, f.handler.Validate(context.Background(), bindingRequest(t, replacement, f.reserved.Name)), "PlatformPodNotEnrolled")
	f.registry.PlatformDaemonSets[0].BootstrapPods = append(f.registry.PlatformDaemonSets[0].BootstrapPods,
		sandboxworkload.ObjectIdentity{Name: replacement.Name, UID: replacement.UID})
	f.saveRegistry(t)
	// The scheduler is not the recorded controller creator. Its identity does
	// not substitute for the authoritative, explicitly enrolled Pod UID.
	expectAllowed(t, f.handler.Validate(context.Background(), bindingRequest(t, replacement, f.reserved.Name)))
	replacement.Spec.NodeName = f.reserved.Name
	if err := f.client.Update(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	expectAllowed(t, f.handler.Validate(context.Background(), podRequest(t, replacement, replacement)))
	f.registry.PlatformDaemonSets[0].BootstrapPods = f.registry.PlatformDaemonSets[0].BootstrapPods[:1]
	f.saveRegistry(t)
	expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, replacement, replacement)), "PlatformPodNotEnrolled")
}

func TestEnrollmentDoesNotWaiveControllerTemplateOrServiceAccount(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Pod){
		"forged owner":          func(p *corev1.Pod) { p.OwnerReferences[0].UID = "forged-daemon-uid" },
		"no owner":              func(p *corev1.Pod) { p.OwnerReferences = nil },
		"noncontroller owner":   func(p *corev1.Pod) { p.OwnerReferences[0].Controller = ptr.To(false) },
		"noncanonical owner":    func(p *corev1.Pod) { p.OwnerReferences[0].BlockOwnerDeletion = nil },
		"wrong controller API":  func(p *corev1.Pod) { p.OwnerReferences[0].APIVersion = "apps/v2" },
		"same UID other image":  func(p *corev1.Pod) { p.Spec.Containers[0].Image = "registry.example.com/other:1.0" },
		"wrong ServiceAccount":  func(p *corev1.Pod) { p.Spec.ServiceAccountName = "different-account" },
		"credentials injection": func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = ptr.To(true) },
		"host network changed":  func(p *corev1.Pod) { p.Spec.HostNetwork = true },
		"extra label":           func(p *corev1.Pod) { p.Labels["example.com/extra"] = "true" },
		"extra annotation":      func(p *corev1.Pod) { p.Annotations = map[string]string{"example.com/extra": "true"} },
		"extra finalizer":       func(p *corev1.Pod) { p.Finalizers = []string{"example.com/finalizer"} },
		"missing revision":      func(p *corev1.Pod) { delete(p.Labels, appsv1.ControllerRevisionHashLabelKey) },
		"extra volume": func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "injected", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		},
		"extra helper": func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "unregistered", Image: "registry.example.com/other:1.0"})
		},
		"ephemeral containers": func(p *corev1.Pod) {
			p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "unregistered", Image: "registry.example.com/other:1.0"}}}
		},
		"unknown affinity": func(p *corev1.Pod) {
			p.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms =
				append(p.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms, corev1.NodeSelectorTerm{})
		},
		"no affinity": func(p *corev1.Pod) { p.Spec.Affinity = nil },
		"wrong target": func(p *corev1.Pod) {
			p.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = targetAffinity("ordinary-worker")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			pod := f.platformPod.DeepCopy()
			mutate(pod)
			if err := f.client.Update(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			expectDenied(t, f.handler.Validate(context.Background(), bindingRequest(t, pod, f.reserved.Name)), "")
			pod.Spec.NodeName = f.reserved.Name
			if err := f.client.Update(context.Background(), pod); err != nil {
				t.Fatal(err)
			}
			for _, subresource := range []string{"", "ephemeralcontainers", "resize"} {
				request := podRequest(t, pod, pod)
				request.SubResource = subresource
				expectDenied(t, f.handler.Validate(context.Background(), request), "")
			}
		})
	}
}

func TestRegisteredDaemonTemplateAndLiveIdentityArePinned(t *testing.T) {
	for name, mutate := range map[string]func(client.Object){
		"namespace replaced": func(o client.Object) {
			if _, ok := o.(*corev1.Namespace); ok {
				o.SetUID("different-namespace-uid")
			}
		},
		"DaemonSet replaced": func(o client.Object) {
			if _, ok := o.(*appsv1.DaemonSet); ok {
				o.SetUID("different-daemon-uid")
			}
		},
		"DaemonSet terminating": func(o client.Object) {
			if _, ok := o.(*appsv1.DaemonSet); ok {
				o.SetDeletionTimestamp(ptr.To(metav1.Now()))
			}
		},
		"DaemonSet lacks version": func(o client.Object) {
			if _, ok := o.(*appsv1.DaemonSet); ok {
				o.SetResourceVersion("")
			}
		},
		"same UID template drift": func(o client.Object) {
			if d, ok := o.(*appsv1.DaemonSet); ok {
				d.Spec.Template.Spec.Containers[0].Image = "registry.example.com/changed:1.0"
			}
		},
		"same UID selector drift": func(o client.Object) {
			if d, ok := o.(*appsv1.DaemonSet); ok {
				d.Spec.Selector.MatchExpressions = []metav1.LabelSelectorRequirement{{Key: "other", Operator: metav1.LabelSelectorOpDoesNotExist}}
			}
		},
		"ServiceAccount replaced during lookup": func(o client.Object) {
			if _, ok := o.(*corev1.ServiceAccount); ok {
				o.SetUID("")
			}
		},
		"Node no longer eligible": func(o client.Object) {
			if n, ok := o.(*corev1.Node); ok {
				n.Labels["example.com/platform"] = "false"
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.handler.reader = mutateReader{Reader: f.client, mutate: mutate}
			request := podRequest(t, f.platformPod, nil)
			request.UserInfo.Username = f.registry.PlatformDaemonSets[0].TrustedCreator
			expectDenied(t, f.handler.Validate(context.Background(), request), "")
		})
	}
}

func TestDaemonSetDefaultsAndGeneratedSchedulingFields(t *testing.T) {
	f := newFixture(t)
	pod := f.platformPod.DeepCopy()
	pod.Spec.DNSPolicy, pod.Spec.RestartPolicy = corev1.DNSClusterFirst, corev1.RestartPolicyAlways
	pod.Spec.DeprecatedServiceAccount = pod.Spec.ServiceAccountName
	pod.Spec.SchedulerName = corev1.DefaultSchedulerName
	pod.Spec.TerminationGracePeriodSeconds = ptr.To(int64(30))
	pod.Spec.Priority, pod.Spec.PreemptionPolicy = ptr.To(int32(0)), ptr.To(corev1.PreemptLowerPriority)
	pod.Spec.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent
	pod.Spec.Containers[0].TerminationMessagePolicy = corev1.TerminationMessageReadFile
	pod.Spec.Containers[0].TerminationMessagePath = corev1.TerminationMessagePathDefault
	request := podRequest(t, pod, nil)
	request.UserInfo.Username = f.registry.PlatformDaemonSets[0].TrustedCreator
	expectAllowed(t, f.handler.Validate(context.Background(), request))
	// Existing template affinity is replaced only in the required Node-affinity
	// field; eligibility still comes from the pinned, unmodified template.
	f.daemon.Spec.Template.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "example.com/platform", Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}}},
		}}},
	}}
	if err := f.client.Update(context.Background(), f.daemon); err != nil {
		t.Fatal(err)
	}
	digest, err := CanonicalDaemonSetDigest(f.daemon)
	if err != nil {
		t.Fatal(err)
	}
	f.registry.PlatformDaemonSets[0].TemplateDigest = digest
	f.saveRegistry(t)
	expectAllowed(t, f.handler.Validate(context.Background(), request))
	pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{Operator: corev1.TolerationOpExists})
	request.Object.Raw = marshal(t, pod)
	expectDenied(t, f.handler.Validate(context.Background(), request), "DaemonSetTemplateMismatch")
}

func TestDaemonSetNodeNameFieldSelectors(t *testing.T) {
	longName := strings.Repeat("worker", 10) + ".node.example.com"
	for _, test := range []struct {
		name     string
		node     string
		operator corev1.NodeSelectorOperator
		value    string
		allowed  bool
	}{
		{"in long name", longName, corev1.NodeSelectorOpIn, longName, true},
		{"not in long name", "reserved-worker", corev1.NodeSelectorOpNotIn, longName, true},
		{"in different name", "reserved-worker", corev1.NodeSelectorOpIn, longName, false},
		{"not in same long name", longName, corev1.NodeSelectorOpNotIn, longName, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			ctx := context.Background()
			if f.reserved.Name != test.node {
				if err := f.client.Delete(ctx, f.reserved); err != nil {
					t.Fatal(err)
				}
				f.reserved.Name, f.reserved.ResourceVersion = test.node, ""
				if err := f.client.Create(ctx, f.reserved); err != nil {
					t.Fatal(err)
				}
				f.registry.Nodes[0].Name = test.node
				f.platformPod.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = targetAffinity(test.node)
				if err := f.client.Update(ctx, f.platformPod); err != nil {
					t.Fatal(err)
				}
			}
			f.daemon.Spec.Template.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: test.operator, Values: []string{test.value}}},
				}}},
			}}
			if err := f.client.Update(ctx, f.daemon); err != nil {
				t.Fatal(err)
			}
			digest, err := CanonicalDaemonSetDigest(f.daemon)
			if err != nil {
				t.Fatal(err)
			}
			f.registry.PlatformDaemonSets[0].TemplateDigest = digest
			f.saveRegistry(t)
			create := podRequest(t, f.platformPod, nil)
			create.UserInfo.Username = f.registry.PlatformDaemonSets[0].TrustedCreator
			for _, request := range []admission.Request{create, bindingRequest(t, f.platformPod, test.node)} {
				response := f.handler.Validate(ctx, request)
				if test.allowed {
					expectAllowed(t, response)
				} else {
					expectDenied(t, response, "DaemonSetSchedulingMismatch")
				}
			}
		})
	}
}

func TestDaemonSetAPIDefaultedCompatibility(t *testing.T) {
	for _, operation := range []string{"unscheduled create", "direct create", "binding", "update", "ephemeralcontainers", "resize"} {
		t.Run(operation, func(t *testing.T) {
			assigned := operation != "unscheduled create" && operation != "binding"
			f := defaultedDaemonFixture(t, assigned)
			var request admission.Request
			switch operation {
			case "unscheduled create", "direct create":
				request = podRequest(t, f.platformPod, nil)
				request.UserInfo.Username = f.registry.PlatformDaemonSets[0].TrustedCreator
			case "binding":
				request = topologyBindingRequest(t, f)
			default:
				request = podRequest(t, f.platformPod, f.platformPod)
				if operation != "update" {
					request.SubResource = operation
				}
			}
			expectAllowed(t, f.handler.Validate(context.Background(), request))
			current := &corev1.Pod{}
			if err := f.client.Get(context.Background(), client.ObjectKeyFromObject(f.platformPod), current); err != nil {
				t.Fatal(err)
			}
			if current.Spec.NodeName != f.platformPod.Spec.NodeName || current.ResourceVersion != f.platformPod.ResourceVersion {
				t.Fatal("read-only validation changed the persisted Pod")
			}
			digest, err := CanonicalDaemonSetDigest(f.daemon)
			if err != nil || digest != f.registry.PlatformDaemonSets[0].TemplateDigest {
				t.Fatal("Pod default normalization changed the registered DaemonSet digest")
			}
		})
	}
}

func TestDaemonSetDefaultedTolerationParity(t *testing.T) {
	f := defaultedDaemonFixture(t, true)
	spec := f.platformPod.Spec
	if len(f.daemon.Spec.Template.Spec.Tolerations) != 1 || len(spec.Tolerations) != 7 || spec.HostNetwork ||
		!ptr.Deref(spec.EnableServiceLinks, false) || spec.Priority == nil || *spec.Priority != 0 ||
		ptr.Deref(spec.PreemptionPolicy, "") != corev1.PreemptLowerPriority ||
		spec.ServiceAccountName != "default" || spec.DeprecatedServiceAccount != "default" {
		t.Fatal("fixture no longer represents the reported Kubernetes Pod defaults")
	}
	for _, reordered := range []bool{false, true} {
		t.Run(fmt.Sprintf("reordered=%t", reordered), func(t *testing.T) {
			pod := f.platformPod.DeepCopy()
			if reordered {
				slices.Reverse(pod.Spec.Tolerations)
			}
			expectAllowed(t, f.handler.Validate(context.Background(), podRequest(t, pod, f.platformPod)))
		})
	}
	for name, mutate := range map[string]func(*corev1.Pod){
		"finite not-ready duration": func(p *corev1.Pod) {
			p.Spec.Tolerations[1].TolerationSeconds = ptr.To(int64(300))
		},
		"missing original toleration": func(p *corev1.Pod) {
			p.Spec.Tolerations = p.Spec.Tolerations[1:]
		},
		"changed toleration effect": func(p *corev1.Pod) {
			p.Spec.Tolerations[3].Effect = corev1.TaintEffectNoExecute
		},
		"duplicate toleration": func(p *corev1.Pod) {
			p.Spec.Tolerations = append(p.Spec.Tolerations, p.Spec.Tolerations[1])
		},
		"network-unavailable without hostNetwork": func(p *corev1.Pod) {
			p.Spec.Tolerations = append(p.Spec.Tolerations, corev1.Toleration{
				Key: corev1.TaintNodeNetworkUnavailable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule,
			})
		},
		"nondefault priority":   func(p *corev1.Pod) { p.Spec.Priority = ptr.To(int32(1)) },
		"nondefault preemption": func(p *corev1.Pod) { p.Spec.PreemptionPolicy = ptr.To(corev1.PreemptNever) },
	} {
		t.Run(name, func(t *testing.T) {
			pod := f.platformPod.DeepCopy()
			mutate(pod)
			expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, pod, f.platformPod)), "DaemonSetTemplateMismatch")
		})
	}
}

func TestDaemonSetDefaultedPodStillRequiresExactFields(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Pod){
		"wrong region": func(p *corev1.Pod) { p.Labels[corev1.LabelTopologyRegion] = "other-region" },
		"wrong zone":   func(p *corev1.Pod) { p.Labels[corev1.LabelTopologyZone] = "other-zone" },
		"unapproved topology key": func(p *corev1.Pod) {
			p.Labels["topology.kubernetes.io/rack"] = "example-rack"
		},
		"unapproved label": func(p *corev1.Pod) { p.Labels["example.com/unapproved"] = "true" },
		"unapproved annotation": func(p *corev1.Pod) {
			p.Annotations = map[string]string{"example.com/unapproved": "true"}
		},
		"changed service links": func(p *corev1.Pod) { p.Spec.EnableServiceLinks = ptr.To(false) },
		"wrong service account": func(p *corev1.Pod) { p.Spec.ServiceAccountName = "another-account" },
		"wrong account alias":   func(p *corev1.Pod) { p.Spec.DeprecatedServiceAccount = "another-account" },
		"wrong generation":      func(p *corev1.Pod) { p.Labels[daemonGenerationLabel] = "2" },
		"forged controller UID": func(p *corev1.Pod) { p.OwnerReferences[0].UID = "forged-daemon-uid" },
		"changed scheduling intent": func(p *corev1.Pod) {
			p.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = targetAffinity("ordinary-worker")
		},
	} {
		for _, updating := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/update=%t", name, updating), func(t *testing.T) {
				f := defaultedDaemonFixture(t, true)
				pod := f.platformPod.DeepCopy()
				mutate(pod)
				request := podRequest(t, pod, nil)
				if updating {
					request = podRequest(t, pod, f.platformPod)
				} else {
					request.UserInfo.Username = f.registry.PlatformDaemonSets[0].TrustedCreator
				}
				expectDenied(t, f.handler.Validate(context.Background(), request), "")
			})
		}
	}
	f := defaultedDaemonFixture(t, false)
	pod := f.platformPod.DeepCopy()
	pod.Labels[corev1.LabelTopologyZone] = f.reserved.Labels[corev1.LabelTopologyZone]
	request := podRequest(t, pod, nil)
	request.UserInfo.Username = f.registry.PlatformDaemonSets[0].TrustedCreator
	expectDenied(t, f.handler.Validate(context.Background(), request), "DaemonSetTemplateMismatch")
}

func TestDaemonSetBindingValidatesAppliedTopologyMetadata(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Binding){
		"wrong region": func(b *corev1.Binding) { b.Labels[corev1.LabelTopologyRegion] = "other-region" },
		"wrong zone":   func(b *corev1.Binding) { b.Labels[corev1.LabelTopologyZone] = "other-zone" },
		"unapproved topology key": func(b *corev1.Binding) {
			b.Labels["topology.kubernetes.io/rack"] = "example-rack"
		},
		"unapproved label":   func(b *corev1.Binding) { b.Labels["example.com/unapproved"] = "true" },
		"changed selector":   func(b *corev1.Binding) { b.Labels["app"] = "other-daemon" },
		"changed generation": func(b *corev1.Binding) { b.Labels[daemonGenerationLabel] = "2" },
		"unapproved annotation": func(b *corev1.Binding) {
			b.Annotations = map[string]string{"example.com/unapproved": "true"}
		},
		"forged Pod UID": func(b *corev1.Binding) { b.UID = "forged-pod-uid" },
	} {
		t.Run(name, func(t *testing.T) {
			f := defaultedDaemonFixture(t, false)
			request := topologyBindingRequest(t, f)
			var binding corev1.Binding
			if err := json.Unmarshal(request.Object.Raw, &binding); err != nil {
				t.Fatal(err)
			}
			mutate(&binding)
			request.Object.Raw = marshal(t, binding)
			expectDenied(t, f.handler.Validate(context.Background(), request), "")
		})
	}
}

func TestDaemonSetTopologyRequiresFreshPinnedNode(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Node){
		"changed zone":     func(n *corev1.Node) { n.Labels[corev1.LabelTopologyZone] = "another-zone" },
		"missing zone":     func(n *corev1.Node) { delete(n.Labels, corev1.LabelTopologyZone) },
		"replaced Node":    func(n *corev1.Node) { n.UID = "replacement-node-uid" },
		"changed provider": func(n *corev1.Node) { n.Spec.ProviderID = "example://another-worker" },
	} {
		t.Run(name, func(t *testing.T) {
			f := defaultedDaemonFixture(t, true)
			f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
				if node, ok := object.(*corev1.Node); ok {
					mutate(node)
				}
			}}
			expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, f.platformPod, f.platformPod)), "")
		})
	}
	f := defaultedDaemonFixture(t, true)
	var reads int
	f.handler.reader = mutateReader{Reader: f.client, mutate: func(object client.Object) {
		if node, ok := object.(*corev1.Node); ok {
			reads++
			// Destination, persisted Pod and proposed Pod each validate once;
			// change only the subsequent authoritative snapshot recheck.
			if reads > 3 {
				node.Labels[corev1.LabelTopologyRegion] = "changed-during-validation"
			}
		}
	}}
	expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, f.platformPod, f.platformPod)), "NodeChanged")
}

func TestDaemonSetTopologyDoesNotOverridePinnedTemplateLabels(t *testing.T) {
	f := defaultedDaemonFixture(t, true)
	f.daemon.Spec.Template.Labels[corev1.LabelTopologyRegion] = "template-region"
	if err := f.client.Update(context.Background(), f.daemon); err != nil {
		t.Fatal(err)
	}
	digest, err := CanonicalDaemonSetDigest(f.daemon)
	if err != nil {
		t.Fatal(err)
	}
	f.registry.PlatformDaemonSets[0].TemplateDigest = digest
	f.saveRegistry(t)
	expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, f.platformPod, f.platformPod)), "DaemonSetTemplateMismatch")
}

func TestDaemonSetExplicitServiceLinksAreNotDefaultedAway(t *testing.T) {
	f := defaultedDaemonFixture(t, true)
	f.daemon.Spec.Template.Spec.EnableServiceLinks = ptr.To(false)
	if err := f.client.Update(context.Background(), f.daemon); err != nil {
		t.Fatal(err)
	}
	digest, err := CanonicalDaemonSetDigest(f.daemon)
	if err != nil {
		t.Fatal(err)
	}
	f.registry.PlatformDaemonSets[0].TemplateDigest = digest
	f.saveRegistry(t)
	f.platformPod.Spec.EnableServiceLinks = ptr.To(false)
	if err := f.client.Update(context.Background(), f.platformPod); err != nil {
		t.Fatal(err)
	}
	expectAllowed(t, f.handler.Validate(context.Background(), podRequest(t, f.platformPod, f.platformPod)))
	changed := f.platformPod.DeepCopy()
	changed.Spec.EnableServiceLinks = ptr.To(true)
	expectDenied(t, f.handler.Validate(context.Background(), podRequest(t, changed, f.platformPod)), "DaemonSetTemplateMismatch")
}

func defaultedDaemonFixture(t *testing.T, assigned bool) *placementFixture {
	t.Helper()
	f := newFixture(t)
	f.daemon.Annotations = map[string]string{appsv1.DeprecatedTemplateGeneration: "1"}
	template := &f.daemon.Spec.Template.Spec
	template.ServiceAccountName, template.DeprecatedServiceAccount = "", ""
	template.RestartPolicy, template.DNSPolicy = corev1.RestartPolicyAlways, corev1.DNSClusterFirst
	template.SchedulerName = corev1.DefaultSchedulerName
	template.SecurityContext = &corev1.PodSecurityContext{}
	template.TerminationGracePeriodSeconds = ptr.To(int64(30))
	template.Containers[0].ImagePullPolicy = corev1.PullIfNotPresent
	template.Containers[0].TerminationMessagePath = corev1.TerminationMessagePathDefault
	template.Containers[0].TerminationMessagePolicy = corev1.TerminationMessageReadFile
	template.Tolerations = []corev1.Toleration{{Key: "example.com/dedicated", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
	if err := f.client.Update(context.Background(), f.daemon); err != nil {
		t.Fatal(err)
	}
	account := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name: "default", Namespace: f.platformNS.Name, UID: "platform-default-account-uid",
	}}
	if err := f.client.Create(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	f.reserved.Labels[corev1.LabelTopologyRegion] = "example-region"
	f.reserved.Labels[corev1.LabelTopologyZone] = "example-zone"
	if err := f.client.Update(context.Background(), f.reserved); err != nil {
		t.Fatal(err)
	}
	pod := daemonPod(f.daemon, f.reserved.Name)
	pod.Labels[daemonGenerationLabel] = "1"
	pod.Spec.EnableServiceLinks = ptr.To(corev1.DefaultEnableServiceLinks)
	pod.Spec.ServiceAccountName, pod.Spec.DeprecatedServiceAccount = "default", "default"
	pod.Spec.PreemptionPolicy, pod.Spec.Priority = ptr.To(corev1.PreemptLowerPriority), ptr.To(int32(0))
	pod.Spec.Tolerations = []corev1.Toleration{
		{Key: "example.com/dedicated", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
		{Key: "node.kubernetes.io/unreachable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
		{Key: "node.kubernetes.io/disk-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		{Key: "node.kubernetes.io/memory-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		{Key: "node.kubernetes.io/pid-pressure", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		{Key: "node.kubernetes.io/unschedulable", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	}
	if assigned {
		pod.Spec.NodeName = f.reserved.Name
		pod.Labels[corev1.LabelTopologyRegion] = "example-region"
		pod.Labels[corev1.LabelTopologyZone] = "example-zone"
	}
	if err := f.client.Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	f.platformPod = pod
	digest, err := CanonicalDaemonSetDigest(f.daemon)
	if err != nil {
		t.Fatal(err)
	}
	f.registry.PlatformDaemonSets[0].TemplateDigest = digest
	f.saveRegistry(t)
	return f
}

func topologyBindingRequest(t *testing.T, f *placementFixture) admission.Request {
	t.Helper()
	request := bindingRequest(t, f.platformPod, f.reserved.Name)
	var binding corev1.Binding
	if err := json.Unmarshal(request.Object.Raw, &binding); err != nil {
		t.Fatal(err)
	}
	binding.Labels = map[string]string{
		corev1.LabelTopologyRegion: "example-region", corev1.LabelTopologyZone: "example-zone",
	}
	request.Object.Raw = marshal(t, binding)
	return request
}

func TestCanonicalDaemonSetDigestIsTypedSelectorAndTemplateJSON(t *testing.T) {
	f := newFixture(t)
	expectedJSON := []byte(`{"selector":` + string(marshal(t, f.daemon.Spec.Selector)) +
		`,"template":` + string(marshal(t, f.daemon.Spec.Template)) + `}`)
	if !json.Valid(expectedJSON) {
		t.Fatal("invalid expected canonical JSON")
	}
	expectedSum := sha256.Sum256(expectedJSON)
	digest, err := CanonicalDaemonSetDigest(f.daemon)
	if err != nil || digest != hex.EncodeToString(expectedSum[:]) {
		t.Fatalf("unexpected digest %q: %v", digest, err)
	}
	copy := f.daemon.DeepCopy()
	copy.ResourceVersion, copy.UID = "another-version", "another-uid"
	copy.Status.NumberReady = 10
	same, err := CanonicalDaemonSetDigest(copy)
	if err != nil || same != digest {
		t.Fatal("metadata or status entered the template digest")
	}
	copy.Spec.Template.Spec.ServiceAccountName = "different-account"
	changed, err := CanonicalDaemonSetDigest(copy)
	if err != nil || changed == digest {
		t.Fatal("ServiceAccount identity was omitted from the template digest")
	}
	for _, daemon := range []*appsv1.DaemonSet{nil, {}, {Spec: appsv1.DaemonSetSpec{Template: f.daemon.Spec.Template}}} {
		if _, err := CanonicalDaemonSetDigest(daemon); err == nil {
			t.Fatal("incomplete digest input accepted")
		}
	}
}
