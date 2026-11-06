//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves `finalized-character:read-context` (web/operations.json:199),
// the first operation of the finalization family and the read every later bind
// of that family compares against. It binds that one name and installs nothing
// else, so `finalized-character:commit`, `-list-state-candidates`,
// `-read-state-candidate` and `-decide-state-candidate` keep failing closed at
// 501 GO_OPERATION_NOT_MIGRATED: none of them can be served yet, and a handler
// installed over a missing piece would be worse than an unbound name.
//
// # The reference
//
//	electron/controllers/generation-controller.ts:218
//	  register('finalized-character:read-context', 1, (owner, request) => owner.readFinalizedCharacterContext(request.draftId))
//	electron/services/main-generation-owner.ts:1512
//	  readFinalizedCharacterContext: (draftId: number) => finalizedCharacters.readContext(draftId)
//	electron/services/finalized-character-generation.ts:20-28
//	  if (!Number.isSafeInteger(draftId) || draftId < 1) throw new Error('GENERATION_CHARACTER_DRAFT_INVALID')
//	  const context = SummaryRepository.readFinalizedCharacterContext(draftId, this.scope, this.db)
//	  const contextId = randomUUID()
//	  if (this.contexts.size >= 64) this.contexts.delete(this.contexts.keys().next().value!)
//	  this.contexts.set(contextId, structuredClone(context))
//	  return { contextId, context: structuredClone(context), originProjectId: this.transferOrigin() }
//
// The wire declaration is src/shared/finalized-character-generation.ts:21:
//
//	'finalized-character:read-context': { args: [{ draftId: number }];
//	  return: { contextId: string; context: FinalizedCharacterContext; originProjectId?: string } }
//
// and the renderer stores the answer, then reads it field by field
// (src/services/workflows/commands/finalize-chapter.command.ts:285-288): it
// compares context.source.finalizationId, .contentHash, .chapterNumber and
// context.content against the draft it finalized and refuses
// FINALIZED_CHARACTER_SOURCE_CHANGED itself when they differ. That is why the
// answer has to carry the store's own field names rather than a re-naming DTO:
// the store value (internal/store/finalized_character_context.go:131-141) is
// already FinalizedCharacterContext (src/shared/finalized-continuity.ts:113-124),
// field for field and in the reference's order, so it is returned as it is.
//
// # The answer, field by field
//
//	contextId        the reference's fresh per-call opaque id (:23)
//	context          store.FinalizedCharacterContext: projectId and epoch (the
//	                 admitted scope), source {draftId, finalizationId,
//	                 chapterNumber, contentHash} (:416), content, projectionGeneration,
//	                 identityRevision, identityStatus (:417), sourceOrder
//	                 {continuityEpoch, chapterNumber, authoritativeFinalizationRevision} (:418-419)
//	                 and characters [{characterId, displayNameSnapshot, aliases, fields}] (:414)
//	originProjectId  absent (see below)
//
// originProjectId is `this.transferOrigin()` (finalized-character-generation.ts:27),
// the main-verified transfer authority of a cross-project transfer. This lane
// has no transfer origin: nothing in this module records one, and the renderer's
// only consumer (finalizedCharacterPromptCards, src/shared/finalized-continuity.ts:126)
// treats the argument as optional and renders `undefined` as "no transfer". The
// key is therefore omitted rather than filled with the browser's project id,
// which would fabricate authority the reference keeps in the main process.
//
// # The context id
//
// The reference mints a random id per call and caches the context under it in a
// 64-entry Map, because its own `commit` (:69-72) looks the id up and compares
// the cached context with the proven one, and its `admit` (:36-58) requires the
// browser to hand the same id back for a `finalized-character-state` run.
//
// This lane serves neither: `finalized-character:commit` is unbound (the
// finalized-character family's later names are still 501), no begin in this lane
// freezes a `finalized-character-state` selection, and
// internal/store/finalized_character_context.go has no commit for the id to
// reach. So a cache here would be state nothing can honour: it would have to be
// process-local, per-tab, bounded and invalidated by any writer, and a commit
// that trusted a browser-chosen key in it would accept an id the client chose
// instead of one the server proved. The honest answer is that the id is not
// needed by anything this lane serves yet, so no cache is installed and the id
// resolves to nothing here.
//
// The id is still minted fresh per call, in the reference's own format (a
// version 4 UUID from the crypto random source), so the shape the renderer
// stores and will one day send back is the shape the reference sends. Whatever
// serves `finalized-character:commit` must not trust it: the reference's commit
// itself re-reads the context and proves it (proveFinalizedCharacterGeneration
// plus an isDeepStrictEqual against the cached context, :71-74), so that lane has
// to derive the context from the admitted database again and compare, or replace
// the id with a value it can re-derive (a context digest) - not read back a
// process-local cache.
//
// # The admission
//
// The scope is the frozen project epoch, never anything from the request body:
// the request is `{ draftId }` and the frozen three-key context is the last wire
// argument. productionDurableOpen.borrowAuthorDatabase
// (production_durable_linux.go:313-360) parses that context, proves the caller
// and the tab, refuses a project this lane did not open, and re-proves the live
// lease through the runtime before handing over one (*sql.DB, release) pair; the
// release re-verifies the permit, so a read commits nothing. The scope is then
// the borrowed project and the borrowed lease, exactly the way the two shipped
// store-scoped binders spell a ProjectEpoch
// (production_generation_write_linux.go:357, production_blueprint_write_linux.go:169).
// A stale, foreign, closed or absent lease is refused there before a row is read,
// which is the Go side of the reference's assertCurrent (:21) and of
// projectAccess.assertCurrentProjectContext (generation-controller.ts:186).
//
// # The store call
//
// store.FinalizedCharacters.ReadContext (internal/store/finalized_character_context.go:185-204)
// opens its own `BeginTx(ReadOnly: true)` and commits it without a write, so the
// adapter calls that and does NOT wrap it in a second transaction:
// ReadFinalizedCharacterContextTx takes a caller-owned tx and would nest a
// read-only transaction inside another one for no gain. Because of that choice
// the port-only refusal ErrFinalizedCharacterReadTransactionRequired has no
// producer in this file; it is still mapped below so a future nesting variant
// cannot leak an unmapped error.
//
// # The refusals
//
// The reference throws; this lane answers the same symbolic code. The store's
// own values already carry those strings, and the table below is the one place
// that says so, so the wire code and the store value cannot drift apart
// unnoticed. Nothing else is mapped: a SQLite diagnostic, a path or a provider
// message is replaced by the reference's own unmapped-failure code
// (generationIpcErrorCode's last line, generation-controller.ts:101).
type productionFinalizedCharacterReads struct {
	durable *productionDurableOpen
}

