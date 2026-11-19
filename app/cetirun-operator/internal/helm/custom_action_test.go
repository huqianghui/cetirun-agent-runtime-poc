// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package helm

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

func customActionConfig() *action.Configuration {
	return &action.Configuration{Releases: storage.Init(driver.NewMemory()),
		KubeClient:   &kubefake.PrintingKubeClient{Out: io.Discard},
		Capabilities: chartutil.DefaultCapabilities.Copy(), Log: func(string, ...any) {}}
}

func customActionPlan(t *testing.T, ch *chart.Chart) *PreparedRelease {
	t.Helper()
	p, err := prepareChart(ch, customInput(8080), false)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCustomActionRejectsContextDependentExtrasBeforeApply(t *testing.T) {
	for _, mode := range []string{"install", "upgrade", "capability", "hook"} {
		t.Run(mode, func(t *testing.T) {
			ch := testChart(t)
			condition := ".Release.IsInstall"
			if mode == "upgrade" {
				condition = "gt .Release.Revision 1"
			} else if mode == "capability" {
				condition = `.Capabilities.APIVersions.Has "example.com/v1"`
			}
			annotation := ""
			if mode == "hook" {
				annotation = "\n  annotations:\n    helm.sh/hook: pre-install"
			}
			ch.Templates = append(ch.Templates, &chart.File{Name: "templates/conditional.yaml",
				Data: []byte("{{ if " + condition + " }}\napiVersion: v1\nkind: Secret\nmetadata:\n  name: unexpected" + annotation + "\n{{ end }}\n")})
			p := customActionPlan(t, ch)
			cfg := customActionConfig()
			if mode == "upgrade" {
				if err := cfg.Releases.Create(&release.Release{Name: p.releaseName, Namespace: "cetirun-example",
					Version: 1, Chart: ch, Config: p.Values, Info: &release.Info{Status: release.StatusDeployed}}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "capability" {
				cfg.Capabilities.APIVersions = append(cfg.Capabilities.APIVersions, "example.com/v1")
			}
			if err := p.prepareCustomAction(cfg, "cetirun-example", p.releaseName); err == nil {
				t.Fatal("action-only object or hook passed preflight")
			}
			if p.actionRevision != 0 {
				t.Fatal("rejected plan was sealed for application")
			}
		})
	}
}

func TestCustomActionRejectsCRDsIncludingDependencies(t *testing.T) {
	ch := testChart(t)
	dependency := &chart.Chart{Metadata: &chart.Metadata{Name: "extra", Version: "1.0.0"},
		Files: []*chart.File{{Name: "crds/extra.yaml", Data: []byte("kind: CustomResourceDefinition\n")}}}
	ch.AddDependency(dependency)
	if _, err := prepareChart(ch, customInput(nil), false); err == nil {
		t.Fatal("dependency CRD accepted")
	}
}

func TestCustomAppliesOnlyFrozenOutputAndCanProveSuccess(t *testing.T) {
	ch := testChart(t)
	p := customActionPlan(t, ch)
	cfg := customActionConfig()
	if err := p.prepareCustomAction(cfg, "cetirun-example", p.releaseName); err != nil {
		t.Fatal(err)
	}
	// The source is not executed a second time, even if capabilities or
	// conditional templates change after approval.
	ch.Templates = append(ch.Templates, &chart.File{Name: "templates/late.yaml",
		Data: []byte("apiVersion: v1\nkind: Secret\nmetadata:\n  name: unexpected-late\n")})
	cfg.Capabilities.APIVersions = append(cfg.Capabilities.APIVersions, "example.com/v1")
	if err := p.installOrUpgradeCustom(cfg, "cetirun-example", p.releaseName); err != nil {
		t.Fatal(err)
	}
	applied, err := p.customApplied(cfg, "cetirun-example", p.releaseName)
	if err != nil || !applied {
		t.Fatalf("durable successful application not recognized: %v", err)
	}
	last, _ := cfg.Releases.Last(p.releaseName)
	snapshot, _ := json.Marshal(last)
	if strings.Contains(last.Manifest, "unexpected-late") || len(last.Hooks) != 0 || len(last.Chart.CRDObjects()) != 0 {
		t.Fatal("frozen application retained a source-chart execution path")
	}
	for _, mutate := range []func(*release.Release){
		func(r *release.Release) { r.Info.Status = release.StatusFailed },
		func(r *release.Release) {
			r.Manifest += "\n---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: unexpected\n"
		},
		func(r *release.Release) { object(object(r.Config["runtime"])["bindings"])["agentUID"] = "other" },
	} {
		var record release.Release
		if err := json.Unmarshal(snapshot, &record); err != nil {
			t.Fatal(err)
		}
		mutate(&record)
		if err := cfg.Releases.Update(&record); err != nil {
			t.Fatal(err)
		}
		if ok, _ := p.customApplied(cfg, "cetirun-example", p.releaseName); ok {
			t.Fatal("unverified or failed release was treated as success")
		}
		if err := cfg.Releases.Update(last); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCustomActionRejectsReleaseChangeAfterPreflight(t *testing.T) {
	p := customActionPlan(t, testChart(t))
	cfg := customActionConfig()
	if err := p.prepareCustomAction(cfg, "cetirun-example", p.releaseName); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Releases.Create(&release.Release{Name: p.releaseName, Namespace: "cetirun-example", Version: 1,
		Config: p.Values, Info: &release.Info{Status: release.StatusPendingInstall}}); err != nil {
		t.Fatal(err)
	}
	if err := p.installOrUpgradeCustom(cfg, "cetirun-example", p.releaseName); err == nil {
		t.Fatal("existing-name/pending conflict was swallowed")
	}
}

func TestCustomEmptyNodeSelectorValueMustRetainItsKey(t *testing.T) {
	for _, mutate := range []bool{false, true} {
		ch := testChart(t)
		input := customInput(nil)
		input["nodeSelector"] = map[string]any{"example.com/approved": ""}
		if mutate {
			for _, file := range ch.Templates {
				if file.Name == "templates/_platform.tpl" {
					from := "nodeSelector:\n  {{- toYaml . | nindent 2 }}"
					if !strings.Contains(string(file.Data), from) {
						t.Fatal("node selector mutation did not match")
					}
					file.Data = []byte(strings.Replace(string(file.Data), from,
						"nodeSelector:\n  example.com/unapproved: \"\"", 1))
				}
			}
		}
		p, err := prepareChart(ch, input, false)
		if err != nil {
			t.Fatal(err)
		}
		err = p.prepareCustomAction(customActionConfig(), "cetirun-example", p.releaseName)
		if (err != nil) != mutate {
			t.Fatalf("mutation=%v error=%v", mutate, err)
		}
	}
}
