# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

PYTHON ?= python3
REGISTRY ?= $(shell scripts/cetirun-release-metadata.sh registry)
VERSION ?= $(shell scripts/cetirun-release-metadata.sh version)
GIT_SHA ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
VERIFY_PULL ?= true
FORCE_PUBLISH ?= false
ACCEPT_VSCODE_SERVER_LICENSE_TERMS ?= false
DEPLOY_CONFIG ?=
.DEFAULT_GOAL := help

.PHONY: help check test lint docs-check license-check ci-test python-test \
	python-check go-test go-lint portal-test portal-lint chart-check \
	bicep-check shell-check workflow-check version-check set-version \
	build-images publish-images publish-charts publish require-registry artifact-check deploy deploy-plan deploy-verify

.NOTPARALLEL: publish publish-images publish-charts

help:
	@echo "Offline validation (install tools/dependencies separately):"
	@echo "  make check       Run lint and unit tests; no cluster/cloud or downloads"
	@echo "  make test        Go, portal, auth, runtime, and CI helper unit tests"
	@echo "  make lint        Headers, Go vet, TypeScript, Python, Helm, Bicep, shell, workflows"
	@echo "  make docs-check  Run make -C site check (separate from check)"
	@echo "Release:"
	@echo "  make set-version VERSION=<version>  (e.g. 0.1.0-dev.1)"
	@echo "  make build-images REGISTRY=<registry> [VERSION=X.Y.Z]"
	@echo "  make publish REGISTRY=<registry> [VERSION=X.Y.Z]"
	@echo "  Pull defaults use MCR; publishing requires an explicit writable REGISTRY."
	@echo "  VERSION defaults to the Runtime Chart; MCR publication uses the ADO backing-ACR pipeline."
	@echo "  Build/publish overrides use temporary copies; only set-version edits source."
	@echo "  Image builds require ACCEPT_VSCODE_SERVER_LICENSE_TERMS=true"
	@echo "Deployment (explicit environment authorization required):"
	@echo "  make deploy-plan DEPLOY_CONFIG=/path/to/environment.yaml  (offline)"
	@echo "  make deploy DEPLOY_CONFIG=/path/to/environment.yaml"
	@echo "  make deploy-verify DEPLOY_CONFIG=/path/to/environment.yaml  (read-only live)"
	@echo "Prerequisites: Go from .go-version, Node 24 + npm ci, scripts/ci/requirements.txt,"
	@echo "  helm, bicep, shellcheck, actionlint, scripts/check-license-headers.py."
	@echo "Workflow lint covers seven validation workflows; publisher/E2E workflows are excluded."
	@echo "See python3 scripts/ci/validate.py --help for individual gates."

check: lint test

test: ci-test python-test go-test portal-test memory-native-test

.PHONY: memory-native-test
memory-native-test:
	node --test app/cetirun-memory-context/tests/native.test.mjs

lint: version-check artifact-check license-check python-check go-lint portal-lint chart-check bicep-check shell-check workflow-check

docs-check license-check ci-test python-test python-check go-test go-lint portal-test portal-lint chart-check bicep-check shell-check workflow-check:
	$(PYTHON) -B scripts/ci/validate.py $@

require-registry:
	@test -n "$(REGISTRY)" || { echo "REGISTRY is required" >&2; exit 1; }

version-check:
	$(PYTHON) -B scripts/cetirun-version.py check "$(VERSION)"

set-version:
	@command -v helm >/dev/null 2>&1 || { echo "helm is required" >&2; exit 1; }
	$(PYTHON) -B scripts/cetirun-version.py set "$(VERSION)"
	helm dependency update --skip-refresh app/cetirun-platform
	$(PYTHON) -B scripts/cetirun-version.py check "$(VERSION)"

artifact-check:
	$(PYTHON) -B scripts/cetirun-release.py check

build-images publish-images publish-charts publish: require-registry artifact-check
	@test "$(FORCE_PUBLISH)" = false -o "$(FORCE_PUBLISH)" = true || { echo "FORCE_PUBLISH must be true or false" >&2; exit 1; }
	@test "$(VERIFY_PULL)" = true || { echo "Unified releases require Chart pull verification" >&2; exit 1; }
	REGISTRY="$(REGISTRY)" VERSION="$(VERSION)" GIT_SHA="$(GIT_SHA)" ACCEPT_VSCODE_SERVER_LICENSE_TERMS="$(ACCEPT_VSCODE_SERVER_LICENSE_TERMS)" $(PYTHON) -B scripts/cetirun-release.py $@ $(if $(filter true,$(FORCE_PUBLISH)),--force,)

deploy-plan deploy deploy-verify:
	@test -n "$(DEPLOY_CONFIG)" || { echo "DEPLOY_CONFIG is required; see app/cetirun-platform/README.md" >&2; exit 1; }
	bash scripts/cetirun-platform-deploy.sh --config "$(DEPLOY_CONFIG)" $(if $(filter deploy-plan,$@),--plan,) $(if $(filter deploy-verify,$@),--verify,) $(if $(filter command line,$(origin VERSION)),--version "$(VERSION)",) $(if $(filter command line,$(origin REGISTRY)),--registry "$(REGISTRY)",)
