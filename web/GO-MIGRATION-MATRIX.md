# Full Go backend migration matrix

## Audit boundary and status

Source baseline: `b69dc19c` (`fix(web): persist shelf visibility and fence concurrent draft commits`). Read-only audit of source; no production data, private files, credentials, runtime, CI or module manifests were changed. The only audit outputs are this document and `GO-MIGRATION-PLAN.md`. Existing React/CodeMirror stays; this is **not** a Go HTTP proxy over the Node workers. Prior Node acceptance records are reference tests, not Go parity evidence, and the latest Node fixes are not asserted production-deployed.

Go policy/security, project-runtime lease/event and HTTP foundations passed GitHub-only tests at `0350a13c` (52 top-level / 115 including subtests, plus race/build/smoke). Read-only physical project/SQLite v7 inspection passed GitHub-only qualification at `f24e145e` (81 top-level / 237 including subtests, zero skipped), while full author-data admission/migration remains **IN PROGRESS**, with its exact scope and remaining compatibility limits in [GO-DATA-ADMISSION.md](GO-DATA-ADMISSION.md). At `e7457541`, two fixed navigation intents are implemented and bound in the binary, with authenticated HTTP + caller-private SSE contract tests passing in [Actions 41378784128](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41378784128). **2/281 operation rows are IMPLEMENTED (HTTP/SSE tested); 54 are IN PROGRESS (isolated draft/core/review/revision/postprocess-ledger/LLM-history/narrative/exemption/plot-read-clear/ordinary-blueprint HTTP/repository tests, NOT bound in binary); 225 remain NOT STARTED.** The private-copy session + draft integration at `3a2bc966` passed [Actions 41381280660](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41381280660), but cannot count as durable author saving; see [GO-PROJECT-SESSIONS.md](GO-PROJECT-SESSIONS.md). These simple links do not imply project, AI or author-data business readiness. A registered name, compiling skeleton, security wrapper, HTTP 200, empty list or explicit unsupported response is not implementation. Update a row only with actual Go handler/service/repository code and passing contract/domain evidence; infrastructure progress does not promote operation status.

Round7 source `73244fa7` passed [Actions 41385531191](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41385531191): native OpenAI request/SSE/visible reasoning filtering (72 provider +922 scanner differential cases), exact-origin native egress, and Linux cross-process writer module. **No operation status is promoted by these modules**: providers remain unbound and workflows/source-target CAS/reservations/diagnostics/Gemini and production writer coordination are missing. Full suite183 top-level/1512 including subtests PASS; one foreign-owner lock test skipped due to nonroot runner, disclosed in GO-CI-ACCEPTANCE.md. See [GO-AI-TRANSPORT.md](GO-AI-TRANSPORT.md) and [GO-WRITER-LOCK.md](GO-WRITER-LOCK.md).

Round9 source `a5e38a20` passed [Actions 41391829983](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41391829983): **162 actual original review/revision repository differential cases**, source snapshot create, transactional pending replacement, exact source/target merge CAS and idempotency, private-session authenticated HTTP and exclusive content-pool mutation guard. Full suite **227 top-level/1996 including subtests PASS, 0 fail, 1 foreign-owner permission skip, 13 packages**, plus vet/race/build/noNode smoke. Fourteen operations below move only to IN PROGRESS: no durable author save, main binding or review-cycle owner parity. Bound cycle/recovery/generation requests fail closed, not skipped-success. See [GO-REVIEW-REVISION.md](GO-REVIEW-REVISION.md). Gemini now also qualified; older round7 missing lists describe that historical round, not current provider status.

Round10 source `64d044fd` passed [Actions 41395057332](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41395057332): **132 original postprocess/history repository cases**, finalized-source identity/hash and transactional step/aggregate, nullable token SUM/history finish/error redaction, authenticated private-copy HTTP. Full suite **244 top-level/2177 including subtests PASS,0 fail,1 foreign-owner skip,13 packages**, plus vet/race/build/noNode smoke. Nine operations move only to IN PROGRESS; ledger status does not prove external postprocess effect or billing/generation authority. See [GO-POST-HISTORY.md](GO-POST-HISTORY.md).

Round11 source `f49dd968` passed [Actions 41397459923](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41397459923): **160 original narrative/exemption repository differential cases**, JS trim/UTF16, frozen finalized-source evidence, consistent event status/relevance/dormancy, transactional confirmation, exemption upsert/revoke and authenticated private-copy HTTP. Full suite **258 top-level/2353 including subtests PASS,0 fail,1 foreign-owner skip,13 packages**, plus vet/race/build/noNode smoke. Nine operations move only to IN PROGRESS; no durable author save, AI confirmation or full consistency-preflight authority. See [GO-NARRATIVE.md](GO-NARRATIVE.md).

Round12 source `b0f1fb27` passed [Actions 41400535697](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41400535697): **60 original PlotTreeRepository +271 original shared validation cases (542 numeric-decoder replays)**, consistent source aggregation/ordered JSON.stringify SHA256, corrupt snapshot isolation/current-source CAS, clear and denied renderer save. Full suite **270 top-level/2986 including subtests PASS,0 fail,1 foreign-owner skip,13 packages** plus vet/race/build/noNode. Only read/clear move to IN PROGRESS; plot save remains NOT STARTED despite isolated transactional primitive because generation graph ownership is absent. InvalidUTF8/lone-surrogate and unenumerated legacy-Date.parse compatibility remain full-acceptance boundaries. See [GO-PLOT-TREE.md](GO-PLOT-TREE.md).

Round13 final strengthened-fixture source `81620db9` passed [Actions 41405028511](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41405028511) (previous `c001a386` also passed): **160 original BlueprintRepository cases**, ordinary CRUD/atomic range+dual ledgers/strict hash read-only replay/author notes+clock retention/pending backfill. Full suite **285 top-level/3165 including subtests PASS,0 fail,1 foreign-owner skip,13 packages** plus vet/race/build/noNode. Eight rows move only to IN PROGRESS; arbitrary serialized JSON insertion order is semantic-only, Unicode/punctuation canonical keys explicitly rejected, generated/recovery/completed proof authority remains absent; notes denial/complete/clear-all do not promote status. See [GO-BLUEPRINTS.md](GO-BLUEPRINTS.md).

