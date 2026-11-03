# Go — generation chain wiring spec (43 operations)

Evidence-based implementation spec. Baseline `232844b9`. Read-only analysis; verified against
`web/operations.json` by set comparison: **281 total, 110 bound, 171 unbound**, of which these 43
generation-family ops are exactly the unbound set. **None of the 43 is bound anywhere today.**

## Correction to earlier assumptions

- `projectsession.Manager.BeginGenerationLedger` (`session.go:183`) is the **read-lane scratch-copy**
  primitive (tests only). The **durable** lane is `authorruntime.ReadLifecycle` +
  `projectsession.ActiveCoordinator` and has **no generation accessor** — the adapter must add one that
  type-asserts `*admittedAuthor` and wraps `handle.DB()` (`projectfs/author_linux.go:151`) in
  `store.NewGenerationLedger`.
- `generationowner.Executor.SecretStorage` is the concrete `*secrets.Storage` (AES-GCM), but Go's
  models.json holds a **plaintext** `apiKey` (`globalconfig.ModelRecord` is `map[string]any`), so a
  pass-through credential shim needs a 1-line interface widening.
- `OrdinaryService` exposes only `Execute` + `SuspendForProjectClose`; `Pause`/`Cancel`/`Resume`/
  `DiscardCandidate` are on the hidden `*generation.Service` → need thin accessors.
- The ledger **deliberately rejects** agent/graph/import/finalization manifests
  (`glOrdinaryBinding`, `generation_ledger.go:363-380`) and `Service.Execute` rejects
  `AgentToolNames` (`service.go:288`). Those families need a separate lane / gated widening.
- Biggest missing piece for `generation:*`: the **RunBinding builder** (Node
  `electron/services/generation-source-binding.ts`, 625 lines) computing the 9-part fingerprint +
  sourceRefs that ledger `open` requires (`GLBinding`, `store/generation_contract.go:293`), plus the
  budget policy (`main-generation-plan.ts`) and batch/composition/preparation stores.

## Runtime API (verified)

- `internal/generation/service.go`: `Ledger` interface (`:22`), `Request` (`:40-55`), `Dependencies`
  (`:77-85`), `New` (`:117`), `Execute(ctx, Request)` (`:282`), `Resume` (`:922`), `Pause`/`Cancel`
  (`:849`/`:852`), `DiscardCandidate` (`:795`), `SuspendForProjectClose` (`:859`),
  `NativeProvider(dispatch llm.Dispatch)` (`provider.go:25`).
- `internal/generationowner/`: `Scope` (`lease.go:26`), `Profile` (`:32-46`), `Receipt` (`:65-76`),
  `NewRegistry` (`:144`), `Registry.Begin` (`:145`), `Executor.Prepare` (`dispatch.go:33`),
  `Executor.Dispatch` (`:42`), `Registry.BindSources` (`binding.go:50`), `Binding.Fingerprint`
  (`:57`), `Task`/`ParseTask`/`BuildPlan` (`plan.go:17/35/60`), `NewOrdinaryService` (`ordinary.go:95`),
  `OrdinaryService.Execute` (`ordinary.go:110`), `SuspendForProjectClose` (`:156`).
- `internal/store/generation_ledger.go`: `NewGenerationLedger` (`:29`), `CallJSON` (`:242`);
  `open` requires `projectId, epoch, fingerprint` (9 SHA-256 keys), `contextSnapshotId`,
  `sourceManifest`, `sourceRefs`, `operation, uiActionNonce, frozenInputHash, budget{...}`
  (`:526`, `generation_contract.go:293-352`); `get` (`:307`), `budget` (`:320`), `receipt` (`:458`),
  `replaceUnstartedBinding` (`:1055`).
- `internal/authorruntime/`: `admittedAuthor` (`durable_linux.go:67-75`),
  `OpenProjectDurable` (`:128`), `SaveCore` (`:259`); `projectfs.AuthorDatabase.DB()` (`:151`).
