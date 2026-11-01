# Native-browser acceptance

These scripts connect to the already-managed CDP browser and native-web test gateway. They do not install dependencies, start a browser, start a gateway, rebuild artifacts, or invoke app domain operations directly.

## Deterministic transport tests

From the project root, use the existing Node runtime (with `node:module.registerHooks`/`stripTypeScriptTypes` support) and no newly installed dependencies:

```sh
node --import ./web/browser/register-typescript.mjs --test web/browser/browser-transport.test.mjs
node --test web/security.test.mjs
```

## Real UI acceptance

The isolated test container mounts the checkout as `/workspace`, the copied test data as `/data`, and its existing dependencies as `/app/node_modules`.

```sh
docker exec ai-novel-native-test node /workspace/web/browser/native-acceptance.mjs
```

When the gateway must remain on its app/LAN network and cannot reach the managed CDP browser, run a separate disposable driver on the browser network. The already-managed relay must expose the same gateway hostname on that network; no app domain code or model endpoint changes are involved:

```sh
docker run --rm --init --name ai-novel-native-browser \
  --network docker_app_net --entrypoint node \
  -v /root/AI/AI-Novel-Writer:/workspace -w /workspace \
  ai-novel-writer:1.4.0 /workspace/web/browser/native-acceptance.mjs
```

The relay must inject its fixed isolated-test principal header on every upstream request, as the authenticated production nginx ingress does. Chrome's download subsystem may not preserve Playwright's page-level extra HTTP headers; a plain unauthenticated relay can therefore cancel real downloads even when rendered-page requests work. Do not weaken gateway authentication or replace browser download acceptance with a fetch to hide that test-ingress mismatch.

This driver inherits neither the backend's proxy environment nor its network topology. It reuses the image's installed Playwright dependency and only the source/artifact workspace mount.

The script creates only uniquely named acceptance projects in the isolated managed library. It uses a fresh context and closes only the context/pages it created. It never chooses or modifies the original `部署验收测试` project. The remote browser connection is disconnected at the end.

Coverage:

1. Library rendered with `data-runtime=web` and no `window.aiNovelAPI` shim.
2. Create a work through the actual managed-library BrowserDialog.
3. Chinese IME composition through CDP `Input.imeSetComposition` and `Input.insertText`; save setup, refresh and reopen, and return-to-library/reopen persistence.
4. Screenshots and document/body horizontal-overflow assertions at 760px and 390px. The intentional horizontally scrollable navigation ribbon is permitted to contain wider child buttons; it must not widen the document or body.
5. Upload a UTF-8 two-chapter TXT through the native file input, inspect author-manuscript preview, confirm, require actual run-sheet completion (including mandatory AI postprocessing, up to 240 seconds), then require authoritative chapter read projection.
6. TXT export through the nested managed-directory dialog; receive an actual Playwright browser download and compare downloaded bytes to both unique uploaded Chinese chapters.
7. Portable `.ainovel` save dialog and real archive download, upload the bytes through the native file input, restore into a new managed-library folder, preserve the open original, and reopen the restored copy after a refresh.
8. Open original and restored copy in different tabs with different client IDs, make concurrent distinct setup edits, refresh/reopen both, and verify no project-state bleed.
9. Fail on renderer `pageerror` events. HTTP status/error codes are captured without request bodies, cookies or CSRF tokens.

`authorInference` is disabled using the real checkbox label to avoid optional reverse-inference generation. Mandatory authoritative-import postprocessing may still require model configuration. A provider/configuration limitation is not silently treated as successful AI processing.

Artifacts are written to `.native-test-data/browser-acceptance/<UTC timestamp>/`: JSON report, screenshots, downloaded TXT, and downloaded `.ainovel`. Backend sources and browser artifacts must be rebuilt/restarted by their owner before the acceptance run; a fresh UI build with an old operation allowlist/runtime can otherwise correctly return HTTP 404 for new operations.

Optional environment variables:

