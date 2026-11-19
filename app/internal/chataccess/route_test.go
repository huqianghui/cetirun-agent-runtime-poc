// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chataccess

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestPrivateRouteContract(t *testing.T) {
	caller := Caller{Namespace: "platform", PodLabels: map[string]string{"app": "portal"}}
	route := PrivateRoute("example", "uid-example", "cetirun-example", 8080, []Caller{caller})
	route.SetUID("route-uid")
	if err := ValidatePrivateRoute(route, "example", "uid-example", "cetirun-example", 8080, caller); err != nil {
		t.Fatal(err)
	}
	zeroOmitted := route.DeepCopy()
	zeroOmitted.Object["spec"].(map[string]any)["http"].([]any)[0].(map[string]any)["retries"] = map[string]any{}
	if err := ValidatePrivateRoute(zeroOmitted, "example", "uid-example", "cetirun-example", 8080, caller); err != nil {
		t.Fatal("typed zero attempts encoding rejected", err)
	}
	for name, mutate := range map[string]func(*unstructured.Unstructured){
		"retries": func(r *unstructured.Unstructured) {
			r.Object["spec"].(map[string]any)["http"].([]any)[0].(map[string]any)["retries"] = map[string]any{"attempts": int64(2)}
		},
		"default retries": func(r *unstructured.Unstructured) {
			delete(r.Object["spec"].(map[string]any)["http"].([]any)[0].(map[string]any), "retries")
		},
		"public gateway": func(r *unstructured.Unstructured) {
			r.Object["spec"].(map[string]any)["gateways"] = []any{"public/gateway"}
		},
		"foreign owner": func(r *unstructured.Unstructured) {
			owners := r.GetOwnerReferences()
			owners[0].UID = "foreign"
			r.SetOwnerReferences(owners)
		},
		"broad export": func(r *unstructured.Unstructured) {
			r.Object["spec"].(map[string]any)["exportTo"] = []any{"platform", "*"}
		},
		"redirect": func(r *unstructured.Unstructured) {
			r.Object["spec"].(map[string]any)["http"].([]any)[0].(map[string]any)["redirect"] = map[string]any{"authority": "outside.example.com"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := route.DeepCopy()
			mutate(changed)
			if ValidatePrivateRoute(changed, "example", "uid-example", "cetirun-example", 8080, caller) == nil {
				t.Fatal("unsafe Chat route accepted")
			}
		})
	}
}
