# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

ARG BASE_IMAGE=mcr.microsoft.com/aks/ai-runtime/cetirun/cetirun-base:0.1.1
FROM ${BASE_IMAGE} AS runtime-source
USER root
WORKDIR /opt/cetirun-pi
COPY --chmod=0644 *.mjs ./
RUN chmod 0755 /opt/cetirun-pi pi-tui.mjs
USER 1024:1024
RUN test "$(id -u):$(id -g)" = "1024:1024" \
    && for source in *.mjs; do node --check "$source" || exit 1; done \
    && node server.mjs --help \
    && ./pi-tui.mjs --help

FROM ${BASE_IMAGE}
ARG VERSION=0.1.1
ARG GIT_SHA=unknown

USER root
WORKDIR /opt/cetirun-pi

# Older published bases need the fixed npm dependency; npm 12 already includes it.
RUN inherited_undici="$(node -p 'require("/usr/local/lib/node_modules/npm/node_modules/undici/package.json").version')" && \
    case "${inherited_undici}" in \
      6.26.0|6.27.0|6.28.0) ;; \
      *) echo "Review the inherited npm undici version before replacing it" >&2; exit 1 ;; \
    esac && \
    if [ "${inherited_undici}" = "6.26.0" ]; then \
      npm_undici_stage="$(mktemp -d)" && \
      curl --fail --location --retry 3 --max-time 120 \
        https://registry.npmjs.org/undici/-/undici-6.27.0.tgz \
        --output "${npm_undici_stage}/undici.tgz" && \
      echo "cb4ecdf54572260fbbbd0fb03929ff04ca1be8b3d3bb41e60e8b4a72fc2b1036  ${npm_undici_stage}/undici.tgz" | sha256sum --check && \
      tar -xzf "${npm_undici_stage}/undici.tgz" -C "${npm_undici_stage}" && \
      rm -r /usr/local/lib/node_modules/npm/node_modules/undici && \
      mv "${npm_undici_stage}/package" /usr/local/lib/node_modules/npm/node_modules/undici && \
      rm -r "${npm_undici_stage}" && \
      test "$(node -p 'require("/usr/local/lib/node_modules/npm/node_modules/undici/package.json").version')" = "6.27.0"; \
    fi && \
    npm --version

COPY --chmod=0644 package.json package-lock.json ./
RUN npm ci --omit=dev --ignore-scripts --no-audit --no-fund \
    && node --input-type=module -e 'await import("@earendil-works/pi-coding-agent")'

# Pi's find tool needs fd; install it at build time so runtime stays offline.
ARG TARGETARCH
ARG FD_VERSION=10.5.0
RUN case "${TARGETARCH}" in \
      amd64) fd_target=x86_64-unknown-linux-musl; fd_sha=761c72dc8e120d85b22292063be8a796e2eeb20eb3e4f38b8fa2343ccf3514a7 ;; \
      arm64) fd_target=aarch64-unknown-linux-musl; fd_sha=d76c4317f7d5dba69f8a2a15856c90c777e7f0dd4e85f0de8c76de6992c374d4 ;; \
      *) echo "Unsupported Pi image architecture" >&2; exit 1 ;; \
    esac && \
    fd_dir="$(mktemp -d)" && \
    curl --fail --location --retry 3 --max-time 120 \
      "https://github.com/sharkdp/fd/releases/download/v${FD_VERSION}/fd-v${FD_VERSION}-${fd_target}.tar.gz" \
      --output "${fd_dir}/fd.tar.gz" && \
    echo "${fd_sha}  ${fd_dir}/fd.tar.gz" | sha256sum --check && \
    tar -xzf "${fd_dir}/fd.tar.gz" -C "${fd_dir}" --strip-components=1 && \
    install -m 755 "${fd_dir}/fd" /usr/local/bin/fd && \
    install -D -m 644 "${fd_dir}/LICENSE-MIT" /licenses/fd/LICENSE-MIT && \
    install -D -m 644 "${fd_dir}/LICENSE-APACHE" /licenses/fd/LICENSE-APACHE && \
    rm -r "${fd_dir}" && fd --version

COPY --from=runtime-source /opt/cetirun-pi/ ./
RUN for tool in sh cp mkdir mv rm chmod mktemp find cmp fd rg; do \
      command -v "$tool" >/dev/null || exit 1; \
    done \
    && ln -s /opt/cetirun-pi/pi-tui.mjs /usr/local/bin/pi-tui

LABEL org.opencontainers.image.source="https://github.com/Azure/CetiRun" \
      org.opencontainers.image.title="cetirun-pi-agent" \
      org.opencontainers.image.description="Pi Custom Agent with platform AI Gateway and native login modes" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${GIT_SHA}" \
      org.opencontainers.image.licenses="MIT"
ENV HOME=/home/vibe PI_TELEMETRY=0 PI_OFFLINE=1
USER 1024:1024
RUN test "$(id -u):$(id -g)" = "1024:1024" \
    && node server.mjs --help \
    && pi-tui --help \
    && node --input-type=module -e 'await import("./pi-runtime.mjs")'
WORKDIR /home/vibe
EXPOSE 8080
ENTRYPOINT ["node", "/opt/cetirun-pi/server.mjs"]
