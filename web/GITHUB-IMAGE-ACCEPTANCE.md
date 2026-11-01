# GitHub-hosted native image build: first verified publication

## Verified source and run

- Private source repository: https://github.com/qingwo1991-debug/ai-novel-writer-web
- Runtime/build source commit: `d18f43f987c0a02e25db96f3b70cb76b1eae85b8`.
- GitHub Actions run: https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41363172818 — **success**.
- Pipeline: `.github/workflows/native-web-image.yml`; target platform **linux/amd64**.
- The clean hosted build did **not** use the server-local desktop/toolchain image.

## Published image

```text
ghcr.io/qingwo1991-debug/ai-novel-writer-web@sha256:84801a8a9016f115899c5be419cf2a29a1d112f82be3312fdc12609b0e95cf37
```

Both the full `sha-d18f43f987c0a02e25db96f3b70cb76b1eae85b8` tag and `latest` were pushed with that digest in the successful job log. Use the digest for reviewed deployment; mutable tags and reruns are not immutable versions.

## What passed

1. The dependency-free security/sanitizer/departure/transport command passed **65 tests** on the hosted Node 22 job.
2. A standalone Node 22 / pnpm 11.11.0 toolchain installed the frozen lockfile with all lifecycle scripts disabled; no local builder image or private author volume was available.
3. TypeScript, native-Web Vite, Node worker bundling, reviewed runtime dependency extraction and native module loading completed.
4. The loaded final image passed an empty-data smoke test before registry login: no network, read-only root, dropped capabilities, bounded PIDs and `/data`/`/tmp` tmpfs.
5. Health returned native Node/non-Electron; unauthenticated bootstrap returned 401; an actual in-memory SQLite query and LanceDB/Arrow/ZIP imports succeeded. The final image contained neither Electron/Chromium nor VNC/Xvfb executables.
6. The exact smoke-tested image was subsequently published using repository-scoped `GITHUB_TOKEN`. There was no deployment step.

## Limits and operational handoff

- This is clean-build and isolated-image evidence, not production login or full feature acceptance. Bounded real browser/model acceptance was recorded separately against the local immutable native image in `BROWSER-ACCEPTANCE.md` and `PLANNING-ACCEPTANCE.md`.
- Production nginx, old service and author data were not switched by this workflow or source push.
- The existing server GitHub OAuth credential has no `read:packages` scope; a Packages API inspection returned 403. Package visibility/anonymous pull and a server pull of this image are **not claimed verified**. New GHCR packages are normally private; an owner should check Packages access settings. Do not change visibility merely to avoid authentication.
- For a private pull, provision a separate minimally scoped `read:packages` credential through Docker's credential storage. Never put it in source, Compose, logs or command arguments. Build publication itself requires no extra PAT.
- Follow-up acceptance documentation/test-driver-only commits may use `[skip ci]` without rebuilding an unchanged production runtime; the image revision above remains explicit.
- GitHub emitted a nonfatal warning for older SHA-pinned action runtimes being executed under Node 24. Application tests/build still explicitly used Node 22. No warning is represented as an application test failure or as proof of future action compatibility.