// productionFinalizedCharacterContextName is the one name this file binds.
const productionFinalizedCharacterContextName = "finalized-character:read-context"

const (
	// productionFinalizedCharacterDraftInvalid is the reference's request guard
	// (finalized-character-generation.ts:22). The controller enforces the arity
	// and the three-key session (generation-controller.ts:184) and no request key
	// allowlist, so an extra key in `{ draftId }` is not refused here either.
	productionFinalizedCharacterDraftInvalid = "GENERATION_CHARACTER_DRAFT_INVALID"
	// productionFinalizedCharacterRequestFailed is generation-controller.ts:101:
	// every failure this lane cannot name symbolically.
	productionFinalizedCharacterRequestFailed = "GENERATION_REQUEST_FAILED"
	// productionFinalizedCharacterMaxSafeInteger is Number.MAX_SAFE_INTEGER, the
	// bound Number.isSafeInteger uses.
	productionFinalizedCharacterMaxSafeInteger = 9007199254740991
)

// productionFinalizedCharacterReadRefusals is the store's refusal set with the
// code each one is published under. Every code is the value's own string; the
// table exists so the mapping is stated once, next to the line that raises it.
var productionFinalizedCharacterReadRefusals = [...]struct {
	refusal error
	code    string
}{
	// summary-repository.ts:402 !database || !projectId?.trim() || !epoch?.trim()
	{store.ErrFinalizedCharacterScopeRequired, "FINALIZED_CHARACTER_SCOPE_REQUIRED"},
	// summary-repository.ts:406 and :409
	{store.ErrFinalizedCharacterSourceChanged, "FINALIZED_CHARACTER_SOURCE_CHANGED"},
	// summary-repository.ts:413
	{store.ErrFinalizedCharacterIDRetired, "FINALIZED_CHARACTER_ID_RETIRED"},
	// summary-repository.ts:84
	{store.ErrFinalizedCharacterFieldRevisionInvalid, "FINALIZED_CHARACTER_FIELD_REVISION_INVALID"},
	// summary-repository.ts:75, already published by the summary port
	// (internal/store/summary.go:22) and reused here instead of a second value.
	{store.ErrSummaryCharacterSnapshotInvalid, "FINALIZED_CHARACTER_SNAPSHOT_INVALID"},
	// Port-only precondition (finalized_character_context.go:41-45); unreachable
	// through ReadContext, which opens its own transaction.
	{store.ErrFinalizedCharacterReadTransactionRequired, "FINALIZED_CHARACTER_READ_TRANSACTION_REQUIRED"},
}

// newProductionFinalizedCharacterReads wraps the lane's durable bridge, the only
// object that can name the author database.
func newProductionFinalizedCharacterReads(durable *productionDurableOpen) *productionFinalizedCharacterReads {
	return &productionFinalizedCharacterReads{durable: durable}
}

