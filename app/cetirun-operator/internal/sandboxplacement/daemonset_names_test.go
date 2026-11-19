// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxplacement

import (
	"context"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/sandboxworkload"
	corev1 "k8s.io/api/core/v1"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apiserver/pkg/storage/names"
	"k8s.io/utils/ptr"
)

func TestDaemonSetGeneratedNameBoundaries(t *testing.T) {
	for label, name := range map[string]string{
		"57":                  strings.Repeat("d", 57),
		"58":                  strings.Repeat("d", 58),
		"59":                  strings.Repeat("d", 59),
		"63":                  strings.Repeat("d", 63),
		"long DNS subdomain":  strings.Repeat("d", 63) + ".platform.example.com",
		"separator at cutoff": strings.Repeat("d", 57) + ".platform.example.com",
		"dash at cutoff":      strings.Repeat("d", 57) + "-platform.example.com",
		"252":                 strings.Repeat(strings.Repeat("d", 63)+".", 3) + strings.Repeat("e", 60),
		"253":                 strings.Repeat(strings.Repeat("d", 63)+".", 3) + strings.Repeat("e", 61),
	} {
		for _, operation := range []string{"create", "direct", "binding", "update", "ephemeralcontainers", "resize"} {
			t.Run(label+"/"+operation, func(t *testing.T) {
				f := namedDaemonFixture(t, name)
				pod := f.platformPod
				if len(pod.Name) != min(len(name)+1, 58)+5 || len(pod.Name) > 63 || pod.GenerateName != name+"-" {
					t.Fatal("fixture does not match Kubernetes' separate name/generateName behavior")
				}
				expectAllowed(t, f.handler.Validate(context.Background(), daemonAdmissionRequest(t, f, operation)))
			})
		}
	}
}

func TestDaemonSetGeneratedNameKeepsExactOwnerAndGenerateName(t *testing.T) {
	name := strings.Repeat("d", 58) + ".platform.example.com"
	otherName := strings.Repeat("d", 58) + ".other.example.com"
	for label, mutate := range map[string]func(*corev1.Pod){
		"wrong prefix":           func(p *corev1.Pod) { p.Name = "x" + p.Name[1:] },
		"untruncated name":       func(p *corev1.Pod) { p.Name = p.GenerateName + "bcdfg" },
		"short suffix":           func(p *corev1.Pod) { p.Name = p.Name[:len(p.Name)-1] },
		"long suffix":            func(p *corev1.Pod) { p.Name += "b" },
		"nonrandom suffix":       func(p *corev1.Pod) { p.Name = p.Name[:58] + "abcde" },
		"suffix separator":       func(p *corev1.Pod) { p.Name = p.Name[:58] + "bc-df" },
		"wrong generateName":     func(p *corev1.Pod) { p.GenerateName = otherName + "-" },
		"truncated generateName": func(p *corev1.Pod) { p.GenerateName = p.GenerateName[:58] },
		"missing generate dash":  func(p *corev1.Pod) { p.GenerateName = name },
		"same prefix other owner": func(p *corev1.Pod) {
			p.OwnerReferences[0].Name = otherName
		},
		"other owner UID":    func(p *corev1.Pod) { p.OwnerReferences[0].UID = "other-daemon-uid" },
		"other owner API":    func(p *corev1.Pod) { p.OwnerReferences[0].APIVersion = "apps/v2" },
		"noncontroller":      func(p *corev1.Pod) { p.OwnerReferences[0].Controller = ptr.To(false) },
		"noncanonical owner": func(p *corev1.Pod) { p.OwnerReferences[0].BlockOwnerDeletion = nil },
		"other namespace":    func(p *corev1.Pod) { p.Namespace = "ordinary-workloads" },
	} {
		t.Run(label, func(t *testing.T) {
			f := namedDaemonFixture(t, name)
			mutate(f.platformPod)
			expectDenied(t, f.handler.Validate(context.Background(), daemonAdmissionRequest(t, f, "direct")), "")
		})
	}
}

func TestDaemonSetGenerateNameUsesKubernetesPrefixValidation(t *testing.T) {
	for _, length := range []int{57, 58, 59, 252, 253, 254} {
		name := strings.Repeat("d", length)
		expected := name + "-"
		if len(apivalidation.NameIsDNSSubdomain(expected, true)) != 0 {
			expected = name
		}
		if daemonPodGenerateName(name) != expected {
			t.Fatal("controller prefix did not use Kubernetes DNS-subdomain prefix validation")
		}
	}
	// The cached validator masks a trailing dash before checking length.
	// Preserve this behavior instead of truncating metadata.generateName.
	maximumName := strings.Repeat("d", 253)
	if daemonPodGenerateName(maximumName) != maximumName+"-" {
		t.Fatal("maximum-length controller name lost its Kubernetes generation prefix")
	}
	f := namedDaemonFixture(t, maximumName)
	f.platformPod.GenerateName = maximumName
	expectDenied(t, f.handler.Validate(context.Background(), daemonAdmissionRequest(t, f, "create")), "DaemonSetOwnerMismatch")
	f = namedDaemonFixture(t, maximumName)
	f.platformPod.GenerateName = ""
	expectAllowed(t, f.handler.Validate(context.Background(), daemonAdmissionRequest(t, f, "create")))
}

func TestDaemonSetGeneratedSuffixMatchesKubernetesGenerator(t *testing.T) {
	for _, name := range []string{"platform-daemon", strings.Repeat("d", 58), strings.Repeat("d", 58) + ".example.com"} {
		prefix := name + "-"
		for range 64 {
			pod := &corev1.Pod{}
			pod.GenerateName, pod.Name = prefix, names.SimpleNameGenerator.GenerateName(prefix)
			if !daemonPodNameMatches(pod, name) {
				t.Fatal("public Kubernetes SimpleNameGenerator produced a rejected identity")
			}
		}
	}
}

func namedDaemonFixture(t *testing.T, name string) *placementFixture {
	t.Helper()
	if !validName(name) || len(apivalidation.NameIsDNSSubdomain(name+"-", true)) != 0 {
		t.Fatal("fixture requires a valid DaemonSet name and public controller generation prefix")
	}
	f := newFixture(t)
	ctx := context.Background()
	if err := f.client.Delete(ctx, f.daemon); err != nil {
		t.Fatal(err)
	}
	f.daemon.Name, f.daemon.ResourceVersion = name, ""
	if err := f.client.Create(ctx, f.daemon); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(ctx, f.platformPod); err != nil {
		t.Fatal(err)
	}
	pod := f.platformPod
	pod.GenerateName = name + "-"
	pod.Name = names.SimpleNameGenerator.GenerateName(pod.GenerateName)
	pod.ResourceVersion = ""
	pod.OwnerReferences[0].Name = name
	if err := f.client.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	f.registry.PlatformDaemonSets[0].Name = name
	f.registry.PlatformDaemonSets[0].BootstrapPods = []sandboxworkload.ObjectIdentity{{Name: pod.Name, UID: pod.UID}}
	f.saveRegistry(t)
	return f
}
