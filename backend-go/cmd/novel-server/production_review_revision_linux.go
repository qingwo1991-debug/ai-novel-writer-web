//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sync"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/reviewrevprep"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves `review-revision:prepare` and supplies the begin-time freeze
// that makes `generation:begin` open a review/revise run over a prepared context.
// The family is four names (web/operations.json:312-315):
//
//	review-revision:prepare          BOUND here (arity 1)
//	review-revision:commit-review    NOT bound (see below)
//	review-revision:commit-revision  NOT bound (see below)
//	review-revision:read-recovery    NOT bound (see below)
//
// # The reference
//
//	electron/controllers/generation-controller.ts:240
//	  register('review-revision:prepare', 1, (owner, request) => owner.prepareReviewRevision(request))
//	electron/services/review-revision-generation.ts:143-148
//	  prepare(request) { this.assertCurrent(); return this.db.transaction(() => {
//	    const context = captureReviewRevisionContext(this.db, request, this.scope.projectId)
//	    return this.remember(context, this.parent(context)) })() }
//	electron/services/review-revision-generation.ts:119-121
//	  private remember(context, lineage = {}): PreparedReviewRevisionContext {
//	    const prepared = { contextId: randomUUID(), context: structuredClone(context), ...lineage } ... }
//	src/shared/review-revision-generation.ts:174
//	  'review-revision:prepare': { args: [PrepareReviewRevisionRequest]; return: PreparedReviewRevisionContext }
//	src/services/workflows/commands/review-revision-command.ts:94-104
//	  prepared = await ipc.invokeWithProjectSession(session, 'review-revision:prepare', {
//	    operation, draftId, expectedDraft: { chapterNumber, version, status, contentHash },
//	    authorInputs, ...confirmation, uiLocale })
//
// The answer is therefore exactly `{ contextId, context }`: `parent()` returns {}
// for every operation this lane admits, because a confirmation needs
// parseHumanConfirmedReviewSnapshot and a recheck needs ReviewCycleRepository
// .planRecheck, and neither is ported (internal/store/review_revision_context.go:
// 74-86 refuses those two requests instead of under-freezing them). A lineage
// field this lane cannot derive is left absent rather than guessed.
//
// # The producer and the registry
//
// internal/store/review_revision_context.go:239 Capture is the freeze producer. It
// returns the exact bytes `JSON.stringify(context)` emits plus their sha256, so
// this file never re-encodes a frozen value: the context travels to the browser as
// `frozen.Text` and to the ledger as the same bytes, because
// internal/store/review_revision_context.go:34-41 guarantees plotJSON reproduces
// Text byte for byte.
//
// internal/reviewrevprep/store.go is the in-process handle registry the browser's
// opaque `reviewRevisionContextId` resolves against. It is constructed exactly once
// per process (productionReviewRevisionRegistry): this handler and the begin
// admission call the same accessor, because a second construction would mint
// handles the admission cannot resolve.
//
// # Why three names stay unbound
//
// A name that can only refuse stays unbound, and each of the three has a concrete
// missing port rather than an unfinished adapter:
//
//	commit-review    needs ReviewCycleRepository.create (electron/repositories/
//	                 review-cycle-repository.ts, 826 lines; no review_cycles writer
//	                 exists in internal/store), and the cycle row is the whole point
//	                 of the name: a later recheck departs from it. The ledger now
//	                 admits reviewRevisionEffect (internal/store/generation_ledger.go
//	                 :478, :562, :589) but nothing in this lane stamps one, so the
//	                 receipt half of the same commit has no writer either.
//	commit-revision  needs assertMateriallyCompleteRevision (src/services/workflows/
//	                 commands/refinement-completeness.ts:1-52) and isNoopRevision
//	                 (src/shared/review-cycle.ts). Neither is ported, so a commit
//	                 here would persist a revision the reference refuses - an empty
//	                 or a no-op replacement of the author's own chapter - which is
//	                 worse than a 501.
//	read-recovery    needs `saved`, whose proof recomputes the saved review body
//	                 through reviewBody() (review-revision-generation.ts:78-91) and
//	                 re-proves the persisted review row. Since nothing in this lane
//	                 can stamp an effect, `saved` is always undefined here, so the
//	                 name would answer a recovery object that can never report the
//	                 one fact the workflow looks for; it stays a 501.
//
// # The admission
//
// Prepare goes through productionDurableOpen.borrowAuthorDatabase
// (production_durable_linux.go:313-364), the identical fence every other binder in
// this lane uses: it parses the frozen three-key context, names the caller's own
// cookie/tab owner, refuses a project this lane never opened, re-proves the live
// lease and the admitted handle identity, and hands over one (*sql.DB, release)
// pair. Capture itself writes nothing - it opens a deferred read transaction on one
// reserved connection - so a prepare that is refused, and one that succeeds, both
// leave the author's database byte for byte as they found it: the handle lives in
// this process only.
type productionReviewRevisions struct {
	durable *productionDurableOpen
}

