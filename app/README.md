# app/

CetiRun is an open-source managed runtime for hosting AI agents and AI applications.

This directory contains its deployable application components.

Start with the [site architecture guide](../site/content/en/docs/concepts/architecture.md)
or [Install CetiRun](../site/content/en/docs/getting-started/platform.md).

| Directory | Description |
|-----------|-------------|
| `cetirun-platform/` | Umbrella chart that installs the operator and portal together. |
| `cetirun-operator/` | Kubernetes operator for Agent and Team CRDs and the Hermes-based [CetiAdmin](cetirun-operator/internal/cetiadmin). Includes the operator Helm chart, operations skills, and helpers. |
| `cetirun-chart/` | Per-instance agent Helm chart. It contains templates, values, configs, and named Dockerfiles for OpenClaw, Hermes, shared base, and desktop images. |
| `cetirun-portal/` | Self-service portal to create, manage, and monitor runtime instances. It contains a Go backend and a React frontend. |
| `cetirun-ai-gateway/` | AI Gateway: the `cetirun-ai-gateway` image, open-source LiteLLM on Azure Linux, and its Helm chart, which installs standalone with bundled PostgreSQL and Valkey. |
