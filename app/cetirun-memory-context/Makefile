# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

VERSION ?= dev
GIT_SHA ?= $(shell git rev-parse HEAD)

.PHONY: test lint build native-test
test:
	GOPROXY=off GOTOOLCHAIN=local go test ./... -count=1
	node --test tests/native.test.mjs
native-test:
	node --test tests/native.test.mjs
lint:
	GOPROXY=off GOTOOLCHAIN=local go vet ./...
build:
	CGO_ENABLED=0 GOPROXY=off GOTOOLCHAIN=local go build -trimpath -ldflags "-X main.version=$(VERSION) -X main.commit=$(GIT_SHA)" -o bin/cetirun-memory ./cmd/cetirun-memory
