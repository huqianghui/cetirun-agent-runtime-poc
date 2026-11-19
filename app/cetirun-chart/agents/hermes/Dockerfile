# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

ARG BASE_IMAGE=mcr.microsoft.com/aks/ai-runtime/cetirun/cetirun-base:latest
ARG HERMES_VERSION=0.22.7
ARG HERMES_RELEASE=0.22.7
ARG HERMES_GIT_SHA=b200f5bc60e31588ba2cb60e288029c4a54ba5fa
ARG HERMES_SOURCE_SHA256=1f7d43fa1a515283929a739c42e10bf8d48910e523eccec74b2a2bece858d2b5
ARG HERMES_CPYTHON_SHA256=e02d9ac21cebda6e46435369744a92c65cc7cd8669697694122cf97ffacd1e53
ARG HERMES_SQLITE_VERSION=3530400
ARG HERMES_SQLITE_SHA256=0e9483900e92cd5de8fd48d16bf9200145a61f7fd5be542a5ac81d8a9516eb9c

FROM ${BASE_IMAGE} AS hermes-source
ARG HERMES_GIT_SHA
ARG HERMES_SOURCE_SHA256

USER vibe
WORKDIR /usr/local/lib/hermes-agent
RUN curl -fsSL --retry 3 --retry-all-errors --retry-delay 15 --retry-max-time 1800 \
      --connect-timeout 15 --max-time 1800 \
      "https://codeload.github.com/NousResearch/hermes-agent/tar.gz/${HERMES_GIT_SHA}" \
      -o /tmp/hermes-agent.tar.gz && \
    echo "${HERMES_SOURCE_SHA256}  /tmp/hermes-agent.tar.gz" | sha256sum --check && \
    tar xzf /tmp/hermes-agent.tar.gz --strip-components=1 -C /usr/local/lib/hermes-agent && \
    rm /tmp/hermes-agent.tar.gz

FROM ${BASE_IMAGE} AS hermes-sqlite-build
ARG HERMES_SQLITE_VERSION
ARG HERMES_SQLITE_SHA256

USER root
RUN tdnf install -y binutils gcc glibc-devel kernel-headers make && \
    tdnf clean all
RUN (curl -fsSL --retry 1 --retry-all-errors --connect-timeout 15 --max-time 60 \
      -o /tmp/sqlite.tar.gz \
      "https://sqlite.org/2026/sqlite-autoconf-${HERMES_SQLITE_VERSION}.tar.gz" || \
     curl -fsSL --retry 3 --retry-all-errors --connect-timeout 15 --max-time 120 \
      -o /tmp/sqlite.tar.gz \
      "https://sources.buildroot.net/sqlite/sqlite-autoconf-${HERMES_SQLITE_VERSION}.tar.gz") && \
    echo "${HERMES_SQLITE_SHA256}  /tmp/sqlite.tar.gz" | sha256sum --check && \
    tar xzf /tmp/sqlite.tar.gz -C /tmp && \
    cd "/tmp/sqlite-autoconf-${HERMES_SQLITE_VERSION}" && \
    CFLAGS="-O2 \
      -DSQLITE_ENABLE_FTS3 \
      -DSQLITE_ENABLE_FTS3_PARENTHESIS \
      -DSQLITE_ENABLE_FTS4 \
      -DSQLITE_ENABLE_FTS5 \
      -DSQLITE_ENABLE_RTREE \
      -DSQLITE_ENABLE_GEOPOLY \
      -DSQLITE_ENABLE_COLUMN_METADATA \
      -DSQLITE_ENABLE_UNLOCK_NOTIFY \
      -DSQLITE_ENABLE_DBSTAT_VTAB \
      -DSQLITE_ENABLE_DBPAGE_VTAB \
      -DSQLITE_ENABLE_MATH_FUNCTIONS \
      -DSQLITE_ENABLE_PREUPDATE_HOOK \
      -DSQLITE_ENABLE_SESSION \
      -DSQLITE_SECURE_DELETE \
      -DSQLITE_THREADSAFE=1 \
      -DSQLITE_MAX_VARIABLE_NUMBER=250000" \
      ./configure --prefix=/opt/sqlite-fixed --disable-static && \
    make -j"$(nproc)" && \
    make install

FROM ${BASE_IMAGE} AS hermes-toolchain
ARG HERMES_VERSION
ARG HERMES_GIT_SHA
ARG NPM_REGISTRY=https://registry.npmjs.org/
ARG PYPI_INDEX_URL=https://pypi.org/simple/