// The one name this file binds, spelled once so a test and the binder cannot drift
// apart.
const productionReviewRevisionPrepare = "review-revision:prepare"

// productionReviewRevisionAuthorInputID is the sourceManifest.authorInputs id the
// frozen context itself is carried under: the producer's own literal
// (internal/store/review_revision_context.go:173) and the value the admission
// verifier requires (internal/admission/reviewcycle_effect.go:205-207).
const productionReviewRevisionAuthorInputID = "review-revision-context"

// productionReviewRevisionServerKeyBytes is the registry's key length. The registry
// refuses anything below 32 bytes (internal/reviewrevprep/store.go:82); the key is
// process-random, so no other process can resolve the handles this one mints.
const productionReviewRevisionServerKeyBytes = 32

// The family's own symbolic refusals. The first three are the reference's own
// strings (review-revision-generation.ts:118-124); the last is the lane's
// fail-closed answer for a registry this process could not construct. The fourth
// reference code, GENERATION_REVIEW_CONTEXT_REQUIRED, is published by the table
// below for the three "prepare again" refusals instead of being a sentinel here,
// because no code path in this lane raises it by identity.
var (
	errProductionReviewRevisionInvalid  = errors.New("GENERATION_REVIEW_CONTEXT_INVALID")
	errProductionReviewRevisionMismatch = errors.New("GENERATION_REVIEW_CONTEXT_MISMATCH")
	errProductionReviewRevisionChanged  = errors.New("GENERATION_REVIEW_CONTEXT_CHANGED")
	errProductionReviewRevisionStore    = errors.New("REVIEW_REVISION_CONTEXT_STORE_UNAVAILABLE")
)

// productionReviewRevisionOperations is REVIEW_REVISION_OPERATIONS
// (review-revision-context.ts:19): the only three operations that carry a frozen
// review-revision context.
var productionReviewRevisionOperations = [...]string{
	store.ReviewRevisionOperationReviewChapter,
	store.ReviewRevisionOperationRefineDraft,
	store.ReviewRevisionOperationRefineFromReview,
}

// productionReviewRevisionIsOperation reports whether an operation belongs to the
// review/revise loop.
func productionReviewRevisionIsOperation(operation string) bool {
	for _, candidate := range productionReviewRevisionOperations {
		if candidate == operation {
			return true
		}
	}
	return false
}

