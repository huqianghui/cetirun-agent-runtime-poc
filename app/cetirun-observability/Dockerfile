# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# The ADX-Mon collector and ingestor (https://github.com/Azure/adx-mon, MIT
# License), built from pinned upstream source with its vendored modules. The
# cetirun-observability chart runs /collector as a DaemonSet and /ingestor as
# a StatefulSet. The collector is built without cgo: it reads container log
# files, not the systemd journal.

ARG VERSION=dev
ARG GIT_SHA=unknown

FROM --platform=$BUILDPLATFORM mcr.microsoft.com/oss/go/microsoft/golang:1.27.3-1@sha256:25d710943a357c3b8669289f4f8af073f9c6c2a44a5b31341cf8997ea592dcba AS builder
ARG TARGETOS
ARG TARGETARCH
ARG ADX_MON_GIT_SHA=27f7654b4980c2a485f773a75bd70e9b10dd9ae7
ARG ADX_MON_SRC_SHA256=289f14f24867d925f8673a01b41e7632a393042ecf6ce70c98e4a127743cf0a2
ENV GOTOOLCHAIN=local GOFLAGS=-mod=vendor CGO_ENABLED=0
RUN curl -fsSL --retry 5 --retry-delay 5 --retry-all-errors \
      "https://codeload.github.com/Azure/adx-mon/tar.gz/${ADX_MON_GIT_SHA}" -o /tmp/adx-mon.tar.gz && \
    echo "${ADX_MON_SRC_SHA256}  /tmp/adx-mon.tar.gz" | sha256sum --check && \
    mkdir /src && tar xzf /tmp/adx-mon.tar.gz -C /src --strip-components=1 && rm /tmp/adx-mon.tar.gz
WORKDIR /src
RUN for binary in collector ingestor; do \
      GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -trimpath \
        -ldflags "-X github.com/Azure/adx-mon/pkg/version.Version=${ADX_MON_GIT_SHA} -X github.com/Azure/adx-mon/pkg/version.GitCommit=${ADX_MON_GIT_SHA}" \
        -o "/out/${binary}" "./cmd/${binary}" || exit 1; \
    done

FROM mcr.microsoft.com/azurelinux/distroless/base:3.0@sha256:370fb2c55e7f0474fed019623cce6a8db65a4d70dedfca9219fc69d6c7627f89
ARG VERSION
ARG GIT_SHA
ARG ADX_MON_GIT_SHA=27f7654b4980c2a485f773a75bd70e9b10dd9ae7

LABEL org.opencontainers.image.source="https://github.com/Azure/CetiRun"
LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.revision="${GIT_SHA}"
LABEL org.opencontainers.image.licenses="MIT"
LABEL org.opencontainers.image.description="ADX-Mon collector and ingestor, upstream commit ${ADX_MON_GIT_SHA}"

COPY --from=builder /out/collector /out/ingestor /
COPY --from=builder /src/LICENSE /licenses/adx-mon.LICENSE

# The chart runs the collector as root, to read the node's container log
# files; the ingestor keeps this user.
USER 65532:65532
ENTRYPOINT ["/ingestor"]