## Validated inventory

`operations.json` contains **281 exposed operations: 217 project, 42 global, 22 flexible**, plus **13 excluded** operations. A static scan of literal `*.handle(...)` and generation `register(...)` registrations in `electron/controllers/*.ts` and `electron/mcp/mcp-ipc-bridge.ts` found **294 distinct names**: every exposed name has a source registration, and every other registered name is explicitly excluded. This validates source membership, not runtime authorization or semantic completeness. Worker overrides and nonregistry HTTP contracts are separate below.

| Area (prefix) | Project | Global | Flexible | Total |
|---|---:|---:|---:|---:|
| agent-generation | 8 | 0 | 0 | 8 |
| chapter | 4 | 0 | 0 | 4 |
| character-avatar | 4 | 0 | 0 | 4 |
| character-identity | 1 | 0 | 0 | 1 |
| character-proposal | 6 | 0 | 0 | 6 |
| cloud-backup | 3 | 3 | 2 | 8 |
| config | 0 | 2 | 0 | 2 |
| db | 103 | 0 | 0 | 103 |
| dialog | 0 | 0 | 9 | 9 |
| editor-inline | 4 | 0 | 0 | 4 |
| finalization | 2 | 0 | 0 | 2 |
| finalization-generation | 5 | 0 | 0 | 5 |
| finalized-character | 5 | 0 | 0 | 5 |
| fs | 7 | 0 | 3 | 10 |
| generation | 23 | 0 | 0 | 23 |
| graph-generation | 5 | 0 | 0 | 5 |
| import-generation | 2 | 0 | 0 | 2 |
| kb | 19 | 0 | 0 | 19 |
| legacy-roster | 7 | 0 | 0 | 7 |
| llm | 0 | 11 | 5 | 16 |
| mcp | 0 | 8 | 0 | 8 |
| model-provider-resource | 0 | 1 | 0 | 1 |
| official-homepage | 0 | 1 | 0 | 1 |
| project | 5 | 5 | 3 | 13 |
| prompt | 0 | 3 | 0 | 3 |
| review-revision | 4 | 0 | 0 | 4 |
| skills | 0 | 6 | 0 | 6 |
| startup | 0 | 2 | 0 | 2 |
| **Total** | **217** | **42** | **22** | **281** |

The 103 `db:*` operations subdivide into: close 1; project-core 4; import-global-facts 1; project-clear 1; import-run 18; blueprint 11; character 3; draft 16; continuity 4; consistency-exemption 3; narrative-thread 6; plot-tree 3; recovery-candidate 4; finalization-link 1; revision 9; review 7; post-process 6; LLM history 3; summary 2.

## Replacement/dependency key

Package labels below are **proposed Go boundaries**, not claims those packages exist. All rows inherit strict positional JSON arguments, existing result/error shape, server-owned caller identity, and source contract validation. P = project + current lease; G = serialized global authority; F = originating capability/run/lease authority, never “whatever project is open now.” Registry scope is routing, not authorization.

