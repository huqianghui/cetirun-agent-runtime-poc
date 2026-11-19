# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

FROM mcr.microsoft.com/oss/go/microsoft/golang:1.27.3-1@sha256:25d710943a357c3b8669289f4f8af073f9c6c2a44a5b31341cf8997ea592dcba AS builder
ARG VERSION=dev
WORKDIR /src
COPY app/internal/ ./app/internal/
COPY app/cetirun-connections-mcp/ ./app/cetirun-connections-mcp/
WORKDIR /src/app/cetirun-connections-mcp
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-X github.com/Azure/CetiRun/app/cetirun-connections-mcp/internal/mcp.Version=${VERSION}" \
    -o /cetirun-connections-mcp .

FROM mcr.microsoft.com/azurelinux/distroless/base:3.0@sha256:370fb2c55e7f0474fed019623cce6a8db65a4d70dedfca9219fc69d6c7627f89
COPY --from=builder /cetirun-connections-mcp /cetirun-connections-mcp
USER 65532:65532
EXPOSE 8443
ENTRYPOINT ["/cetirun-connections-mcp"]
