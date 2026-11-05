//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's BATCH and DIRECTORY read slice of the
// generation family: four names of web/operations.json:219-224, bound per name
// with server.Bind (internal/transport/server.go:73-77) exactly the way
// production_importrun_reads_linux.go and production_singleton_reads_linux.go
// install their own slices:
//
//	generation:read-batch                 store.ReadGenerationBatchProgress
//	generation:list-batches               store.ListGenerationBatchHistory
//	generation:confirm-batch-finalization store.ConfirmGenerationBatchFinalization
//	generation:list-directory-progress    store.ListDirectoryGenerationProgress
//
// The family binder transport.BindGeneration (internal/transport/generation.go:37)
// is not used: it installs thirteen names at once, including generation:begin,
// generation:execute, generation:cancel, generation:pause and generation:resume,
// which production_open_linux.go already binds through BindGenerationBegin,
// BindGenerationExecute and BindGenerationLifecycle, so taking it would fail
// startup with OPERATION_ALREADY_BOUND. transport.GenerationProvider has no field
// for any of these four names either, so they cannot travel
// BindGenerationReads (internal/transport/generation.go:146-165) without editing
// that file. Bound one at a time, this file publishes exactly four SELECT-only
// operations and nothing else; server.Bind refuses a name this file should not
// publish (OPERATION_NOT_EXPOSED) or one a sibling slice already took
// (OPERATION_ALREADY_BOUND), so a collision is a startup error instead of silent
// dead code.
//
// # Nothing here writes
//
// All four delegate to internal/store/batch_progress.go, whose every statement is
// a SELECT inside a deferred read transaction that is committed without a write.
// Per-chapter batch state is DERIVED, never stored: there is no batch table, the
// batch id is the root run's run_id, and progress comes from
// usage_receipt_json.draftCommit plus finalization_outbox and post_process_runs
// (electron/services/generation-draft-effects.ts:40-51, :104-153).
// confirm-batch-finalization is a read that CONFIRMS a finalization which already
// happened: it refuses GENERATION_BATCH_FINALIZATION_REQUIRED unless the chapter
// carries a published finalization id AND its post-process run is complete
// (main-generation-owner.ts:1534-1538), and it writes nothing.
//
// # The fence is the existing one
//
// Each name takes the frozen three-key project session as its LAST argument
// (electron/controllers/generation-controller.ts:182-186 pops it, and
// src/services/ipc-client.ts:53-68 appends it for every generation: channel), so
// the wire arity is the request arity plus one, and the frozen context is
// args[arity-1]. A malformed argument list is 400 INVALID_ARGUMENTS and a missing
// or malformed frozen context is 400 PROJECT_SESSION_REQUIRED before any database
// work, exactly like the family adapter (internal/transport/generation.go:277-291).
// A stale, foreign or closed lease is then refused by
// borrowAuthorDatabase (production_durable_linux.go:308-357), the identical fence
// every other production read and write uses, before a single row is read. The
// Electron-only GENERATION_PROJECT_SESSION_REQUIRED never appears on this lane:
// it is the renderer's own pre-invoke check (src/services/ipc-client.ts:81).
//
// # Envelope
//
// transport.generationFailure(false, err) is the family's read envelope: a public
// HTTP error keeps its status and code, a symbolic repository code becomes
// 500 + that code, and anything else becomes 500 REQUEST_FAILED. These names
// answer the same way, so the renderer cannot tell which binder served them.
//
// # The one honest reduction
//
// verifiedCommit (generation-draft-effects.ts:65-77) recomputes the run's visible
// composition from its artifacts and compares both the composition hash and the
// composition text. This lane carries no port of sanitizeDraftText /
// composeVisibleContinuation (src/shared/draft-visible-text.ts:31-50,
// src/shared/visible-continuation.ts:29-33), so the store compares the receipt
// against the composition hash that the run's own compose-visible receipt froze,
// against sha256 of the receipt's content, and against the draft row the receipt
// names. A receipt contradicted by any of those is still refused with
// GENERATION_DRAFT_RECEIPT_INVALID; what is NOT re-derived is a tampered artifact
// whose stored composition receipt was left alone. See the report note on
// internal/store/batch_progress.go:bpVerifyCommit.

const (
	// The four bound names.
	productionBatchReadName      = "generation:read-batch"
	productionBatchListName      = "generation:list-batches"
	productionBatchConfirmName   = "generation:confirm-batch-finalization"
	productionBatchDirectoryName = "generation:list-directory-progress"

	// Wire arity: the request arity plus the frozen project session
	// (src/shared/generation-owner-contract.ts:312-317).
	productionBatchReadArity      = 2
	productionBatchListArity      = 1
	productionBatchConfirmArity   = 2
	productionBatchDirectoryArity = 1
)

// productionBatchOperation is one bound name and its wire arity.
type productionBatchOperation struct {
	name  string
	arity int
}

// productionBatchOperations is the whole of what this file can publish.
var productionBatchOperations = [...]productionBatchOperation{
	{productionBatchReadName, productionBatchReadArity},
	{productionBatchListName, productionBatchListArity},
	{productionBatchConfirmName, productionBatchConfirmArity},
	{productionBatchDirectoryName, productionBatchDirectoryArity},
}

