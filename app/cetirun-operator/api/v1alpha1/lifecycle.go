// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import "regexp"

const (
	AnnotationRestartRequestID = "cetirun.microsoft.com/restart-request-id"
	// SandboxPreparationFinalizer identifies retained single-start lifetimes.
	SandboxPreparationFinalizer    = "cetirun.microsoft.com/sandbox-preparation"
	SandboxHoldFinalizer           = "cetirun.microsoft.com/sandbox-hold"
	SandboxBootstrapFinalizer      = "cetirun.microsoft.com/sandbox-bootstrap"
	AnnotationSandboxAuthorization = "cetirun.microsoft.com/sandbox-authorization"
	AnnotationSandboxReauthorize   = "cetirun.microsoft.com/sandbox-reauthorize"
)

var restartRequestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

func ValidRestartRequestID(id string) bool {
	return restartRequestIDPattern.MatchString(id)
}
