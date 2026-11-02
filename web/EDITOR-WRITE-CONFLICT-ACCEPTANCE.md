# Native browser writable-editor acceptance

## Actual result

`web/browser/editor-write-conflict-acceptance.mjs` completed on **2026-11-02T01:19:51Z**, job `bash-520`, exit **0**. This is a real Playwright/CDP native-browser run, not a component mock or injected-store test.

| Scenario | Result |
| --- | --- |
| Open the one owned synthetic project and chapter 1 draft v1 in actual writable CodeMirror | PASS |
| Insert Chinese Unicode text, verify dirty state, undo to exact baseline, redo to exact edited text | PASS |
| Explicit Ctrl+S produces a successful real server save; reload/reopen restores exact text and saved state | PASS |
| Hold delivery of an actual successful server-save response, type more while saving, then deliver the unchanged response: newer text survives and remains dirty | PASS |
| Save that newer text using the updated saved baseline, then reload/reopen: exact final text survives | PASS |

Totals: **4 named steps passed, 0 failed, 111 API responses, 3 real draft saves, 0 blocked requests, 0 errors, 0 paid dispatches**. The owned context was closed. No shared-browser close, production access, service stop, direct domain-write API request, SQL write, store injection, chapter generation, or settings mutation was performed by this driver.

Private report: `.native-test-data/browser-editor-write/2026-11-02T01-19-51-306Z/report.json`.

The report retains only safe operation/status/method records, step outcomes, hashes, lengths and booleans. Initial body: 26 UTF-16 units; final body: 80 units, SHA-256 `56995269058ff613dece1ac274c8b1297bc35c2809fdb93058779ddb41add2fc`. No author prose, cookies, grants, request/response bodies, server paths, lease/session IDs, or screenshots are written to the report. Artifact directory mode is `0700`, report mode `0600`.

## Fixture provenance and boundaries

The writable fixture is **only** the book previously created through the library UI regression:

- Title: `Native修复验收-2026-11-02T01-13-45-849Z-a87979`
- Domain project ID: `43e85eaa-094d-4a06-aa19-d5e044b016e5`
- A coordinator stopped the isolated gateway, seeded one synthetic chapter-1 blueprint and one draft-v1 body offline in that newly created empty book, rebuilt, and restarted the gateway before this run. This does **not** receive AI workflow or manual first-draft-creation UI credit.

The driver hardguards the exact isolated origin, exact fixture title and project ID, captured project path, actual draft ID, outgoing save body and expected saved baseline. It does not discover an arbitrary writable user book. Every UI save is restricted to that captured chapter-1 v1 draft and this run's expected text/baseline. Read operations are explicitly enumerated; generation/MCP/global-settings/other mutation requests and external origins fail closed.

The script is intentionally not a generic fixture seeder. If this synthetic fixture is unavailable, finalized, migrated to a different ID, or no longer begins with the known synthetic baseline, stop and prepare an explicitly authorized new isolated fixture; do not loosen guards to reuse author content. Repeated runs append a unique timestamp marker to this same disposable book and leave it retained, not deleted.

## Run

With the existing copied-data backend, test relay, and managed CDP browser already running, from the checkout:

```sh
node --check web/browser/editor-write-conflict-acceptance.mjs

docker run --rm --init --name ai-novel-editor-write-driver \
  --network docker_app_net \
  --mount type=bind,src="$PWD",dst=/workspace \
  --workdir /app --entrypoint node ai-novel-writer:1.4.0 \
  /workspace/web/browser/editor-write-conflict-acceptance.mjs
```

The only accepted `NOVEL_TEST_BROWSER_URL` is `http://ai-novel-native-test:6081`. The script uses the existing browser `ws://172.18.0.22:3000?timeout=600000` and Playwright installed in the disposable driver image. It creates and closes one owned context; **never add `browser.close()`**. It does not start/stop/rebuild any services. Verify copied-data mounts and disabled model endpoint policy privately before running. A five-minute watchdog closes only the owned context; failed runs exit 1 and retain sanitized evidence.

## Meaning of the delayed-save check

The interception calls `route.fetch()` on the **actual UI-generated save**, verifies the real backend's HTTP 200/success result, and then gates delivery of that unchanged response with a promise. While the response is withheld, normal keyboard input appends more Chinese text in CodeMirror. `route.fulfill({ response })` forwards the actual response, without fabricated status/body or modified request. The UI must retain the newer text and remain dirty. A second real UI Ctrl+S uses the newly persisted baseline; another reload verifies persistence.

This is a real delayed-acknowledgement/newer-typing regression, **not** an independently stale-server content CAS conflict. No body is altered offline or through a parallel domain-write call while the worker is live. A second browser identity is intentionally prevented from editing this book by the separately verified ownership lock. The filename includes “conflict” for the broader editor-safety acceptance work; this run does **not** claim a real stale-content conflict dialog, historical rollback, finalized-text editing, physical OS IME composition, autosave, or AI finalization. Server CAS/finalization-race coverage is a separate HTTP-boundary regression and must be reported separately.
