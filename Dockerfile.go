# Migration-only Go binary built by GitHub Actions.
# CGO is enabled for SQLite. Match the GitHub runner's glibc baseline so the
# uploaded binary cannot require a newer libc than the runtime image provides.
FROM ubuntu:24.04

LABEL org.opencontainers.image.title="AI Novel Writer Go migration candidate"
LABEL org.opencontainers.image.licenses="GPL-3.0-only"

COPY backend-go/out/novel-server /usr/local/bin/novel-server
COPY web/operations.json /web/operations.json
WORKDIR /backend-go
USER 65532:65532
EXPOSE 16082
ENTRYPOINT ["/usr/local/bin/novel-server"]
