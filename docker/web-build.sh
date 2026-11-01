#!/bin/sh
set -eu
# Reuse the already-installed pinned dependency toolchain without copying a
# second node_modules tree onto a nearly-full server. The final runtime image
# below is built separately and does not launch any desktop components.
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
docker run --rm --init \
  --entrypoint /bin/sh \
  -v "$ROOT:/workspace" \
  -w /workspace \
  ai-novel-writer:1.4.0 -c '
    set -eu
    if [ ! -e node_modules ]; then ln -s /app/node_modules node_modules; fi
    node /app/node_modules/typescript/bin/tsc --noEmit --project tsconfig.web.json
    node /app/node_modules/vite/bin/vite.js build --mode web --config vite.web.config.ts
    node web/build.mjs
  '
