#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -e

if [ "${CETIRUN_INGRESS_MTLS:-}" = "true" ]; then
  export TTYD_BIND_ARGS="--interface 127.0.0.1"
else
  export TTYD_BIND_ARGS=""
fi

export HERMES_HOME="${HERMES_HOME:-/home/vibe/.hermes}"
export HERMES_PYTHON=/usr/local/bin/hermes-python
if [ "${CETIRUN_AI_GATEWAY:-}" != true ]; then
  echo "AI Gateway is required" >&2
  exit 1
fi
/usr/local/bin/hermes-prepare-dependencies
"$HERMES_PYTHON" /etc/cetirun/hermes-runtime.py check

if [ "${CETIRUN_CETIADMIN_ENABLED:-}" = "true" ]; then
  if [ ! -x "${HERMES_PYTHON:-}" ] || [ ! -f "${CETIRUN_CETIADMIN_CONFIG:-}" ]; then
    echo "ERROR: CetiAdmin requires native Hermes Python and its generated monitor config" >&2
    exit 1
  fi
  if ! "$HERMES_PYTHON" /opt/cetirun/cetiadmin/scripts/check_skill_sources.py >/dev/null 2>&1; then
    echo "ERROR: CetiAdmin skill source check failed; scheduler not started" >&2
    exit 1
  fi
  if ! cetiadmin_sync_result=$("$HERMES_PYTHON" /opt/cetirun/cetiadmin/scripts/cron_sync.py \
      --config "$CETIRUN_CETIADMIN_CONFIG" 2>/dev/null); then
    echo "ERROR: CetiAdmin cron synchronization failed; scheduler not started" >&2
    exit 1
  fi
  # The helper cannot export into this shell; the scheduler must inherit its validated timezone.
  if ! cetiadmin_timezone=$("$HERMES_PYTHON" -c '
import json
import sys
from zoneinfo import ZoneInfo

result = json.load(sys.stdin)
with open(sys.argv[1], encoding="utf-8") as source:
    expected = json.load(source)["monitor"]["timezone"]
if result.get("status") not in {"disabled", "paused", "created", "updated", "unchanged"}:
    sys.exit(1)
timezone = result["timezone"]
if timezone != expected or ZoneInfo(timezone).key != timezone:
    sys.exit(1)
print(timezone)
' "$CETIRUN_CETIADMIN_CONFIG" <<< "$cetiadmin_sync_result" 2>/dev/null); then
    echo "ERROR: CetiAdmin cron readback failed; scheduler not started" >&2
    exit 1
  fi
  export HERMES_TIMEZONE="$cetiadmin_timezone"
  export PATH="/opt/cetirun/cetiadmin/bin:$PATH"
  unset cetiadmin_sync_result cetiadmin_timezone
fi

cat > /tmp/supervisord.conf << EOF
[unix_http_server]
file=/tmp/supervisor.sock

[supervisorctl]
serverurl=unix:///tmp/supervisor.sock

[rpcinterface:supervisor]
supervisor.rpcinterface_factory=supervisor.rpcinterface:make_main_rpcinterface

[supervisord]
nodaemon=true
logfile=/dev/null
logfile_maxbytes=0
pidfile=/tmp/supervisord.pid

[program:hermes-gateway]
command=/bin/sh /etc/cetirun/hermes-gateway.sh
priority=40
autorestart=true
stopasgroup=true
killasgroup=true
environment=HERMES_HOME="%(ENV_HERMES_HOME)s"
stdout_logfile=/dev/stdout
stdout_logfile_maxbytes=0
stderr_logfile=/dev/stderr
stderr_logfile_maxbytes=0

EOF

if [ "${TTYD_ENABLED}" = "true" ]; then
  if ! echo "${TTYD_PORT}" | grep -qE '^[0-9]+$' || [ "${TTYD_PORT}" -lt 1024 ] || [ "${TTYD_PORT}" -gt 65535 ]; then
    echo "ERROR: TTYD_PORT must be a number between 1024 and 65535, got '${TTYD_PORT}'" >&2
    exit 1
  fi
  if [ ! -x "${TTYD_SHELL}" ]; then
    echo "ERROR: TTYD_SHELL must be an executable path, got '${TTYD_SHELL}'" >&2
    exit 1
  fi
  if ! echo "${TTYD_BASE_PATH}" | grep -qE '^/[a-z0-9/-]+$'; then
    echo "ERROR: TTYD_BASE_PATH must match ^/[a-z0-9/-]+$, got '${TTYD_BASE_PATH}'" >&2
    exit 1
  fi
  cat >> /tmp/supervisord.conf << 'TTYD_EOF'

[program:ttyd]
command=ttyd %(ENV_TTYD_BIND_ARGS)s --port %(ENV_TTYD_PORT)s --base-path %(ENV_TTYD_BASE_PATH)s --writable --check-origin %(ENV_TTYD_SHELL)s
priority=25
autorestart=true
startretries=5
stdout_logfile=/dev/stdout
stdout_logfile_maxbytes=0
stderr_logfile=/dev/stderr
stderr_logfile_maxbytes=0
TTYD_EOF
fi

if [ "$TAILSCALE_ENABLED" = "true" ]; then
  if [ -n "$TAILSCALE_HOSTNAME" ]; then
    POD_ORDINAL=${HOSTNAME##*-}
    if echo "$POD_ORDINAL" | grep -qE '^[0-9]+$'; then
      TS_HOST="${TAILSCALE_HOSTNAME}-${POD_ORDINAL}"
    else
      TS_HOST="${TAILSCALE_HOSTNAME}-${HOSTNAME}"
    fi
  else
    TS_HOST="$HOSTNAME"
  fi
  TS_HOST=$(printf '%s' "$TS_HOST" | tr -cd 'a-zA-Z0-9-' | tr '[:upper:]' '[:lower:]' | head -c 63)
  if [ -z "$TS_HOST" ]; then
    echo "ERROR: Tailscale hostname resolved to empty after sanitization" >&2
    exit 1
  fi
  export TS_HOST

  sudo mkdir -p /dev/net
  sudo mknod /dev/net/tun c 10 200 2>/dev/null || true
  sudo chmod 600 /dev/net/tun
  if [ ! -c /dev/net/tun ]; then
    echo "ERROR: Failed to create /dev/net/tun" >&2
    exit 1
  fi

  cat > /tmp/tailscale-up.sh << 'TSUP_SCRIPT'
#!/bin/bash
sleep 5
sudo tailscale up --authkey="$TS_AUTHKEY" --hostname="$TS_HOST" --accept-dns=false
TSUP_SCRIPT
  chmod +x /tmp/tailscale-up.sh

  cat >> /tmp/supervisord.conf << 'TAILSCALE'

[program:tailscaled]
command=sudo tailscaled --state=/var/lib/tailscale/tailscaled.state
priority=3
autorestart=true
stdout_logfile=/dev/stdout
stdout_logfile_maxbytes=0
stderr_logfile=/dev/stderr
stderr_logfile_maxbytes=0

[program:tailscale-up]
command=/tmp/tailscale-up.sh
priority=4
startsecs=0
autorestart=unexpected
stdout_logfile=/dev/stdout
stdout_logfile_maxbytes=0
stderr_logfile=/dev/stderr
stderr_logfile_maxbytes=0
TAILSCALE

  if [ "$TAILSCALE_SERVE_ENABLED" = "true" ]; then
    cat > /tmp/tailscale-serve.sh << 'TSSERVE_SCRIPT'
#!/bin/bash
sleep 10
sudo tailscale serve --bg "http://127.0.0.1:${TAILSCALE_SERVE_PORT}"
TSSERVE_SCRIPT
    chmod +x /tmp/tailscale-serve.sh
    cat >> /tmp/supervisord.conf << 'TSSERVE'

[program:tailscale-serve]
command=/tmp/tailscale-serve.sh
priority=5
startsecs=0
autorestart=unexpected
stdout_logfile=/dev/stdout
stdout_logfile_maxbytes=0
stderr_logfile=/dev/stderr
stderr_logfile_maxbytes=0
TSSERVE
  fi
fi

exec supervisord -n -c /tmp/supervisord.conf
