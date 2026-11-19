# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# cetirun-operator Makefile

# Image name for the operator
REGISTRY ?= $(shell ../../scripts/cetirun-release-metadata.sh registry)
IMG ?= $(REGISTRY)/cetirun-operator:$(VERSION)

# Go build settings
PYTHON ?= python3
GOFLAGS ?=
CGO_ENABLED ?= 0
VERSION ?= $(shell ../../scripts/cetirun-release-metadata.sh version)
GIT_SHA ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
LDFLAGS = -X github.com/Azure/CetiRun/app/cetirun-operator/internal/buildinfo.Version=$(VERSION) \
	-X github.com/Azure/CetiRun/app/cetirun-operator/internal/buildinfo.Commit=$(GIT_SHA)
CONTROLLER_GEN_VERSION := v0.21.0
CONTROLLER_GEN ?= controller-gen
# Bound each package or complementary controller test group.
GO_TEST_TIMEOUT := 30m
GO_CONTROLLER_TEST_GROUP := ^Test(Reconciled|Planned|Native|Container|Common)
# Bound each complementary partition of the full tagged suite.
GO_ENVTEST_TIMEOUT := 50m
GO_TEST_SHARD_INDEX ?= 1
GO_TEST_SHARD_COUNT ?= 1
GO_TEST_RUNNER = $(PYTHON) -B ../../scripts/ci/go_test.py \
	--shard-index $(GO_TEST_SHARD_INDEX) --shard-count $(GO_TEST_SHARD_COUNT) --
GO_ENVTEST_PACKAGES := ./test/integration ./internal/helm ./internal/admission ./internal/controller ./internal/sandboxwriter ./internal/sandboxcoordination ./cmd/sandbox-config-change
GO_ENVTEST_SPLIT := APIServer

.PHONY: all build build-terminal build-node-guard build-sandbox-config-change test vet fmt lint docker-build docker-push install-crd \
	tools generate manifests check-controller-gen sync-crds check-crds \
	verify-generated test-envtest test-sandbox-integration test-sandbox-lifecycle

CRD_SOURCE_DIR := config/crd/bases
CRD_CHART_DIR := chart/templates/crds
CRD_FILES := cetirun.microsoft.com_agents.yaml cetirun.microsoft.com_teams.yaml \
	cetirun.microsoft.com_sandboxes.yaml cetirun.microsoft.com_agentpolicies.yaml

all: build

## Build the operator binary and its Skill Marketplace worker
build:
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/manager ./cmd/main.go
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/skill-marketplace ./cmd/skill-marketplace

## Build the independent Custom terminal and static HTTP probe
build-terminal:
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -o bin/terminal ./cmd/terminal
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -o bin/loopback-probe ./cmd/loopback-probe

## Build the standalone opt-in node guard prototype; not part of operator startup
build-node-guard:
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -o bin/sandbox-node-guard ./cmd/sandbox-node-guard

## Build the explicit coordinated configuration updater; no cluster access
build-sandbox-config-change:
	CGO_ENABLED=$(CGO_ENABLED) go build $(GOFLAGS) -o bin/sandbox-config-change ./cmd/sandbox-config-change

## Run unit tests
test:
ifeq ($(GO_TEST_SHARD_INDEX)/$(GO_TEST_SHARD_COUNT),1/1)
	@set -eu; \
		packages="$$(go list -f '{{if ne .ImportPath "github.com/Azure/CetiRun/app/cetirun-operator/internal/controller"}}{{.ImportPath}}{{end}}' ./...)"; \
		test -n "$$packages"; \
		go test $$packages -v -count=1 -timeout=$(GO_TEST_TIMEOUT)
	go test ./internal/controller -v -count=1 -timeout=$(GO_TEST_TIMEOUT) -run '$(GO_CONTROLLER_TEST_GROUP)'
	go test ./internal/controller -v -count=1 -timeout=$(GO_TEST_TIMEOUT) -skip '$(GO_CONTROLLER_TEST_GROUP)'
else
	$(GO_TEST_RUNNER) ./... -v -count=1 -timeout=$(GO_TEST_TIMEOUT)
endif

## Run go vet
vet:
	go vet ./...

## Run gofmt
fmt:
	gofmt -w .

## Download dependencies
deps:
	go mod download
	go mod tidy

## Build the Docker image
docker-build:
	docker build -f Dockerfile -t $(IMG) \
		--build-arg VERSION=$(VERSION) --build-arg GIT_SHA=$(GIT_SHA) ../../

