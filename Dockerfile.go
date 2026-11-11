# Migration-only Go binary built by GitHub Actions.
# CGO is enabled for SQLite. Match the GitHub runner's glibc baseline so the
# uploaded binary cannot require a newer libc than the runtime image provides.

# Frontend assets use the same pinned pnpm/Vite pipeline as Dockerfile.web.
# Node exists only in this builder stage; the runtime stage below never sees it.
FROM node:22-bookworm-slim AS frontend
WORKDIR /build
ENV NODE_ENV=development \
    ELECTRON_SKIP_BINARY_DOWNLOAD=1 \
    PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml .npmrc ./
# Same pinned manager and lifecycle-script policy as Dockerfile.web: no Electron
# or browser download, native binaries come from the registry packages.
RUN node -e 'if(JSON.parse(require("node:fs").readFileSync("package.json")).packageManager!=="pnpm@11.11.0")throw new Error("Update the pinned native build toolchain")' \
 && npm install --global pnpm@11.11.0 --ignore-scripts --no-audit --no-fund \
 && pnpm install --ignore-scripts --frozen-lockfile
COPY tsconfig*.json vite.web.config.ts index.html ./
COPY src ./src
COPY public ./public
COPY electron ./electron
# The canonical browser build (vite.web.config.ts writes dist-web/). No typecheck
# or test run here: this stage ships assets, not verification.
RUN node /build/node_modules/vite/bin/vite.js build --mode web --config vite.web.config.ts

FROM ubuntu:24.04

LABEL org.opencontainers.image.title="AI Novel Writer Go migration candidate"
LABEL org.opencontainers.image.licenses="GPL-3.0-only"

# The base image ships no trust store. Every cloud model provider is HTTPS, so
# without one every TLS dial failed with an unknown-authority error before the
# request left the process, and only plain-HTTP local endpoints worked. The
# bundle is copied from the builder stage instead of installed with apt-get:
# the CI runner intermittently cannot resolve archive.ubuntu.com, and a
# trust store does not need a package manager to be deterministic.
COPY --from=frontend /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

COPY backend-go/out/novel-server /usr/local/bin/novel-server
COPY web/operations.json /web/operations.json
# Vite output from the builder stage: /build/dist-web -> /app/dist-web.
COPY --from=frontend /build/dist-web /app/dist-web
# -policy defaults to ../web/operations.json, so the working directory stays one
# level below /web. The listener also stays loopback-only: authentication is the
# trusted X-Novel-User header, so the container must be reached through a proxy.
WORKDIR /backend-go
USER 65532:65532
EXPOSE 16082
ENTRYPOINT ["/usr/local/bin/novel-server", "-static-dir", "/app/dist-web"]
