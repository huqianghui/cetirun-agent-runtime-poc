# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

ARG BASE_IMAGE
FROM ${BASE_IMAGE}
USER root
WORKDIR /opt/cetirun-coding-example
COPY --chmod=0644 runtime.py ./
COPY workspace/ ./workspace/
RUN chmod -R a+rX /opt/cetirun-coding-example \
    && command -v python3 \
    && for tool in sh cp mkdir mv rm chmod mktemp find cmp; do command -v "$tool" || exit 1; done \
    && test "$(id -u vibe):$(id -g vibe)" = "1024:1024"
ENV HOME=/home/vibe PYTHONDONTWRITEBYTECODE=1
USER 1024:1024
RUN python3 /opt/cetirun-coding-example/runtime.py --help
WORKDIR /home/vibe
EXPOSE 8080
CMD ["--host", "0.0.0.0"]
ENTRYPOINT ["python3", "/opt/cetirun-coding-example/runtime.py", "serve"]
