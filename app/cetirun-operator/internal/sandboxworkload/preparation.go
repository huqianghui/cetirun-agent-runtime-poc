// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandboxworkload

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	strictjson "sigs.k8s.io/json"
)

const (
	PreparationReceiptKey = "preparation.json"
	NativeStartAnnotation = "sandbox.cetirun.microsoft.com/native-started"
)

// PreparationPrerequisites contains persisted identities, never credentials.
// Field names preserve the existing controller receipt format.
type PreparationPrerequisites struct {
	AgentUID           types.UID
	AgentGeneration    int64
	Revision           string
	ObservedAt         time.Time
	NamespaceUID       types.UID
	Home               ObjectIdentity
	Skills             ObjectIdentity
	SkillsMode         SkillsMode `json:",omitempty"`
	RuntimeSecret      ObjectIdentity
	ChartVersion       string
	Helpers            Helpers
	PublicationSources []SourceReference `json:",omitempty"`
	Placement          *PlacementBinding `json:",omitempty"`
}

type PreparationReceipt struct {
	Version         string                   `json:"version"`
	AgentName       string                   `json:"agentName"`
	AgentUID        types.UID                `json:"agentUID"`
	NativeExecution bool                     `json:"nativeExecution,omitempty"`
	Prerequisites   PreparationPrerequisites `json:"prerequisites"`
}

func PreparationReceiptName(uid types.UID) string {
	sum := sha256.Sum256([]byte(uid))
	return "sandbox-preparation-" + hex.EncodeToString(sum[:20])
}

// DecodePreparationReceipt checks the common persisted envelope. Callers still
// validate their current execution or historical cleanup transition separately.
func DecodePreparationReceipt(object *corev1.ConfigMap, agent ObjectIdentity) (*PreparationReceipt, error) {
	if object == nil {
		return nil, fmt.Errorf("Sandbox preparation receipt is required")
	}
	var receipt PreparationReceipt
	raw := object.Data[PreparationReceiptKey]
	problems, err := strictjson.UnmarshalStrict([]byte(raw), &receipt)
	if err != nil || len(problems) != 0 || len(raw) > 64*1024 || len(object.Data) != 1 || len(object.BinaryData) != 0 ||
		object.UID == "" || object.ResourceVersion == "" ||
		!equality.Semantic.DeepEqual(object.OwnerReferences, []metav1.OwnerReference{AgentOwner(agent.Name, agent.UID)}) ||
		object.Annotations[AgentUIDAnnotation] != string(agent.UID) ||
		!((receipt.Version == "v1" && receipt.Prerequisites.Placement == nil) ||
			(receipt.Version == "v2" && receipt.Prerequisites.Placement != nil)) ||
		receipt.AgentName != agent.Name || receipt.AgentUID != agent.UID ||
		receipt.Prerequisites.AgentUID != agent.UID || receipt.Prerequisites.AgentGeneration < 1 ||
		receipt.Prerequisites.NamespaceUID == "" || receipt.Prerequisites.Home.Name == "" || receipt.Prerequisites.Home.UID == "" {
		return nil, fmt.Errorf("Sandbox preparation ownership receipt is invalid")
	}
	if inputs := receipt.Prerequisites; inputs.SkillsMode != "" {
		if err := inputs.SkillsMode.ValidateIdentity(inputs.Skills); err != nil {
			return nil, err
		}
		if len(inputs.PublicationSources) != 0 || inputs.Placement != nil {
			return nil, fmt.Errorf("empty skills preparation cannot contain a publication or non-native placement")
		}
	}
	return &receipt, nil
}