// productionReviewRevisionRefusals is the refusal set with the code each one is
// published under. The producer's sentinels already carry their reference code, so
// this table restates them rather than formatting a message, and the registry's own
// codes are published where the reference has no equivalent (a capacity or a
// validation refusal main can never see, because main mints the handle itself).
var productionReviewRevisionRefusals = [...]struct {
	refusal error
	code    string
}{
	// review-revision-context.ts:20, :39-53 (the request guards) and :149.
	{store.ErrReviewRevisionContextInvalid, "GENERATION_REVIEW_CONTEXT_INVALID"},
	// :60 the authored draft is no longer the chapter's latest version, or no longer
	// matches the version the renderer selected.
	{store.ErrReviewRevisionSourceChanged, "GENERATION_REVIEW_SOURCE_CHANGED"},
	// :101, :116, :121, :125, :127.
	{store.ErrReviewRevisionHistoryChanged, "GENERATION_REVIEW_HISTORY_CHANGED"},
	// The two arms this lane refuses instead of under-freezing (:77-91, :61-67).
	{store.ErrReviewRevisionContextConfirmationUnavailable, "REVIEW_REVISION_CONTEXT_CONFIRMATION_UNAVAILABLE"},
	{store.ErrReviewRevisionContextRecheckUnavailable, "REVIEW_REVISION_CONTEXT_RECHECK_UNAVAILABLE"},
	// :149-151 a stored field the reference's own TypeErrors depend on, refused by
	// name here (see the producer's header).
	{store.ErrReviewRevisionContextWorldbuildingInvalid, "REVIEW_REVISION_CONTEXT_WORLDBUILDING_INVALID"},
	{store.ErrReviewRevisionContextBlueprintInvalid, "REVIEW_REVISION_CONTEXT_BLUEPRINT_INVALID"},
	{store.ErrReviewRevisionContextAuthorSourceInvalid, "REVIEW_REVISION_CONTEXT_AUTHOR_SOURCE_INVALID"},
	// :39-40 the owning session names no project.
	{store.ErrReviewRevisionContextProjectRequired, "REVIEW_REVISION_CONTEXT_PROJECT_REQUIRED"},
	// A handle this process never minted, one it already collected, and one whose
	// caller epoch moved. The reference collapses all three into "prepare again"
	// (:120 `if (!prepared) throw GENERATION_REVIEW_CONTEXT_REQUIRED`), and so does
	// this table: an unguessable id that is gone tells a renderer nothing except to
	// prepare again.
	{reviewrevprep.ErrHandleUnknown, "GENERATION_REVIEW_CONTEXT_REQUIRED"},
	{reviewrevprep.ErrHandleExpired, "GENERATION_REVIEW_CONTEXT_REQUIRED"},
	{reviewrevprep.ErrHandleRevoked, "GENERATION_REVIEW_CONTEXT_REQUIRED"},
	// A handle presented by another caller, generation or purpose. The reference
	// cannot see this state - its map is per owner and it compares a purpose only
	// (import-inspection-store.ts:156, :165) - so it is the misuse code, not the
	// absence one: the caller does hold a handle, just not for this run.
	{reviewrevprep.ErrCallerMismatch, "GENERATION_REVIEW_CONTEXT_INVALID"},
	{reviewrevprep.ErrGenerationMismatch, "GENERATION_REVIEW_CONTEXT_INVALID"},
	{reviewrevprep.ErrPurposeMismatch, "GENERATION_REVIEW_CONTEXT_INVALID"},
	{reviewrevprep.ErrBindingInvalid, "GENERATION_REVIEW_CONTEXT_INVALID"},
	// The registry's own capacity and validation refusals, published verbatim so a
	// prepare that hit a bound is distinguishable from one that hit a bug.
	{reviewrevprep.ErrContextInvalid, "REVIEW_REVISION_PREP_CONTEXT_INVALID"},
	{reviewrevprep.ErrTextTooLarge, "REVIEW_REVISION_PREP_CONTEXT_TOO_LARGE"},
	{reviewrevprep.ErrLimit, "REVIEW_REVISION_PREP_LIMIT"},
	{reviewrevprep.ErrContextChanged, "REVIEW_REVISION_PREP_CONTEXT_CHANGED"},
	// A registry this process could not build, or could not key.
	{reviewrevprep.ErrServerKeyInvalid, "REVIEW_REVISION_CONTEXT_STORE_UNAVAILABLE"},
	{reviewrevprep.ErrOptionsInvalid, "REVIEW_REVISION_CONTEXT_STORE_UNAVAILABLE"},
	{reviewrevprep.ErrStoreUnavailable, "REVIEW_REVISION_CONTEXT_STORE_UNAVAILABLE"},
	{errProductionReviewRevisionStore, "REVIEW_REVISION_CONTEXT_STORE_UNAVAILABLE"},
	// The two refusals the freeze itself raises: a selection that is not the one the
	// context was frozen for, and a context the database no longer reproduces
	// (review-revision-generation.ts:122-143).
	{errProductionReviewRevisionMismatch, "GENERATION_REVIEW_CONTEXT_MISMATCH"},
	{errProductionReviewRevisionChanged, "GENERATION_REVIEW_CONTEXT_CHANGED"},
}