- `NOVEL_TEST_CDP`: full remote CDP URL, default `ws://172.18.0.22:3000?timeout=600000`.
- `NOVEL_TEST_BROWSER_URL`: gateway URL, default `http://ai-novel-native-test:6081`.
- `NOVEL_TEST_ARTIFACTS`: container-visible artifact directory.
- `NOVEL_TEST_PROJECT_NAME`: unique new project name (not an existing project), except in the explicitly selected transfer-only resume mode.
- `NOVEL_TEST_TRANSFER_ONLY=1`: reopen an existing successful acceptance fixture and run only TXT download, archive restore, and two-tab isolation; no new model dispatch or import is started. Requires exact `NOVEL_TEST_PROJECT_NAME` and `NOVEL_TEST_FIXTURE_STAMP`. Use a new `NOVEL_TEST_ARTIFACTS` directory to preserve the original strict-model report.
- `NOVEL_TEST_RESTORED_ONLY=1`: with transfer-only mode, skip already-verified downloads/import and verify an existing restored copy's refresh/reopen persistence and independent concurrent tab writes. Requires `NOVEL_TEST_RESTORED_PROJECT_NAME`; `NOVEL_TEST_TRANSFER_REPORT` records the previous actual-download/restore evidence path.
- `NOVEL_TEST_FIXTURE_STAMP`: existing fixture's exact unique stamp, used by transfer-only verification.
- `NOVEL_TEST_ORIGINAL_REPORT`: optional original strict-report path recorded by transfer-only evidence.
- `NOVEL_TEST_REMOTE_DOWNLOAD_DIR`: optional unique `/tmp/novel-native-acceptance-<run>` path **inside the remote browser container**. Scopes `Browser.setDownloadBehavior` to the owned browser context, requires actual `downloadWillBegin` plus `downloadProgress:completed` and `download.failure() === null`, then requests exact-file copying from the host helper. This avoids Playwright `saveAs()` looking for a remote CDP path on the driver filesystem. Start the bounded host `remote-download-copy.mjs <host artifact output>` helper before the driver. It copies only the two expected real browser files from fixed container `browserless` and records receipts; HTTP fetch diagnostics are never accepted as browser downloads.
- `NOVEL_TEST_PROBE_ONLY=1`: read-only library probe, no project mutations.
- `NOVEL_TEST_ALLOW_IMPORT_POSTPROCESS_LIMITATION=1`: explicitly allow durable manuscript read-projection verification after reload if mandatory AI postprocessing fails or does not finish within the 240-second run-sheet wait. The JSON report records this limitation and does not claim successful AI processing. Missing backend operation HTTP 404s still fail rather than being disguised as a model limitation. This opt-in is off by default.

## Read-only resources and tab lifecycle

`resource-ui-acceptance.mjs` accepts `NOVEL_TEST_PROJECT_NAME` for an existing isolated acceptance fixture, optional `NOVEL_TEST_PROJECT_TITLE` for a restored directory whose title remains the original, `NOVEL_TEST_RUNTIME_IMAGE` for exact runtime provenance, and the common CDP/URL/artifact variables. It navigates architecture, cast/relationship graph, blueprint, finalized manuscript, plot/threads, knowledge, history, prompts and skills without editing or dispatching models. Unexpected generation/mutation/install requests are blocked and fail the report. MCP is covered only if its navigation exists; fixture-empty history is explicitly identified rather than presented as historical-comparison coverage. The final step triggers real `pagehide` by navigating its own page to `about:blank`, records whether the unloading `/api/release` request/HTTP 200 can be observed through CDP, waits the product's 1500ms idle-release grace plus 300ms, and requires the same fixture to open successfully from a fresh tab with a different client ID and no HTTP 409. An unobservable unloading response is explicitly reported, never fabricated. `NOVEL_TEST_LIFECYCLE_ONLY=1` skips already-proven resource navigation when repeating just this scoped lifecycle assertion. It closes only its own context. Screenshots/reports stay in ignored private artifacts.

## Nested-dialog diagnostic

```sh
docker exec -e NOVEL_TEST_PROJECT_NAME='<existing acceptance project name>' ai-novel-native-test node /workspace/web/browser/dialog-diagnostic.mjs
```

This diagnostic opens an existing acceptance project, opens the author-import chooser, and records actual modal DOM ancestors, aria-hidden, pointer events, z-index and bounds in `.native-test-data/browser-dialog-diagnostic/`. It does not upload files or confirm an import. Use it to distinguish a hidden/inaccessible browser modal from a selector failure. The fixed integration uses a dedicated body overlay prompt layer rather than a chooser portal nested under `#root` while a legacy modal has hidden that root.
