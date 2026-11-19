#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -e

if [ "${CETIRUN_INGRESS_MTLS:-}" = "true" ]; then
  export TTYD_BIND_ARGS="--interface 127.0.0.1"
else
  export TTYD_BIND_ARGS=""
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

[program:openclaw]
command=openclaw gateway $* --allow-unconfigured
priority=40
autorestart=true
stopasgroup=true
killasgroup=true
stdout_logfile=/dev/stdout
stdout_logfile_maxbytes=0
stderr_logfile=/dev/stderr
stderr_logfile_maxbytes=0
EOF

if [ "${TTYD_ENABLED}" = "true" ]; then
  # Improve ttyd+vim copy/paste UX by disabling vim mouse capture by default.
  # This allows browser-native text selection and copy in terminal sessions.
  mkdir -p /home/vibe
  if [ ! -f /home/vibe/.vimrc ] || ! grep -q '^set mouse=$' /home/vibe/.vimrc 2>/dev/null; then
    printf '\nset mouse=\n' >> /home/vibe/.vimrc
  fi

  # Validate ttyd port is a number in unprivileged range
  if ! echo "${TTYD_PORT}" | grep -qE '^[0-9]+$' || [ "${TTYD_PORT}" -lt 1024 ] || [ "${TTYD_PORT}" -gt 65535 ]; then
    echo "ERROR: TTYD_PORT must be a number between 1024 and 65535, got '${TTYD_PORT}'" >&2
    exit 1
  fi
  # Validate ttyd shell is an absolute path to an executable
  if [ ! -x "${TTYD_SHELL}" ]; then
    echo "ERROR: TTYD_SHELL must be an absolute path to an executable, got '${TTYD_SHELL}'" >&2
    exit 1
  fi
  # Validate ttyd base path matches the Helm schema pattern
  if ! echo "${TTYD_BASE_PATH}" | grep -qE '^/[a-z0-9/-]+$'; then
    echo "ERROR: TTYD_BASE_PATH must match ^/[a-z0-9/-]+\$, got '${TTYD_BASE_PATH}'" >&2
    exit 1
  fi
cat >> /tmp/supervisord.conf << 'TTYD_EOF'

[program:ttyd]
command=ttyd %(ENV_TTYD_BIND_ARGS)s --port %(ENV_TTYD_PORT)s --base-path %(ENV_TTYD_BASE_PATH)s --writable --check-origin --debug 4 %(ENV_TTYD_SHELL)s
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
  # Derive per-pod hostname: <prefix>-<ordinal> or fall back to pod name
  if [ -n "$TAILSCALE_HOSTNAME" ]; then
    POD_ORDINAL=${HOSTNAME##*-}
    if echo "$POD_ORDINAL" | grep -qE '^[0-9]+$'; then
      TS_HOST="${TAILSCALE_HOSTNAME}-${POD_ORDINAL}"
    else
      # Not a StatefulSet pod name — use full hostname as suffix
      TS_HOST="${TAILSCALE_HOSTNAME}-${HOSTNAME}"
    fi
  else
    TS_HOST="$HOSTNAME"
  fi

  # Sanitize hostname: strip anything that isn't lowercase alnum or hyphen,
  # then truncate to 63 chars (Tailscale/DNS label limit).
  # This prevents command injection via shell metacharacters in TS_HOST.
  TS_HOST=$(printf '%s' "$TS_HOST" | tr -cd 'a-zA-Z0-9-' | tr '[:upper:]' '[:lower:]' | head -c 63)
  if [ -z "$TS_HOST" ]; then
    echo "ERROR: Tailscale hostname resolved to empty after sanitization" >&2
    exit 1
  fi
  export TS_HOST

  # Kernel networking: create TUN device
  sudo mkdir -p /dev/net
  sudo mknod /dev/net/tun c 10 200 2>/dev/null || true
  sudo chmod 600 /dev/net/tun
  if [ ! -c /dev/net/tun ]; then
    echo "ERROR: Failed to create /dev/net/tun — kernel networking requires NET_ADMIN capability" >&2
    exit 1
  fi

  # Write wrapper scripts so env vars are expanded by bash at runtime
  # (not by supervisord text substitution, which has no quoting/escaping).
  cat > /tmp/tailscale-up.sh << 'TSUP_SCRIPT'
#!/bin/bash
sleep 5
echo "tailscale-up: running tailscale up..."
sudo tailscale up --authkey="$TS_AUTHKEY" --hostname="$TS_HOST" --accept-dns=false
echo "tailscale-up: connected successfully"
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
echo "tailscale-serve: running tailscale serve..."
sudo tailscale serve --bg "http://127.0.0.1:${TAILSCALE_SERVE_PORT}"
echo "tailscale-serve: serve configured successfully"
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
