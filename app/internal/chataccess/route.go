// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package chataccess

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
)

var RouteGVK = schema.GroupVersionKind{Group: "networking.istio.io", Version: "v1beta1", Kind: "VirtualService"}

func RouteName(agent string) string {
	name := "portal-chat-" + agent
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

func routeSpec(agent, namespace string, port int32, exports []string) map[string]any {
	host := ServiceName(agent) + "." + namespace + ".svc.cluster.local"
	namespaces := make([]any, len(exports))
	for i, value := range exports {
		namespaces[i] = value
	}
	return map[string]any{
		"hosts": []any{host}, "gateways": []any{"mesh"}, "exportTo": namespaces,
		"http": []any{map[string]any{
			"name": "portal-chat", "timeout": "600s", "retries": map[string]any{"attempts": int64(0)},
			"route": []any{map[string]any{"destination": map[string]any{"host": host, "port": map[string]any{"number": int64(port)}}}},
		}},
	}
}

func PrivateRoute(agent, uid, namespace string, port int32, callers []Caller) *unstructured.Unstructured {
	exports := []string{}
	for _, caller := range callers {
		exports = append(exports, caller.Namespace)
	}
	slices.Sort(exports)
	exports = slices.Compact(exports)
	value := &unstructured.Unstructured{Object: map[string]any{"spec": routeSpec(agent, namespace, port, exports)}}
	value.SetGroupVersionKind(RouteGVK)
	value.SetName(RouteName(agent))
	value.SetNamespace(namespace)
	value.SetAnnotations(map[string]string{AgentUIDAnnotation: uid})
	value.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "cetirun-operator"})
	value.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "cetirun.microsoft.com/v1alpha1", Kind: "Agent", Name: agent, UID: types.UID(uid), Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}})
	return value
}

// Istio's typed serialization may omit the zero attempts field, but must retain
// the explicit retries object. An absent retries object still enables defaults.
func RouteSpecsEqual(actual, desired any) bool {
	value, ok := actual.(map[string]any)
	if !ok {
		return false
	}
	copy := runtime.DeepCopyJSONValue(value).(map[string]any)
	if routes, ok := copy["http"].([]any); ok && len(routes) == 1 {
		if route, ok := routes[0].(map[string]any); ok {
			if retries, ok := route["retries"].(map[string]any); ok && len(retries) == 0 {
				retries["attempts"] = int64(0)
			}
		}
	}
	return reflect.DeepEqual(copy, desired)
}

func ValidatePrivateRoute(value *unstructured.Unstructured, agent, uid, namespace string, port int32, caller Caller) error {
	owner := metav1.GetControllerOf(value)
	exports, ok, err := unstructured.NestedStringSlice(value.Object, "spec", "exportTo")
	if err != nil || !ok || len(exports) == 0 || len(exports) > 8 || !slices.Contains(exports, caller.Namespace) {
		return fmt.Errorf("Chat route does not include the Portal namespace")
	}
	for _, item := range exports {
		if len(validation.IsDNS1123Label(item)) != 0 {
			return fmt.Errorf("Chat route has an unbounded namespace export")
		}
	}
	if value.GetUID() == "" || !value.GetDeletionTimestamp().IsZero() || value.GetName() != RouteName(agent) || value.GetNamespace() != namespace ||
		value.GetAnnotations()[AgentUIDAnnotation] != uid || owner == nil || owner.UID != types.UID(uid) || owner.Kind != "Agent" ||
		!RouteSpecsEqual(value.Object["spec"], routeSpec(agent, namespace, port, exports)) {
		return fmt.Errorf("Chat requires an owned private route with retries disabled")
	}
	return nil
}
