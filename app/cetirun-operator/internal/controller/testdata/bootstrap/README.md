# Pre-mTLS bootstrap RBAC contract

`pre-340-rbac.json` freezes the namespace Role and RoleBinding returned by
`operatorNamespaceRBAC` in product commit
`74a7aedb57f039878af4dcc7b73cf9124c1070ca`, before PR #340 added
`destinationrules`. Its source file is
`app/cetirun-operator/internal/controller/agent_controller.go`. The Git blob is
`52ffa26d634296c9fc03fe01fed9da7ff82e5570`.

The namespace and ServiceAccount arguments are synthetic. This is constructor
output, not a backup of a real cluster. Bootstrap adds its recorded UIDs,
ownership, annotations and finalizers separately. The migration tests keep those
identities from their own fixtures.

Tests load these historical rules directly. To create the old rules, they must
not delete a field from today's Role or call the production migration helper.
The explicit compatibility test allows only the documented `destinationrules`
addition and an unchanged binding contract.

This fixture replay is offline/isolated-API evidence. It does not execute the
old Operator binary or qualify an AKS upgrade, arbitrary historical versions,
or a general RBAC migration.
