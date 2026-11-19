// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"time"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/cetirun-portal/internal/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// The Portal checks the identity and age of an observation, never policy
// compatibility itself. Admission and the Operator own compatibility rules.
func teamCompatibilityObservation(team *api.Team) *api.TeamCompatibilityStatus {
	if !team.Spec.PolicyConfigured() || team.Status.Compatibility == nil {
		return nil
	}
	report := team.Status.Compatibility.DeepCopy()
	if !compatibilityReferenceMatches(report.Team, team) || !team.DeletionTimestamp.IsZero() ||
		report.Policy.Name != team.Spec.AgentPolicyRef || len(report.Sandboxes) != len(team.Spec.SandboxRefs) {
		return unknownCompatibility(report, "ConfigurationChanged", "Configuration changed; waiting for the Operator to check this version.")
	}
	seen := make(map[string]bool, len(report.Sandboxes))
	for _, result := range report.Sandboxes {
		if !slices.Contains(team.Spec.SandboxRefs, result.Sandbox.Name) || seen[result.Sandbox.Name] {
			return unknownCompatibility(report, "ConfigurationChanged", "Sandbox selection changed; waiting for the Operator to check this version.")
		}
		seen[result.Sandbox.Name] = true
	}
	if report.State == api.CompatibilityUnknown {
		return unknownCompatibility(report, report.Reason, report.Message)
	}
	now := time.Now()
	if report.CheckedAt.IsZero() || report.ValidUntil.IsZero() || report.CheckedAt.After(now) ||
		!report.ValidUntil.After(now) || !report.ValidUntil.After(report.CheckedAt.Time) ||
		report.ValidUntil.Sub(report.CheckedAt.Time) > 30*time.Second {
		return unknownCompatibility(report, "ObservationExpired", "The compatibility observation is unavailable or expired; waiting for a current check.")
	}
	return report
}

func compatibilityReferenceMatches(reference api.CompatibilityReference, object metav1.Object) bool {
	return reference.Name == object.GetName() && reference.UID != "" &&
		reference.UID == string(object.GetUID()) && reference.Generation == object.GetGeneration()
}

func unknownCompatibility(report *api.TeamCompatibilityStatus, reason, message string) *api.TeamCompatibilityStatus {
	report.State, report.Reason, report.Message = api.CompatibilityUnknown, reason, message
	report.Issues = nil
	for i := range report.Sandboxes {
		report.Sandboxes[i].State = api.CompatibilityUnknown
		report.Sandboxes[i].Reason = reason
		report.Sandboxes[i].Issues = nil
	}
	return report
}

// Read references once for the whole response. A missing dependency or a read
// failure does not hide Teams or turn an old observation into a successful one.
func (h *AdminTeamHandler) refreshCompatibilityReferences(ctx context.Context, teams []TeamDTO) {
	if !slices.ContainsFunc(teams, func(team TeamDTO) bool { return team.Compatibility != nil }) {
		return
	}
	policies, policyErr := h.Kube.ListAgentPolicies(ctx)
	boxes, sandboxErr := h.Kube.ListSandboxes(ctx)
	policyByName := make(map[string]*api.AgentPolicy, len(policies))
	for i := range policies {
		policyByName[policies[i].Name] = &policies[i]
	}
	boxByName := make(map[string]*api.Sandbox, len(boxes))
	for i := range boxes {
		boxByName[boxes[i].Name] = &boxes[i]
	}
	if policyErr != nil || sandboxErr != nil {
		slog.Debug("Compatibility references unavailable", "error", errors.Join(policyErr, sandboxErr))
	}
	for i := range teams {
		report := teams[i].Compatibility
		if report == nil || report.State == api.CompatibilityUnknown {
			continue
		}
		if policyErr != nil || sandboxErr != nil {
			unknownCompatibility(report, "DependenciesUnavailable", "Current Policy and Sandbox versions could not be read.")
			continue
		}
		policy := policyByName[teams[i].AgentPolicyRef]
		changed := policy == nil && report.Policy.UID != ""
		if policy != nil {
			changed = !policy.DeletionTimestamp.IsZero() || !compatibilityReferenceMatches(report.Policy, policy)
		}
		for _, result := range report.Sandboxes {
			box := boxByName[result.Sandbox.Name]
			if (box == nil && result.Sandbox.UID != "") || (box != nil &&
				(!box.DeletionTimestamp.IsZero() || !compatibilityReferenceMatches(result.Sandbox, box))) {
				changed = true
				break
			}
		}
		if changed {
			unknownCompatibility(report, "ConfigurationChanged", "A referenced Policy or Sandbox changed; waiting for the Operator to check the current combination.")
		}
	}
}

var compatibilityFieldPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.\[\]-]{0,255}$`)

// Only the versioned, fixed-message compatibility contract may cross this
// boundary. Ordinary admission and API transport errors retain their existing
// handling; arbitrary Kubernetes error text is not a compatibility diagnostic.
func compatibilityErrorDetails(err error) []api.CompatibilityIssue {
	var status apierrors.APIStatus
	if !errors.As(err, &status) || status.Status().Details == nil {
		return nil
	}
	var issues []api.CompatibilityIssue
	for _, cause := range status.Status().Details.Causes {
		if cause.Type != "CetiRunCompatibility" || len(cause.Message) > 4096 || len(issues) >= 128 {
			continue
		}
		decoder := json.NewDecoder(bytes.NewBufferString(cause.Message))
		decoder.DisallowUnknownFields()
		var issue api.CompatibilityIssue
		if err := decoder.Decode(&issue); err != nil || !issue.Valid() {
			continue
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			continue
		}
		if issue.Field != cause.Field || (issue.Field != "" && !compatibilityFieldPattern.MatchString(issue.Field)) {
			continue
		}
		valid := true
		for _, reference := range []*api.CompatibilityReference{issue.Team, issue.Policy, issue.Sandbox} {
			if reference != nil && (len(validation.IsDNS1123Subdomain(reference.Name)) != 0 ||
				len(reference.UID) > 128 || reference.Generation < 0) {
				valid = false
			}
		}
		if valid {
			issues = append(issues, issue)
		}
	}
	return issues
}

func writeCompatibilityError(w http.ResponseWriter, err error) bool {
	// A structured underlying rejection does not resolve an uncertain coordinated
	// operation. Keep the established recovery/conflict response in that case.
	if kube.IsConfigurationCoordinationUnavailable(err) || kube.IsConfigurationCoordinationConflict(err) ||
		apierrors.IsConflict(err) || errors.Is(err, kube.ErrConfigurationChanged) || errors.Is(err, kube.ErrAgentPolicyChanged) {
		return false
	}
	issues := compatibilityErrorDetails(err)
	if len(issues) == 0 {
		return false
	}
	writeJSON(w, http.StatusUnprocessableEntity, ErrorResponse{Error: ErrorDetail{
		Code: "configuration_incompatible", Message: "The configuration could not be saved because its Policy and Sandbox bindings are incompatible.", Details: issues,
	}})
	return true
}