## Push the Docker image
docker-push:
	../../scripts/cetirun-release-metadata.sh check-push-registry "$(IMG)"
	docker push $(IMG)

## Explicit tool setup (network access may be required; not an offline check).
tools:
	GOBIN="$(CURDIR)/bin" GOTOOLCHAIN=local go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

## Generate DeepCopy methods from API types
generate: check-controller-gen
	$(CONTROLLER_GEN) object paths="./api/..."

## Generate CRD YAML from API types; chart RBAC remains a source template
manifests: check-controller-gen
	$(CONTROLLER_GEN) crd paths="./api/..." \
		output:crd:artifacts:config=$(CRD_SOURCE_DIR)
	$(MAKE) sync-crds

## Copy generated CRDs into Helm-managed templates.
sync-crds:
	mkdir -p $(CRD_CHART_DIR)
	cp $(addprefix $(CRD_SOURCE_DIR)/,$(CRD_FILES)) $(CRD_CHART_DIR)/

## Verify that the chart packages the generated CRDs without drift.
check-crds:
	@set -e; for file in $(CRD_FILES); do \
		diff -u $(CRD_SOURCE_DIR)/$$file $(CRD_CHART_DIR)/$$file; \
	done

check-controller-gen:
	@test "$$($(CONTROLLER_GEN) --version)" = "Version: $(CONTROLLER_GEN_VERSION)" || \
		{ echo "Install controller-gen $(CONTROLLER_GEN_VERSION) or set CONTROLLER_GEN to its path" >&2; exit 1; }

verify-generated: generate manifests
	git diff --exit-code -- api/v1alpha1/zz_generated.deepcopy.go $(CRD_SOURCE_DIR) $(CRD_CHART_DIR)

## Uses only a local API server and etcd; never connects to an existing cluster.
test-envtest:
	@test -n "$(KUBEBUILDER_ASSETS)" && test -x "$(KUBEBUILDER_ASSETS)/kube-apiserver" && test -x "$(KUBEBUILDER_ASSETS)/etcd" && test -x "$(KUBEBUILDER_ASSETS)/kubectl" || { echo "Set KUBEBUILDER_ASSETS to preinstalled envtest binaries, including kubectl"; exit 1; }
ifeq ($(GO_TEST_SHARD_INDEX)/$(GO_TEST_SHARD_COUNT),1/1)
	USE_EXISTING_CLUSTER=false go test -tags=envtest $(GO_ENVTEST_PACKAGES) -run='$(GO_ENVTEST_SPLIT)' -count=1 -v -timeout=$(GO_ENVTEST_TIMEOUT)
	USE_EXISTING_CLUSTER=false go test -tags=envtest $(GO_ENVTEST_PACKAGES) -skip='$(GO_ENVTEST_SPLIT)' -count=1 -v -timeout=$(GO_ENVTEST_TIMEOUT)
else
	USE_EXISTING_CLUSTER=false $(GO_TEST_RUNNER) -tags=envtest $(GO_ENVTEST_PACKAGES) -count=1 -v -timeout=$(GO_ENVTEST_TIMEOUT)
endif

## Local Sandbox bootstrap and Gateway rejection; not runtime qualification.
test-sandbox-integration:
	@test -n "$(KUBEBUILDER_ASSETS)" && test -x "$(KUBEBUILDER_ASSETS)/kube-apiserver" && test -x "$(KUBEBUILDER_ASSETS)/etcd" || { echo "Set KUBEBUILDER_ASSETS to preinstalled envtest binaries"; exit 1; }
	USE_EXISTING_CLUSTER=false go test -tags=envtest ./test/integration -run '^(TestSandboxBootstrap|TestGatewayOnlyRejectsUnsupportedHeldProfiles)' -count=1 -v

## Durable lifecycle state against isolated API servers; no live runtime providers.
test-sandbox-lifecycle:
	@test -n "$(KUBEBUILDER_ASSETS)" && test -x "$(KUBEBUILDER_ASSETS)/kube-apiserver" && test -x "$(KUBEBUILDER_ASSETS)/etcd" && test -x "$(KUBEBUILDER_ASSETS)/kubectl" || { echo "Set KUBEBUILDER_ASSETS to preinstalled envtest binaries, including kubectl"; exit 1; }
	USE_EXISTING_CLUSTER=false go test -tags=envtest ./internal/sandboxwriter ./internal/sandboxcoordination ./cmd/sandbox-config-change -count=1 -v

## Clean build artifacts
clean:
	rm -rf bin/
