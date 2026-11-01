# Native browser acceptance evidence

## Scope and provenance

- These are real rendered-UI/CDP checks against the native Node gateway, not a desktop bridge or mocked domain API.
- Only uniquely created acceptance fixtures were mutated. The pre-existing author project was not modified.
- Strict import and settings checks used workspace build job **402** on base image `sha256:9a6be22fc866f129a8cb949b83a583b09c4d716d45cbf3251c00aaf892966eff`; this was **not an immutable final application image**.
- Transfer job **447** and restored/tab job **451** used application image `sha256:7986107a68da734aca974372874e6efec7c36346c62d6ace7e89d5a16f51d2d7`, with read-only root, dropped capabilities, and the production flock entrypoint.
- Final-image read-only resource jobs **459/460** and lifecycle job **461** used application image `sha256:8730c6b5ddd51578a7a633cd6bd6d7905eb68ad2d9bbf554977b56d1ccfd5e02`.
- The separate disposable browser driver and relay used base image `sha256:9a6be22fc866f129a8cb949b83a583b09c4d716d45cbf3251c00aaf892966eff`.
- The backend stayed on its provider-reachable application network; the CDP driver/relay stayed on the browser network. The isolated relay injected a fixed test principal on **all** upstream requests, matching trusted production ingress behavior. No production authentication was weakened.
- Reports/screenshots/downloads remain private in ignored `.native-test-data`; links below are local evidence references, not published artifacts. This summary contains no prose, cookies, provider credentials, or download capabilities.

## Evidence matrix

| Check | Result | Evidence |
| --- | --- | --- |
| Managed-library create; Chinese IME composition; setup save/reload/reopen | PASS | [Strict report](../.native-test-data/browser-acceptance/2026-11-01T22-51-28-922Z/report.json) |
| Responsive 760px and 390px document widths | PASS | Same strict report; no horizontal document overflow |
| Actual native TXT file upload; two chapters; inference explicitly disabled | PASS | Same strict report; 464-byte input |
| Mandatory actual-model import postprocessing | PASS | Same strict report; run state `completed`, all 3 finalization steps complete, about 113 seconds; no relaxed-success option used |
| Actual Chrome TXT export/download | PASS | [Transfer report](../.native-test-data/browser-transfer/2026-11-01T23-remote-copy-2/report.json), [exact-file receipts](../.native-test-data/browser-transfer/2026-11-01T23-remote-copy-2/remote-download-copy-receipts.json); **574 bytes** |
| Actual Chrome `.ainovel` download, ZIP magic, upload same downloaded bytes, restore fresh copy | PASS | Same transfer report and receipts; **644703 bytes**; restore-success UI and new manifest identity verified |
| Restored copy library discovery, reopen, refresh, persisted setup | PASS | [Restored/tab report](../.native-test-data/browser-transfer/2026-11-01T23-restored-tabs/report.json), job 451 exit 0 |
| Two concurrent tabs on original/restored projects; distinct tab IDs; independent saves and refresh/reopen | PASS | Same restored/tab report, concurrent save/refresh assertions passed; `differentClientIds` and `separatelyPersisted` true; no renderer errors |
| Browser-specific settings and fixed external link | PASS | [Settings report](../.native-test-data/browser-settings/2026-11-01T22-59-44-187Z/report.json), job 432: no desktop window controls; server-managed proxy; server-library/version copy; deployment-managed updates; fixed HTTPS link with null opener |
| Final-image read-only architecture, cast/graph, blueprint, finalized manuscript, plot/threads, knowledge, history, prompts and skills navigation | PASS | [Resource report](../.native-test-data/browser-resources/2026-11-01T23-final8730-2/report.json), all 9 navigation steps passed; zero API errors/model dispatches. History was fixture-empty and MCP has no stage-shell navigation, so neither historical comparison nor MCP UI is claimed. |
| Actual idle pagehide and fresh-client reopen after 1800ms | PASS | [Lifecycle report](../.native-test-data/browser-resources/2026-11-01T23-final8730-lifecycle/report.json), job 461 exit 0; genuine release request observed; new tab client opened same project without HTTP 409. Unloading release response was not CDP-observable and is **not** claimed HTTP 200. |

## Harness failures and their resolution

- The strict report's **overall status remains failed** because the next browser-download step was canceled. Its completed actual-model import assertions are preserved, not relabeled.
- The first download cancellation was an isolated relay mismatch: Chrome's download requests did not preserve Playwright page-level principal headers. The relay was corrected to inject its trusted test principal on every request; gateway download responses then returned 200.
- Remote CDP `download.saveAs()` attempted to copy a Chrome-container `/tmp` path on the separate driver filesystem (`ENOENT`). The harness now scopes `Browser.setDownloadBehavior` to its **own context**, requires `download.failure() === null` plus genuine CDP download completion, and asks the bounded host helper to copy that exact GUID file.
- Same-origin fetches were diagnostic only and were **never** accepted as browser downloads. Receipts and assertions validate actual Chrome TXT/archive bytes.
- An early CDP completion assertion raced event delivery; the harness now waits for the matching GUID's completion event.
- Job 447's transfer report remains overall failed because the harness sampled a cached chooser option list before its asynchronous library refresh. A fresh read-only chooser proved the restored project existed; the harness now waits for the restored option. Job 451 then verified restore persistence and both tabs without another download or model call.
- Closing an automation context exposed the server's two-minute stale-tab lease grace. Parent implementation work added pagehide release. Jobs 459/460 initially waited for an unloading response CDP did not expose, so their overall status remains failed despite all navigation steps passing. Job 461 instead verified the authoritative behavior: actual pagehide, observed release request, fresh client reopening after 1800ms with no HTTP 409; it passed without lease overrides or direct API mutations.

## Reproduction

- See [browser README](browser/README.md) for driver topology, strict import, transfer-only and restored-only resume modes.
- Main harness: [`browser/native-acceptance.mjs`](browser/native-acceptance.mjs).
- Exact completed-file host copier: [`browser/remote-download-copy.mjs`](browser/remote-download-copy.mjs).
- Read-only library diagnostic: [`browser/library-diagnostic.mjs`](browser/library-diagnostic.mjs).
- Final-image resource/lifecycle harness: [`browser/resource-ui-acceptance.mjs`](browser/resource-ui-acceptance.mjs); `NOVEL_TEST_LIFECYCLE_ONLY=1` resumes only the lifecycle assertion.
- Paid model work was **not repeated** during transfer/restore/tab resumes. No production source changes were made for the chooser timing issue.
