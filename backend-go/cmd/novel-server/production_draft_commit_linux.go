//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves generation:commit-draft in the production lane: the write of
// the chapter-draft family, wire arity 1, the request plus the frozen session
// context.
//
// The reference handler is electron/controllers/generation-controller.ts:271-274,
// which is `readActionContext(owner, request.handle)` followed by
// `owner.commitDraft(request)`; the owner body is main-generation-owner.ts:
// 1539-1545 and the effect itself is GenerationDraftEffects.commit
// (electron/services/generation-draft-effects.ts:154-203) plus the recomposition
// in GenerationRunRepository.visibleComposition
// (electron/repositories/generation-run-repository.ts:460-516). The wire types are
// src/shared/generation-owner-contract.ts:131-137 (request) and :118-128 (receipt).
//
// # Where the work happens
//
// The transaction cannot be assembled from this package: it has to open
// BEGIN IMMEDIATE and, before it closes, read the batch rows it just extended,
// write contents and drafts, and stamp the receipt into the attempt that produced
// the composition's last artifact. store.CommitGenerationDraft
// (internal/store/draft_commit_tx.go) is that transaction, statement for statement;
// this file is the fence in front of it and the envelope behind it:
//
//	admit          the frozen three-key session and the live writer lease
//	parse          the four request fields, no key allowlist (the reference has none)
//	requireRun     the run identity, with the reference's two codes
//	commit         the one IMMEDIATE transaction
//
// # The admission
//
// admit is the gate generation:begin and generation:read already use
// (synthetic_generation_linux.go:318-336): the frozen session, then the live
// writer lease, then the ledger and the author's own database under one
// admission. A stale, foreign or closed lease is refused there, before a row is
// read, and the release closure is this file's only handle to the database.
//
// commit-draft deliberately does NOT take production_generation_write_linux.go:243's
// `mutation()` gate: that gate additionally requires a committed model-egress
// policy and the executor built from it, and this operation dials nothing. It
// writes rows the shipped ledger already owns.
//
// # Codes this file does not raise, and why
//
//   - GENERATION_DRAFT_PREPARATION_*: the commit path never reads a preparation
//     (main-generation-owner.ts:1539-1545).
//   - GENERATION_SOURCE_CHANGED: assertSources' last step is
//     deps.rebuildBinding(), which has no Go port; see the reduction list in
//     internal/store/draft_commit_tx.go.
//   - the knowledge-source gate: guardKnowledge only runs when the frozen
//     manifest carries a knowledgeSnapshot, and no binder in this lane can freeze
//     one (generation:begin's selection has no such key,
//     synthetic_generation_linux.go:396-401), so the gate is unreachable here
//     rather than skipped.
//   - GENERATION_DRAFT_REPLAY_NOT_SAVED
//     (main-generation-owner.ts:1543): that arrow function is never invoked,
//     because readCommit returning a receipt is handled inside commit itself.
//
// # The envelope
//
// productionGenerationMutation is this family's mutating adapter
// (production_generation_write_linux.go:186-224, a copy of
// internal/transport/generation.go:260-304): the wire arity is the request plus the
// frozen context at args[arity], that context must be exactly the three frozen
// keys, and every failure leaves as the business DTO
// {success:false,error:"Error: CODE"} - the shape the renderer's
// invokeWithProjectSession rejects on - instead of a 500 that would carry a
// database diagnostic.

// productionDraftCommit is this file's one service value: the owner it projects
// through. It is built by newProductionDraftCommit over the same *productionGeneration
// generation:begin already uses.
type productionDraftCommit struct {
	owner *productionGeneration
}

// newProductionDraftCommit builds the service. A nil owner installs nothing: Bind
// then leaves generation:commit-draft failing closed at
// GO_OPERATION_NOT_MIGRATED instead of half-admitting it.
func newProductionDraftCommit(owner *productionGeneration) *productionDraftCommit {
	return &productionDraftCommit{owner: owner}
}

