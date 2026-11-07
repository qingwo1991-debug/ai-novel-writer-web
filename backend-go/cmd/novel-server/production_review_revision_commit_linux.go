//go:build linux

package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves the two remaining names of the review-revision family
// (web/operations.json:314-315):
//
//	review-revision:commit-revision   BOUND here (arity 1)
//	review-revision:read-recovery     BOUND here (arity 1)
//
// # The reference
//
//	electron/controllers/generation-controller.ts:242-243
//	  register('review-revision:commit-revision', 1, (owner, request) => owner.commitRevision(request))
//	  register('review-revision:read-recovery', 1, (owner, request) => owner.readReviewRevisionRecovery(request.handle))
//	electron/services/review-revision-generation.ts:234-271
//	  commitRevision(request, assertSources) { ... this.db.transaction(() => { ... }).immediate() }
//	electron/services/review-revision-generation.ts:272-300
//	  readRecovery(handle, sourcesMatch, snapshot): ReviewRevisionRecovery
//	src/shared/review-revision-generation.ts:176-177
//	  'review-revision:commit-revision': { args: [RevisionGenerationCommitRequest]; return: ReviewRevisionCommitReceipt }
//	  'review-revision:read-recovery': { args: [{ handle: MainGenerationRunHandle }]; return: ReviewRevisionRecovery }
//	src/services/workflows/commands/review-revision-command.ts:172-174
//	  ipc.invokeWithProjectSession(session, 'review-revision:commit-revision', {
//	    contextId: prepared.contextId, handle, expectedCompositionHash: composition.textHash })
//	src/services/workflows/review-revision-recovery-workflow.ts:14
//	  ipc.invokeWithProjectSession(session, 'review-revision:read-recovery', { handle })
//
// The wire shape is therefore exactly the reference's: commit-revision carries
// `{contextId, handle, expectedCompositionHash}` and read-recovery carries
// `{handle}`, each with the frozen three-key project context as the trailing
// argument, because both controller rows are arity 1.
//
// # What the two names answer
//
// commit-revision is one BEGIN IMMEDIATE transaction
// (internal/store/reviewrevision_commit_tx.go): the pending revision row, its
// content row, the discarded-pending sweep and the effect stamped on the attempt
// that settled the last composed artifact, then the receipt re-read from those
// rows. The body is never taken off the wire: the composition the run already
// stored is recomposed from the run's own artifacts and must hash to the value
// the caller named, exactly as readVisibleComposition does
// (generation-run-repository.ts:659-674). So a refusal at any point - including
// the material-length gate and the no-op gate, which run before the first INSERT
// - leaves the author's database file byte for byte as it was found.
//
// read-recovery is a read: one deferred transaction over the run, its frozen
// context, its receipt, its composition, its attempts and its latest candidate,
// answering the reference's ReviewRevisionRecovery shape. Nothing is written, so
// a refused recovery cannot change a byte either.
//
// # The one field this lane cannot prove
//
// `sourceStatus` is `compareGenerationSourceBindings(run.binding, rebuildBinding(
// run.binding, currentModelReceipt(run.binding)))` (main-generation-owner.ts:
// 1005-1007), and rebuildBinding is buildGenerationSourceBinding over every
// material the run froze. That rebuild has no Go port, and a narrower comparison
// would answer 'current' where the reference answers 'conflict' - the unsafe
// direction, because the renderer reads canResume to decide whether to spend
// another model call. The store therefore answers the reference's own fallback,
// 'conflict' (review-revision-generation.ts:276 catch), and canResume is false by
// the reference's own formula. A committed run is still fully readable: the
// renderer's workflow skips its source gate when `saved` is present
// (review-revision-recovery-workflow.ts:20), and `saved` is proved here against
// the revision row, its frozen source and the composition it was composed from.
//
// # Why the served count does not move in this file
//
// The binder below is the family's own second half, but its only call site is
// production_open_linux.go:596, which installs the family binder this batch may
// not edit. Until that line gains
//
//	if bindErr := newProductionReviewRevisionCommits(open.durable).Bind(server); bindErr != nil {
//
// beside `newProductionReviewRevisions(open.durable).Bind(server)`, the built lane
// still binds 238 names and production_review_inline_residual_linux.go still
// records these two as refused, because its test asserts that every name the
// record calls served is really bound. The two names are therefore served by this
// file (and proved over real HTTP by its test) but not yet installed by the lane:
// the wiring line above plus productionServedOperations 238 -> 240 is the change
// that lands them.
type productionReviewRevisionCommits struct {
	durable *productionDurableOpen
}

// The two names this file binds, spelled once so a test and the binder cannot
// drift apart.
const (
	productionReviewRevisionCommitRevisionName = "review-revision:commit-revision"
	productionReviewRevisionReadRecoveryName   = "review-revision:read-recovery"
)

