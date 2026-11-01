#!/bin/sh
set -eu
umask 077

# Never launch the desktop bootstrap: docker/bootstrap.py seeds desktop
# preferences and emits shell proxy assignments. Native deployment provides
# HTTP_PROXY/HTTPS_PROXY/NO_PROXY explicitly; do not eval generated/user data.
export NODE_ENV=production AI_NOVEL_RUNTIME=web
export NOVEL_DATA_ROOT="${NOVEL_DATA_ROOT:-/data}"
export NODE_USE_ENV_PROXY="${NODE_USE_ENV_PROXY:-1}"
case "$NOVEL_DATA_ROOT" in
  /*) ;;
  *) printf '%s\n' 'NOVEL_DATA_ROOT must be absolute.' >&2; exit 64 ;;
esac
if [ ! -d "$NOVEL_DATA_ROOT" ] || [ ! -w "$NOVEL_DATA_ROOT" ]; then
  printf '%s\n' 'Data mount is missing or not writable by the configured UID/GID; prepare ownership offline. No automatic chown is performed.' >&2
  exit 73
fi
command -v python3 >/dev/null
command -v flock >/dev/null
node --input-type=module -e 'if(!process.allowedNodeEnvironmentFlags.has("--use-env-proxy"))throw new Error("Node22 with environment proxy support required")'

cd /app
# Lifetime kernel lock survives exec, is released on any process exit (including
# SIGKILL), and needs no PID/hostname stale-lock guessing after container restart.
# The lock file intentionally stays on disk: deleting it could split ownership.
# Do not run old desktop writers against the same volume: they do not join flock.
export NOVEL_VOLUME_LOCK_HELD=1
exec flock --no-fork --nonblock --conflict-exit-code 75 \
  "$NOVEL_DATA_ROOT/.native-web.gateway.lock" node dist-server/server.mjs