// Bind installs exactly one name, one server.Bind call
// (internal/transport/server.go:73-77), the same way production_draft_context_linux.go:
// 250-258 installs its two. BindGeneration cannot be used: it installs thirteen
// names atomically (internal/transport/generation.go:37), including the five this
// lane already binds through BindGenerationBegin/Execute/Lifecycle, so a second
// BindGeneration is OPERATION_ALREADY_BOUND.
func (c *productionDraftCommit) Bind(server *transport.Server) error {
	if c == nil || c.owner == nil || c.owner.inner == nil {
		// Nothing to serve: the name stays unbound, so the server keeps answering
		// GO_OPERATION_NOT_MIGRATED instead of half-admitting the operation.
		return nil
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	return server.Bind("generation:commit-draft", productionGenerationMutation(1,
		func(ctx context.Context, caller transport.Caller, expected map[string]any, args []any) (any, error) {
			return c.Commit(ctx, caller, expected, args[0])
		}))
}

// Commit answers generation:commit-draft: it admits the session, resolves the run
// under that lease, and hands the transaction to the store.
func (c *productionDraftCommit) Commit(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	if c == nil || c.owner == nil || c.owner.inner == nil {
		return nil, errProductionGenerationUnavailable
	}
	// The request carries no content (GenerationDraftCommitRequest, :131-137), so
	// the only fields parsed here are the four the server may trust.
	handle, request, err := productionDraftCommitParse(arg)
	if err != nil {
		return nil, err
	}
	_, scope, ledger, db, release, err := c.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer release()
	if ledger == nil || db == nil {
		return nil, errProductionGenerationUnavailable
	}
	if _, _, err := productionDraftCommitRun(ctx, ledger, scope, handle); err != nil {
		return nil, err
	}
	// assertSources' live epoch is the lease this admission just proved. The
	// transaction compares it with the run's frozen epoch at the reference's own
	// point - after the duplicate replay, never before it.
	request.Handle = store.BatchRunHandle{
		ProjectID:    handle.ProjectID,
		Epoch:        handle.Epoch,
		RootActionID: handle.RootActionID,
		RunID:        handle.RunID,
	}
	request.LiveEpoch = scope.Epoch
	receipt, err := store.CommitGenerationDraft(ctx, db, request)
	if err != nil {
		return nil, err
	}
	return productionDraftCommitReceipt(receipt), nil
}

// productionDraftCommitParse is the request projection. The reference does not
// validate an allowlist here: the controller only checks the session envelope and
// the arity, and commit compares the fields it cares about with `!==`. A field of
// the wrong JSON type therefore fails that comparison rather than the parse, which
// is why an absent chapterNumber becomes a value no manifest can hold.
func productionDraftCommitParse(arg any) (syntheticHandle, store.DraftCommitRequest, error) {
	request := store.DraftCommitRequest{}
	object := syntheticMap(arg)
	if object == nil {
		return syntheticHandle{}, request, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	// requireRun's first act is repository.get(handle.runId): a handle without the
	// four frozen keys cannot name a run.
	handle, err := syntheticParseHandle(object["handle"])
	if err != nil {
		return syntheticHandle{}, request, err
	}
	request.Source = syntheticString(object["source"])
	request.BatchID = syntheticString(object["batchId"])
	request.ExpectedCompositionHash = syntheticString(object["expectedCompositionHash"])
	request.ChapterNumber = productionDraftCommitChapter(object["chapterNumber"])
	return handle, request, nil
}

// productionDraftCommitChapter is the request's chapterNumber as a number the
// manifest can only match when the wire really carried that integer. Anything else
// becomes -1, which no chapter number can equal, so the target check refuses it
// with GENERATION_DRAFT_TARGET_INVALID exactly like `"1" !== 1` does in the
// reference.
func productionDraftCommitChapter(value any) int64 {
	switch number := value.(type) {
	case float64:
		if number != float64(int64(number)) {
			return -1
		}
		return int64(number)
	case int64:
		return number
	case int:
		return int64(number)
	case json.Number:
		if parsed, err := number.Int64(); err == nil {
			return parsed
		}
	}
	return -1
}

// productionDraftCommitRun is requireRun(handle) (main-generation-owner.ts:156-164)
// without the execution flag: the run must exist, belong to this project, and
// match the handle on all four keys. Both codes are the reference's own -
// GENERATION_RUN_NOT_FOUND when the row is absent, GENERATION_RUN_IDENTITY_MISMATCH
// when it is another run's - because the controller runs this through
// readActionContext before the transaction opens.
func productionDraftCommitRun(ctx context.Context, ledger *store.GenerationLedger, scope generationowner.Scope, handle syntheticHandle) (map[string]any, map[string]any, error) {
	value, err := syntheticCall(ctx, ledger, "get", handle.RunID)
	if err != nil {
		// The ledger reports an absent row as GENERATION_RUN_MISSING from its own
		// guard (internal/store/generation_ledger.go:369); the reference's requireRun
		// reports the same absence as GENERATION_RUN_NOT_FOUND
		// (main-generation-owner.ts:156-158), which is the code the renderer
		// recognises, so the absence is renamed here and every other ledger failure
		// passes through untouched.
		if err.Error() == "GENERATION_RUN_MISSING" {
			return nil, nil, errors.New("GENERATION_RUN_NOT_FOUND")
		}
		return nil, nil, err
	}
	run := syntheticMap(value)
	if run == nil {
		return nil, nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	binding := syntheticMap(run["binding"])
	if binding == nil {
		return nil, nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	manifest := syntheticMap(binding["sourceManifest"])
	if manifest == nil || syntheticString(run["rootActionId"]) != handle.RootActionID ||
		syntheticString(run["runId"]) != handle.RunID ||
		syntheticString(binding["projectId"]) != scope.Project || handle.ProjectID != scope.Project ||
		syntheticString(binding["epoch"]) != handle.Epoch {
		return nil, nil, errors.New("GENERATION_RUN_IDENTITY_MISMATCH")
	}
	return binding, manifest, nil
}

// productionDraftCommitReceipt is the wire receipt (:118-128): the five fields the
// renderer compares against the text it asked to save, and nothing else - no path,
// no ledger row, no attempt id.
func productionDraftCommitReceipt(receipt *store.DraftCommitReceipt) map[string]any {
	return map[string]any{
		"success":     true,
		"id":          receipt.ID,
		"version":     receipt.Version,
		"contentHash": receipt.ContentHash,
		"content":     receipt.Content,
	}
}