// productionReviewRevisionCommitRefusals is the four codes only a revision commit
// can raise, paired with the store sentinels that raise them. The rest of the
// family's codes are resolved by productionReviewRevisionCode, so this table never
// restates a neighbour's row.
var productionReviewRevisionCommitRefusals = [...]struct {
	refusal error
	code    string
}{
	{store.ErrRevisionCommitCompositionRequired, "GENERATION_REVIEW_COMPOSITION_REQUIRED"},
	{store.ErrRevisionCommitEmpty, "GENERATION_REVIEW_REVISION_EMPTY"},
	{store.ErrRevisionCommitIncomplete, "GENERATION_REVIEW_REVISION_INCOMPLETE"},
	{store.ErrRevisionCommitNoop, "GENERATION_REVIEW_REVISION_NOOP"},
}

// newProductionReviewRevisionCommits wraps the lane's durable bridge, the only
// object that can name the author database.
func newProductionReviewRevisionCommits(durable *productionDurableOpen) *productionReviewRevisionCommits {
	return &productionReviewRevisionCommits{durable: durable}
}

// Bind installs `review-revision:commit-revision` and
// `review-revision:read-recovery`. A nil or closed bridge installs nothing, so
// both names keep failing closed at GO_OPERATION_NOT_MIGRATED instead of being
// published as handlers that can only refuse.
func (r *productionReviewRevisionCommits) Bind(server *transport.Server) error {
	if r == nil || r.durable == nil {
		return nil
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if err := server.Bind(productionReviewRevisionCommitRevisionName, r.operation(1, r.commitRevision)); err != nil {
		return err
	}
	return server.Bind(productionReviewRevisionReadRecoveryName, r.operation(1, r.readRecovery))
}

// operation is the adapter both names share: the arity check, the frozen
// three-key context, the borrow, and the single refusal mapping. Nothing in it can
// take a project path, an epoch or a database from the request.
func (r *productionReviewRevisionCommits) operation(arity int,
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
			// Already symbolic: a lease, ownership or closed-lane refusal.
			return nil, err
		}
		if db == nil || release == nil {
			return nil, errProductionReviewRevisionStore
		}
		defer release()
		answer, err := call(ctx, caller, db, contextValue, args[:arity])
		if err != nil {
			return nil, productionReviewRevisionCommitFailure(err)
		}
		return answer, nil
	}
}

// commitRevision serves `review-revision:commit-revision`.
func (r *productionReviewRevisionCommits) commitRevision(ctx context.Context, caller transport.Caller, db *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	request, ok := args[0].(map[string]any)
	if !ok || request == nil {
		return nil, errProductionReviewRevisionInvalid
	}
	parsed, err := productionReviewRevisionCommitRevisionRequest(request, contextValue)
	if err != nil {
		return nil, err
	}
	registry, err := productionReviewRevisionRegistry()
	if err != nil {
		return nil, err
	}
	// assertPrepared (:243 `this.assertPrepared(request.contextId, run)`): the handle
	// must be the one this caller minted for this run, and it must still carry the
	// bytes the run froze. The purpose is bound per call from the run's own
	// operation, so a handle prepared for a review cannot be spent on a revision.
	parsed.Request.AssertPrepared = productionReviewRevisionCommitPrepared(registry, caller,
		contextValue.LeaseID, contextValue.ProjectID, parsed.ContextID)
	return store.NewReviewRevisions(db).CommitRevision(ctx, parsed.Request)
}

// readRecovery serves `review-revision:read-recovery`. The request is exactly
// `{handle}` (generation-controller.ts:243 reads `request.handle` and nothing
// else), and the answer is the store's projection of one read snapshot.
func (r *productionReviewRevisionCommits) readRecovery(ctx context.Context, caller transport.Caller, db *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	request, ok := args[0].(map[string]any)
	if !ok || request == nil {
		return nil, errProductionReviewRevisionInvalid
	}
	parsed, err := productionReviewRevisionRecoveryRequest(request, contextValue)
	if err != nil {
		return nil, err
	}
	return store.NewReviewRevisions(db).ReadRevisionRecovery(ctx, parsed)
}

// productionReviewRevisionCommitParsed is the parsed commit-revision request: the
// handle the assertion closure resolves, and the store request the write runs on.
type productionReviewRevisionCommitParsed struct {
	ContextID string
	Request   store.ReviewRevisionCommitRequest
}