| Key | Source controller / principal services and repositories | Go replacement and non-negotiable contract |
|---|---|---|
| AG | generation-controller; agent-generation, agent-domain-tool-effect | generation/agent: durable root/round/claim/finish state, idempotent domain effects and child workflow references; tool result is not write authority |
| CH | chapter-lifecycle-controller; chapter-deletion-service/repository | chapter: resumable deletion outbox; delete publication/knowledge/continuity consistently; ambiguous legacy KB absence needs explicit confirmation |
| AV | character-avatar-controller; character-asset-service/repository, avatar-image | assets: caller-bound image choice, content/type/size validation, atomic assets + DB receipt, orphan recovery; compression optional |
| CP | generation-controller; character-proposal-service, character-roster repositories, *generation-proof | characters: immutable artifact/source proof, stable identities, author approval and candidate decisions in transaction |
| CL | cloud-backup-controller; webdav-backup-service, cloud credential/binding/metadata-lock stores | cloud: bounded DAV HTTP/XML, append-only generations and completion marker, hash/size validation, cancellation, restore-copy, global serialized metadata writes |
| CFG | config-controller; config-utils, app-data-locator | settings: preserve committed global generation, atomic validated JSON updates, corrupt-file refusal, allowed fields and admin-owned proxy |
| DB | db-controller; database, project-data-locator, migrations | projectdb: authorized captured DB handle; close releases tab rather than closing another owner's DB; schema admission precedes writable access |
| CORE | project-core/import-global-facts/project-clear repositories | projectdb/core: source CAS and generated proof; transactional selective clear without destroying retained author data |
| IMP | import-controller; import-run/source-identity/inspection/effect repositories, import-generation | import: bounded TXT/MD/EPUB inspection, purpose-bound HMAC source identity, preview CAS, execution epoch/expiry, stage transitions and idempotent effect ledger |
| BP | blueprint-repository; generation source/range author-edit receipts | planning: atomic ranges and character-sync receipts; generated commits prove current source; finalization notes are admission-only |
| ROSTER | character/character-roster repositories, character-roster-author | characters: preserve roster revisions, identities, author scope, current-state projections and source ownership |
| DR | draft/content repositories, draft-source-guard, finalized-draft-import | drafts: contents pool + metadata transaction, version allocation, lineage/staleness and authority/export snapshots; body/status CAS, no blind overwrite |
| CONT | summary/current-character projection, consistency-exemption, narrative-thread, plot-tree repositories | continuity: source receipts, exemptions and thread events; derived writes remain gated by proven generation/finalization |
| REC | recovery-candidate-repository | recovery: preserve unsaved text and durable dispositions, bind records to active project; never silently replace working draft |
| REV | revision/review/review-cycle repositories | review: source snapshot CAS, atomic pending replacement/merge, merged hash/recheck obligations and retry idempotency |
| POST | post-process-repository, finalization-generation | postprocess: step lifecycle and failures tied to exact finalized source; do not manufacture passed state |
| HIST | llm/summary repositories | history: preserve ordering, nullable token usage and source distinctions; diagnostic history is not generation proof |
| DLG | project/fs/archive/import/kb controllers, external-file-grant-service, server dialog protocol | transfer: browser uploads/selection and private staging; grants bind caller, purpose, expiry, root and origin; no arbitrary server folder picker |
| INLINE | generation-controller; editor-inline-generation | generation/inline: frozen selection/source, immutable artifact, recovery and cancellation; UI applies with matching editor/draft revision |
| FIN | finalization-controller; finalization-service/repository, manuscript-publisher | finalization: SQLite is authority; outbox then atomic file projection; committed=true can coexist with publication failure; retry takes receipt ID only |
| FG | generation-controller; finalization-generation-source/service, *proof | generation/finalization: source-bound steps and immutable artifact proof, transactional admission, resume/retry without duplicate effect |
| FS | fs-controller, external-file-grant-controller, safe filesystem adapters | safefs: containment AND no-link identity checks, per-file mutex/CAS, lease recheck inside lock, atomic publication and commitState; deny traversal |
| GEN | generation-controller; main-generation-owner/plan, generation-run-service/repository, source-binding, knowledge-source gate | generation: durable roots/budgets/attempts/artifacts, nine-part fingerprint, material decision, source recheck and exact composition receipts; no provider retry after unknown outcome |
| GRAPH | generation-controller; graph-generation/source | generation/graph: source hash + artifact confirmation; graph-save shortcut remains rejected |
| KB | kb-controller; knowledge-base, vector-store, embedding, generation-knowledge-source | knowledge: retain Lance canonical documents/chunks, copies, vector spaces, receipts and search semantics until real non-Node adapter qualifies; no empty substitute |
| LEG | generation-controller; legacy-roster-source/context/proof/generation | generation/legacy: qualify legacy source and adoption, generation evidence and explicit staging; preserve historical read-only authority |
| LLM | llm-controller; llm providers/factory, model-execution-lease, discovery, endpoint-fetch | providers: OpenAI-compatible + native Gemini, generation/discovery/embedding protocols, reasoning/visible filtering, finish/usage semantics, caller-owned streaming and model lease; actual-dispatch egress check |
| MCP | mcp-ipc-bridge/manager | integrations/mcp: JSON-RPC stdio + legacy SSE lifecycle, bounded buffers/pending/tool catalog, admin config and effects policy; external user tools separately disclosed |
| LINK | official-homepage/model-provider-resource controllers | links: fixed validated browser-navigation event, never shell/open arbitrary URL |
| PROJ | project-controller, project/archive/restore/peek services, global-data migration, library-visibility | library: canonical manifest + SQLite migration admission, stable project ID/session, recent/hidden distinction, safe create/open/delete and overview capability |
| ARCH | project-archive-controller; portable-project-archive/format/assets, project-archive/restore services | archive: streamed ZIP/ZIP64, exact manifest/field policy, integrity limits, frozen portable authority, new-copy restore and no-overwrite publication |
| PROMPT | app-data-controller; config-utils, builtin-prompt templates | prompts: validated keys/language, persisted user override and load receipt hashes matching generation binding |
| SKILL | app-data-controller; writing-skill-package, shared writing-skills | skills: inspect-before-install receipts/expiry/caller, GitHub source rules, bounded UTF-8 bundles and owned safe paths; declarative prompt assets, not executed plugins |
| START | startup-controller and worker overrides | startup: committed global generation, ready/blocked projection and appearance revision acknowledgement; no guessed success |

## Per-operation Go backlog

Each row's key expands to the concrete source/dependency/replacement decision above. The operation name specifies the individual action; listed rejection gates are intentional behavior, not missing CRUD features.

