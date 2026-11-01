#!/usr/bin/env bash
# Full Electron application, virtual display, and LAN browser portal.
set -Eeuo pipefail

export DISPLAY=:99
export HOME=/data/home
export XDG_CONFIG_HOME=/data/config
export XDG_CACHE_HOME=/data/cache

mkdir -p /data/{user-data,workspaces,home,config,cache,imports,exports}
# Seed only missing preferences; keep models, works and existing settings.
proxy_exports=$(python3 /opt/novel-docker/bootstrap.py)
eval "$proxy_exports"

rm -f /tmp/.X99-lock /tmp/.X11-unix/X99
children=()
cleanup() {
  trap - EXIT INT TERM
  for pid in "${children[@]}"; do kill "$pid" 2>/dev/null || true; done
  if [[ -n ${DBUS_SESSION_BUS_PID:-} ]]; then kill "$DBUS_SESSION_BUS_PID" 2>/dev/null || true; fi
  wait || true
}
trap cleanup EXIT
trap 'exit 0' INT TERM

eval "$(dbus-launch --sh-syntax)"
Xvfb :99 -screen 0 1600x1000x24 -nolisten tcp &
children+=("$!")
ready=0
for attempt in {1..50}; do
  if xdotool getdisplaygeometry >/dev/null 2>&1; then ready=1; break; fi
  sleep 0.1
done
if [[ $ready != 1 ]]; then echo '[entrypoint] X display did not start' >&2; exit 1; fi

# 5900 is loopback-only, never published to the host.
x11vnc -display :99 -localhost -nopw -forever -shared -quiet \
  -o /tmp/x11vnc.log -rfbport 5900 &
children+=("$!")
python3 /opt/novel-portal/server.py &
children+=("$!")

# Opt-in internal CDP port for smoke tests; do NOT publish this port.
extra_args=()
if [[ ${NOVEL_DEBUG:-0} == 1 ]]; then
  extra_args+=(--inspect=127.0.0.1:9229 --remote-debugging-address=127.0.0.1 --remote-debugging-port=9222)
fi
/app/node_modules/.bin/electron . \
  --no-sandbox \
  --disable-gpu \
  --disable-dev-shm-usage \
  --disable-software-rasterizer \
  --user-data-dir=/data/user-data \
  --lang=zh-CN \
  "${extra_args[@]}" &
children+=("$!")
echo '[entrypoint] Open http://<server>:6080/ (trusted LAN only; no authentication)'
# If any required process fails, stop the others so Docker can restart the stack.
wait -n "${children[@]}"