// productionReviewRevisionCode is the reference's symbolic code for a refusal, or
// "" for anything this lane does not name.
func productionReviewRevisionCode(err error) string {
	for _, entry := range productionReviewRevisionRefusals {
		if errors.Is(err, entry.refusal) {
			return entry.code
		}
	}
	return ""
}

// productionReviewRevisionFailure is the refusal the browser receives: the
// transport's own error unchanged, a named refusal's symbolic code, or the family's
// invalid code. Never a SQLite message and never a draft body.
func productionReviewRevisionFailure(err error) error {
	if err == nil {
		return nil
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return public
	}
	if code := productionReviewRevisionCode(err); code != "" {
		return errors.New(code)
	}
	return errProductionReviewRevisionInvalid
}

// The process's one registry. The build function is a variable so a test can supply
// a store with a clock seam, and the once is what makes "one per process" true
// regardless of how many binders run.
var (
	productionReviewRevisionPrepBuild = productionReviewRevisionNewRegistry
	productionReviewRevisionPrepOnce  sync.Once
	productionReviewRevisionPrepStore *reviewrevprep.Store
	productionReviewRevisionPrepErr   error
)

// productionReviewRevisionNewRegistry builds the production registry from a
// process-random key. No renderer value, no project path and no generation is mixed
// in, so an entry MAC only ever proves "this process minted this handle".
func productionReviewRevisionNewRegistry() (*reviewrevprep.Store, error) {
	key := make([]byte, productionReviewRevisionServerKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, errProductionReviewRevisionStore
	}
	return reviewrevprep.NewStore(key, reviewrevprep.StoreOptions{})
}

// productionReviewRevisionRegistry returns the process's registry, constructing it
// on the first call and never again.
func productionReviewRevisionRegistry() (*reviewrevprep.Store, error) {
	productionReviewRevisionPrepOnce.Do(func() {
		productionReviewRevisionPrepStore, productionReviewRevisionPrepErr = productionReviewRevisionPrepBuild()
	})
	if productionReviewRevisionPrepErr != nil || productionReviewRevisionPrepStore == nil {
		return nil, errProductionReviewRevisionStore
	}
	return productionReviewRevisionPrepStore, nil
}

// productionReviewRevisionBinding is the identity every handle is minted and
// resolved under. All three values come from the authenticated request path, never
// from a body: the caller is the cookie session plus the tab that holds the lease
// (the reference's webContentsId, import-inspection-store.ts:126-135, spelled the
// way projectsession.Owner spells it), the generation is the live project lease,
// and the purpose is the operation, so a handle prepared for a review cannot admit
// a refine.
func productionReviewRevisionBinding(caller transport.Caller, epoch, operation string) reviewrevprep.Binding {
	return reviewrevprep.Binding{
		Caller:     caller.SessionID + ":" + caller.ClientID,
		Generation: epoch,
		Purpose:    operation,
	}
}

// newProductionReviewRevisions wraps the lane's durable bridge, the only object
// that can name the author database.
func newProductionReviewRevisions(durable *productionDurableOpen) *productionReviewRevisions {
	return &productionReviewRevisions{durable: durable}
}

