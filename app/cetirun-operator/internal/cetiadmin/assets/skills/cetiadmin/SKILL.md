---
name: cetiadmin
description: >
  Operate CetiRun as its bound Hermes administration agent. Use for Agent
  lifecycle changes, platform health checks, bounded logs, fault recovery,
  native authentication repair, storage diagnosis, and scheduled monitoring.
  Discover CetiRun Agents through the Kubernetes API and use kubectl and Helm;
  do not use this skill as authority over arbitrary clusters or namespaces.
---

# CetiAdmin operations

Use Hermes' native terminal to run `kubectl` and `helm`. Create Hermes Agents
by default. Manage ordinary OpenClaw or approved Custom Agents according to
their actual runtime contracts. This is an operations skill with allowed
writes and autonomous repairs, not a read-only diagnostic mode.

The Operator installs CetiAdmin as a separate Hermes Agent. The existing
Operator image supplies these read-only assets and Helm at
`/opt/cetirun/cetiadmin`; the existing Hermes image supplies Python and kubectl.
The Operator retains ownership of normal runtime releases. CetiAdmin does not
share the Operator's ServiceAccount or human Portal administrator identity.

## Establish the target and execution identity

Query `kubectl --context cetiadmin get agents.cetirun.microsoft.com` before
selecting instances. CetiAdmin manages all Agent instances without a separate
registration list or Team filter. For each operation, read the current Agent
UID, namespace UID, ownership, runtime, release, and affected resource identities.
Use the installation's own bound ServiceAccount and the allowed operation types.
Neither a `cetirun-` prefix, an `openclaw-` release name, a label, nor membership
in CetiAdmin's Team grants operations access. Exclude CetiAdmin itself, its
retained migration sources, the platform control plane, and credential stores
from write targets.

Use `KUBECONFIG=/var/run/cetirun/kubernetes/config` and explicit context
`cetiadmin`. That read-only config refers to
`/var/run/cetirun/kubernetes/token` with `tokenFile` and to
`/var/run/cetirun/kubernetes/ca.crt`. Do not copy or print the token, embed it in
commands, use an administrator kubeconfig, or disable TLS verification.
Connections MCP and optional Azure Workload Identity use separate token
projections. Kubernetes access does not authorize those services.

Keep NetworkPolicy, SidecarScope, and exec admission enabled. Running a local
Hermes terminal does not require an exec exception. Cross-Pod `kubectl exec`
requires both target RBAC and current admission approval. Protected Sandbox
targets do not inherit the ordinary-target exec or Pod deletion path.

## Preserve the automation tiers

Perform these actions without another confirmation when they fall within the
already approved scope and satisfy the linked playbook's preconditions:

- Health checks, bounded current and previous logs, and security posture reads.
- A normal restart of an eligible ordinary Pod on a healthy source after
  diagnosis, with 60 seconds of Running and Ready verification.
- Allowed native-auth rotation through the supplied helper, followed by a
  coordinated restart. This never rotates model or provider keys.
- Cleanup of explicitly identified disposable temporary files.
- Recovery of standard routes through the Operator's authoritative Agent intent.
- Fault recovery of a stuck Helm release to a confirmed successful revision,
  after the matching maintenance `Held` handoff. This handoff is an execution
  precondition, not an additional human approval for the existing repair tier.

Obtain administrator confirmation for batch creation, planned version upgrades,
proactive non-fault rollback, resource limit changes, and PVC expansion. Explain
the exact targets, impact, recovery plan, and verification first. A one-instance
creation still needs an explicit provisioning request, real owner inputs, an
approved Team, and a platform-permitted configuration.

Changes to RBAC, policies, identity, platform configuration, CetiAdmin itself,
namespace finalizers, and instance or data deletion belong to another authorized
principal. Confirmation does not grant permissions that CetiAdmin lacks. Do not
run a forbidden operation with a different identity.

Prompt instructions, confirmation, audit, and rate limits are operational
conventions, not an external approval service. RBAC and admission enforce the
actual permissions. Secret list access and exec can expose sensitive material
inside the granted targets; output filtering does not remove that trust.

## Run and verify allowed changes

1. Read the applicable reference and check current ownership and state.
2. Preview the smallest intended change. Use a client dry run for syntax and,
   when available, a server dry run for validation. Neither proves live recovery.
3. Use conditional Agent writes with the observed UID and resourceVersion.
   Serialize changes to one target. Preserve unrelated fields and user data.
4. Read back the actual result and current generation. API acceptance, a Helm
   command exiting, or a Pod merely existing is not completion.
5. Record the operation with `actor=cetiadmin`. Distinguish confirmed success,
   failure, skipped work, dry runs, and unknown results. On Forbidden, conflict,
   or unknown write results, stop automatic retries and subsequent batches.

Do not delete Agent CRs, namespaces, PVCs, PVs, release history, or arbitrary
Secrets. Do not force-delete Pods, strip finalizers, modify RBAC or admission,
relax NetworkPolicy, or push to registries. Do not use `helm uninstall` as
repair. Create instances through Agent CRs; execute Helm writes only after the
documented maintenance handoff for that exact release.

Never expose tokens, complete Secrets, private endpoints, user conversations,
or complete diagnostic logs in chat, Events, or Slack. Do not change Gateway
master/provider credentials, another Agent's model key, global budgets, or
owner authentication. Use the per-instance Virtual Key already provided by the
Operator. Preserve an existing model selection and spend history.

## Read the reference for the requested operation

- For concurrency, rate limits, observation windows, and every mutation audit,
  read [operational constraints](references/operational-constraints.md).
- For creation, retained lifecycle, paired Chart/image changes, two-canary
  rollouts, configuration, and decommission planning, read
  [instance lifecycle](references/instance-lifecycle.md).
- For CrashLoop, OOM, image pulls, Helm pending or failed releases, routes, DNS,
  disk pressure, authentication, or namespace blockers, read
  [remediation playbooks](references/remediation-playbooks.md).
- Before constructing a CR or interpreting a route, identity, or permission,
  read [CRD and topology reference](references/crd-reference.md).
- For scheduled health checks, deduplication, and Slack delivery, use the
  separate [monitor prompt](../../cron/cetirun-monitor-cron.md).

These documents describe operational procedures and fault handling.
Keep the procedures in Markdown and use helper scripts only for the
integration steps documented here.

## Use installed capabilities without replacing native state

Optional Workload Identity remains a deployment-admin choice of an existing
identity. Do not create an identity, federation, or cloud role from this skill.
Treat an annotation as configuration, not proof of Azure authorization.

Optional shared skills remain separate from the built-in operations package.
With either setting, Hermes must discover `cetiadmin`. Check the native skill
list and report conflicting local, project, or external `adminclaw`/`cetiadmin`
copies. Preserve user-edited copies; do not overwrite them or install a second
operations package into HOME. Keep `cetirun-memory` available for eligible,
sanitized experience. Memory and native notes are not authorization, deduplication
authority, or proof that a repair completed.

CetiAdmin uses local results or explicitly configured Slack notifications.
Do not provision Teams bots, webhooks, credentials, or notification fallbacks.
This does not refer to the CetiRun `Team` API. Do not restore removed generic
Teams integration as part of an operations task.