USER root
WORKDIR /usr/local/lib/hermes-agent
COPY --from=hermes-source /usr/local/lib/hermes-agent/ /usr/local/lib/hermes-agent/
COPY agents/hermes/pm/build.py /usr/local/lib/hermes-agent/cetirun_pm/build.py
ENV HERMES_HOME=/tmp/cetirun-hermes-build-home
ENV HERMES_INSTALL_DIR=/usr/local/lib/hermes-agent
ENV HERMES_RUNTIME_DIR=/usr/local/lib/hermes-agent/tools
ENV npm_config_registry=${NPM_REGISTRY}
ENV UV_DEFAULT_INDEX=${PYPI_INDEX_URL}
RUN python3 cetirun_pm/build.py --source . --phase tools \
      --version "${HERMES_VERSION}" --commit "${HERMES_GIT_SHA}"
ENV PATH=/usr/local/lib/hermes-agent/bin:$PATH
ENV HERMES_PYTHON=/usr/local/lib/hermes-agent/bin/python3

FROM hermes-toolchain AS hermes-ui-build
ENV npm_config_install_links=false
RUN node apps/desktop/scripts/build-webapp.mjs --deps-only && \
    node scripts/build/node-deps.mjs --source . --workspace ui-tui && \
    node scripts/build/tui.mjs --source . --out /tmp/hermes-tui-product
COPY agents/hermes/compat/apply_webapp_storage.py agents/hermes/compat/webapp_storage.ts /tmp/cetirun-webapp-storage/
RUN python3 /tmp/cetirun-webapp-storage/apply_webapp_storage.py --root /usr/local/lib/hermes-agent && \
    node apps/desktop/scripts/build-webapp.mjs --no-install && \
    rm -rf /tmp/cetirun-webapp-storage

FROM hermes-toolchain AS hermes-build
ARG HERMES_CPYTHON_SHA256
ENV CC=gcc CXX=g++
COPY agents/hermes/pm/build_sqlite.py /usr/local/lib/hermes-agent/cetirun_pm/build_sqlite.py
RUN tdnf install -y binutils gcc gcc-c++ make glibc-devel kernel-headers && tdnf clean all
COPY --from=hermes-sqlite-build /opt/sqlite-fixed/ /opt/sqlite-fixed/
RUN hermes_python_version="$(python3 -c 'import sys; print(".".join(map(str, sys.version_info[:3])))')" && \
    curl -fsSL --retry 3 --connect-timeout 15 --max-time 180 \
      "https://www.python.org/ftp/python/${hermes_python_version}/Python-${hermes_python_version}.tar.xz" \
      -o /tmp/cpython.tar.xz && \
    echo "${HERMES_CPYTHON_SHA256}  /tmp/cpython.tar.xz" | sha256sum --check && \
    tar xf /tmp/cpython.tar.xz -C /tmp && \
    python3 cetirun_pm/build_sqlite.py --python-source "/tmp/Python-${hermes_python_version}" \
      --sqlite /opt/sqlite-fixed --out /usr/local/lib/hermes-agent/sqlite && \
    rm -rf /tmp/cpython.tar.xz "/tmp/Python-${hermes_python_version}"
RUN python3 cetirun_pm/build.py --source . --phase dependencies
COPY --from=hermes-ui-build /usr/local/lib/hermes-agent/apps/desktop/dist-webapp/ /usr/local/lib/hermes-agent/apps/desktop/dist-webapp/
COPY --from=hermes-ui-build /usr/local/lib/hermes-agent/apps/desktop/src/entry.ts /usr/local/lib/hermes-agent/apps/desktop/src/entry.ts
COPY --from=hermes-ui-build /usr/local/lib/hermes-agent/apps/desktop/src/lib/cetirun-webapp-storage.ts /usr/local/lib/hermes-agent/apps/desktop/src/lib/cetirun-webapp-storage.ts
COPY --from=hermes-ui-build /usr/local/lib/hermes-agent/hermes_cli/web_routers/webapp.py /usr/local/lib/hermes-agent/hermes_cli/web_routers/webapp.py
COPY --from=hermes-ui-build /tmp/hermes-tui-product/ /usr/local/lib/hermes-agent/tui-product/
COPY agents/hermes/pm/ /usr/local/lib/hermes-agent/cetirun_pm/
COPY agents/hermes/compat/workload_mcp.py /usr/local/lib/hermes-agent/cetirun_compat/workload_mcp.py
COPY agents/hermes/compat/apply_workload_mcp.py /tmp/apply-workload-mcp.py
COPY configs/apply-memory-skills.py /tmp/apply-memory-skills.py
RUN .venv/bin/python /tmp/apply-workload-mcp.py --root /usr/local/lib/hermes-agent && \
    .venv/bin/python /tmp/apply-memory-skills.py --runtime hermes --root /usr/local/lib/hermes-agent && \
    rm /tmp/apply-workload-mcp.py /tmp/apply-memory-skills.py && \
    .venv/bin/python cetirun_pm/apply_generation.py --root /usr/local/lib/hermes-agent && \
    .venv/bin/python cetirun_pm/build.py --source . --phase assemble

