# CetiRun cert-manager support

This directory contains the pinned upstream cert-manager controller values and
image/archive lock used by CetiRun's optional `publicTLS` deployment mode. It
is not a CetiRun Helm chart and it does not create public DNS, Gateway API, or
Certificate resources by itself.

The `cetirun-platform` umbrella chart owns the port-80 ACME Gateway,
HTTPRoute, Issuers, and Certificates when `publicTLS.enabled: true`. The
normal platform deployment derives their release name and namespace from the
`platform` section and their ingress namespace from `istio.ingressNamespace`.
cert-manager remains a separately installable Helm release, either managed by
another approved workflow (`certManager.mode: existing`) or installed from the
explicit archive in the deployment configuration (`certManager.mode: install`).

For install mode, download the exact public archive outside the repository and
set `publicTLS.certManager.chartArchive` to its absolute path. The deployment
checks its checksum, chart version, controller image digests, Gateway API
support, and readiness before it creates the HTTP-01 resources. It never
downloads or upgrades a floating cert-manager chart.

New installations use `config.gatewayAPI.enabled: true`. Existing installations
may also use the deprecated `config.enableGatewayAPI` field. The preflight check
follows cert-manager's precedence: an explicit `gatewayAPI.enabled` value takes
priority, an unset or null value falls back to `enableGatewayAPI`, and the
`--enable-gateway-api` controller flag overrides the configuration file.

The `examples/` files are fragments for the `publicTLS` section of a full
environment configuration. They are synthetic and do not contain usable
cluster, Azure, identity, or credential values. Read the
[public DNS and TLS guide](../../site/content/en/docs/operations/public-ingress.md)
before a connected deployment.
