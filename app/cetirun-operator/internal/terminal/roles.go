// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package terminal

import (
	"github.com/Azure/CetiRun/app/internal/terminalpolicy"
	rbacv1 "k8s.io/api/rbac/v1"
)

func ReadRoles(namespace, workload, agentName, agentUID string) (*rbacv1.Role, *rbacv1.ClusterRole) {
	return terminalpolicy.ReadRoles(namespace, workload, agentName, agentUID)
}
