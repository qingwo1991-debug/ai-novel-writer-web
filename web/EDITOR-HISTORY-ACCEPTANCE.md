# Native editor/history: bounded read-only acceptance subset

## Verdict

**PARTIAL PASS — not full editor acceptance.** The existing dedicated copied fixture contains chapter 1 **final v1 only** and chapter 2 with no draft. This round verifies native CodeMirror read-only display, copy-action feedback, version-history browsing and same-version comparison without altering final authority. Chinese editing, undo, manual save/reload persistence, autosave and new-draft rollback remain **unexercised / pending** because no eligible writable or older version is available.

The shipped script is deliberately read-only. It contains no dormant draft-writing branch, no domain API calls, no application-store access and no injected state. A changed fixture stops the test rather than enabling previously untested mutations.

## Fixed isolated runtime and safety boundary

- Tested image: `ghcr.io/qingwo1991-debug/ai-novel-writer-web@sha256:84801a8a9016f115899c5be419cf2a29a1d112f82be3312fdc12609b0e95cf37`.
- Observed test-container image ID: `sha256:00010f8df6050c89d430eb35222bbc5610a4e8c45dbfb4bca9aa25273d06d977`.
- Browser origin is hard-guarded to `http://ai-novel-native-test:6081`, through the parent-managed isolated relay/gateway on copied `.native-test-data`.
- Only dedicated project `微型规划验收-2026-09-18-1940b` is opened. Production `ai-novel-writer-web`, `official8445`, and `/root/AI/ai-novel-writer-data` are not targets and were not modified.
- The script creates and closes its **own CDP browser context only**. It never closes the shared browser, runs Chrome setup, restarts a service, commits, pushes or installs dependencies.
- An explicit browser-route allowlist denies paid generation, every `*:execute`, LLM generation, MCP calls, global-settings mutations, draft create/update and every non-allowlisted operation. External origins are denied. Service workers are blocked so they cannot bypass routing.
- Only read-only domain operations plus normal rendered-UI project open/startup acknowledgement and own-client transport cleanup are allowed. Project open can update test-fixture recency metadata; it is not a global-settings edit.
- Per-action waits are bounded; a 540-second watchdog closes only the owned context. No paid-call waits or automatic generation retries are present.

## Passing checks

| Step | Evidence / scope |
| --- | --- |
| `native-runtime-and-dedicated-fixture` | Rendered library opens the exact fixture; runtime is Web, without desktop preload API. |
| `finalized-chapter-readonly-baseline` | Real CodeMirror is visible, nonempty and `contenteditable=false`; UI says finalized/read-only; rendered version switcher identifies the final. |
| `copy-finalized-chapter-via-rendered-menu` | Clicks the real chapter menu's copy action and observes its success feedback. This does **not** claim independent system-clipboard byte equality. |
| `empty-chapter-no-manual-first-draft-ui` | Second chapter shows its empty/blueprint surface and AI writing entry, with no writable prose CodeMirror. No AI button is clicked. |
| `history-browse-final-and-empty-chapters` | Empty chapter has no versions; first chapter has final v1 only. Selecting it displays a read-only same-as-current comparison. Current-only history correctly offers no rollback target. |
| `finalized-authority-preserved-after-acceptance` | Returning to the final shows read-only CodeMirror with exactly the same rendered content as baseline. No finalized content/status or draft write operation is dispatched. |
| `no-paid-or-global-mutation-dispatch` | No paid/MCP, domain-write or global-settings mutation is dispatched, and no blocked attempt, HTTP failure or runtime/request error is observed in the final run. |

Final shipped-script run: **2026-11-02 00:43:32–00:43:40 UTC**, exit 0, **7 passed / 0 failed / 1 unsupported group**, 87 HTTP response records, 0 blocked requests, 0 runtime/request errors, and 4 private screenshots. No paid/domain/global-settings mutation was dispatched. Syntax checking and `git diff --check` also passed.

Unsupported group: `rollback-edit-save-reload`. This group is explicitly not included in the passing count. No original author prose, response bodies, credentials, cookies or storage tokens appear in this document or console summaries.

## Source-derived limits, not runtime defects

Locators and lifecycle behavior were read before browser control:

- `src/components/stage/chapter/ChapterPlan.tsx`: an empty chapter presents its blueprint and AI first-draft entry; this surface has no manual first-draft editor.
- `src/components/stage/history/StageHistory.tsx`: history renders read-only comparisons; rollback is offered only when the selected version is not latest. A lone final cannot be used to manufacture an older rollback target.
- `src/components/stage/history/use-version-history.ts`: the intended rollback creates a new draft rather than overwriting an earlier version. **That mutation was not exercised here.**
- `src/shared/draft-status.ts` and `src/components/stage/chapter/ChapterView.tsx`: finalized prose is read-only.
- `src/components/editor/use-draft-actions.ts` and `src/components/editor/CodeMirrorEditor.tsx`: explicit save wiring exists. `autoSaveInterval` remains a config field/default, but no chapter autosave timer/reference was found in the inspected editor/stage source. **There was no eligible writable draft for a runtime autosave test, so no autosave failure or success is claimed.**

No runtime bug was established. Two preliminary harness runs stopped safely: the first over-restricted the mandatory startup acknowledgement; the second used a whitespace-sensitive history locator that did not account for the adjacent current badge. Neither dispatched a domain mutation. The final script permits only the in-memory acknowledgement and uses the corrected source-backed locator.

## Reproduction

Load the `browser-cdp` skill first. Reuse the provided remote CDP endpoint; do not start or reset Chrome. With the same isolated parent-managed service and fixture available:

```sh
docker run --rm --init --network docker_app_net \
  --entrypoint node \
  -v /root/AI/AI-Novel-Writer:/workspace -w /workspace \
  ai-novel-writer:1.4.0 \
  /workspace/web/browser/editor-history-acceptance.mjs
```

`NOVEL_TEST_CDP` may override the connection endpoint; the browser origin and fixture remain hard-guarded. The driver uses the preexisting checkout symlink to `/app/node_modules`; it is not the tested gateway image and does not install anything.

## Private artifacts and publication policy

Each invocation writes private `report.json` and screenshots under `.native-test-data/browser-editor/<timestamp>/` (artifact directory mode 0700, files 0600). Screenshots may contain fixture text; do not attach or publish them. Reports retain only request operation/status metadata, final-content length/hash, step outcomes and private diagnostics; no credential/cookie dump is collected. Console output is limited to step names, counts and final status.

Only this sanitized document and `web/browser/editor-history-acceptance.mjs` are intended deliverables. Other app/runtime/matrix/shared documents are outside this lane's ownership. Full editor acceptance must remain pending until a separately authorized UI-created writable fixture and multiple versions exist without paid dispatch or state injection.