// productionReviewRevisionCommitRevisionRequest reads RevisionGenerationCommitRequest
// (src/shared/review-revision-generation.ts:139-143): exactly
// `{contextId, handle, expectedCompositionHash}`. The handle's project is left in
// the request rather than compared here: the reference's requireRun compares it
// with the run it is about to write and raises GENERATION_RUN_IDENTITY_MISMATCH,
// which is the answer the store gives and the answer this parser preserves.
func productionReviewRevisionCommitRevisionRequest(request map[string]any,
	contextValue projectsession.Context) (productionReviewRevisionCommitParsed, error) {
	var parsed productionReviewRevisionCommitParsed
	if !productionReviewRevisionExactKeys(request, "contextId", "handle", "expectedCompositionHash") {
		return parsed, errProductionReviewRevisionInvalid
	}
	contextID, ok := productionReviewRevisionText(request["contextId"])
	if !ok {
		return parsed, errProductionReviewRevisionInvalid
	}
	handle, ok := request["handle"].(map[string]any)
	if !ok || !productionReviewRevisionExactKeys(handle, "projectId", "epoch", "rootActionId", "runId") {
		return parsed, errProductionReviewRevisionInvalid
	}
	projectID, projectOK := productionReviewRevisionText(handle["projectId"])
	epoch, epochOK := productionReviewRevisionText(handle["epoch"])
	rootActionID, rootOK := productionReviewRevisionText(handle["rootActionId"])
	runID, runOK := productionReviewRevisionText(handle["runId"])
	compositionHash, hashOK := productionReviewRevisionText(request["expectedCompositionHash"])
	if !projectOK || !epochOK || !rootOK || !runOK || !hashOK {
		return parsed, errProductionReviewRevisionInvalid
	}
	parsed.ContextID = contextID
	parsed.Request = store.ReviewRevisionCommitRequest{
		ProjectID: contextValue.ProjectID,
		// The live lease, never the body's: a commit is fenced on the epoch the
		// author's session holds right now (review-revision-generation.ts:225-227).
		LiveEpoch: contextValue.LeaseID,
		Handle: store.ReviewCommitHandle{
			ProjectID:    projectID,
			Epoch:        epoch,
			RootActionID: rootActionID,
			RunID:        runID,
		},
		ExpectedCompositionHash: compositionHash,
	}
	return parsed, nil
}

// productionReviewRevisionRecoveryRequest reads `{handle}`
// (src/shared/review-revision-generation.ts:177). The outer object is exactly one
// key: the renderer sends the handle alone, and a body that names a project, an
// epoch or a path is a request this lane never agreed to serve.
func productionReviewRevisionRecoveryRequest(request map[string]any,
	contextValue projectsession.Context) (store.ReviewRevisionRecoveryRequest, error) {
	var parsed store.ReviewRevisionRecoveryRequest
	if !productionReviewRevisionExactKeys(request, "handle") {
		return parsed, errProductionReviewRevisionInvalid
	}
	handle, ok := request["handle"].(map[string]any)
	if !ok || !productionReviewRevisionExactKeys(handle, "projectId", "epoch", "rootActionId", "runId") {
		return parsed, errProductionReviewRevisionInvalid
	}
	projectID, projectOK := productionReviewRevisionText(handle["projectId"])
	epoch, epochOK := productionReviewRevisionText(handle["epoch"])
	rootActionID, rootOK := productionReviewRevisionText(handle["rootActionId"])
	runID, runOK := productionReviewRevisionText(handle["runId"])
	if !projectOK || !epochOK || !rootOK || !runOK {
		return parsed, errProductionReviewRevisionInvalid
	}
	parsed.ProjectID = contextValue.ProjectID
	parsed.Handle = store.ReviewCommitHandle{
		ProjectID:    projectID,
		Epoch:        epoch,
		RootActionID: rootActionID,
		RunID:        runID,
	}
	return parsed, nil
}

// productionReviewRevisionCommitFailure is the refusal the browser receives: the
// transport's own error unchanged, one of the two names' codes, a neighbour's code
// from the family table, a store code that is already symbolic, or the family's
// invalid code. Never a SQLite message and never a draft body.
func productionReviewRevisionCommitFailure(err error) error {
	if err == nil {
		return nil
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return public
	}
	if code := productionReviewRevisionCommitCode(err); code != "" {
		return errors.New(code)
	}
	return errProductionReviewRevisionInvalid
}

// productionReviewRevisionCommitCode resolves a refusal to its symbolic code.
func productionReviewRevisionCommitCode(err error) string {
	if err == nil {
		return ""
	}
	for _, entry := range productionReviewRevisionCommitRefusals {
		if errors.Is(err, entry.refusal) {
			return entry.code
		}
	}
	if code := productionReviewRevisionCode(err); code != "" {
		return code
	}
	// The store's composition reader and the ledger's root admission raise their
	// own symbolic codes (GENERATION_COMPOSITION_INTEGRITY_FAILED,
	// ARTIFACT_INTEGRITY_FAILED, GENERATION_OPERATION_UNSUPPORTED). They are
	// published as themselves; anything that is not one all-caps constant is not a
	// code and falls through to the family's invalid answer.
	if productionReviewRevisionCommitSymbolic(err.Error()) {
		return err.Error()
	}
	return ""
}

// productionReviewRevisionCommitSymbolic reports whether a message is one
// uppercase symbolic constant: no space, no punctuation, no path, no quote.
func productionReviewRevisionCommitSymbolic(text string) bool {
	if len(text) < 2 || text[0] < 'A' || text[0] > 'Z' {
		return false
	}
	for index := 1; index < len(text); index++ {
		character := text[index]
		if (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '_' {
			continue
		}
		return false
	}
	return true
}
