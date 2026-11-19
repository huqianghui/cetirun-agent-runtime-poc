# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# CetiRun AI Gateway image: open-source LiteLLM, installed as pinned packages on
# the Azure Linux Python base from MCR.
#
# requirements.txt pins the upstream LiteLLM lockfile for the Gateway's runtime
# extras, with the exclusions documented at its top. The
# prisma layout below reproduces the upstream image's: the prisma CLI and
# engines live under /opt/prisma and the generated client records those paths,
# so migrations need neither npm nor network access at runtime.
#
# One image, two roles: the shared Gateway, with PostgreSQL, and the per-agent
# Copilot proxy, with no database. Starting never touches a database; migrating
# is the separate `migrate` command (entrypoint.py).

ARG PYTHON_IMAGE=mcr.microsoft.com/azurelinux/base/python:3.12@sha256:6a4fcacee291dffbb7a95881a8549cda0ff0999628975eebeec5b7ce6acb10fa

FROM ${PYTHON_IMAGE} AS builder
# Package sources. Networks that block the public registries point these at an
# enterprise feed, as the other CetiRun images allow.
ARG PYPI_INDEX_URL=https://pypi.org/simple/
ARG NPM_REGISTRY=https://registry.npmjs.org/

# npm installs the prisma CLI during `prisma generate`. Without it on the path,
# prisma falls back to downloading its own Node, which needs libatomic, absent
# from Azure Linux.
RUN tdnf install -y ca-certificates nodejs npm && tdnf clean all

ENV PATH=/app/.venv/bin:${PATH} \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PIP_NO_CACHE_DIR=1

COPY requirements.txt /tmp/requirements.txt
# --no-deps: the list is the complete set, so nothing outside it, including
# litellm-enterprise, is pulled in. Wheels only, so nothing is compiled here.
#
# pip check must pass: a requirement the set does not meet fails the build.
RUN python3 -m venv /app/.venv && \
    pip install --index-url "${PYPI_INDEX_URL}" --no-deps --only-binary=:all: -r /tmp/requirements.txt && \
    pip check

RUN schema="$(python -c 'import litellm_proxy_extras, os; print(os.path.join(os.path.dirname(litellm_proxy_extras.__file__), "schema.prisma"))')" && \
    HOME=/opt/prisma XDG_CACHE_HOME=/opt/prisma/.cache PRISMA_BINARY_CACHE_DIR=/opt/prisma/binaries \
    npm_config_cache=/tmp/npm-cache npm_config_registry="${NPM_REGISTRY}" CHECKPOINT_DISABLE=1 \
    prisma generate --schema="${schema}" && \
    rm -rf /tmp/npm-cache

FROM ${PYTHON_IMAGE}
ARG VERSION=dev
ARG GIT_SHA=unknown

LABEL org.opencontainers.image.source="https://github.com/Azure/CetiRun"
LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.revision="${GIT_SHA}"
LABEL org.opencontainers.image.licenses="MIT"

# nodejs runs the prisma CLI for migrations; tzdata and libsndfile match the
# upstream runtime.
RUN tdnf install -y ca-certificates nodejs tzdata libsndfile && tdnf clean all

ENV PATH=/app/.venv/bin:${PATH} \
    PRISMA_BINARY_CACHE_DIR=/opt/prisma/binaries \
    PRISMA_CLI_PATH=/opt/prisma/binaries/node_modules/.bin/prisma \
    PRISMA_CLI_QUERY_ENGINE_TYPE=binary \
    PRISMA_OFFLINE_MODE=true \
    CHECKPOINT_DISABLE=1 \
    PYTHONUNBUFFERED=1 \
    PYTHONPATH=/opt/cetirun-ai-gateway/python \
    HOME=/home/nonroot

COPY --from=builder /app/.venv /app/.venv
COPY --from=builder /opt/prisma /opt/prisma
COPY entrypoint.py /usr/local/bin/cetirun-ai-gateway
# CetiRun's additions to the proxy: the guard callback the entrypoint always
# registers, and the per-request log callback it registers when the chart turns
# the request log on.
COPY cetirun_ai_gateway /opt/cetirun-ai-gateway/python/cetirun_ai_gateway

# Runs as the base image's nonroot user (65532). The checks fail the build
# rather than ship an image whose migrations would reach for the network or a
# missing CLI, or that carries the enterprise package. /app/schema.prisma is
# where the proxy's startup schema-drift check looks, as in the upstream image;
# migrations use the copy with migrations beside it (entrypoint.py). The last
# check loads both callbacks against the LiteLLM installed here, and the
# LiteLLM entry points the entrypoint uses to register them, so a LiteLLM
# upgrade that moves what they build on fails the build, not the proxy's start.
# It also checks that the guard replaced each LiteLLM function it replaces.
RUN chmod 0755 /usr/local/bin/cetirun-ai-gateway && \
    test "$(id -un 65532)" = nonroot && \
    cp "$(python -c 'import litellm_proxy_extras, os; print(os.path.dirname(litellm_proxy_extras.__file__))')/schema.prisma" /app/schema.prisma && \
    find /app/.venv -type d -path "*/tornado/test" -prune -exec rm -rf {} + && \
    chmod -R a+rX /opt/prisma /app /opt/cetirun-ai-gateway && \
    test -x /opt/prisma/binaries/node_modules/.bin/prisma && \
    test -f /opt/prisma/binaries/node_modules/prisma/build/index.js && \
    python -c "from prisma.client import BINARY_PATHS; paths = list(BINARY_PATHS.query_engine.values()); assert paths and all(p.startswith('/opt/prisma/') for p in paths), paths" && \
    python -c "import importlib.util, sys; sys.exit(importlib.util.find_spec('litellm_enterprise') is not None)" && \
    LITELLM_LOCAL_MODEL_COST_MAP=True python -c "import litellm; from litellm.integrations.custom_logger import CustomLogger; from cetirun_ai_gateway.request_log import handler; from cetirun_ai_gateway.guard import handler as guard; assert isinstance(handler, CustomLogger) and isinstance(guard, CustomLogger); assert callable(litellm.logging_callback_manager.add_litellm_callback); from litellm.proxy.proxy_cli import run_server; assert callable(run_server.main); import cetirun_ai_gateway.guard as module; assert all(module.PATCHED.values()), module.PATCHED; assert litellm.expose_router_debug_in_errors is False"

USER 65532:65532
WORKDIR /app
EXPOSE 4000/tcp

ENTRYPOINT ["/usr/local/bin/cetirun-ai-gateway"]
CMD ["--port", "4000"]