| Operation | Scope | Replacement key / special gate | Go status |
|---|:---:|---|---|
| agent-generation:begin | P | AG | NOT STARTED |
| agent-generation:read | P | AG | NOT STARTED |
| agent-generation:resume | P | AG | NOT STARTED |
| agent-generation:round | P | AG | NOT STARTED |
| agent-generation:claim-tool | P | AG | NOT STARTED |
| agent-generation:finish-tool | P | AG | NOT STARTED |
| agent-generation:register-workflow | P | AG | NOT STARTED |
| agent-generation:commit-domain-tool | P | AG; atomic proven effect | NOT STARTED |
| character-avatar:choose | P | AV | NOT STARTED |
| character-avatar:commit | P | AV | NOT STARTED |
| character-avatar:read-batch | P | AV | NOT STARTED |
| character-avatar:remove | P | AV | NOT STARTED |
| character-identity:read | P | CP | NOT STARTED |
| character-proposal:stage | P | CP; legacy admission separate | NOT STARTED |
| character-proposal:read | P | CP | NOT STARTED |
| character-proposal:list-pending-finalized | P | CP | NOT STARTED |
| character-proposal:read-pending-finalized | P | CP | NOT STARTED |
| character-proposal:approve | P | CP | NOT STARTED |
| character-proposal:cancel | P | CP | NOT STARTED |
| chapter:delete-finalized | P | CH | NOT STARTED |
| chapter:retry-deletion | P | CH | NOT STARTED |
| chapter:confirm-legacy-knowledge-absent | P | CH | NOT STARTED |
| chapter:list-incomplete-deletions | P | CH | NOT STARTED |
| cloud-backup:view | P | CL | NOT STARTED |
| cloud-backup:connect | G | CL; credential/endpoint gate | NOT STARTED |
| cloud-backup:confirm-binding | P | CL; global single writer | NOT STARTED |
| cloud-backup:backup | P | CL; global single writer | NOT STARTED |
| cloud-backup:list | G | CL | NOT STARTED |
| cloud-backup:restore-copy | F | CL/ARCH; origin-owned operation | NOT STARTED |
| cloud-backup:cancel | F | CL; operation owner | NOT STARTED |
| cloud-backup:clear-credential | G | CL | NOT STARTED |
| config:get | G | CFG | NOT STARTED |
| config:set | G | CFG | NOT STARTED |
| db:close | P | DB; release tab, retain live project | NOT STARTED |
| db:project-core-get | P | CORE; raw nullable/Buffer wire and original normalizers | IN PROGRESS — isolated HTTP+original TS oracle PASS 6d5f2f71; not binary bound |
| db:project-core-update | P | CORE; original allowlist, charactersArch read-only | IN PROGRESS — isolated HTTP+original TS oracle PASS 6d5f2f71; not binary bound |
| db:project-core-commit-generated | P | CORE/GEN; same transaction proof | NOT STARTED |
| db:project-core-synopsis-commit | P | CORE; 11-field plain source/target CAS only; generated/recovery rejected | IN PROGRESS — isolated HTTP+original TS oracle PASS 6d5f2f71; missing generated/recovery owner |
| db:import-global-facts-commit | P | CORE/IMP | NOT STARTED |
| db:project-clear-generated-data | P | CORE | NOT STARTED |
| db:import-run-prepare-inspection | P | IMP | NOT STARTED |
| db:import-run-author-preview | P | IMP | NOT STARTED |
| db:import-run-finalize-parsing | P | IMP | NOT STARTED |
| db:import-run-get | P | IMP | NOT STARTED |
| db:import-run-list-resumable | P | IMP | NOT STARTED |
| db:import-run-list-chapters | P | IMP | NOT STARTED |
| db:import-run-effect-receipt-get | P | IMP | NOT STARTED |
| db:import-run-effect-receipt-prepare | P | IMP | NOT STARTED |
| db:import-run-effect-receipt-commit | P | IMP; domain effect in transaction | NOT STARTED |
| db:import-run-start-resume | P | IMP | NOT STARTED |
| db:import-run-renew-execution | P | IMP | NOT STARTED |
| db:import-run-restart | P | IMP | NOT STARTED |
| db:import-run-request-cancel | P | IMP | NOT STARTED |
| db:import-run-cancel-at-boundary | P | IMP | NOT STARTED |
| db:import-run-complete-batch | P | IMP; stage checkpoint gate | NOT STARTED |
| db:import-run-advance-stage | P | IMP | NOT STARTED |
| db:import-run-fail | P | IMP | NOT STARTED |
| db:import-run-complete | P | IMP; evidence not counters alone | NOT STARTED |
| db:blueprint-get-all | P | BP; nullable/raw projection and permissive JSON characters | IN PROGRESS — isolated repository/HTTP/oracle; main/durable missing |
| db:blueprint-get | P | BP; native binding parity/bool guard | IN PROGRESS — isolated repository/HTTP/oracle; main/durable missing |
| db:blueprint-upsert | P | BP; original overwrite, NOT AI CAS authority | IN PROGRESS — isolated repository/HTTP/oracle; JSON property-order boundary |
| db:blueprint-upsert-many | P | BP; atomic batch rollback | IN PROGRESS — isolated repository/HTTP/oracle; JSON property-order boundary |
| db:blueprint-commit-range | P | BP/GEN; atomic range+dual-ledger, strict hash replay and author-group retention | IN PROGRESS — ordinary commit only; generation/recovery owner denied, canonical key domain limited |
| db:blueprint-character-sync-list-pending | P | BP/CP; lazy legacy-ledger backfill and pending ordering | IN PROGRESS — isolated repository/HTTP/oracle; actual character sync missing |
| db:blueprint-character-sync-get | P | BP/CP; pending proof projection, completed owner failclosed | IN PROGRESS — isolated repository/HTTP/oracle; completed authority missing |
| db:blueprint-character-sync-complete | P | BP/CP | NOT STARTED |
| db:blueprint-update-notes | P | BP; reject direct finalization bypass | NOT STARTED |
| db:blueprint-delete | P | BP; native identity binding and atomic statement | IN PROGRESS — isolated repository/HTTP/oracle; recovery/ledger retention unchanged |
| db:blueprint-clear-all | P | BP | NOT STARTED |
| db:character-get-all | P | ROSTER | NOT STARTED |
| db:character-roster-read | P | ROSTER | NOT STARTED |
| db:character-roster-commit | P | ROSTER; author scope | NOT STARTED |
| db:draft-import-finalized-batch | P | DR/IMP; source receipt | NOT STARTED |
| db:draft-create | P | DR | NOT STARTED |
| db:draft-list | P | DR; transport/drafts.go + projectsession/http_test.go | IN PROGRESS — isolated private-copy HTTP PASS; no durable project open/binary binding |
| db:draft-list-all | P | DR; transport/drafts.go + projectsession/http_test.go | IN PROGRESS — isolated private-copy HTTP PASS; no durable project open/binary binding |
| db:draft-get-meta | P | DR; transport/drafts.go + transport/drafts_test.go | IN PROGRESS — isolated HTTP PASS; no durable project open/binary binding |
| db:draft-get-full | P | DR; transport/drafts.go + projectsession/http_test.go | IN PROGRESS — isolated private-copy HTTP PASS; no durable project open/binary binding |
| db:draft-get-latest | P | DR; original metadata query, version-only order | IN PROGRESS — isolated HTTP+store PASS 30373e51; not binary bound |
| db:draft-get-finalized | P | DR; exact finalized status, original metadata query | IN PROGRESS — isolated HTTP+store PASS 30373e51; not binary bound |
| db:draft-get-max-finalized-chapter | P | DR; scalar MAX with original null fallback | IN PROGRESS — isolated HTTP+store PASS 30373e51; not binary bound |
| db:draft-authority-sequence | P | DR | NOT STARTED |
| db:draft-export-snapshot | P | DR | NOT STARTED |
| db:draft-export-authority-current | P | DR; revalidate receipt | NOT STARTED |
| db:continuity-save-finalized | P | CONT; reject direct admission bypass | NOT STARTED |
| db:continuity-save-character-state-candidates | P | CONT; reject direct admission bypass | NOT STARTED |
| db:continuity-list-before | P | CONT | NOT STARTED |
| db:continuity-read-source | P | CONT | NOT STARTED |
| db:consistency-exemption-list | P | CONT; raw keys/reasons + exact flag, SQLite ordering | IN PROGRESS — isolated repository/HTTP/oracle; main unbound |
| db:consistency-exemption-save | P | CONT; JS trim/UTF16, upsert reactivates | IN PROGRESS — isolated repository/HTTP/oracle; actual preflight missing |
| db:consistency-exemption-revoke | P | CONT; stable key retained, missing no-op | IN PROGRESS — isolated repository/HTTP/oracle; actual preflight missing |
| db:narrative-thread-list | P | CONT; snapshot-consistent chapter/event status+dormancy | IN PROGRESS — isolated repository/HTTP/oracle; durable project missing |
| db:narrative-thread-list-relevant | P | CONT; terminal excluded, inclusive window/substring, legacy BLOB read matching | IN PROGRESS — isolated repository/HTTP/oracle; not AI context workflow |
| db:narrative-thread-plan-create | P | CONT; JS UTF16/trim range, atomic readback | IN PROGRESS — isolated repository/HTTP/oracle; main unbound |
| db:narrative-thread-plan-update | P | CONT; exact row mutation+readback, no invented CAS | IN PROGRESS — isolated repository/HTTP/oracle; main unbound |
| db:narrative-thread-plan-delete | P | CONT; FK cascade/abort atomicity | IN PROGRESS — isolated repository/HTTP/oracle; main unbound |
| db:narrative-thread-event-confirm | P | CONT; frozen outbox evidence/current finalized, source+plan+insert atomic | IN PROGRESS — isolated repository/HTTP/oracle; full finalization owner missing |
| db:plot-tree-read | P | CONT; consistent sources + exact ordered hash; historical snapshot validation | IN PROGRESS — isolated repository/HTTP/oracle; encoding/date compatibility boundaries remain |
| db:plot-tree-save | P | GRAPH; original public gate retained, isolated Save prototype CAS is NOT generation owner | NOT STARTED |
| db:plot-tree-clear | P | CONT; explicit clear only, source hash unchanged | IN PROGRESS — isolated repository/HTTP/oracle; main/durable save missing |
| db:draft-next-version | P | DR; preview, not allocation authority | IN PROGRESS — isolated HTTP+store PASS 30373e51; not binary bound |
| db:draft-update-status | P | DR; finalized transitions guarded | NOT STARTED |
| db:draft-update-content | P | DR; expectedContent CAS; transport/drafts.go + projectsession/http_test.go | IN PROGRESS — real SQLite private-copy HTTP concurrent CAS PASS; not durable author save/binary bound |
| db:draft-delete | P | DR; finalized deletion via CH | NOT STARTED |
| db:recovery-candidate-record | P | REC | NOT STARTED |
| db:recovery-candidate-list | P | REC | NOT STARTED |
| db:recovery-candidate-update | P | REC | NOT STARTED |
| db:recovery-candidate-resolve | P | REC | NOT STARTED |
| db:finalization-link-knowledge-document | P | FIN/KB; verify linkage | NOT STARTED |
| db:revision-create | P | REV; exact expectedSource, transaction MAX allocation | IN PROGRESS — isolated HTTP+original oracle PASS a5e38a20; cycle owner missing |
| db:revision-replace-pending | P | REV; insert/discard atomic rollback | IN PROGRESS — isolated HTTP+original oracle PASS a5e38a20; bound cycles rejected |
| db:revision-list | P | REV; nullable raw metadata | IN PROGRESS — isolated HTTP+original oracle PASS a5e38a20; not binary bound |
| db:revision-get-pending | P | REV; exact pending filter/index-only ordering | IN PROGRESS — isolated HTTP+original oracle PASS a5e38a20; not binary bound |
| db:revision-get-full | P | REV; frozen source + content pool, Buffer/REAL | IN PROGRESS — isolated HTTP+original oracle PASS a5e38a20; not binary bound |
| db:revision-next-index | P | REV; preview not allocation authority | IN PROGRESS — isolated HTTP+original oracle PASS a5e38a20; not binary bound |
| db:revision-merge | P | REV; source+target full body CAS/idempotency/content exclusivity | IN PROGRESS — isolated HTTP+original oracle PASS a5e38a20; cycle/recheck receipt missing |
| db:revision-mark-merged | P | REV; legacy metadata-only unbound behavior, bound merge required | IN PROGRESS — isolated HTTP+original oracle PASS a5e38a20; final workflow review pending |
| db:revision-mark-discarded | P | REV; pending only, bound cycles failclosed | IN PROGRESS — isolated HTTP+original oracle PASS a5e38a20; cycle reconciliation missing |
| db:review-create | P | REV; exact source snapshot/content+metadata atomicity | IN PROGRESS — isolated HTTP+original oracle PASS a5e38a20; reviewCycle confirmation rejected |
| db:review-list | P | REV; metadata only/index ASC | IN PROGRESS — isolated HTTP+original oracle PASS a5e38a20; not binary bound |
| db:review-get-latest | P | REV; frozen source/content/index DESC | IN PROGRESS — isolated HTTP+original oracle PASS a5e38a20; not binary bound |
| db:review-get-full | P | REV; source nullable/Buffer body | IN PROGRESS — isolated HTTP+original oracle PASS a5e38a20; not binary bound |
| db:review-cycle-get | P | REV | NOT STARTED |
| db:review-cycle-list-pending-rechecks | P | REV | NOT STARTED |
| db:review-next-index | P | REV; preview not allocation authority | IN PROGRESS — isolated HTTP+original oracle PASS a5e38a20; not binary bound |
| db:post-process-create-run | P | POST; latest finalized identity+hash, atomic run/steps, nullable labels | IN PROGRESS — isolated ledger HTTP/oracle, not executor or binary bound |
| db:post-process-get-latest-run | P | POST; exact outbox lookup and timestamp/rowid tie | IN PROGRESS — isolated ledger HTTP/oracle, durable workflow missing |
| db:post-process-get-steps | P | POST; nullish defaults/raw flags/index order | IN PROGRESS — isolated ledger HTTP/oracle, not external side-effect proof |
| db:post-process-mark-step-ok | P | POST; step+aggregate atomic, changes exactly1 | IN PROGRESS — isolated ledger HTTP/oracle, runner/receipts authority missing |
| db:post-process-mark-step-failed | P | POST; attempt+failure+summary atomic rollback | IN PROGRESS — isolated ledger HTTP/oracle, runner/recovery missing |
| db:post-process-is-all-passed | P | POST; latest stored aggregate, no manufactured execution | IN PROGRESS — isolated ledger HTTP/oracle, executor missing |
| db:log-llm-call | P | HIST; nullable usage/error default, not billing authority | IN PROGRESS — isolated HTTP/oracle, model workflow integration missing |
| db:get-llm-stats | P | HIST; unknown SUM null vs known zero | IN PROGRESS — isolated HTTP/oracle, not binary bound |
| db:get-llm-history | P | HIST; raw success, finish CASE, no raw error_message | IN PROGRESS — isolated HTTP/oracle, not binary bound |
| db:save-summary-snapshot | P | HIST; legacy vs formal continuity | NOT STARTED |
| db:get-latest-summary | P | HIST | NOT STARTED |
| dialog:select-legacy-project | F | DLG/PROJ | NOT STARTED |
| dialog:select-folder | F | DLG; global library chooser override | NOT STARTED |
| dialog:select-export-directory | F | DLG | NOT STARTED |
| dialog:select-project-archive-export | F | DLG/ARCH | NOT STARTED |
| dialog:select-project-archive | F | DLG/ARCH | NOT STARTED |
| dialog:select-project-restore-target | F | DLG/ARCH | NOT STARTED |
| dialog:select-knowledge-files | F | DLG/KB | NOT STARTED |
| dialog:select-knowledge-folder | F | DLG/KB | NOT STARTED |
| dialog:select-novel-files | F | DLG/IMP; returns inspection not path authority | NOT STARTED |
| editor-inline:begin | P | INLINE | NOT STARTED |
| editor-inline:read-recovery | P | INLINE | NOT STARTED |
| editor-inline:execute | P | INLINE | NOT STARTED |
| editor-inline:cancel | P | INLINE | NOT STARTED |
| finalization:commit | P | FIN; expectedContent required | NOT STARTED |
| finalization:retry | P | FIN; persisted snapshot only | NOT STARTED |
| finalization-generation:read | P | FG | NOT STARTED |
| finalization-generation:begin | P | FG | NOT STARTED |
| finalization-generation:execute | P | FG | NOT STARTED |
| finalization-generation:commit | P | FG; artifact-derived values only | NOT STARTED |
| finalization-generation:cancel | P | FG | NOT STARTED |
| finalized-character:read-context | P | CP/FG | NOT STARTED |
| finalized-character:commit | P | CP/FG | NOT STARTED |
| finalized-character:list-state-candidates | P | CP | NOT STARTED |
| finalized-character:read-state-candidate | P | CP | NOT STARTED |
| finalized-character:decide-state-candidate | P | CP | NOT STARTED |
| fs:read-file | P | FS | NOT STARTED |
| fs:write-file | P | FS; expected body and commitState | NOT STARTED |
| fs:list-dir | P | FS | NOT STARTED |
| fs:mkdir | P | FS | NOT STARTED |
| fs:check-exists | P | FS | NOT STARTED |
| fs:read-json | P | FS | NOT STARTED |
| fs:write-json | P | FS | NOT STARTED |
| fs:grant-read-file | F | FS/DLG | NOT STARTED |
| fs:grant-write-file | F | FS/DLG | NOT STARTED |
| fs:grant-mkdir | F | FS/DLG | NOT STARTED |
| generation:prepare-draft-context | P | GEN/KB | NOT STARTED |
| generation:commit-draft | P | GEN/DR; proof + source CAS | NOT STARTED |
| generation:read-context | P | GEN | NOT STARTED |
| generation:retry-draft-short-outline | P | GEN; separate proven attempt | NOT STARTED |
| generation:begin-batch | P | GEN | NOT STARTED |
| generation:read-batch | P | GEN | NOT STARTED |
| generation:list-batches | P | GEN | NOT STARTED |
| generation:list-recovery | P | GEN | NOT STARTED |
| generation:find-blueprint-recovery | P | GEN | NOT STARTED |
| generation:confirm-batch-finalization | P | GEN/FIN | NOT STARTED |
| generation:list-directory-progress | P | GEN | NOT STARTED |
| generation:compose-visible | P | GEN; exact IDs/hash/algorithm arity | NOT STARTED |
| generation:read-visible-composition | P | GEN | NOT STARTED |
| generation:begin | P | GEN; preparation/material decision | NOT STARTED |
| generation:bind-material-decision | P | GEN | NOT STARTED |
| generation:execute | P | GEN; reserve before dispatch | NOT STARTED |
| generation:read | P | GEN | NOT STARTED |
| generation:list | P | GEN | NOT STARTED |
| generation:cancel | P | GEN | NOT STARTED |
| generation:pause | P | GEN | NOT STARTED |
| generation:resume | P | GEN; source/KB recheck | NOT STARTED |
| generation:restart | P | GEN; new binding, retained history | NOT STARTED |
| generation:discard-candidate | P | GEN | NOT STARTED |
| graph-generation:begin | P | GRAPH | NOT STARTED |
| graph-generation:read | P | GRAPH | NOT STARTED |
| graph-generation:execute | P | GRAPH | NOT STARTED |
| graph-generation:confirm | P | GRAPH | NOT STARTED |
| graph-generation:cancel | P | GRAPH | NOT STARTED |
| import-generation:read | P | IMP/GEN | NOT STARTED |
| import-generation:execute | P | IMP/GEN | NOT STARTED |
| kb:import-document | P | KB/DLG | NOT STARTED |
| kb:import-folder | P | KB/DLG | NOT STARTED |
| kb:import-text | P | KB | NOT STARTED |
| kb:import-finalized-text | P | KB/FIN; receipt + content proof | NOT STARTED |
| kb:import-planning-text | P | KB/GEN; generated source proof | NOT STARTED |
| kb:import-reference-text | P | KB/IMP; idempotent source receipt | NOT STARTED |
| kb:search | P | KB | NOT STARTED |
| kb:search-writing-context | P | KB | NOT STARTED |
| kb:search-with-scope | P | KB | NOT STARTED |
| kb:list-documents | P | KB | NOT STARTED |
| kb:read-document-copy | P | KB | NOT STARTED |
| kb:save-document-copy | P | KB; mark index dirty | NOT STARTED |
| kb:reindex-document-copy | P | KB; source currentness | NOT STARTED |
| kb:remove-document | P | KB; receipts + vector rows | NOT STARTED |
| kb:clear-all | P | KB; explicit destructive authority | NOT STARTED |
| kb:stats | P | KB | NOT STARTED |
| kb:get-vectorless-count | P | KB | NOT STARTED |
| kb:get-vector-rebuild-status | P | KB | NOT STARTED |
| kb:backfill-vectors | P | KB; isolated generation then activation | NOT STARTED |
| legacy-roster:read-source | P | LEG | NOT STARTED |
| legacy-roster:adopt-existing | P | LEG | NOT STARTED |
| legacy-roster:begin | P | LEG | NOT STARTED |
| legacy-roster:read | P | LEG | NOT STARTED |
| legacy-roster:execute | P | LEG | NOT STARTED |
| legacy-roster:stage | P | LEG; explicit admission proof | NOT STARTED |
| legacy-roster:cancel | P | LEG | NOT STARTED |
| llm:begin-execution-lease | F | LLM; origin ownership | NOT STARTED |
| llm:close-execution-lease | F | LLM; tombstone idempotency | NOT STARTED |
| llm:generate | F | LLM | NOT STARTED |
| llm:generate-stream | F | LLM; request ID + origin owner | NOT STARTED |
| llm:cancel | F | LLM; stream owner | NOT STARTED |
| llm:list-models | G | LLM; sanitized projection | NOT STARTED |
| llm:discover-models | G | LLM; saved/unsaved credential gate | NOT STARTED |
| llm:save-model | G | LLM; blank key reuse only same endpoint | NOT STARTED |
| llm:delete-model | G | LLM; routing/default references | NOT STARTED |
| llm:get-stage-models | G | LLM | NOT STARTED |
| llm:set-stage-model | G | LLM | NOT STARTED |
| llm:set-default-model | G | LLM | NOT STARTED |
| llm:get-default-model | G | LLM | NOT STARTED |
| llm:set-default-embedding-model | G | LLM | NOT STARTED |
| llm:get-default-embedding-model | G | LLM | NOT STARTED |
| llm:test-connection | G | LLM; real protocol/endpoint test | NOT STARTED |
| mcp:load-config | G | MCP; administrator | NOT STARTED |
| mcp:connect | G | MCP; administrator | NOT STARTED |
| mcp:disconnect | G | MCP; administrator | NOT STARTED |
| mcp:disconnect-all | G | MCP; administrator | NOT STARTED |
| mcp:list-tools | G | MCP; sanitized catalog | NOT STARTED |
| mcp:list-resources | G | MCP; current gateway admin restriction | NOT STARTED |
| mcp:call-tool | G | MCP; admin baseline + tool/effect policy | NOT STARTED |
| mcp:get-servers-status | G | MCP; sanitized projection | NOT STARTED |
| model-provider-resource:open | G | LINK; internal/transport/navigation.go + navigation_test.go | IMPLEMENTED — fixed IDs, authenticated RPC, caller-private SSE; e7457541 remote PASS |
| official-homepage:open | G | LINK; internal/transport/navigation.go + navigation_test.go | IMPLEMENTED — fixed URL, authenticated RPC, caller-private SSE; e7457541 remote PASS |
| project:import-legacy-copy | F | PROJ/IMP; source untouched | NOT STARTED |
| project:get-runtime-context | F | PROJ; truthful no-project state | NOT STARTED |
| project:create | G | PROJ; granted controlled library | NOT STARTED |
| project:open | G | PROJ; obtains project authority | NOT STARTED |
| project:save | P | PROJ; current-source semantics | NOT STARTED |
| project:update-config | P | PROJ/CORE | NOT STARTED |
| project:recent-list | G | PROJ; visible library override | NOT STARTED |
| project:peek-overview | G | PROJ; opaque peek capability | NOT STARTED |
| project:overview-current | P | PROJ | NOT STARTED |
| project:recent-remove | G | PROJ; persistent hide, not delete | NOT STARTED |
| project:archive-export | P | ARCH | NOT STARTED |
| project:archive-restore | F | ARCH; originating grant | NOT STARTED |
| project:delete | P | PROJ/CH; unique current owned project | NOT STARTED |
| prompt:load-global | G | PROMPT | NOT STARTED |
| prompt:save-global | G | PROMPT | NOT STARTED |
| prompt:delete-global | G | PROMPT | NOT STARTED |
| review-revision:prepare | P | REV/GEN | NOT STARTED |
| review-revision:commit-review | P | REV/GEN; immutable artifact proof | NOT STARTED |
| review-revision:commit-revision | P | REV/GEN; immutable artifact proof | NOT STARTED |
| review-revision:read-recovery | P | REV/GEN | NOT STARTED |
| skills:list-user | G | SKILL | NOT STARTED |
| skills:inspect-github | G | SKILL | NOT STARTED |
| skills:install-github | G | SKILL; original inspection authority | NOT STARTED |
| skills:pick-local | G | SKILL/DLG | NOT STARTED |
| skills:install-local | G | SKILL; original inspection authority | NOT STARTED |
| skills:uninstall-user | G | SKILL | NOT STARTED |
| startup:get-state | G | START | NOT STARTED |
| startup:appearance-ack | G | START | NOT STARTED |