FROM ${BASE_IMAGE}
ARG VERSION=dev
ARG GIT_SHA=unknown
ARG HERMES_VERSION
ARG HERMES_GIT_SHA

USER root
COPY --from=hermes-build /usr/local/lib/hermes-agent/ /usr/local/lib/hermes-agent/
COPY --from=hermes-sqlite-build /opt/sqlite-fixed/lib/libsqlite3.so.3.53.4 /usr/local/lib/hermes-agent/sqlite/
COPY agents/hermes/plugins/cetirun-memory-context/ /usr/local/lib/hermes-agent/plugins/cetirun-memory-context/
COPY agents/hermes/pm/hermes-python agents/hermes/pm/hermes-prepare-dependencies /usr/local/bin/
ENV LD_LIBRARY_PATH=/usr/local/lib/hermes-agent/sqlite
RUN tdnf install -y tzdata && tdnf clean all && \
    ln -s libsqlite3.so.3.53.4 /usr/local/lib/hermes-agent/sqlite/libsqlite3.so.0 && \
    ln -s libsqlite3.so.3.53.4 /usr/local/lib/hermes-agent/sqlite/libsqlite3.so && \
    ln -sf /usr/local/lib/hermes-agent/libexec/hermes /usr/local/bin/hermes && \
    chmod 755 /usr/local/bin/hermes-python /usr/local/bin/hermes-prepare-dependencies && \
    mkdir -p /etc/hermes && \
    cp /usr/local/lib/hermes-agent/cetirun-image.json /etc/hermes/image-provenance.json && \
    find /usr/local/lib/hermes-agent ! -type l \
      \( ! -perm -a+r -o -perm /022 -o \( -type d ! -perm -a+x \) \) \
      -exec chmod a+rX,go-w {} + && \
    chmod 0444 /etc/hermes/image-provenance.json

ENV HERMES_HOME=/home/vibe/.hermes
ENV HERMES_INSTALL_DIR=/usr/local/lib/hermes-agent
ENV HERMES_RUNTIME_DIR=/usr/local/lib/hermes-agent/tools
ENV HERMES_PYTHON=/usr/local/bin/hermes-python
ENV HERMES_WEBAPP_DIST=/usr/local/lib/hermes-agent/apps/desktop/dist-webapp
ENV HERMES_TUI_DIR=/usr/local/lib/hermes-agent/tui-product
ENV HERMES_DISABLE_LAZY_INSTALLS=1
ENV PYTHONDONTWRITEBYTECODE=1
ENV PATH=/usr/local/lib/hermes-agent/libexec:/usr/local/lib/hermes-agent/.venv/bin:$PATH

USER vibe
WORKDIR /home/vibe
RUN /usr/local/bin/hermes-python /usr/local/lib/hermes-agent/cetirun_pm/verify.py

LABEL org.opencontainers.image.source="https://github.com/Azure/CetiRun"
LABEL org.opencontainers.image.description="CetiRun Hermes gateway and Webapp image"
LABEL org.opencontainers.image.version="${VERSION}"
LABEL org.opencontainers.image.revision="${GIT_SHA}"
LABEL org.opencontainers.image.licenses="MIT"
LABEL io.cetirun.hermes.version="${HERMES_VERSION}"
LABEL io.cetirun.hermes.revision="${HERMES_GIT_SHA}"

COPY --chown=vibe:vibe configs/builtin-skill-sync.sh /usr/local/share/cetirun/builtin-skill-sync.sh
COPY --chown=vibe:vibe agents/hermes/bootstrap/hermes-entrypoint.sh /usr/local/bin/hermes-entrypoint.sh
COPY --chown=vibe:vibe skills/connected-services/ /home/vibe/.hermes/skills/connected-services/
RUN chmod 755 /usr/local/bin/hermes-entrypoint.sh \
    /usr/local/share/cetirun/builtin-skill-sync.sh

EXPOSE 8642 7681 9120

ENTRYPOINT ["/usr/local/bin/hermes-entrypoint.sh"]
