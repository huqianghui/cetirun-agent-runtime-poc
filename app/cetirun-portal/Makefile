# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

REGISTRY ?= $(shell ../../scripts/cetirun-release-metadata.sh registry)
IMAGE_REPO ?= $(REGISTRY)/cetirun-portal
IMAGE_TAG  ?= $(VERSION)
NPM_REGISTRY ?= https://registry.npmjs.org/
VERSION ?= $(shell ../../scripts/cetirun-release-metadata.sh version)
GIT_SHA ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
LDFLAGS = -X github.com/Azure/CetiRun/app/cetirun-portal/internal/buildinfo.Version=$(VERSION) \
	-X github.com/Azure/CetiRun/app/cetirun-portal/internal/buildinfo.Commit=$(GIT_SHA)

##@ Development

.PHONY: frontend-install
frontend-install: ## Install frontend dependencies
	cd web && NPM_CONFIG_REGISTRY="$(NPM_REGISTRY)" NPM_CONFIG_REPLACE_REGISTRY_HOST=npmjs npm ci

.PHONY: frontend-dev
frontend-dev: ## Start frontend dev server with hot reload
	cd web && npm run dev

.PHONY: frontend-build
frontend-build: ## Build frontend for production
	cd web && npm run build

.PHONY: backend-run
backend-run: ## Run backend locally (requires kubeconfig and env vars)
	go run .

.PHONY: run
run: frontend-build backend-run ## Build frontend and run backend

##@ Build

.PHONY: build
build: ## Build Go binary (assumes web/dist already exists)
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/portal .

.PHONY: test
test: ## Run Go tests
	go test ./... -v -count=1

.PHONY: lint
lint: ## Run linters
	golangci-lint run ./...
	cd web && npm run lint

.PHONY: docker-build
docker-build: ## Build Docker image (run from repo root)
	docker build --build-arg NPM_REGISTRY=$(NPM_REGISTRY) \
		--build-arg VERSION=$(VERSION) --build-arg GIT_SHA=$(GIT_SHA) \
		-t $(IMAGE_REPO):$(IMAGE_TAG) -f Dockerfile ../..

.PHONY: docker-push
docker-push: ## Push Docker image
	../../scripts/cetirun-release-metadata.sh check-push-registry "$(IMAGE_REPO)"
	docker push $(IMAGE_REPO):$(IMAGE_TAG)

##@ Deploy

.PHONY: helm-install
helm-install: ## Install Helm chart
	helm upgrade --install cetirun-portal ./chart \
		--namespace cetirun-portal \
		--create-namespace \
		--set image.tag=$(IMAGE_TAG)

.PHONY: helm-uninstall
helm-uninstall: ## Uninstall Helm chart
	helm uninstall cetirun-portal --namespace cetirun-portal