## Contracts beyond the 281-operation count

All below are **NOT STARTED** for Go; do not inflate the 281 count with them.

| Surface | Existing contract that must survive |
|---|---|
| `GET /healthz` | Explicit runtime health; Go must report Go, not copy `runtime: node`; readiness must not imply business parity |
| `GET /api/bootstrap` | `{csrfToken, runtime: 'web', generation}`; HttpOnly `novel_session`, same-origin client and authenticated principal |
| `POST /api/operations/<encoded-name>` | `{args: [...]}` → `{result: ...}`; unknown operation denied; transport failures `{error:{code,message}}`; retain domain `{success:false,...}` results rather than flattening every error |
| `GET /api/events?client=...` | SSE `id`, JSON `{channel,args}`, heartbeat, replay using `Last-Event-ID`, slow-reader bound; bootstrap waits for SSE open |
| `GET /api/library` | Includes hidden entries for chooser; `project:recent-list` excludes them; peek capability is not path authority |
| `POST /api/upload-folders`, `POST /api/uploads` | JSON folder request; raw file bytes with X-Upload-Name/Folder/Relative; caller ownership, safe relative paths, per-file/session/folder quotas |
| `POST /api/dialogs/<id>` | Resolve caller's pending dialog with browser upload IDs/selected controlled project; ten-minute expiry and initiating request binding |
| `browser:prepare-download` | Special gateway operation outside registry; caller-owned export grant + relative path → download-ready event |
| `GET /api/downloads/<token>` | Session-owned expiring token, attachment filename, directory ZIP support, no-store; preserve current session-level download authority deliberately |
| `POST /api/release` | Best-effort departure hint, not forced task cancel; same-tab refresh and in-flight work retain owner |
| Static `GET`/`HEAD` | Existing built `dist-web`, MIME, CSP/security headers, no accidental arbitrary file access; no replacement frontend |
| Events | Preserve actual shared event payloads: llm stream chunk/reasoning/done/error; generation snapshot/reasoning; cloud/import/KB progress; MCP status/tools; browser dialog/download-ready/open-external/project-lost |
| Internal worker helpers | `web:library-list`, `web:library-show-project`, `web:resolve-project-grant` stay internal, never prefix-exposed |

