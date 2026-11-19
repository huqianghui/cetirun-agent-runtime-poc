#!/bin/bash
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

set -e

DISPLAY_NUM=${DISPLAY_NUM:-99}
SCREEN_RES=${SCREEN_RESOLUTION:-1920x1080x24}
VNC_PORT=${VNC_PORT:-5900}
NOVNC_PORT=${NOVNC_PORT:-6080}
DESKTOP_LOGGING=${DESKTOP_LOGGING:-false}
VNC_BIND_ARGS=
CHROME_BIND_ARGS=
NOVNC_LISTEN=${NOVNC_PORT}
if [ "${CETIRUN_INGRESS_MTLS:-}" = "true" ]; then
  VNC_PORT=5900
  NOVNC_PORT=6080
  VNC_BIND_ARGS="-listen 127.0.0.1 -localhost"
  NOVNC_LISTEN="127.0.0.1:6080"
  CHROME_BIND_ARGS="--remote-debugging-address=127.0.0.1"
fi

# Resolve log destinations based on DESKTOP_LOGGING flag.
if [ "${DESKTOP_LOGGING}" = "true" ]; then
  VNC_LOG=/dev/stderr
  VNC_LOG_MAXBYTES=0
  NOVNC_LOG_FLAG="--log-file /dev/stderr"
else
  VNC_LOG=/dev/null
  VNC_LOG_MAXBYTES=0
  NOVNC_LOG_FLAG=""
fi

# noVNC ships without an index.html, so accessing the root URL returns a directory listing.
# /usr/share/novnc is owned by root and not writable at runtime, so we copy the files to
# /tmp/novnc/desktop/ (writable) and create a redirect index.html there.
# Placing files under /desktop/ lets websockify serve them at the /desktop path prefix
# that Istio routes to, so relative asset paths (./core/rfb.js) resolve correctly
# without any URI rewrite (same pattern as ttyd's --base-path /term).
# websockify is pointed at /tmp/novnc instead of /usr/share/novnc (see supervisord config below).
rm -rf /tmp/novnc
mkdir -p /tmp/novnc/desktop
cp -r /usr/share/novnc/* /tmp/novnc/desktop/
# Fix noVNC WebSocket path: default is 'websockify' → wss://host/websockify,
# which misses the /desktop/ VirtualService prefix route and hits the catch-all.
# We create a real index.html (not a symlink to vnc_auto.html) that redirects with
# the correct path param. Using a symlink would be dangerous: a redirected write
# (cat >) would follow the link and clobber vnc_auto.html. The URL ?path= overrides
# both the JS default and any stale localStorage value in the browser.
cat > /tmp/novnc/desktop/index.html << NOVNC_INDEX
<!DOCTYPE html>
<html>
<head><meta http-equiv="refresh" content="0;url=vnc_auto.html?autoconnect=true&amp;resize=scale&amp;path=${NOVNC_PATH_PREFIX}websockify"></head>
<body></body>
</html>
NOVNC_INDEX

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
environment=DISPLAY=":${DISPLAY_NUM}",DBUS_SESSION_BUS_ADDRESS="unix:path=/tmp/dbus-session-bus",DBUS_SYSTEM_BUS_ADDRESS="unix:path=/tmp/dbus-session-bus"

[program:dbus]
command=dbus-daemon --session --nofork --address=unix:path=/tmp/dbus-session-bus
priority=5
autorestart=true
stdout_logfile=/dev/null
stderr_logfile=/dev/null

[program:xvfb]
command=Xvfb :${DISPLAY_NUM} -screen 0 ${SCREEN_RES} -ac +extension GLX +render -noreset
priority=10
autorestart=true
stdout_logfile=/dev/null
stderr_logfile=/dev/null

[program:fluxbox]
command=fluxbox
priority=20
autorestart=true
startretries=10
stdout_logfile=/dev/null
stderr_logfile=/dev/null

[program:x11vnc]
command=x11vnc -display :${DISPLAY_NUM} -forever -shared ${VNC_BIND_ARGS} -rfbport ${VNC_PORT} -nopw -xkb -noxrecord -noxfixes -threads
priority=20
autorestart=true
startretries=10
stdout_logfile=${VNC_LOG}
stdout_logfile_maxbytes=${VNC_LOG_MAXBYTES}
stderr_logfile=${VNC_LOG}
stderr_logfile_maxbytes=${VNC_LOG_MAXBYTES}

[program:novnc]
command=websockify ${NOVNC_LOG_FLAG} --web /tmp/novnc ${NOVNC_LISTEN} localhost:${VNC_PORT}
priority=30
autorestart=true
stdout_logfile=${VNC_LOG}
stdout_logfile_maxbytes=${VNC_LOG_MAXBYTES}
stderr_logfile=${VNC_LOG}
stderr_logfile_maxbytes=${VNC_LOG_MAXBYTES}

[program:chrome]
command=google-chrome --no-sandbox --disable-gpu --no-first-run --disable-dev-shm-usage --start-maximized --remote-debugging-port=18800 ${CHROME_BIND_ARGS} --no-zygote --disable-sync --user-data-dir=/home/vibe/.config/google-chrome/openclaw
priority=35
autorestart=true
startretries=5
startsecs=3
stdout_logfile=/dev/null
stderr_logfile=/dev/stderr
stderr_logfile_maxbytes=0
EOF

exec supervisord -n -c /tmp/supervisord.conf
