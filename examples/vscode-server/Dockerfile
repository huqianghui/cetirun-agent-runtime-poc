# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

ARG BASE_IMAGE=mcr.microsoft.com/aks/ai-runtime/cetirun/cetirun-base:0.1.1
FROM ${BASE_IMAGE} AS runtime-source
USER root
WORKDIR /opt/cetirun-vscode
COPY --chmod=0644 start.mjs proxy.mjs browser-storage.mjs ./
COPY gateway-extension ./gateway-extension
RUN chmod 0755 /opt/cetirun-vscode \
    && chmod -R a+rX,u+w,go-w gateway-extension
USER 1024:1024
RUN test "$(id -u):$(id -g)" = "1024:1024" \
    && for source in *.mjs; do node --check "$source" || exit 1; done \
    && node -e 'const fs = require("node:fs"); \
      for (const entry of fs.readdirSync("gateway-extension", {recursive: true, withFileTypes: true})) { \
        if (entry.isFile()) fs.readFileSync(`${entry.parentPath}/${entry.name}`); \
      }' \
    && node start.mjs --help

FROM ${BASE_IMAGE}
ARG TARGETARCH
ARG ACCEPT_VSCODE_SERVER_LICENSE_TERMS=false

USER root
WORKDIR /opt/cetirun-vscode
COPY install.py vscode-lock.json ./
RUN test "$ACCEPT_VSCODE_SERVER_LICENSE_TERMS" = true \
    && python3 install.py --arch "$TARGETARCH" --output /opt/vscode \
    && for tool in sh cp mkdir mv rm chmod mktemp find cmp git node; do command -v "$tool" >/dev/null || exit 1; done
COPY --from=runtime-source /opt/cetirun-vscode/ ./
RUN cp -a gateway-extension /opt/vscode/server/extensions/cetirun-gateway-config
LABEL org.opencontainers.image.licenses="MIT AND LicenseRef-Microsoft-VS-Code-Server" \
      org.opencontainers.image.description="Official VS Code Server with GitHub Copilot for an individual CetiRun development workspace"
ENV HOME=/home/vibe VSCODE_SERVER_LICENSE_ACCEPTED=true
USER 1024:1024
RUN test "$(id -u):$(id -g)" = "1024:1024" \
    && node start.mjs --help \
    && node -e 'const fs = require("node:fs"); \
      for (const entry of fs.readdirSync("/opt/vscode/server/extensions/cetirun-gateway-config", {recursive: true, withFileTypes: true})) { \
        if (entry.isFile()) fs.readFileSync(`${entry.parentPath}/${entry.name}`); \
      }'
WORKDIR /home/vibe
EXPOSE 8080
ENTRYPOINT ["node", "/opt/cetirun-vscode/start.mjs"]
