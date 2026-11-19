# CetiRun infrastructure

Before you deploy, read
[Infrastructure](../site/content/en/docs/operations/infrastructure.md) and the
[platform checklist](../site/content/en/docs/getting-started/platform.md).

[Agent Identity setup](../site/content/en/docs/operations/agent-identity.md)
gives information about the other resources that are necessary for Agent
connections: Key Vault, workload federation, certificates, and provider
registrations. The infrastructure templates do not create these resources.

| Directory | Purpose |
| --- | --- |
| `cluster/` | AKS Bicep, node pools, identity, CSI, and monitoring configuration |
| `storage/` | Shared Azure Blob NFS Bicep and subnet-aware deployment helper |
| `istio/` | Shared authentication, routing, security headers, and egress policy |
| `cert-manager/` | Pinned upstream cert-manager controller support files for optional public DNS and HTTP-01 TLS |

Deployment scripts change cloud resources and can change the local Kubernetes
configuration. Supply reviewed parameters that are specific to the environment.
Default parameter files and registry access are possibly not available.

For one HTTPS hostname on an existing managed Istio ingress, configure the
optional `publicTLS` section in the same environment file that you pass to
`make deploy`. The deployment uses the platform umbrella chart for its Gateway
API and certificate resources and can install or verify the pinned upstream
cert-manager release from `cert-manager/`. Read
[public DNS and TLS operations](../site/content/en/docs/operations/public-ingress.md)
before using it.
