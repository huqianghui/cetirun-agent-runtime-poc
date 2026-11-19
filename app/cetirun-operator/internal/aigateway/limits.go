// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package aigateway

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// KeyLimits are the request limits of a virtual key. A nil total, or a model
// missing from a per-model map, is unlimited. The Gateway rejects every
// request of a key with a zero limit, so a limit is either positive or absent.
//
// LiteLLM stores the totals in the key's own columns and the per-model maps
// in the key's metadata (model_rpm_limit and model_tpm_limit); /key/info
// returns both. Per-model limits apply to the model name a request uses.
type KeyLimits struct {
	MaxParallelRequests *int64
	RPM                 *int64
	TPM                 *int64
	ModelRPM            map[string]int64
	ModelTPM            map[string]int64
}

// invalidLimit records a stored limit the client cannot read as a positive
// integer. It never equals a desired limit, so a caller rewrites it.
const invalidLimit int64 = -1

// IsZero reports whether the limits leave every dimension unlimited.
func (l KeyLimits) IsZero() bool {
	return l.MaxParallelRequests == nil && l.RPM == nil && l.TPM == nil && len(l.ModelRPM) == 0 && len(l.ModelTPM) == 0
}

// Equal compares two sets of limits. An empty per-model map equals a nil one.
func (l KeyLimits) Equal(other KeyLimits) bool {
	return equalLimit(l.MaxParallelRequests, other.MaxParallelRequests) && equalLimit(l.RPM, other.RPM) &&
		equalLimit(l.TPM, other.TPM) && maps.Equal(l.ModelRPM, other.ModelRPM) && maps.Equal(l.ModelTPM, other.ModelTPM)
}

func equalLimit(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Validate rejects zero, negative and empty-named limits, which the Gateway
// would treat as rejecting every request rather than as unlimited.
func (l KeyLimits) Validate() error {
	for name, value := range map[string]*int64{
		"max parallel requests": l.MaxParallelRequests, "RPM": l.RPM, "TPM": l.TPM,
	} {
		if value != nil && *value < 1 {
			return fmt.Errorf("virtual key %s limit must be positive", name)
		}
	}
	for kind, limits := range map[string]map[string]int64{"RPM": l.ModelRPM, "TPM": l.ModelTPM} {
		for model, value := range limits {
			if strings.TrimSpace(model) == "" {
				return fmt.Errorf("virtual key per-model %s limit needs a model name", kind)
			}
			if value < 1 {
				return fmt.Errorf("virtual key per-model %s limit must be positive", kind)
			}
		}
	}
	return nil
}

// addTo adds the limits to a /key/generate body. Absent limits are omitted,
// which LiteLLM stores as no limit.
func (l KeyLimits) addTo(body map[string]any) {
	if l.MaxParallelRequests != nil {
		body["max_parallel_requests"] = *l.MaxParallelRequests
	}
	if l.RPM != nil {
		body["rpm_limit"] = *l.RPM
	}
	if l.TPM != nil {
		body["tpm_limit"] = *l.TPM
	}
	if len(l.ModelRPM) > 0 {
		body["model_rpm_limit"] = l.ModelRPM
	}
	if len(l.ModelTPM) > 0 {
		body["model_tpm_limit"] = l.ModelTPM
	}
}

// UpdateKeyLimits replaces every request limit of the key identified by token
// and leaves its other settings unchanged. An absent total is sent as an
// explicit null, which LiteLLM stores as no limit. The per-model maps are sent
// as top-level fields, which LiteLLM merges into the stored metadata one map
// at a time; an empty map clears it, while a null would leave the old map in
// place. The client never sends metadata itself: /key/update replaces stored
// metadata wholesale, which would drop the other entries on the key.
func (c *Client) UpdateKeyLimits(ctx context.Context, token string, limits KeyLimits) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	body := map[string]any{
		"key": token, "max_parallel_requests": nil, "rpm_limit": nil, "tpm_limit": nil,
		"model_rpm_limit": map[string]int64{}, "model_tpm_limit": map[string]int64{},
	}
	limits.addTo(body)
	_, err := c.do(ctx, http.MethodPost, "/key/update", body, nil)
	return err
}

// keyLimitsInfo is the part of a /key/info response that carries limits.
type keyLimitsInfo struct {
	MaxParallelRequests json.RawMessage `json:"max_parallel_requests"`
	RPMLimit            json.RawMessage `json:"rpm_limit"`
	TPMLimit            json.RawMessage `json:"tpm_limit"`
	Metadata            json.RawMessage `json:"metadata"`
}

// limits decodes the stored limits. A value that is not a positive integer
// becomes invalidLimit instead of an error, so a key an administrator edited
// by hand still reads and can be corrected.
func (info keyLimitsInfo) limits() KeyLimits {
	var metadata struct {
		ModelRPMLimit json.RawMessage `json:"model_rpm_limit"`
		ModelTPMLimit json.RawMessage `json:"model_tpm_limit"`
	}
	if text := strings.TrimSpace(string(info.Metadata)); text != "" && text != "null" {
		if err := json.Unmarshal(info.Metadata, &metadata); err != nil {
			metadata.ModelRPMLimit, metadata.ModelTPMLimit = json.RawMessage(`"unreadable"`), json.RawMessage(`"unreadable"`)
		}
	}
	return KeyLimits{
		MaxParallelRequests: limitValue(info.MaxParallelRequests), RPM: limitValue(info.RPMLimit), TPM: limitValue(info.TPMLimit),
		ModelRPM: modelLimits(metadata.ModelRPMLimit), ModelTPM: modelLimits(metadata.ModelTPMLimit),
	}
}

func limitValue(raw json.RawMessage) *int64 {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return nil
	}
	value := parseLimit(text)
	return &value
}

func modelLimits(raw json.RawMessage) map[string]int64 {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		// Not a map: report a model limit no policy can match.
		return map[string]int64{"": invalidLimit}
	}
	if len(values) == 0 {
		return nil
	}
	limits := make(map[string]int64, len(values))
	for model, value := range values {
		limits[model] = parseLimit(strings.TrimSpace(string(value)))
	}
	return limits
}

// parseLimit reads a JSON number as a positive integer. LiteLLM returns
// integers; an integral float is accepted as well.
func parseLimit(text string) int64 {
	if value, err := strconv.ParseInt(text, 10, 64); err == nil {
		if value < 1 {
			return invalidLimit
		}
		return value
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || value < 1 || value != math.Trunc(value) || value >= math.MaxInt64 {
		return invalidLimit
	}
	return int64(value)
}