// Bind installs `review-revision:prepare` and nothing else. A nil or closed bridge
// installs nothing, so the name keeps failing closed at GO_OPERATION_NOT_MIGRATED
// instead of being published as a handler that can only refuse. Bind is called
// beside BindGenerationBegin on purpose: a handle is only useful to a begin that
// can resolve it.
func (r *productionReviewRevisions) Bind(server *transport.Server) error {
	if r == nil || r.durable == nil {
		return nil
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	// The reference's register call takes 1 argument; the frozen context follows it,
	// exactly as every other binder in this lane spells the fence.
	return server.Bind(productionReviewRevisionPrepare, r.operation(1, r.prepare))
}

// operation is the adapter all four names would share: the arity check, the frozen
// three-key context, the borrow, and the single refusal mapping. Nothing in it can
// take a project path, an epoch or a database from the request.
func (r *productionReviewRevisions) operation(arity int,
	call func(context.Context, transport.Caller, *sql.DB, projectsession.Context, []any) (any, error)) transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if ctx == nil || len(args) != arity+1 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		contextValue, err := projectsession.ParseContext(args[arity])
		if err != nil {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
		}
		db, release, err := r.durable.borrowAuthorDatabase(ctx, caller, args[arity])
		if err != nil {
			// Already symbolic: a lease, ownership or closed-lane refusal. It is not a
			// store refusal, so it does not go through the mapper below.
			return nil, err
		}
		if db == nil || release == nil {
			// The bridge never returns a half-open borrow; if it ever does the lane
			// fails closed rather than reading a database nobody admitted.
			return nil, errProductionReviewRevisionStore
		}
		defer release()
		answer, err := call(ctx, caller, db, contextValue, args[:arity])
		if err != nil {
			// The one and only mapping layer.
			return nil, productionReviewRevisionFailure(err)
		}
		return answer, nil
	}
}

// prepare serves `review-revision:prepare`: one capture, one handle. The project id
// is the lease's own, never a body field, because the reference takes it from the
// owning session scope (review-revision-context.ts:39-40).
func (r *productionReviewRevisions) prepare(ctx context.Context, caller transport.Caller, db *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	request, ok := args[0].(map[string]any)
	if !ok || request == nil {
		return nil, errProductionReviewRevisionInvalid
	}
	return productionReviewRevisionPrepareContext(ctx, caller, db, contextValue, request)
}

// productionReviewRevisionPrepareContext is the served prepare over an already
// admitted database: Capture, then Prepare. Its errors are the producer's and the
// registry's own values, unmapped, so the caller can still ask errors.Is which
// refusal it was.
func productionReviewRevisionPrepareContext(ctx context.Context, caller transport.Caller, db *sql.DB,
	contextValue projectsession.Context, request map[string]any) (any, error) {
	frozen, err := store.NewReviewRevisionContexts(db).Capture(ctx, contextValue.ProjectID, request)
	if err != nil {
		return nil, err
	}
	registry, err := productionReviewRevisionRegistry()
	if err != nil {
		return nil, err
	}
	binding := productionReviewRevisionBinding(caller, contextValue.LeaseID, frozen.Operation)
	id, err := registry.Prepare(binding, reviewrevprep.PreparedContext{
		ProjectID:     contextValue.ProjectID,
		Operation:     frozen.Operation,
		DraftID:       frozen.DraftID,
		ChapterNumber: frozen.ChapterNumber,
		Version:       frozen.Version,
		Status:        frozen.Status,
		SourceHash:    frozen.SourceHash,
		Text:          frozen.Text,
		ContextHash:   frozen.ContextHash,
	})
	if err != nil {
		return nil, err
	}
	// `{ contextId, context }` (PreparedReviewRevisionContext without a lineage).
	// The context is the frozen bytes, never a re-encoding of them: the renderer
	// freezes what it receives into the begin request, and JSON.parse followed by
	// JSON.stringify reproduces these bytes exactly.
	return map[string]any{
		"contextId": id,
		"context":   json.RawMessage(frozen.Text),
	}, nil
}

// productionReviewRevisionFreeze is the begin-time half. A begin that carries a
// `reviewRevisionContextId` must freeze the prepared context into the run's source
// manifest before any row is written; a begin that carries none freezes nothing.
//
// It is called by the begin admission (synthetic_generation_linux.go), after the
// lease is admitted and before the model lease is taken, so a refusal here reserves
// nothing.
func productionReviewRevisionFreeze(ctx context.Context, caller transport.Caller, db *sql.DB,
	scope generationowner.Scope, selection *syntheticSelection) (map[string]any, error) {
	handle := ""
	if selection != nil {
		handle = selection.ReviewRevisionContextID
	}
	if handle == "" {
		// The reference refuses a review operation that carries none (:119-121).
		// This lane keeps accepting it, because generation:begin here is generic and
		// its own material-decision probe opens a review-chapter run with no context.
		// Such a run freezes no reviewRevisionContext, so every commit of the family
		// refuses it rather than acting on an unproven context.
		return nil, nil
	}
	if selection == nil || !productionReviewRevisionIsOperation(selection.Operation) {
		// :118 `if (selection.reviewRevisionContextId) throw
		// GENERATION_REVIEW_CONTEXT_INVALID` for a non-review operation.
		return nil, errProductionReviewRevisionInvalid
	}
	registry, err := productionReviewRevisionRegistry()
	if err != nil {
		return nil, err
	}
	return productionReviewRevisionFreezeWith(registry, ctx, caller, db, scope, selection, handle)
}

