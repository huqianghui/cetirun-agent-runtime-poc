// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const PodExpectationKey = "expectation.json"

func PodExpectationName(namespace string, uid types.UID) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + string(uid)))
	return "sandbox-pod-" + hex.EncodeToString(sum[:20])
}

// PodExpectationObject produces controller state, not an execution permit.
// Its writer and admission reader share this format without coupling their
// implementations. Expect always starts held; persistence cannot lift a hold.
func PodExpectationObject(stateNamespace string, expected Expectation) (*corev1.ConfigMap, error) {
	if stateNamespace == "" || stateNamespace == expected.Namespace.Name ||
		expected.Version != ExpectationVersion || expected.Agent.Name == "" || expected.Agent.UID == "" ||
		expected.Namespace.Name == "" || expected.Namespace.UID == "" {
		return nil, fmt.Errorf("invalid controller-owned Pod expectation")
	}
	raw, err := json.Marshal(expected)
	if err != nil {
		return nil, err
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: stateNamespace, Name: PodExpectationName(expected.Namespace.Name, expected.Namespace.UID),
			OwnerReferences: []metav1.OwnerReference{AgentOwner(expected.Agent.Name, expected.Agent.UID)},
			Annotations: map[string]string{
				RevisionAnnotation: expected.Revision, AgentUIDAnnotation: string(expected.Agent.UID),
			},
		},
		Data: map[string]string{PodExpectationKey: string(raw)},
	}, nil
}