- Transport binder patterns to mirror: `drafts.go:23-34` (provider + func variant), `projects.go:25-40`,
  `draft_live_write.go:31-42` (typed request + `draftExpectedContext`), error style
  `draftSaveFailure` (`drafts.go:219-239`), `409 PROJECT_CONTEXT_CHANGED` (`drafts.go:86-88`),
  SSE `Server.Publish` (`server.go:208-215`).
- Browser contracts: `src/shared/generation-owner-contract.ts:104-335`,
  `src/services/generation/generation-runtime.ts:77-130`, `electron/controllers/generation-controller.ts:178-294`;
  `hashAuthorText` = SHA-256 hex (`src/shared/source-ref.ts:22-24`).

## Per-operation status

| Family | Ops | Status |
|---|---|---|
| `generation:execute` | 1 | **READY** — adapter wiring only |
| `generation:begin`, `restart`, `bind-material-decision` | 3 | **SHIM** — needs reduced RunBinding builder |
| `generation:read` | 1 | **SHIM** — needs view projection |
| `generation:list` | 1 | **SHIM** — needs a read-only ledger list query (none exists) |
| `generation:pause`, `cancel`, `resume`, `discard-candidate` | 4 | **SHIM** — needs `OrdinaryService` accessors |
| `generation:retry-draft-short-outline` | 1 | **SHIM** — replay task re-derivation |
| `generation:commit-draft` | 1 | **SHIM** — composition-receipt gate + durable draft-CAS primitive |
| `generation:prepare-draft-context`, `read-context`, `compose-visible`, `read-visible-composition`, `begin-batch`, `read-batch`, `list-batches`, `confirm-batch-finalization`, `list-directory-progress`, `list-recovery`, `find-blueprint-recovery` | 11 | **MISSING** — batch/composition/preparation stores + Node module ports |
| `agent-generation:*` | 8 | **MISSING** — no Go agent engine; ledger rejects `agentSession` |
| `graph-generation:*` | 5 | **MISSING** — ledger rejects `graphGenerationContext` |
| `finalization-generation:*` | 5 | **MISSING** — ledger rejects `finalizationGenerationSlotKey` |
| `import-generation:*` | 2 | **MISSING** — ledger rejects `importSlot`; `internal/importpreview/` is empty |

## Design

**Binder** `backend-go/internal/transport/generation.go`: `GenerationProvider` as a struct of
function fields (nil field ⇒ that group stays unbound ⇒ automatic `501`), so slices land
progressively. `BindGeneration` registers the 43 names, then binds only the groups whose field is
non-nil. Per-op table `{name, arityMin, arityMax, mutating, pathIndex}`; handler does
`draftExpectedContext(args, len(args)-1)`, `409 PROJECT_CONTEXT_CHANGED` on path mismatch,
`400 INVALID_ARGUMENTS` on arity, then dispatch. Read ops → `security.HTTPError`; mutating ops →
business DTO (`{"success":false,"error":"Error: CODE"}`). SSE via an injected
`publish func(channel string, args []any) error` wrapping `Server.Publish`.
`generation:compose-visible` is the one variable-arity op (3 or 4 args + context).

**Adapter** `backend-go/internal/authorruntime/generation_linux.go` (+ `generation_view.go`), because
it needs the unexported `admittedAuthor.handle`. New ledger accessor mirroring
`session.go:183` but on the coordinator:

```go
func (l *ReadLifecycle) BeginGenerationLedger(ctx context.Context, owner projectsession.Owner,
    expected projectsession.Context) (*store.GenerationLedger, func(), error) {
    handle, done, err := l.active.Begin(ctx, owner, expected)
    if err != nil { return nil, nil, err }
    author, ok := handle.(*admittedAuthor)
    if !ok { done(); return nil, nil, ErrDurableInvalid }
    return store.NewGenerationLedger(author.handle.DB(), nil, nil), done, nil
}
```