// productionReviewRevisionFreezeWith is productionReviewRevisionFreeze over an
// explicit registry, so a test can drive the same admission with a clock seam. It
// is the only place the frozen manifest fields are produced, and the lookup it
// performs is a Peek: the reference reads its map without consuming (:120
// `this.contexts.get(...)`), so a retried begin still resolves the same handle.
func productionReviewRevisionFreezeWith(registry *reviewrevprep.Store, ctx context.Context,
	caller transport.Caller, db *sql.DB, scope generationowner.Scope,
	selection *syntheticSelection, handle string) (map[string]any, error) {
	prepared, err := registry.Peek(handle, productionReviewRevisionBinding(caller, scope.Epoch, selection.Operation))
	if err != nil {
		return nil, err
	}
	// The selection must be the one the context was frozen for. This is the block
	// review-revision-generation.ts:122-138 reduces to, checked against the values
	// the producer recorded rather than against a second read of the request.
	prompt := productionReviewRevisionPrompt(prepared.Operation)
	stage := productionReviewRevisionSkillStage(prepared.Operation)
	output := productionReviewRevisionOutput(prepared.Operation)
	finalized := prepared.Status == "finalized"
	if selection.Operation != prepared.Operation ||
		!selection.HasChapterNumber || selection.ChapterNumber != prepared.ChapterNumber ||
		len(selection.PromptKeys) != 1 || selection.PromptKeys[0] != prompt ||
		len(selection.SkillStages) != 1 || selection.SkillStages[0] != stage ||
		selection.Output != output ||
		selection.OutputOverrides != nil ||
		!productionReviewRevisionSameIDs(selection.SelectedDraftIDs, finalized, prepared.DraftID) ||
		!productionReviewRevisionSameFinalizedIDs(selection.SelectedFinalizedDraftIDs, finalized, prepared.DraftID) ||
		!productionReviewRevisionSameAuthorInputs(selection.AuthorInputs, prepared.Text) {
		return nil, errProductionReviewRevisionMismatch
	}
	// The database is the authority: the context must still be exactly what a
	// capture of the same request produces now (:140-143). A moved draft, a moved
	// chapter or a re-finalized history refuses the begin before it opens a run.
	request, err := productionReviewRevisionRequest(prepared)
	if err != nil {
		return nil, errProductionReviewRevisionChanged
	}
	fresh, err := store.NewReviewRevisionContexts(db).Capture(ctx, scope.Project, request)
	if err != nil || fresh.Text != prepared.Text || fresh.ContextHash != prepared.ContextHash {
		return nil, errProductionReviewRevisionChanged
	}
	// The manifest the admission verifier reads: the frozen bytes themselves, their
	// hash, the operation and the single author-input entry
	// (internal/admission/reviewcycle_effect.go:166-207). The bytes are carried as
	// raw JSON so no Go encoder can re-sort or re-escape them; `operation` already
	// equals prepared.Operation because the selection was just proved against it.
	selection.AuthorInputs = []map[string]any{{
		"id":   productionReviewRevisionAuthorInputID,
		"text": prepared.Text,
	}}
	return map[string]any{
		"reviewRevisionContext":     json.RawMessage(prepared.Text),
		"reviewRevisionContextHash": prepared.ContextHash,
	}, nil
}

