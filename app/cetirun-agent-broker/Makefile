# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

.PHONY: build test vet

build:
	go build ./...

test:
	go test ./... -count=1

vet:
	go vet ./...