Per-RPC holds suffice because `Service.Execute` is synchronous and `CloseProject` waits for
`active==0`. **Flag: close-during-execute under the durable coordinator is untested — needs a test.**

Lease fencing: scope = `{Caller: sessionID, Session: sessionID, Tab: clientID, Project: ctx.ProjectID,
Epoch: leaseID}`; CAS/idempotency delegated to `Service` (`findInvocation`/`reserve`) + ledger
(`reserve`/`markDispatched`/`settle`); `verifyOrdinaryRun` re-checks project/epoch/fingerprint on every
`get`/`reserve`; credentials never enter receipts/errors.

Durable lane must also gain `globalconfig.New(globalDir)` + `egress.New(origins)` +
`generationowner.NewRegistry{LoadModel, Current→coordinator lease, AuthorityLock, Now, TTL:4h}`,
exactly like `main.go:270-282`.

**Shims** (small, enumerated): (1) widen `Executor.SecretStorage` to an interface
`{DecryptString([]byte) (string, error)}`; (2) `OrdinaryService.Pause/Cancel/Resume/DiscardCandidate`
accessors; (3) read-only ledger list query; (4) `generation_view.go` projection; (5) reduced
deterministic RunBinding builder.

## Tests (synthetic only, no model/network)

Reuse `transport.fixture(t)` (`server_test.go:24`), `bootstrap`/`rpc` (`:52`/`:68`),
`readFixture(t)` (`read_linux_test.go:24`, needs `GO_SQLITE_CONTRACT` + `baseline-v7.db`),
`fixtureProjectID()` = `8b5929e6-cda4-43e2-a15c-f232571f0036`, and the ledger call pattern in
`projectsession/generation_ledger_test.go:33-114`.

1. Registration & fail-closed: all 43 → 501; unknown → 404; after binding a stub provider only its
   groups answer 200; bad arity → 400; path mismatch → 409.
2. Real-chain end-to-end on a real v7 DB with a **fake dispatch** (scripted `OnVisible` deltas + one
   usage evidence + terminal result) and **fake registry**: begin → execute → read → pause/cancel/
   discard; assert source DB bytes unchanged; assert `generation_*` rows written.
3. Three representative ops with exact browser DTO shapes incl. `textHash == sha256Hex(text)`.
4. Fencing negatives: foreign tab/owner, stale context, concurrent execute+cancel, close-during-execute.

## Slices

| Slice | Ops | Effort |
|---|---|---|
| **S1** ledger + ordinary chain: `begin`, `execute`, `read`, `list`, `pause`, `cancel`, `resume`, `discard-candidate`, `bind-material-decision`, `restart`, `retry-draft-short-outline` | 11 | Runtime ~95% present; highest value |
| **S2** draft/batch/composition context (11 MISSING above) | 11 | New stores + 2 Node ports |
| **S3** `agent-generation:*` | 8 | New domain + engine port |
| **S4** graph/finalization/import generation | 12 | Gate widening + 3 ports; parallel with S3 |

Order: **S1 → S2 → {S3 ∥ S4}**. S1 builds the binder with the full provider struct; later slices fill
fields and the previously-501 ops light up automatically.

## Open flags

1. Credential storage mismatch (plaintext models.json) — shim for now; encryption intent unconfirmed.
2. Close-during-execute under the durable coordinator untested.
3. RunBinding byte-exactness deferred (reduced builder first; fingerprint hash field order must match
   Node for cross-version resume).
4. Budget-policy parity deferred (`BuildPlan` finite admission + `PreflightError` must surface as
   business errors).
5. `MainGenerationRunView` fields beyond the verified subset (`plotOutline`, `budgetDiagnostics`,
   `unsavedTails`, `ledger`) need a later differential pass against Node `viewOf`.
6. Widening the ledger gates for agent/graph/import/finalization is a design decision requiring
   reviewer sign-off before S3/S4.