// productionReviewRevisionRequest rebuilds the request the frozen context was
// captured from, which is reviewRevisionRequest (review-revision-context.ts:23-30).
// Only the fields the admitted operations use are read back: a confirmation and a
// recheck cannot be frozen by this producer, so neither can appear here.
func productionReviewRevisionRequest(prepared reviewrevprep.PreparedContext) (map[string]any, error) {
	var frozen struct {
		UILocale     string `json:"uiLocale"`
		AuthorInputs []any  `json:"authorInputs"`
	}
	if err := json.Unmarshal([]byte(prepared.Text), &frozen); err != nil {
		return nil, err
	}
	if frozen.AuthorInputs == nil {
		return nil, errProductionReviewRevisionChanged
	}
	return map[string]any{
		"operation": prepared.Operation,
		"draftId":   prepared.DraftID,
		"expectedDraft": map[string]any{
			"chapterNumber": prepared.ChapterNumber,
			"version":       prepared.Version,
			"status":        prepared.Status,
			"contentHash":   prepared.SourceHash,
		},
		"authorInputs": frozen.AuthorInputs,
		"uiLocale":     frozen.UILocale,
	}, nil
}

// productionReviewRevisionPrompt is the one prompt key each operation freezes
// (review-revision-generation.ts:124, review-revision-command.ts:126).
func productionReviewRevisionPrompt(operation string) string {
	switch operation {
	case store.ReviewRevisionOperationReviewChapter:
		return "consistency_check"
	case store.ReviewRevisionOperationRefineDraft:
		return "refine_chapter"
	}
	return "refine_from_review"
}

// productionReviewRevisionSkillStage is the one skill stage each operation freezes
// (review-revision-generation.ts:125, review-revision-command.ts:127).
func productionReviewRevisionSkillStage(operation string) string {
	if operation == store.ReviewRevisionOperationReviewChapter {
		return "review"
	}
	return "refinement"
}

// productionReviewRevisionOutput is the output contract each operation freezes
// (review-revision-generation.ts:128).
func productionReviewRevisionOutput(operation string) string {
	if operation == store.ReviewRevisionOperationReviewChapter {
		return "structured-data"
	}
	return "visible-text"
}

// productionReviewRevisionSameIDs is `isDeepStrictEqual(selection.selectedDraftIds,
// finalized ? [] : [context.source.id])` (:129-130) for the working-draft list.
func productionReviewRevisionSameIDs(ids []int64, finalized bool, draftID int64) bool {
	if finalized {
		return len(ids) == 0
	}
	return len(ids) == 1 && ids[0] == draftID
}

// productionReviewRevisionSameFinalizedIDs is the finalized half of the same pair
// (:131).
func productionReviewRevisionSameFinalizedIDs(ids []int64, finalized bool, draftID int64) bool {
	if !finalized {
		return len(ids) == 0
	}
	return len(ids) == 1 && ids[0] == draftID
}

// productionReviewRevisionSameAuthorInputs is `isDeepStrictEqual(selection
// .authorInputs, [{ id: 'review-revision-context', text: JSON.stringify(context) }])`
// (:132). Exactly one entry, exactly the two keys, and a text that is the frozen
// value - compared as JSON rather than as bytes, because the browser re-serializes
// what this lane answered and a key order is not a difference in value.
func productionReviewRevisionSameAuthorInputs(inputs []map[string]any, text string) bool {
	if len(inputs) != 1 {
		return false
	}
	entry := inputs[0]
	if len(entry) != 2 {
		return false
	}
	id, idOK := entry["id"].(string)
	carried, textOK := entry["text"].(string)
	return idOK && textOK && id == productionReviewRevisionAuthorInputID &&
		productionReviewRevisionSameJSON(carried, text)
}

// productionReviewRevisionSameJSON compares two JSON documents as values: decoded,
// then re-encoded by one deterministic encoder, so object key order and integer
// spelling cannot make two equal values disagree.
func productionReviewRevisionSameJSON(left, right string) bool {
	var a, b any
	if json.Unmarshal([]byte(left), &a) != nil || json.Unmarshal([]byte(right), &b) != nil {
		return false
	}
	encodedA, err := json.Marshal(a)
	if err != nil {
		return false
	}
	encodedB, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return bytes.Equal(encodedA, encodedB)
}
