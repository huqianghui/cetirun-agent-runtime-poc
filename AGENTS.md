# Working on CetiRun

CetiRun is an open-source managed runtime for hosting AI agents and AI applications.

Before you change the repository, read [CONTRIBUTING.md](CONTRIBUTING.md),
[DEVELOPMENT.md](DEVELOPMENT.md), and [SECURITY.md](SECURITY.md).

## Scope and collaboration

- Before you edit, make sure that you have the requested checkout, branch, and
  working-tree state. If the task supplies an explicit worktree, use that
  worktree.
- Keep the changes of other contributors. Edit only the files that are
  assigned to the task. Coordinate work that overlaps. Do not revert it.
- Follow a scope that is already approved. Do not reopen the same design
  decision. Before you make changes that expand that scope, ask.
- Use meaningful branch names, for example `fix/portal-session-list`,
  `feat/instance-status`, and `docs/local-development`.
- Unless explicitly requested, do not commit, push, open or merge pull
  requests, or publish releases. Unless explicitly requested, do not change
  repository settings or live resources.

## Implementation and checks

- Follow the conventions that the components already use. Keep changes
  focused.
- Use the validation entry points in [DEVELOPMENT.md](DEVELOPMENT.md).
  If the root Makefile is available, use `make check`, `make test`,
  `make lint`, and `make docs-check`. For an earlier checkout, use the
  documented component commands. Report the checks that are available in that
  checkout.
- First, run offline tests and render the charts locally. Do not run
  deployment scripts, release commands, real-cluster tests, or live service
  integrations without explicit authorization for the target environment.
- Before you run commands, examine them. A build or test task does not
  authorize a cloud login, a dependency upgrade, an unpinned generator, or a
  cluster mutation.
- If a dependency is not available, report it as a limitation. Do not replace
  a check that fails with a weaker check unless you explain the difference.
- Regenerate checked-in artifacts from their source with an agreed tool
  version. Do not edit generated CRDs or deepcopy code by hand.
- Keep build output, caches, local configuration, and unrelated format changes
  out of the diff.

## Versioning and publishing

- `app/cetirun-chart/Chart.yaml` is the default source of the release version.
  For an official version change, run `make set-version VERSION=X.Y.Z`. Do not
  update individual Chart, Go, or Portal version fields by hand.
- Validate the synchronized source with `make check`. Operator and Portal Go
  binaries receive the release version and the Git commit during Makefile,
  Docker, and release-workflow builds.
- After you get explicit authorization and registry authentication, publish
  everything with `make publish REGISTRY=registry.example.com`. If only one
  artifact class is requested, use `make publish-images` or
  `make publish-charts`.
- The publish process includes the `cetirun-pi-agent` demo and makes sure that
  the Charts can be pulled. It does not log in to the registry, deploy to
  Kubernetes, or authorize changes to another registry or environment.

## Public-data and review boundary

- Use synthetic fixtures and public example domains. Do not add credentials,
  tokens, cookies, authorization headers, private endpoints, customer data,
  agent conversations, or internal identifiers and documents.
- During routine development, do not read credential stores, token files,
  browser cookies, or live customer data. Any sensitive diagnostic work must
  have a separately authorized scope.
- Report suspected vulnerabilities through [SECURITY.md](SECURITY.md), not
  through public issues or pull requests.
- Do not invent owner accounts or teams. Changes to review ownership must have
  maintainer confirmation. Refer to [GOVERNANCE.md](GOVERNANCE.md).
- Maintainers decide if they accept a contribution. Agent output does not
  replace independent maintainer review.
- At handoff, list the changed paths, the commands and results, the
  limitations that still apply, and the checks not run. Distinguish source
  inspection, offline validation, and live checks. Do not claim that limited
  tests establish security.
