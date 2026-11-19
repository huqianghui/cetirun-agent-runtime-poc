// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package terminal

import (
	"context"
	"errors"
	"strings"

	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	clientexec "k8s.io/client-go/util/exec"
)

func DetectShell(ctx context.Context, probe func(context.Context, []string) error) (string, error) {
	for _, shell := range terminalpolicy.ShellCandidates() {
		err := probe(ctx, shell.Probe)
		if err == nil {
			return shell.Name, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if !shellMissing(err, shell.Name) {
			return "", err
		}
	}
	return "", terminalpolicy.ErrNoShell
}

func shellMissing(err error, shell string) bool {
	if denied(err) || apierrors.IsNotFound(err) || apierrors.IsTimeout(err) {
		return false
	}
	var status clientexec.ExitError
	if errors.As(err, &status) && (status.ExitStatus() == 126 || status.ExitStatus() == 127) {
		return true
	}
	// CRI runtimes can report a missing executable as an API error instead of
	// exit 127. Do not turn a generic API/transport failure into shell fallback.
	message := strings.ToLower(err.Error())
	return strings.Contains(message, shell) && strings.Contains(message, "exec") &&
		(strings.Contains(message, "executable file not found") ||
			strings.Contains(message, "executable not found") ||
			strings.Contains(message, "no such file or directory"))
}
