// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"sort"
	"strings"

	api "github.com/Azure/CetiRun/app/cetirun-operator/api/v1alpha1"
	"github.com/Azure/CetiRun/app/internal/registryauth"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var defaultConnectionsMCPRuntimeTypes = []string{"hermes", "openclaw"}

// ParseConnectionsMCPRuntimeTypes reads the deployment allowlist. An omitted value
// preserves the established Hermes and OpenClaw behavior; an unknown runtime
// is rejected rather than being silently broadened or ignored.
func ParseConnectionsMCPRuntimeTypes(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return append([]string(nil), defaultConnectionsMCPRuntimeTypes...), nil
	}
	runtimeTypes := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
	})
	if len(runtimeTypes) == 0 {
		return nil, fmt.Errorf("Connections MCP runtime allowlist is empty")
	}
	return normalizeConnectionsMCPRuntimeTypes(runtimeTypes)
}

func normalizeConnectionsMCPRuntimeTypes(runtimeTypes []string) ([]string, error) {
	if len(runtimeTypes) == 0 {
		return append([]string(nil), defaultConnectionsMCPRuntimeTypes...), nil
	}
	allowed := map[string]struct{}{}
	for _, runtimeType := range runtimeTypes {
		runtimeType = strings.ToLower(strings.TrimSpace(runtimeType))
		if runtimeType != "hermes" && runtimeType != "openclaw" && runtimeType != "custom" {
			return nil, fmt.Errorf("Connections MCP runtime type %q is unsupported", runtimeType)
		}
		allowed[runtimeType] = struct{}{}
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("Connections MCP runtime allowlist is empty")
	}
	result := make([]string, 0, len(allowed))
	for runtimeType := range allowed {
		result = append(result, runtimeType)
	}
	sort.Strings(result)
	return result, nil
}

func connectionsMCPRuntimeAllowed(runtimeType api.AgentType, runtimeTypes []string) (bool, error) {
	allowed, err := normalizeConnectionsMCPRuntimeTypes(runtimeTypes)
	if err != nil {
		return false, err
	}
	for _, allowedType := range allowed {
		if string(runtimeType) == allowedType {
			return true, nil
		}
	}
	return false, nil
}

func (r *AgentReconciler) connectionsMCPEnabledFor(instance *api.Agent) (bool, error) {
	// Sandbox now shares the ordinary reconcile path, but its identity
	// projections remain exclusively owned by the Sandbox compiler.
	if r.ConnectionsMCPURL == "" ||
		((sandboxRequested(instance, nil) || instance.Spec.Agent.Type == api.AgentTypeCustom) && !r.nativeConnectionsRequested(instance)) {
		return false, nil
	}
	return connectionsMCPRuntimeAllowed(instance.Spec.Agent.Type, r.ConnectionsMCPRuntimeTypes)
}

func (r *AgentReconciler) connectionsMCPCA(ctx context.Context) ([]byte, error) {
	if r.ConnectionsMCPURL == "" {
		return nil, nil
	}
	if r.OperatorNamespace == "" || r.ConnectionsMCPCASecretName == "" {
		return nil, fmt.Errorf("Connections MCP CA source is not configured")
	}
	var source corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: r.OperatorNamespace, Name: r.ConnectionsMCPCASecretName}, &source); err != nil {
		return nil, fmt.Errorf("Connections MCP CA source is unavailable")
	}
	ca := source.Data["ca.crt"]
	if len(ca) == 0 || len(ca) > 64<<10 || !x509.NewCertPool().AppendCertsFromPEM(ca) || bytes.Contains(ca, []byte("PRIVATE KEY")) {
		return nil, fmt.Errorf("Connections MCP CA source is invalid")
	}
	return ca, nil
}

func (r *AgentReconciler) connectionsMCPCAHash(ctx context.Context) (string, error) {
	ca, err := r.connectionsMCPCA(ctx)
	if err != nil || ca == nil {
		return "", err
	}
	return secretContentHash(map[string][]byte{"ca.crt": ca}), nil
}

func (r *AgentReconciler) ensureConnectionsMCPCA(ctx context.Context, instance *api.Agent) error {
	ca, err := r.connectionsMCPCA(ctx)
	if err != nil || ca == nil {
		return err
	}
	key := types.NamespacedName{Namespace: instanceNamespace(instance.Name), Name: r.ConnectionsMCPCASecretName}
	var target corev1.Secret
	if err := r.Get(ctx, key, &target); errors.IsNotFound(err) {
		target = corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, OwnerReferences: []metav1.OwnerReference{registryauth.Owner(instance)}}, Data: map[string][]byte{"ca.crt": ca}}
		return r.Create(ctx, &target)
	} else if err != nil {
		return err
	}
	owned := false
	for _, owner := range target.OwnerReferences {
		if owner.UID == instance.UID && owner.Kind == "Agent" {
			owned = true
		}
	}
	if !owned {
		return fmt.Errorf("Connections MCP CA target ownership is invalid")
	}
	if len(target.Data) == 1 && bytes.Equal(target.Data["ca.crt"], ca) {
		return nil
	}
	target.Data = map[string][]byte{"ca.crt": ca}
	return r.Update(ctx, &target)
}
