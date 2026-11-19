// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/CetiRun/app/cetirun-operator/internal/runtimebindings"
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	corev1 "k8s.io/api/core/v1"
)

func terminalInput(port any) map[string]any {
	input := customInput(port)
	delete(input, "ttyd") // Exercise the real Chart default: enabled.
	bindings := input["customBindings"].(runtimebindings.Custom)
	bindings.Network.APIServer = []runtimebindings.APIServerEndpoint{
		{IP: "10.96.0.1", Port: 443}, {IP: "192.0.2.10", Port: 6443},
	}
	input["customBindings"] = bindings
	return input
}

func manifestObjects(t *testing.T, p *PreparedRelease) map[string]map[string]any {
	t.Helper()
	result := map[string]map[string]any{}
	for _, document := range strings.Split(string(p.manifest), "---\n") {
		if strings.TrimSpace(document) == "" {
			continue
		}
		var object map[string]any
		if err := json.Unmarshal([]byte(document), &object); err != nil {
			t.Fatal(err)
		}
		result[object["kind"].(string)+"/"+object["metadata"].(map[string]any)["name"].(string)] = object
	}
	return result
}

func TestCustomTerminalIsAnIsolatedChartWorkload(t *testing.T) {
	for _, port := range []any{nil, 7681, 8080} {
		for _, desktop := range []bool{false, true} {
			for _, enabled := range []bool{false, true} {
				t.Run(fmt.Sprintf("api=%v/desktop=%v/terminal=%v", port, desktop, enabled), func(t *testing.T) {
					input := terminalInput(port)
					input["desktop"] = map[string]any{"enabled": desktop}
					if !enabled {
						input["ttyd"] = map[string]any{"enabled": false}
					}
					p, err := PrepareChart(testChart(t), input)
					if err != nil {
						t.Fatal(err)
					}
					objects := manifestObjects(t, p)
					name := terminalpolicy.WorkloadName(text(input["fullnameOverride"]))
					deployment := objects["Deployment/"+name]
					if (deployment != nil) != enabled {
						t.Fatal("terminal deployment does not follow the switch")
					}
					if (objects["Service/"+name] != nil) != enabled {
						t.Fatal("terminal Service does not follow the switch")
					}
					main := objects["StatefulSet/"+text(input["fullnameOverride"])]
					var pod corev1.PodSpec
					if err := decodeObject(object(object(object(main["spec"])["template"])["spec"]), &pod); err != nil {
						t.Fatal(err)
					}
					if len(pod.Containers) != 1+btoi(desktop) || len(pod.Containers[0].Command) != 0 {
						t.Fatal("Agent entrypoint or containers changed")
					}
					for _, env := range pod.Containers[0].Env {
						if strings.HasPrefix(env.Name, "TTYD_") {
							t.Fatal("terminal settings leaked into Agent")
						}
					}
					if enabled {
						var terminalPod corev1.PodSpec
						if err := decodeObject(object(object(object(deployment["spec"])["template"])["spec"]), &terminalPod); err != nil {
							t.Fatal(err)
						}
						if terminalPod.ServiceAccountName == pod.ServiceAccountName {
							t.Fatal("terminal reused Agent identity")
						}
						for _, volume := range terminalPod.Volumes {
							if volume.PersistentVolumeClaim != nil || volume.HostPath != nil {
								t.Fatal("terminal mounted Agent or host data")
							}
						}
						if terminalPod.Containers[0].Image == p.Agent.Image || !strings.Contains(terminalPod.Containers[0].Image, "/cetirun-terminal:") {
							t.Fatal("terminal did not use its dedicated image")
						}
						if objects["AuthorizationPolicy/"+name] == nil || objects["PeerAuthentication/"+name] == nil {
							t.Fatal("terminal lacks gateway identity enforcement")
						}
					}
				})
			}
		}
	}
}