// productionBatchSymbolicCode is generationSymbolicCode
// (internal/transport/generation.go:390-410): a code survives only if it is at
// least two characters, starts with an upper-case ASCII letter and continues with
// upper-case letters, digits or underscores.
var productionBatchSymbolicCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]+$`)

// productionBatchReads serves the four batch/directory reads over the one
// admitted author database the lane already opened.
type productionBatchReads struct {
	durable *productionDurableOpen
}

// newProductionBatchReads wraps the lane's durable open. A nil open installs
// nothing: Bind refuses, so the four names keep failing closed at
// 501 GO_OPERATION_NOT_MIGRATED instead of being published as handlers that can
// only refuse.
func newProductionBatchReads(durable *productionDurableOpen) *productionBatchReads {
	return &productionBatchReads{durable: durable}
}

// Bind installs the four reads, one server.Bind per name.
func (r *productionBatchReads) Bind(server *transport.Server) error {
	if r == nil || r.durable == nil || server == nil {
		return errors.New("PRODUCTION_BATCH_BINDER_INVALID")
	}
	for _, operation := range productionBatchOperations {
		if err := server.Bind(operation.name, r.read(operation)); err != nil {
			return err
		}
	}
	return nil
}

// read serves one of the four names. Arity, the frozen context, the borrow and
// the envelope are shared; only the request shape and the store call differ.
func (r *productionBatchReads) read(operation productionBatchOperation) transport.Operation {
	name, arity := operation.name, operation.arity
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if r == nil || r.durable == nil {
			return nil, errors.New("PRODUCTION_BATCH_UNAVAILABLE")
		}
		if len(args) != arity {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		expected, err := projectsession.ParseContext(args[arity-1])
		if err != nil {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
		}
		// The request itself is validated before the borrow, so a malformed call
		// never opens a database this lane would have to fence afterwards.
		batchID, chapterNumber, finalizationID := "", int64(0), ""
		switch name {
		case productionBatchReadName:
			if batchID, err = productionBatchRequestBatchID(args[0]); err != nil {
				return productionBatchFailure(err)
			}
		case productionBatchConfirmName:
			if batchID, chapterNumber, finalizationID, err = productionBatchRequestConfirmation(args[0]); err != nil {
				return productionBatchFailure(err)
			}
		}
		db, release, err := r.durable.borrowAuthorDatabase(ctx, caller, args[arity-1])
		if release != nil {
			defer release()
		}
		if err != nil {
			return productionBatchFailure(err)
		}
		if db == nil || release == nil {
			return nil, errors.New("PRODUCTION_DURABLE_INVALID")
		}
		var result any
		switch name {
		case productionBatchReadName:
			result, err = store.ReadGenerationBatchProgress(ctx, db, expected.ProjectID, batchID)
		case productionBatchListName:
			result, err = store.ListGenerationBatchHistory(ctx, db, expected.ProjectID)
		case productionBatchConfirmName:
			result, err = store.ConfirmGenerationBatchFinalization(ctx, db, expected.ProjectID, batchID, chapterNumber, finalizationID)
		case productionBatchDirectoryName:
			result, err = store.ListDirectoryGenerationProgress(ctx, db)
		default:
			return nil, errors.New("PRODUCTION_BATCH_OPERATION_UNKNOWN")
		}
		if err != nil {
			return productionBatchFailure(err)
		}
		return result, nil
	}
}

// productionBatchRequestBatchID unwraps `{ batchId }`, the frozen request of
// generation:read-batch (src/shared/generation-owner-contract.ts:312). Exactly one
// key is admitted, the way familyRequestField refuses extra keys
// (internal/transport/generation.go:412-430).
func productionBatchRequestBatchID(value any) (string, error) {
	request, ok := value.(map[string]any)
	if !ok || len(request) != 1 {
		return "", security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	raw, present := request["batchId"]
	if !present {
		return "", security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	batchID, ok := raw.(string)
	if !ok || batchID == "" || !utf8.ValidString(batchID) {
		return "", security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	return batchID, nil
}

// productionBatchRequestConfirmation unwraps `{ batchId, chapterNumber,
// finalizationId }` (:316). All three are required and typed: a malformed
// confirmation must never be mistaken for a confirmation that failed.
func productionBatchRequestConfirmation(value any) (string, int64, string, error) {
	request, ok := value.(map[string]any)
	if !ok || len(request) != 3 {
		return "", 0, "", security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	batchID, ok := request["batchId"].(string)
	if !ok || batchID == "" || !utf8.ValidString(batchID) {
		return "", 0, "", security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	finalizationID, ok := request["finalizationId"].(string)
	if !ok || finalizationID == "" || !utf8.ValidString(finalizationID) {
		return "", 0, "", security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	// The chapter number is read exactly like draftSafeInteger
	// (internal/transport/drafts.go:183): a whole JavaScript-safe number, no string
	// coercion, never below 1.
	chapterNumber, ok := productionBatchInteger(request["chapterNumber"])
	if !ok || chapterNumber < 1 {
		return "", 0, "", security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	return batchID, chapterNumber, finalizationID, nil
}

// productionBatchInteger accepts only a whole JavaScript-safe number.
func productionBatchInteger(value any) (int64, bool) {
	var number float64
	switch value := value.(type) {
	case float64:
		number = value
	case int:
		number = float64(value)
	case int64:
		number = float64(value)
	case json.Number:
		if len(value) > 64 || !json.Valid([]byte(value)) {
			return 0, false
		}
		parsed, err := value.Float64()
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number < -9007199254740991 ||
		number > 9007199254740991 || math.Trunc(number) != number {
		return 0, false
	}
	return int64(number), true
}

// productionBatchFailure is generationFailure(false, err): the read envelope of
// the generation family, so these four names answer exactly like the two names
// transport.BindGenerationReads already installs.
func productionBatchFailure(err error) (any, error) {
	if err == nil {
		return nil, nil
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return nil, security.NewHTTPError(public.Status, public.Code)
	}
	if productionBatchSymbolicCode.MatchString(err.Error()) {
		return nil, security.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return nil, security.NewHTTPError(http.StatusInternalServerError, "REQUEST_FAILED")
}
