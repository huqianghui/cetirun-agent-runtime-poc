// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimebindings

import (
	"encoding/json"
	"errors"
	"fmt"

	strictjson "sigs.k8s.io/json"
)

var ErrSkillsMaterializationPending = errors.New("SkillsMaterializationPending")

// Custom consumes published skills; it cannot install packages in an arbitrary
// user image. Keep configuration mistakes distinct from missing model bindings.
func ValidateCustomSkills(value any) error {
	if value == nil {
		return nil
	}
	var skills struct {
		Shared           bool     `json:"shared"`
		StorageClassName string   `json:"storageClassName"`
		Protocol         string   `json:"protocol"`
		SecretName       string   `json:"secretName"`
		MountOptions     []string `json:"mountOptions"`
		ResourceGroup    string   `json:"resourceGroup"`
		StorageAccount   string   `json:"storageAccount"`
		ContainerName    string   `json:"containerName"`
		MountPath        string   `json:"mountPath"`
		ClawHub          []string `json:"clawHub"`
		NPM              []string `json:"npm"`
		NPMRegistry      string   `json:"npmRegistry"`
		StorageSize      string   `json:"storageSize"`
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("invalid Custom skills configuration")
	}
	problems, err := strictjson.UnmarshalStrict(raw, &skills)
	if err != nil || len(problems) != 0 {
		return fmt.Errorf("invalid or unknown Custom skills fields")
	}
	if len(skills.NPM) != 0 || len(skills.ClawHub) != 0 {
		return fmt.Errorf("%w: Custom requires published skills, not runtime npm or ClawHub installation", ErrSkillsMaterializationPending)
	}
	return nil
}
