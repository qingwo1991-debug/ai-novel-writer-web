# Native Web management acceptance — bounded, no-cost subset

## Scope and isolation
- Actual browser/CDP run: 2026-11-02 00:36:32–00:46:18 UTC, under ten minutes.
- Target only: `http://ai-novel-native-test:6081`, isolated copied data and relay principal.
- Pinned GHCR digest: `sha256:84801a8a9016f115899c5be419cf2a29a1d112f82be3312fdc12609b0e95cf37`.
- Runtime image ID: `sha256:00010f8df6050c89d430eb35222bbc5610a4e8c45dbfb4bca9aa25273d06d977`.
- Production containers, official desktop, production data, and the editor lane project were not opened or changed.
- Only uniquely named `原生管理验收-...` projects were created; models-only checks used library settings.
- All mutations were rendered UI gestures, never direct domain API calls or store injection.
- Own CDP contexts closed; shared browser was never closed. Parent manages test gateway lifecycle.

## Results
| Area | Concrete observed result | Verdict |
|---|---|---|
| Temporary model creation | Saved synthetic custom model with non-real canary credential at inert origin | PASS subset |
| Credential boundary | Edit input blank/password-masked; configured/leave-blank placeholder; response `hasCredential=true`, no raw key | PASS |
| Blank-key update | Renamed only temporary model, saved with blank key, reloaded; configured state persisted | PASS |
| Temporary model deletion | Deleted only canary model through danger confirmation; reloaded original two-model baseline | PASS |
| Existing model preservation | Redacted original model-list fingerprints unchanged; parent independently confirmed canonical model files byte-identical, mode 0600 | PASS |
| Connection/discovery | No test-connection click; one automatic discovery attempt aborted before gateway | NOT TESTED |
| Prompt editor | Template expanded; driver `getByLabel('创作角色定位')` timed out before any edit/save | BLOCKED, not a product-bug finding |
| Prompt save/reload/reset/language scope | No mutation occurred; no acceptance evidence for these behaviors | NOT TESTED |
| Local Skill import | Uploaded synthetic prompt-only `SKILL.md`; compatibility/metadata preview enabled install | PASS |
| Skill install/binding | Installed, bound to chapter drafting in own project, reloaded with binding preserved | PASS |
| Skill cleanup | Explicitly unbound, uninstalled, reloaded: synthetic Skill absent and binding disabled | PASS |
| ZIP/full-body preview | UI accepts `.md,.markdown` or folder, not ZIP; candidate shows metadata/compatibility, not full prompt body | Unsupported / not claimed |

## Safety and evidence
- Initial gateway provider allowlist was empty. Parent later recreated only the isolated test gateway with allowlist `http://127.0.0.1:1` for model-save validation; no provider origin was allowed.
- Browser routing aborted external requests, execution/generation, MCP calls/connect, GitHub Skill operations, discovery and connection tests.
- Model save/delete routing additionally limited mutations to this run's temporary model names and captured temporary ID.
- Zero paid execution/provider requests forwarded; no GitHub import, package commands, real credential entry, commit, push, production restart, or code fix.
- Final content run: 107 API responses, zero HTTP errors/page errors, four private screenshots; two passed steps, one prompt-driver blocker.
- Final models run: 42 API responses, zero HTTP errors/page errors, one private screenshot; model CRUD subset passed, one route-blocked auto-discovery attempt.
- Private reports/screenshots remain ignored under `.native-test-data/browser-management/`:
  - `2026-11-02T00-45-41-345Z/` — supported Skill lifecycle and prompt locator blocker.
  - `2026-11-02T00-46-08-850Z/` — model CRUD/credential preservation subset.
  - Earlier attempt/cleanup reports retained; initial immediate binding assertion was a driver hydration race, not a confirmed runtime defect.
- Reports contain operation names/status/counts, never raw credentials or response bodies. Password screenshot regions are masked.

## Driver and limitations
- Driver: `web/browser/management-acceptance.mjs`; only additional tracked output is this document.
- Fixture derives from `src/shared/__tests__/writing-skills.test.ts` supported YAML frontmatter (`name`, `description`, `version`, `language`, `stage`) and prompt-only text. No invented ZIP manifest or disk fixture was needed.
- Exact historical model run: supplied Docker driver with `NOVEL_MANAGEMENT_MODE=models`, `NOVEL_MANAGEMENT_EXPECT_SAVE=1`, `NOVEL_MANAGEMENT_ENDPOINT=http://127.0.0.1:1`.
- Final driver hardening: exact isolated URL/inert-origin/private-artifact guards, nonzero exit for blocked steps, read-only default; unexecuted prompt write branch removed.
- Historical mixed content run exited zero before exit-code hardening; its JSON explicitly records the prompt blocker. Do not treat that exit code as all-management acceptance.
- No further browser rerun after parent stopped isolated services. Final syntax check only; prompt locator remains unverified and unfixed.
- **Conclusion:** real browser acceptance supports temporary model credential CRUD and local prompt-only Skill lifecycle, not discovery, prompt persistence/language scope, ZIP import, full body preview, or complete management coverage.