// Bind installs `finalized-character:read-context` and nothing else. A nil or
// closed bridge installs nothing, so the name keeps failing closed instead of
// being published as a handler that can only refuse.
func (r *productionFinalizedCharacterReads) Bind(server *transport.Server) error {
	if r == nil || r.durable == nil {
		return nil
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	return server.Bind(productionFinalizedCharacterContextName,
		func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
			// Wire arity is the request arity (one) plus the frozen context.
			if len(args) != 2 {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			admitted, err := projectsession.ParseContext(args[1])
			if err != nil {
				// The reference refuses a missing or malformed session with
				// GENERATION_PROJECT_SESSION_REQUIRED (:184). The frozen context is
				// this lane's transport envelope, and every binder in it answers
				// with the transport's own code.
				return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
			}
			draftID, err := productionFinalizedCharacterDraft(args[0])
			if err != nil {
				return nil, productionFinalizedCharacterReadFailure(err)
			}
			if ctx == nil {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			db, release, err := r.durable.borrowAuthorDatabase(ctx, caller, args[1])
			if err != nil {
				return nil, err
			}
			if db == nil || release == nil {
				// The bridge never returns a half-open borrow; if it ever does the
				// lane fails closed rather than reading a database nobody admitted.
				return nil, errors.New(productionFinalizedCharacterRequestFailed)
			}
			defer release()
			answer, err := productionFinalizedCharacterReadContext(ctx, db, draftID,
				store.FinalizedCharacterScope{ProjectID: admitted.ProjectID, Epoch: admitted.LeaseID})
			if err != nil {
				return nil, productionFinalizedCharacterReadFailure(err)
			}
			return answer, nil
		})
}

// productionFinalizedCharacterDraft is the request projection: `{ draftId }`,
// the only field the reference reads. The controller has already enforced the
// arity, so a non-object request has no draft id and is the reference's own
// GENERATION_CHARACTER_DRAFT_INVALID, not a transport error.
func productionFinalizedCharacterDraft(value any) (int64, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return 0, errors.New(productionFinalizedCharacterDraftInvalid)
	}
	var number float64
	switch raw := object["draftId"].(type) {
	case float64:
		number = raw
	case json.Number:
		parsed, err := raw.Float64()
		if err != nil {
			return 0, errors.New(productionFinalizedCharacterDraftInvalid)
		}
		number = parsed
	default:
		return 0, errors.New(productionFinalizedCharacterDraftInvalid)
	}
	// Number.isSafeInteger(draftId) && draftId >= 1
	if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number ||
		number < 1 || number > productionFinalizedCharacterMaxSafeInteger {
		return 0, errors.New(productionFinalizedCharacterDraftInvalid)
	}
	return int64(number), nil
}

// productionFinalizedCharacterReadContext is the served read over an already
// admitted database and the admitted scope. It is split out of the handler so
// the answer and its refusals can be exercised over an admitted fixture without
// an HTTP round trip, and so nothing in it can quietly take a project id or an
// epoch from the request.
func productionFinalizedCharacterReadContext(ctx context.Context, db *sql.DB, draftID int64,
	scope store.FinalizedCharacterScope) (map[string]any, error) {
	contextID, err := productionFinalizedCharacterContextID()
	if err != nil {
		return nil, productionFinalizedCharacterReadFailure(err)
	}
	// One read-only transaction, opened and committed by the store itself.
	served, err := store.NewFinalizedCharacters(db).ReadContext(ctx, draftID, scope)
	if err != nil {
		return nil, productionFinalizedCharacterReadFailure(err)
	}
	// The two keys of the reference's return value (:25). A JSON null for the
	// context is not possible here: ReadContext refuses instead of answering
	// nothing.
	return map[string]any{"contextId": contextID, "context": served}, nil
}

// productionFinalizedCharacterContextID mints the reference's opaque per-call id
// (finalized-character-generation.ts:23) in the RFC 4122 version 4 shape its
// crypto randomUUID answers. It is not a cache key: see the file header for why
// this lane installs no cache and why a later commit must re-derive the context
// rather than trust this value.
func productionFinalizedCharacterContextID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40 // version 4
	raw[8] = raw[8]&0x3f | 0x80 // RFC 4122 variant
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

// productionFinalizedCharacterReadCode is the reference's symbolic code for a
// store refusal, or "" for anything this lane does not name.
func productionFinalizedCharacterReadCode(err error) string {
	for _, entry := range productionFinalizedCharacterReadRefusals {
		if errors.Is(err, entry.refusal) {
			return entry.code
		}
	}
	return ""
}

// productionFinalizedCharacterReadFailure is the refusal the browser receives:
// the transport's own error unchanged, the store refusal's symbolic code, or the
// reference's unmapped-failure code. Never a SQLite message, a path, a draft body
// or a provider sentence.
func productionFinalizedCharacterReadFailure(err error) error {
	if err == nil {
		return nil
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return public
	}
	if code := productionFinalizedCharacterReadCode(err); code != "" {
		return errors.New(code)
	}
	return errors.New(productionFinalizedCharacterRequestFailed)
}