Excluded names (13) stay denied/replaced intentionally: `window:minimize`, `window:toggle-maximize`, `window:close`, `window:resolve-close`; `update:get-state`, `update:check`, `update:download`, `update:open-release`, `update:defer-reminder`, `update:quit-and-install`; `project:smoke-open-request`, `project:smoke-open-confirm`; `mcp:get-config-path`. Browser reconnect currently requests excluded `update:get-state` if that event has subscribers: resolve through an explicit deployment-capability UI/contract decision, not accidental updater exposure.

## Immediate mismatch/risk register

1. **Go project structs must match browser fields**, including project `id/path/sessionLease`, session `{projectId,leaseId,projectPath}`, returned `requestToken`, `novelConfig.narrativePOV` vs repository `narrativePov`, camelCase DTOs, null/omitted distinctions and array ordering. A path or UI-provided epoch is not authorization. `db:*`/`kb:*` append a session; generation handlers have strict arity and pop a mandatory session.
2. **Shared code is part of backend behavior.** TS types in `src/shared` import repository types; parsers, built-in prompts/skills, token budgets, visible composition, draft units, and hashing execute outside controllers. Go needs equivalent logic and packaged assets, not an import of Node services or a list of names.
3. **Schema and hash byte compatibility matter.** Schema fingerprint is SHA-256 of JS `JSON.stringify` rows from sqlite_master ordered by type/name, including exact DDL. Go map iteration, HTML escaping, integer/float formatting, UTF-16 length vs Unicode rune counts and sorting can change identities/receipts. Use explicit structures/order and cross-language fixture vectors; do not “normalize” historical signed evidence.
4. **LanceDB is authoritative persisted data.** `documents`, canonical `chunks`, `chunks__space_N`, `embedding-spaces.json`, `knowledge-copies`, source receipts and legacy vectors/journal must survive. Arrow IPC snapshots in `vector-migration-snapshot.ts` are a model for qualification, not a presently implemented Go reader or an excuse for permanent Node conversion.
5. **Outbox and proof are not CRUD.** Expected body checks, generated artifact/source proof and formal effect receipt must be in the same SQLite transaction. Finalization publication and cross-store KB writes need recoverable stages, not a false cross-filesystem transaction claim.
6. **Credentials are not all stored alike.** Model profiles historically carry apiKey in global JSON; gateway exposes redacted copies and only reuses saved key for the same provider/protocol/endpoint. Cloud uses server encryption. Preserve global generation and existing key. Envelope is `ANWSEC01 || nonce(12) || tag(16) || ciphertext`, AAD `AI-Novel-Writer/server-secret/v1`; Go AEAD's default ciphertext/tag order differs. Never decrypt production secrets for this audit.
7. **Network enforcement belongs at actual dispatch** for OpenAI, Gemini, discovery, embeddings and every DAV verb. Exact configured origins, redirects denied, sanitized public responses/events/errors, and administrator-owned proxy remain mandatory. Node's saved desktop proxy settings must not rewrite Go process environment. GitHub skills and external MCP require separately explicit network policy.
8. **Concurrency architecture changes must be explicit.** Node singleton project state was isolated by process. Go should capture a per-project runtime/DB and serialize writers, never mutate a global “current DB” while goroutines run; revalidate after waiting for locks. Preserve global one-writer metadata and originating flexible-operation ownership.
9. **MCP boundary:** the core application must require no Node process/package/runtime. Administrator-selected external MCP servers may independently require Node or other runtimes; disclose this external optional dependency rather than forbidding user-selected tools or claiming the whole tool ecosystem is dependency-free. Never use that exception to run the app's old Node business backend.

## Source anchors and audit scale

Read anchors: `web/operations.json`, `web/server.mjs`, `web/worker.ts`, `web/security.mjs`, `web/PLATFORM-CONTRACT.md`, `web/ARCHITECTURE.md`, `web/runtime-dependencies.mjs`, `package.json`; `src/services/browser-transport.ts`, `src/shared/project-format.ts`; controller composition and representative controller/repository/service bodies; `electron/database.ts`, migration registry/runner/adapter, SQLite/project/global/vector migration services, `electron/vector-store.ts`, provider/proof/finalization/archive/WebDAV/MCP implementations. Static inventories covered all controller/MCP registrations. Not every line of the domain was reviewed: top-level source size alone is 22 controllers / 7,000 lines, 79 services / 18,084 lines, 27 repositories / 12,323 lines, 12 migration files / 2,653 lines, excluding shared rules, vector-store and tests. A “full port” cannot realistically be equated with the initial HTTP/lease baseline.
