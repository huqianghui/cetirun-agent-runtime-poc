# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

ARG BASE_IMAGE=mcr.microsoft.com/aks/ai-runtime/cetirun/cetirun-base@sha256:a82d351c9e22f8bba76a0c0a4a30aa703afb3d29eba625a31791e5f7d0da7a07
FROM scratch AS runtime-artifacts

FROM ${BASE_IMAGE} AS runtime-install
ARG CODING_RUNTIME
ARG TARGETARCH
USER root
WORKDIR /opt/cetirun-coding
COPY --chmod=0644 artifacts.json install_runtime.py ./
RUN --mount=type=bind,from=runtime-artifacts,target=/runtime-artifacts \
    test "$TARGETARCH" = amd64 \
    && python3 install_runtime.py "$CODING_RUNTIME" /out /runtime-artifacts \
    && /out/"$CODING_RUNTIME" --version

FROM ${BASE_IMAGE}
ARG CODING_RUNTIME
USER root
WORKDIR /opt/cetirun-coding
COPY --from=runtime-install /out/ /usr/local/bin/
COPY --chmod=0644 native.py server.py index.html ./
COPY profiles/ ./profiles/
RUN chmod -R a+rX /opt/cetirun-coding \
    && printf '#!/bin/sh\nexec python3 /opt/cetirun-coding/server.py tui "$@"\n' > /usr/local/bin/coding-tui \
    && chmod 0755 /usr/local/bin/coding-tui \
    && test "$(id -u vibe):$(id -g vibe)" = "1024:1024" \
    && for tool in sh cp mkdir mv rm chmod mktemp find cmp git python3; do command -v "$tool" || exit 1; done
ENV CODING_RUNTIME=${CODING_RUNTIME} HOME=/home/vibe PYTHONDONTWRITEBYTECODE=1
USER 1024:1024
RUN python3 /opt/cetirun-coding/server.py --help
WORKDIR /home/vibe
EXPOSE 8080
ENTRYPOINT ["python3", "/opt/cetirun-coding/server.py"]
