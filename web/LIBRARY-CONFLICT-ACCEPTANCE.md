# Native browser library and ownership-conflict acceptance

## Scope and safety

`web/browser/library-conflict-acceptance.mjs` exercises real rendered native-Web UI against **only** `http://ai-novel-native-test:6081`. It creates a uniquely named `Native修复验收-<timestamp>-<random>` book in the isolated copied-data library. It does not access production, invoke domain-write APIs directly, inject application stores, generate prose, change global settings, delete projects, or stop the gateway/relay/browser.

The script connects to the existing managed CDP browser at `ws://172.18.0.22:3000?timeout=600000`, creates two owned browser contexts, and closes only those contexts. Never add `browser.close()` to this shared-browser test. Bootstrap/library reads and explicitly enumerated readonly operations are allowed. Creation is restricted to the newly generated title, removal to the captured created path, and chooser confirmations to that same path. All other mutation, paid-generation, MCP and external-origin requests fail closed. Path, project ID and lease are compared in memory, not written to the report.

The isolated backend must already be running against a **copy**, not a production mount, with model endpoint allowlisting empty. The existing test relay supplies the regression principal. This is not a production password-login test. Do not install or restart Chrome. Check `docker inspect ai-novel-native-test-backend` mounts and `NOVEL_ALLOWED_ENDPOINTS=` privately before running; do not publish its environment or auth headers.

## Run

From the repository checkout, with the managed browser and isolated relay/backend already available:

```sh
node --check web/browser/library-conflict-acceptance.mjs

docker run --rm --init --name ai-novel-library-conflict-driver \
  --network docker_app_net \
  --mount type=bind,src="$PWD",dst=/workspace \
  --workdir /app --entrypoint node ai-novel-writer:1.4.0 \
  /workspace/web/browser/library-conflict-acceptance.mjs
```

The driver image supplies Playwright from `/app/node_modules`. Do not start another web server or point the test at production. `NOVEL_TEST_BROWSER_URL`, when specified, must exactly equal the isolated origin. `NOVEL_TEST_CDP`, when specified, must retain the managed browser host. `NOVEL_TEST_ARTIFACT_ROOT` defaults to `/workspace/.native-test-data/browser-library` and must retain that private directory structure.

Reports are written under `.native-test-data/browser-library/<stamp>/report.json`, directory mode `0700`, report mode `0600`. They contain synthetic fixture title, step outcomes, safe operation/method/status records, counts and booleans; no cookies, grants, lease/session IDs, server paths, response bodies, author prose or screenshots. A nonpassing run exits 1. Failure evidence intentionally omits arbitrary Playwright messages because these can expose library contents; use the failing step name and operation/status records for diagnosis. The script has a five-minute context-closing watchdog.

## Actual run

Completed **2026-11-02T01:13:45Z**, job `bash-512`, exit **0**:

| Scenario | Evidence | Result |
| --- | --- | --- |
| Native library | Runtime `web`, no desktop preload | PASS |
| New book | Readonly location says `服务器书库`; no Choose button; title enables create; real UI creation/open | PASS |
| Remove own entry | Own-title menu and confirmation; absent immediately and after reload | PASS |
| Retained-book chooser | Removed book remains in Open book selector; same created identity opens; shelf entry returns | PASS |
| Ownership conflict | A second independent owned context receives actual `project:open` HTTP **409**, no successful contender open | PASS |
| Original owner reload | First context reloads and reopens successfully with the **same lease**, no owner 409 | PASS |

Final counts: **6 passed, 0 failed, 82 API responses (81 × 200, 1 × 409), 0 blocked requests, 0 errors, 0 paid dispatches**. Both owned contexts were closed; no shared browser close was issued. The uniquely created project directory and `.ai-novel/project.db` were independently observed still present after the run. No physical-delete endpoint is permitted by the harness.

Private passing report: `.native-test-data/browser-library/2026-11-02T01-13-45-849Z-a87979/report.json`.

Two earlier harness-development attempts were preserved as stopped reports, not counted as product passes: `2026-11-02T01-11-04-304Z-faf51d` stopped because the route guard initially did not recognize `project:create`'s `projectPath` return shape; `2026-11-02T01-12-39-319Z-014e0e` created successfully but stopped on command-palette navigation timing. The passing script captures only its own creation path and navigates through the rendered work menu instead. Earlier synthetic fixture directories were intentionally retained, never deleted.

## Limits

Successful reopen with unchanged project identity and a retained database demonstrates that removing the shelf entry did not delete the book; this is not a byte-for-byte backup integrity audit. This test does not cover paid models, editor body saves, chapter generation, or finalization CAS. Same-owner reload is distinct from a second independent browser context: the latter must continue to conflict, not silently steal ownership. A maintained safe script and an actual passing isolated run do not authorize production deployment.