func TestCustomTerminalChangesDoNotRestartAgent(t *testing.T) {
	disabled, err := PrepareChart(testChart(t), customInput(8080))
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := PrepareChart(testChart(t), terminalInput(8080))
	if err != nil {
		t.Fatal(err)
	}
	if disabled.WorkloadHash != enabled.WorkloadHash {
		t.Fatal("enabling terminal changed the Agent Pod")
	}
	if disabled.PlanHash == enabled.PlanHash {
		t.Fatal("terminal did not affect the platform plan")
	}
	input := terminalInput(8080)
	input["ttyd"] = map[string]any{"port": 9000}
	customPort, err := PrepareChart(testChart(t), input)
	if err != nil || customPort.CustomTerminalPort() != 9000 {
		t.Fatal(err)
	}
	if enabled.WorkloadHash != customPort.WorkloadHash {
		t.Fatal("terminal port changed Agent workload")
	}
	name := "StatefulSet/" + text(input["fullnameOverride"])
	template := func(p *PreparedRelease) any {
		return object(manifestObjects(t, p)[name]["spec"])["template"]
	}
	if !reflect.DeepEqual(template(disabled), template(enabled)) || !reflect.DeepEqual(template(enabled), template(customPort)) {
		t.Fatal("terminal configuration changed the Agent Pod template")
	}
}

func TestCustomChartVersionDoesNotChangePodTemplate(t *testing.T) {
	chrt := testChart(t)
	before, err := PrepareChart(chrt, terminalInput(8080))
	if err != nil {
		t.Fatal(err)
	}
	name := "StatefulSet/" + text(before.Values["fullnameOverride"])
	oldTemplate := object(manifestObjects(t, before)[name]["spec"])["template"]
	chrt.Metadata.Version = "0.1.99"
	after, err := PrepareChart(chrt, terminalInput(8080))
	if err != nil {
		t.Fatal(err)
	}
	newTemplate := object(manifestObjects(t, after)[name]["spec"])["template"]
	if before.WorkloadHash != after.WorkloadHash || !reflect.DeepEqual(oldTemplate, newTemplate) {
		t.Fatal("Chart version alone changed the Custom Pod template")
	}
}

func TestCustomConfigChecksumTracksInitializerChanges(t *testing.T) {
	for _, path := range []string{"configs/init-home-common.sh", "agents/custom/bootstrap/init-home.sh"} {
		t.Run(path, func(t *testing.T) {
			chrt := testChart(t)
			before, err := PrepareChart(chrt, terminalInput(8080))
			if err != nil {
				t.Fatal(err)
			}
			name := "StatefulSet/" + text(before.Values["fullnameOverride"])
			checksum := func(p *PreparedRelease) any {
				template := object(object(manifestObjects(t, p)[name]["spec"])["template"])
				return object(object(template["metadata"])["annotations"])["checksum/config"]
			}
			oldChecksum := checksum(before)
			for _, file := range chrt.Files {
				if file.Name == path {
					file.Data = append(file.Data, []byte("\n# Changed initializer\n")...)
				}
			}
			after, err := PrepareChart(chrt, terminalInput(8080))
			if err != nil {
				t.Fatal(err)
			}
			if before.WorkloadHash == after.WorkloadHash || oldChecksum == checksum(after) {
				t.Fatal("initializer change did not update both workload and config hashes")
			}
		})
	}
}

func TestCustomTerminalRejectsMissingNetworkAndExecControls(t *testing.T) {
	for _, key := range []string{"networkPolicy", "execRestriction"} {
		input := terminalInput(nil)
		input[key] = map[string]any{"enabled": false}
		if _, err := PrepareChart(testChart(t), input); err == nil {
			t.Fatalf("terminal accepted without %s", key)
		}
	}
	input := terminalInput(nil)
	bindings := input["customBindings"].(runtimebindings.Custom)
	bindings.Network.APIServer = nil
	input["customBindings"] = bindings
	if _, err := PrepareChart(testChart(t), input); err == nil {
		t.Fatal("API egress silently widened")
	}
}

func TestCustomTerminalFinalValidationRejectsTampering(t *testing.T) {
	for _, kind := range []string{"Deployment", "Service", "ServiceAccount", "RoleBinding", "ClusterRoleBinding", "NetworkPolicy", "AuthorizationPolicy", "PeerAuthentication"} {
		t.Run(kind, func(t *testing.T) {
			p, err := PrepareChart(testChart(t), terminalInput(8080))
			if err != nil {
				t.Fatal(err)
			}
			resources := object(p.Values["runtime"])["terminalResources"].([]map[string]any)
			mutated := false
			for _, resource := range resources {
				if resource["kind"] == kind {
					object(resource["metadata"])["annotations"] = map[string]any{"unapproved": "true"}
					mutated = true
					break
				}
			}
			if !mutated {
				t.Fatalf("fixture did not include %s", kind)
			}
			if err := p.ValidateCustom("cetirun-example", "openclaw-example"); err == nil {
				t.Fatal("tampered terminal object accepted")
			}
		})
	}
}
