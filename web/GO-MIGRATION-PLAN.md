# Full Go backend migration plan

## Goal, baseline and honest progress

Replace **all application backend business execution with Go**, retain existing React/CodeMirror frontend and its HTTP/SSE contracts, preserve old data, and remove the core backend's Node process/package/runtime dependency. A Go gateway dispatching to Node workers is not completion. Frontend Node/pnpm/Vite tooling may remain **build-only**. Optional administrator-selected external MCP tools can independently require Node or other runtimes; disclose that separate external dependency, do not prohibit tools arbitrarily or hide a Node app backend behind this exception.

Audit baseline is Node commit `b69dc19c`; its fixes are not asserted deployed. Current production is untouched. This audit owns only `web/GO-MIGRATION-MATRIX.md` and this file. No runtime, CI or go.mod changes were made by the auditor; no private/production data or secrets were read.

**Status (updated through round 16 infrastructure):** nonproduction, **2/281 IMPLEMENTED navigation intents, 54 IN PROGRESS isolated draft/core/review/revision/postprocess-ledger/LLM-history/narrative/exemption/plot-read-clear/ordinary-blueprint HTTP/query/CAS, 225 NOT STARTED**, plus separate HTTP/transfer/event backlog (217 project /42 global /22 flexible total inventory). v7 admission/limited empty-history upgrades/private-copy sessions/credential envelope and private external row sorting have remote synthetic evidence. Native OpenAI/Gemini transport/egress and Linux cross-process writer exclusion are separate modules, not business workflow completion; exact current passing run and limitations are in GO-CI-ACCEPTANCE.md. No durable author project, AI workflow, complete author LanceDB/domain compatibility or production cutover is claimed. Rounds15–16 now have actual synthetic native Lance0.22.3/Arrow IPC schema+row/reopen proof and bounded Go physical asset stage→native scan→original JS validation+cleanup, without Node/Python runtime bridge; see [GO-LANCE-NATIVE-SPIKE.md](GO-LANCE-NATIVE-SPIKE.md) and [GO-KNOWLEDGE-COPY-PREFLIGHT.md](GO-KNOWLEDGE-COPY-PREFLIGHT.md). Copied metadata classifications are not canonical table/domain crosslink admission; no operations promoted. Round14 isolated preflight now reuses the actual legacy `.native-web.gateway.lock`, rejects portable-authority markers and stages/verifies only private SQLite copies under continuous cooperative exclusion: [GO-AUTHOR-PREFLIGHT.md](GO-AUTHOR-PREFLIGHT.md), [Actions 41408608534](https://github.com/qingwo1991-debug/ai-novel-writer-web/actions/runs/41408608534) (316/3319 PASS,0fail,3 explicit nonroot ownership skips,14packages). It grants no writer capability, does not bind main/RPC, inventories only fixed canonical asset entry metadata, and does NOT prove Lance descendants/version/rows. Native Lance canonical text proof and complete historical-authority semantics remain hard gates in [GO-DURABLE-PREFLIGHT-NOTES.md](GO-DURABLE-PREFLIGHT-NOTES.md). Promote rows only against real handlers and contract evidence. Prior Node test or frontend checkmarks do not transfer.

**Execution constraint:** perform **all compilation and preferably all tests in GitHub Actions**, not on this disk-constrained machine. Do not run local `go build`, `go test`, frontend builds, Docker builds/pulls or Go installation. Source reads and bounded static checks are allowed. Parent reports `.github/workflows/go-backend.yml` now defines remote formatting/vet/unit/race/build/smoke checks and artifact output, **no image publishing or deployment**; an unrun job is not passing evidence. The user has authorized the independent private repository/GitHub qualification path; pushing source to that repository is within scope. No publishing or production deployment follows merely from a green partial migration run.

## Architecture decisions

| Concern | Decision / implementation requirement |
|---|---|
| Host | Go `net/http` serves existing built frontend + exact operation transport and SSE. Compile-time/static handler allowlist; no generic file/SQL/command API and no Node subprocess/FFI callback to old services |
| Project isolation | One explicit runtime per canonical project, captured DB handle + project ID + session lease/epoch; mutex/actor serialization and cancellation contexts. Single editable tab ownership, same-tab refresh retention, origin-owned grants/streams/import/cloud operations. Avoid process-global current DB |
| Global state | One serialized global writer; preserve existing committed generation/receipt and atomic JSON layout first. Cloud credential/binding/recency updates from project flows delegate to that writer |
| SQLite | Keep existing SQLite file format, schema version 7, exact DDL lane and semantics. Prefer a qualified `database/sql` pure-Go SQLite driver if CI validates JSON1, pragmas, FK/transactions, busy/WAL and backup behavior. A reviewed CGO driver is a possible non-Node fallback, with explicit libc/native packaging requirements; selection is not settled by this document |
| LanceDB | **Retain existing canonical Lance assets until a real adapter is verified.** Evaluate Go-controlled native Lance/Arrow binding via a supported non-Node C/Rust ABI. Do not assume a Go Arrow reader can read Lance manifests/indexes, or that a Node `.node` addon is a generic native library. Compatibility is a release blocker, not a reason to erase/rebuild data |
| Knowledge alternative | Changing to a Go-native store is a separate approved migration: lossless export/import of all documents/chunks/copies/space metadata/vectors and proofs, immutable original backup, logical verification, search parity and rollback. A text-only portable archive omits vector history and is insufficient for full preservation. No permanent Node converter at startup; optional offline old-runtime fixture generation is not the production runtime |
| Providers | Native Go OpenAI-compatible and Gemini transports; implement policy-specific request fields, discovery, embeddings, streaming reasoning filtering and exact finish/usage/unknown semantics. Egress validation at every resolved HTTP dispatch, not only model-save time |
| Files/archives | Native `os`, hashing, `archive/zip` and bounded XML/HTML parsers. Safe descriptor/root-bound operations, no-link traversal/identity checks, atomic no-overwrite publish, ZIP64 and UTF-8 handling, staged crash recovery |
| Credentials | Go crypto interoperates with existing versioned AES-GCM envelopes/key. Retain key and global generation; preserve model JSON source with sanitized public projections. Do not call model metadata “encrypted” unless it actually is; encrypted migration requires separate rollback design |
| Integrations | DAV via bounded `net/http` + XML; MCP JSON-RPC stdio and existing SSE protocol, administrator-loaded configuration, restricted inherited environment, bounded queues/catalogs and effect policy. External tool runtimes documented separately |
| Frontend | Keep React 19 / CodeMirror 6, stores, editor/history/recovery UX and browser transport. Only narrowly coordinated contract fixes; no rewrite to Go templates or different editor. Shared TS types may remain build-only but Go ports their backend rules explicitly |

## Dependency removal ledger

`package.json` is mixed desktop/frontend/backend; a dependency's presence there is not automatically a server runtime dependency. `web/runtime-dependencies.mjs` explicitly seeds the current Node closure with `better-sqlite3`, `@lancedb/lancedb`, `apache-arrow`, `yauzl`, plus discovered imports.

| Existing dependency / mechanism | Go replacement / acceptance |
|---|---|
| Node 22 server, child_process fork, IPC, EventEmitter, AsyncLocalStorage | Go process, project runtime references, typed events and request contexts; no bundled worker.mjs execution |
| better-sqlite3 13 | Qualified Go driver + exact repositories, transactions and migration/admission protocols; no dynamic N-API binary |
| LanceDB JS 0.22.3 + platform native packages; Arrow 18 / flatbuffers | Real non-Node persisted-format adapter or separately approved verified conversion. Pin version/ABI and qualify actual fixture formats before selecting dependency |
| yauzl 2 | Go ZIP/ZIP64 parsing with equivalent central/local validation, bomb/path/collision/symlink/size/hash protections; CRC alone is not manifest integrity |
| Provider JS HTTP / embedding code | Go transports, bounded SSE parser, generation policy and proof port; no “OpenAI-only means all providers” assumption |
| reflect-metadata and other bundled backend helpers | Audit emitted dependency closure; reimplement only actual business use in Go; no embedded JS VM requiring old app runtime |
| diff-match-patch / shared parsing/counting algorithms | Keep browser usage; port any backend-side use and cross-language vectors, especially UTF-16 vs rune/byte counting and stable hashes |
| Electron/updater/native image | Not loaded by web backend already; stay absent. Browser navigation/deployment capability replaces desktop controls; image optimization optional after validation |
| React, CodeMirror, Radix, Zustand, Markdown, CSS | Retain in shipped static JS; Node compiler/build tools stay in build stage/CI only, not runnable server dependencies |
| Admin-selected external MCP command | Separately declared external tool dependency; core app works without it. Tools can require Node at operator option, but the core must not require/install/spawn Node for business handlers |

## Ordered roadmap and phase exit gates

All phases below are planned unless their evidence is explicitly recorded. Phases are vertical business slices, not merely packages created. Foundation progress does not permit production cutover.

### 0. Contract freeze and CI foundation — IN PROGRESS (parent-reported)

- Lock exact 281-operation/13-exclusion inventory; reconcile Go handler names against registry without automatically exposing all registrations.
- Freeze positional args and return DTOs from `src/shared/ipc-channels.ts` and separately imported contracts; maintain JSON fixtures for null/omission, errors, events, sorting and request tokens. Add the nonregistry endpoints from the matrix.
- Establish Go module/package boundaries, CI-only compilation, unit tests, race tests and artifact output. No local compilation/test/build or deployment. Keep infrastructure “IN PROGRESS” until reviewed CI evidence.
- Exit: Actions artifact provenance and actual checks for policy, session/CSRF/origin, path confinement, per-tab leases and SSE infrastructure; no claims of feature parity yet.

### 1. Data admission before writable CRUD — NOT STARTED

- Global locator: recognize committed receipt/generation, preserve unknown assets, refuse conflicting dual roots and malformed JSON. Never choose an empty root just because another exists.
- Project manifest: `.ai-novel/project.json` schemaVersion 1/storageVersion 1 is **not** SQLite schema version 7; validate exact identity/storage fields.
- Port migration registry/runner M00→1 baseline, M01→2 generation ledger, M02→3 identity, M03→4 review cycle, M04→5 import effects, M05→6 assets, M06→7 review-cycle merge (M06 code is in `m03-review-cycle.ts`). Port data transforms/verifiers as well as DDL; never merely stamp user_version.
- Read-only probe must use a copied DB/WAL/SHM/journal set: SQLite “readonly” connections can still touch original SHM. Detect drift with before/after identity, verify integrity/FK and exact recognized schema fingerprint, then migrate only staging with one transaction per step. Unknown/newer schemas fail closed without modification; explicitly qualified old schema variants need preserved rowids/sequences and domain evidence.
- Keep staging/journal/cutover and source isolation rollback; only after physical + logical verification obtain writable WAL/FK handle and fence stale import executions. No source migrations on live/prod data in this task.
- Exit: CI fixture matrix for recognized versions/legacy variants, corrupt/newer/unknown schemas, WAL-only committed data, interrupted checkpoints and unchanged rejected source; stable identity/content/domain hashes.

### 2. Author workspace vertical slice — NOT STARTED

- Implement library create/open/visible list/hidden chooser/peek/current overview/remove/delete and full project session semantics. Keep persistent shelf hiding vs filesystem deletion distinction from `b69dc19c`.
- Port contents/drafts + project-core + blueprints + author roster + ordinary settings/prompt/skill reads/writes. Draft writes compare exact acknowledged body and status inside transaction. Preserve finalized immutability/authority, lineage and recovery candidates; do not add generic upsert shortcuts.
- Port safe project FS operations, mutex CAS, atomic write commitState, transfer staging, browser dialogs/uploads and genuine downloads; leases rechecked after lock acquisition.
- Exit: retained frontend can create/open, edit/save/reload/restart, show editor history and recover unsaved/conflicting text. Cross-project, same-path new-lease and two-tab stale commits fail with old data intact. Target prior `draft-conflict`, `library-visibility`, editor/write-conflict and management acceptance scenarios in CI.

### 3. Provider transport + durable generation engine — NOT STARTED

- Port model CRUD/stage routing/execution leases/blank-key retention, discovery and connection tests; protect public projections, provider failures and events against credential leakage.
- Implement OpenAI-compatible **and native Gemini** generation/stream/embeddings; URL resolution, all reasoning adapters, output-token parameter policy, structured responses, visible `<think>` filtering across chunk boundaries, usage confidence and stop/length/filter/error/unknown mapping. Handle fragmented/oversized SSE, cancellation and EOF without inventing successful finish.
- Durable roots/budgets/reservations/attempts/artifacts before dispatch; unique nonce/idempotency, partial tails, pause/resume/restart/discard and crash ambiguity. No transparent retry of writes or paid requests after unknown outcome.
- Port frozen source manifest, nine-component fingerprint, model revision, prompt/builtin skill assets, KB snapshot, output contract and composition algorithms. Go JSON hashes need JS-compatible fixture vectors (field order/escaping/numbers/UTF-16 behavior).
- Exit: Actions fake-provider tests (split frames, errors, lost response, hostile output, unknown usage, abort), durable restart and budget proof. Approved real-provider smoke tests are separate opt-in CI evidence; absent secrets means unverified, not pass. UI generation streams/reconnect snapshots must work without Node.

### 4. Proof-carrying domain effects — NOT STARTED

- Port batch/directory planning, short outlines and author recovery; source receipts and generated range commit share the SQLite transaction.
- Character proposals/identity/legacy adoption, finalized character candidates, graph confirmation, editor-inline recovery, agent tool claims/domain effects and review/revision cycles each retain dedicated source/artifact validators.
- Formal commit reads immutable artifact from ledger; caller supplies references, not authoritative generated body. Verify run/project/root/epoch/artifact revision/hash, successful receipt/finish reason, model provenance and current sources in the effect transaction.
- Preserve intentional direct rejections: blueprint notes/continuity writes require finalization admission, plot-tree save requires graph admission, legacy roster staging requires legacy source gate. Route allowlisting does not relax these.
- Finalization: draft expectedContent + authoritative outbox transaction, then recoverable manuscript publication; retry from receipt snapshot only. Deletion has independent recoverable outbox. Report committed/publication states distinctly.
- Exit: CI forged/stale/foreign/tampered proof rejection, exactly-once replay, failed effect rollback, failed file publication with committed receipt, retry after restart, stale source during generation and review recheck obligations. No “generation returned text” shortcut.

### 5. Existing knowledge data and embeddings — NOT STARTED, critical path

- Qualify non-Node Lance reader/writer on fixture copies of the actual 0.22.3 persistence formats before claiming KB readiness. Preserve every canonical `documents`/`chunks` row, `chunks__space_N` (active/inactive/building), `embedding-spaces.json`, copy JSON/hashes and SQL receipts. Handle legacy vectors/journal gates explicitly; never auto-create empty DB on incompatible open.
- Port scoped/text/vector/writing-context search, corpus and chapter filters, dimension/model/distance compatibility, vectorless fallback and source-currentness checks. Edited dirty document copies cannot feed generation as current index evidence.
- Rebuild into isolated space, validate all finite vectors/count/dimension and full canonical coverage, then atomically switch active generation while keeping old data. Provider calls must not begin before required source/receipt checks.
- Use existing Arrow migration snapshot invariants as qualification input, not as existing Go code. A portable text snapshot alone is insufficient preservation. If native binding cannot be qualified, keep this a blocker and propose a separately reviewed migration; do not deploy partial KB silently.
- Exit: before/after logical manifest/hash and counts for all spaces/copies/receipts; same search fixtures and distances within justified float tolerance; no embedding call/data mutation on refused source, crash midway retains prior active space. Old Lance assets retained and reversible.

### 6. Import, portable archives and WebDAV — NOT STARTED

- Implement bounded TXT/MD chapter split + EPUB container/OPF/spine order, DRM rejection, Unicode units and archive expansion protections. Inspection/grant/raw source identity stays server-side; persisted identity is HMAC, not host path.
- Port import previews, authority/source fingerprints, lease epoch/renewal, cancellation boundaries, resumable checkpoints and unique effect namespace/key ledger. Counters do not prove side effects; reconcile partial/unknown records conservatively.
- Archive export freezes authority and assets, snapshots SQLite with committed WAL, applies exact portable field policy/runtime freeze and includes knowledge/copy/avatar assets. ZIP64 manifest hashes, no traversal/case-normalized duplicates/links/overwrite, streamed size limits, staging-owned cleanup and new-identity restore-copy. Preserve origin authority vs live write authority distinctions.
- WebDAV: PROPFIND/MKCOL/GET/PUT actual-dispatch policy; bounded XML/JSON/archive reads, auth/timeout/cancel, append-only generation manifest + archive hash + final completion descriptor, sibling/parent lineage and binding revision CAS. Global metadata updates serialized across concurrent projects.
- Exit: roundtrip/restart on fixture data, malicious ZIP/EPUB/DAV negative suite, cancellation/collision/no partial target publication, remote orphan incomplete generation ignored, multi-project binding race. Approved real DAV evidence optional and explicit, never fabricated.

### 7. Integration completion and frontend compatibility — NOT STARTED

- Port MCP config/load/connect/disconnect/tools/resources/call/status, JSON-RPC framing, legacy SSE, subprocess lifetime, limits and sanitized status events. No browser-supplied command/args/env; administrator lifecycle baseline plus assigned tool/effect confirmation. External tool-specific runtime/network capabilities disclosed separately from core.
- Finish prompt/skill GitHub/local inspection/install/uninstall with bounded safe bundles, hash/caller/expiry proof and correct built-in fallbacks. Fixed external-resource operations emit browser-navigation events only.
- Reconcile deployment-managed updater UI mismatch: reconnect currently may invoke excluded update:get-state. Maintain 13 exclusions or revise the frontend/contract explicitly; do not expose updater by prefix.
- Exit: all 281 operation rows have Go implementation + contract/domain/negative/restart evidence, or explicit agreed product-scope change (not silent disappearance). Existing React/CodeMirror browser acceptance suites run against Go in CI; transport/event/download tests cover nonregistry surface.

### 8. Release qualification and authorized cutover — NOT STARTED

- Actions builds Go binary and frontend assets; final artifact/runtime contains no Node executable, npm/pnpm, backend JS workers, node_modules, Electron, N-API addons or startup dependency on them. Browser JS assets remain expected. Inspect artifact/SBOM/dependency linkage and core process tree; test core with Node absent from PATH/filesystem. Test external MCP separately.
- Pin Go/native-library/platform versions and record license/security/SBOM/native ABI requirements. Do not promise a static binary if the qualified Lance/SQLite choice needs shared libraries.
- CI workload: all supported schema/data fixtures, source/hash parity, contract inventory, race checks, crash/unknown-outcome scenarios, frontend browser flows, memory/large-upload/archive/vector bounds and multi-project/global writer contention. Publish artifacts/results only under approved workflow, no deploy job.
- Cutover needs separate authorization, external writer exclusion/volume lock, verified complete backup (including credential key and DB sidecars/Lance/assets), immutable original and rollback rehearsal. No live audit probing/decryption. Start Go only after all gates; existing production remains untouched until then.
- Rollback is not “run old Node on Go-mutated data.” Restore an explicitly compatible snapshot or qualified backward-compatible dataset; drain/fence tasks and document which writes occurred after cutover. Keep the former volume immutable until retention decision.

## Completion gates (all required)

1. **Core runtime independence:** every core operation executes in Go/native non-Node libraries, no old Node services/proxy/subprocess conversion, no Node backend dependency in final artifact. Frontend Node is build-only. Optional external MCP dependencies clearly separated.
2. **Full behavioral inventory:** 281/281 implemented and tested; 13 exclusions intentional; all gateway-only endpoints/events/files/leases proven; no success placeholders, guessed model tests, empty-data substitutes or inherited Node checkmarks.
3. **Old data preserved:** recognized SQLite lane/domain/rowid/sequence integrity; WAL-safe snapshot; committed global generation + key continuity; complete Lance canonical text/vectors/spaces/copies/receipts; archives/avatars/import/finalization/recovery/history preserved. Unknown data not overwritten. Fixture copies and controlled authorized migration only.
4. **Trust/transactions:** authentication/CSRF/origin, exact roles and ownership, path safety, actual-dispatch egress and redaction; source CAS + proof + receipt in one transaction; recoverable outboxes/cross-store stages and conservative unknown outcomes.
5. **Frontend retained:** existing React/CodeMirror edit/history/conflict/recovery, streams/reconnect, settings/planning/review/import/export/cloud/MCP flows pass against Go, not merely a page load.
6. **Evidence and deployment separation:** Actions compilation/tests/artifact provenance; absent live credentials/services recorded unverified; no local heavy compilation/tests, no audit production/private reads, no push/deploy without permission. Health/CI/skeleton readiness never equals parity or authorization to cut over.

## Evidence recording rule

For each matrix row record: exact Go code path + contract fixture/test name + Actions run/job/artifact + tested failure/restart/ownership cases. Infrastructure may use IN PROGRESS independently. A business row becomes implemented only when real domain code exists; completion requires its tests and cross-feature gates. Real provider/WebDAV/MCP checks with missing approved service access remain explicitly unverified. The initial source audit itself proves inventory and risks only, not any Go runtime behavior.
