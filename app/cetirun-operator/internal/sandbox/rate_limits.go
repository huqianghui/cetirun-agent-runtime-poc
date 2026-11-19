// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package sandbox

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
)

// Minimum Gateway rate limits. They repeat the CRD's minimums for policies
// that reach the controller without schema validation. The Gateway rejects
// every request of a key with a zero limit, and a token limit below a few
// thousand would refuse all but the smallest requests.
const (
	minPolicyRequestLimit   int64 = 1
	minPolicyTokenLimit     int64 = 4096
	maxPolicyRateModels           = 64
	maxPolicyModelNameRunes       = 256
)

// maxPolicyParallelRequests is the largest max_parallel_requests the Gateway
// stores: LiteLLM 1.105.1 keeps it in a 32-bit integer column.
const maxPolicyParallelRequests = 2147483647

// ValidatePolicyRateLimits checks the per-key Gateway rate limits. They are
// enforced by the Gateway on each Agent's key, not by the Sandbox.
func ValidatePolicyRateLimits(limits *v1alpha1.AgentPolicyRateLimits, path *field.Path) field.ErrorList {
	if limits == nil {
		return nil
	}
	var errs field.ErrorList
	minimum := func(value *int64, path *field.Path, min int64) {
		if value != nil && *value < min {
			errs = append(errs, field.Invalid(path, *value, "must be at least "+strconv.FormatInt(min, 10)+"; omit the field for no limit"))
		}
	}
	minimum(limits.MaxParallelRequests, path.Child("maxParallelRequests"), minPolicyRequestLimit)
	if value := limits.MaxParallelRequests; value != nil && *value > maxPolicyParallelRequests {
		errs = append(errs, field.Invalid(path.Child("maxParallelRequests"), *value, "must be at most "+strconv.FormatInt(maxPolicyParallelRequests, 10)+", which the Gateway can store"))
	}
	minimum(limits.RPM, path.Child("rpm"), minPolicyRequestLimit)
	minimum(limits.TPM, path.Child("tpm"), minPolicyTokenLimit)
	modelPath := path.Child("perModel")
	if len(limits.PerModel) > maxPolicyRateModels {
		errs = append(errs, field.TooMany(modelPath, len(limits.PerModel), maxPolicyRateModels))
	}
	for _, model := range slices.Sorted(maps.Keys(limits.PerModel)) {
		value := limits.PerModel[model]
		if model == "" || utf8.RuneCountInString(model) > maxPolicyModelNameRunes || strings.ContainsAny(model, " \t\r\n\v\f") {
			errs = append(errs, field.Invalid(modelPath.Key(model), model, "must be a Gateway model name of 1 to 256 characters without whitespace"))
		}
		if value.RPM == nil && value.TPM == nil {
			errs = append(errs, field.Required(modelPath.Key(model), "set rpm, tpm, or both"))
		}
		minimum(value.RPM, modelPath.Key(model).Child("rpm"), minPolicyRequestLimit)
		minimum(value.TPM, modelPath.Key(model).Child("tpm"), minPolicyTokenLimit)
	}
	return errs
}
