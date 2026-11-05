//go:build linux

package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's WRITE slice of the two families whose
// binders are atomic across reads and writes. It publishes exactly nine names and
// nothing else:
//
//	db:review-create                internal/transport/reviewrevision.go:31
//	db:revision-create              :36
//	db:revision-replace-pending     :37
//	db:revision-merge               :42
//	db:revision-mark-merged         :43
//	db:revision-mark-discarded      :44
//	db:recovery-candidate-record    internal/transport/recovery.go:119
//	db:recovery-candidate-update    :130
//	db:recovery-candidate-resolve   :132
//
// # Why one server.Bind per name instead of the two family binders
//
// Both family binders install a whole family at once, so neither is usable here:
//
//   - BindReviewRevisionRemainder (reviewrevision.go:93) publishes
//     reviewRevisionRemainderOperations (:80-84), which is nine names: the six
//     writes above PLUS three READS - db:review-get-latest, db:review-next-index
//     and db:revision-next-index - that production_singleton_reads_linux.go
//     already binds. Taking the remainder would therefore fail startup with
//     OPERATION_ALREADY_BOUND, and this lane would be publishing reads it does
//     not own.
//   - BindRecovery (recovery.go:32) publishes four names, three of which are the
//     writes above and the fourth db:recovery-candidate-list, which that same
//     singleton read binder already serves.
//
// Each name is installed with server.Bind, the exported single-name binder
// (internal/transport/server.go:73-77), exactly the way
// production_singleton_reads_linux.go:144-163 and
// production_importrun_reads_linux.go:164-178 install their own slices. Bind
// refuses an uninventoried name (OPERATION_NOT_EXPOSED) and an already bound one
// (OPERATION_ALREADY_BOUND), so a name this file should not publish is a startup
// error instead of silent dead code, and a sibling binder that takes one of these
// names first is a startup error instead of a silent collision.
//
// # The write entry point
//
// Every handler borrows through borrowAuthorDatabase
// (production_durable_linux.go:301-357) - the same admission path the lane's
// existing mutations use: db:save-summary-snapshot (the one legacy INSERT
// BindSummary installs, production_continuity_reads_linux.go:39-45) and the three
// self-contained draft mutations (BeginDrafts, :359-365). That borrow parses the
// request's frozen three-key context, names the caller's own cookie/tab identity,
// requires the project path to be one the startup inventory issued a catalog key
// for, and then lets authorruntime re-prove the live lease, the admission permit
// and the handle identity before lending the author's own database. Its release
// closure calls refresh so the permit ADOPTS the content identity the request just
// committed and then re-verifies the handle; a refused write commits nothing, so
// its refresh is only a re-verification. A stale, foreign or closed lease is
// refused there, before a single row is read or written.
//
// Nothing here opens a database, resolves a path, or makes an admission decision,
// and no repository is constructed over a database that failed the pinned v7
// admission. store.ReviewRevisions and store.Recovery do their own
// BEGIN IMMEDIATE on one reserved connection and their own post-write
// verification (internal/store/reviewrevision.go:41-61, which every recovery
// mutation reuses at internal/store/recovery.go:324, :361, :389 and :450 and then
// re-reads its rows at :338-343 and :467-476), so this file contains no SQL at
// all.
//
// # Refusals: the family's own codes, never a swallowed one
//
// Both families answer a MUTATING name with the lane's business-result envelope
// (internal/transport/reviewrevision.go:22, internal/transport/recovery.go:49-60):
// HTTP 200 with {"success":false,"error":"Error: CODE"}. That is also what
// db:draft-update-content and db:save-summary-snapshot return for a refused write,
// so the renderer sees one shape. What this file does NOT copy is the review
// family's fallback, which maps every code outside its own allowlist to
// REVIEW_REVISION_OPERATION_FAILED - that would swallow the lease fence's
// PROJECT_SESSION_NOT_OWNED into a generic code. Instead a symbolic code always
// survives (the same symbolic-only rule production_singleton_reads_linux.go:364-381
// applies to the reads), so:
//
//   - a missing live write lease refuses with PROJECT_SESSION_NOT_OWNED /
//     PROJECT_SESSION_STALE / PROJECT_NOT_OPEN;
//   - a stale CAS refuses with the STORE's code, untouched:
//     REVISION_TARGET_CHANGED for a draft body that moved under
//     db:revision-merge's expectedDraftContent (internal/store/reviewrevision.go:421,
//     :435), SOURCE_DRAFT_CHANGED for a review/revision whose expectedSource no
//     longer matches (:199-207, :234-237), RECOVERY_CANDIDATE_SOURCE_CHANGED for a
//     recovery candidate whose frozen source moved (internal/store/recovery.go:399);
//   - anything else - a raw SQLite sentence included - becomes the family's own
//     fallback code, so no file path, SQL text or driver diagnostic travels.

const (
	productionWriteReviewCreateName           = "db:review-create"
	productionWriteRevisionCreateName         = "db:revision-create"
	productionWriteRevisionReplacePendingName = "db:revision-replace-pending"
	productionWriteRevisionMergeName          = "db:revision-merge"
	productionWriteRevisionMarkMergedName     = "db:revision-mark-merged"
	productionWriteRevisionMarkDiscardedName  = "db:revision-mark-discarded"

	productionWriteRecoveryRecordName  = "db:recovery-candidate-record"
	productionWriteRecoveryUpdateName  = "db:recovery-candidate-update"
	productionWriteRecoveryResolveName = "db:recovery-candidate-resolve"

	// The two families' fallback codes, and the one refusal the review family
	// reports under a second spelling the renderer matches on
	// (internal/transport/reviewrevision.go:22).
	productionWriteReviewFallbackCode   = "REVIEW_REVISION_OPERATION_FAILED"
	productionWriteRecoveryFallbackCode = "RECOVERY_CANDIDATE_OPERATION_FAILED"
	productionWriteSourceDraftChanged   = "SOURCE_DRAFT_CHANGED"
	productionWriteSourceChangedPrefix  = "SourceDraftChangedError: "
)

// productionWriteOperation is one bound name together with the wire shape the
// family binder uses for it (internal/transport/reviewrevision.go:23,
// internal/transport/recovery.go:36-44): the frozen three-key context is always
// the LAST argument, everything before it is the operation's own payload, and
// pathPos names the argument that must BE the frozen project path.
type productionWriteOperation struct {
	name    string
	arity   int
	pathPos int
}

// productionReviewWriteOperations is the six review/revision writes. Arity 3 with
// the path at index 1 for five of them; db:revision-mark-merged is the family's
// one arity-4 name and carries the revision id, the target draft id, the path and
// the context, in that order (reviewrevision.go:23).
var productionReviewWriteOperations = []productionWriteOperation{
	{productionWriteReviewCreateName, 3, 1},
	{productionWriteRevisionCreateName, 3, 1},
	{productionWriteRevisionReplacePendingName, 3, 1},
	{productionWriteRevisionMergeName, 3, 1},
	{productionWriteRevisionMarkMergedName, 4, 2},
	{productionWriteRevisionMarkDiscardedName, 3, 1},
}

// productionRecoveryWriteOperations is the three recovery candidate writes
// (recovery.go:40-43): record is arity 3 with the path at index 1, update and
// resolve are arity 4 with the path at index 2.
var productionRecoveryWriteOperations = []productionWriteOperation{
	{productionWriteRecoveryRecordName, 3, 1},
	{productionWriteRecoveryUpdateName, 4, 2},
	{productionWriteRecoveryResolveName, 4, 2},
}

// productionWrites is the mutation adapter for the lane built by
// productionHandlerWithContract. It holds no state of its own: every call borrows
// the admitted author database for its own request, so a lane whose runtime is
// gone answers the symbolic refusals instead of holding a handle this process no
// longer owns.
type productionWrites struct {
	durable *productionDurableOpen
}

// newProductionWrites returns the mutation adapter over the already admitted
// author runtime. Nothing is resolved, opened or leased here.
func newProductionWrites(open *productionOpen) *productionWrites {
	if open == nil {
		return &productionWrites{}
	}
	return &productionWrites{durable: open.durable}
}

// Bind installs the nine writes, one server.Bind per name. A nil adapter is
// refused rather than published as handlers that could only fail, and every name
// is re-checked against the registry by Bind itself, so a name missing from the
// inventory or already installed by another binder is a startup error.
func (w *productionWrites) Bind(server *transport.Server) error {
	if w == nil || w.durable == nil || server == nil {
		return errors.New("PRODUCTION_MUTATION_BINDER_INVALID")
	}
	handlers := make(map[string]transport.Operation, len(productionReviewWriteOperations)+len(productionRecoveryWriteOperations))
	for _, operation := range productionReviewWriteOperations {
		handlers[operation.name] = w.reviewOperation(operation)
	}
	for _, operation := range productionRecoveryWriteOperations {
		handlers[operation.name] = w.recoveryOperation(operation)
	}
	for _, operations := range [][]productionWriteOperation{productionReviewWriteOperations, productionRecoveryWriteOperations} {
		for _, operation := range operations {
			if err := server.Bind(operation.name, handlers[operation.name]); err != nil {
				return err
			}
		}
	}
	return nil
}

// borrowWriteDatabase is the lane's write entry point: borrowAuthorDatabase, the
// identical borrow the draft mutations and the legacy snapshot INSERT already
// write through. Its release closure refreshes the permit to the committed
// content identity, which is what makes the next operation on this project legal
// instead of AUTHOR_WRITE_STALE.
func (w *productionWrites) borrowWriteDatabase(ctx context.Context, caller transport.Caller, expected any) (*sql.DB, func(), error) {
	if w == nil || w.durable == nil {
		return nil, nil, errors.New("PRODUCTION_MUTATION_UNAVAILABLE")
	}
	return w.durable.borrowAuthorDatabase(ctx, caller, expected)
}

// reviewOperation builds the handler of one review/revision write. Argument order,
// the arity check and every refusal code mirror the family binder's handler for
// the same name (reviewrevision.go:23-27), so the renderer sees one contract
// whichever binder answers it.
func (w *productionWrites) reviewOperation(operation productionWriteOperation) transport.Operation {
	name := operation.name
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if w == nil || w.durable == nil {
			return productionReviewWriteRefusal(errors.New("PRODUCTION_MUTATION_UNAVAILABLE")), nil
		}
		// Arity first, exactly like the family: a malformed request never reaches
		// the borrow, so it can never be mistaken for authority.
		if len(args) != operation.arity {
			return productionReviewWriteRefusal(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")), nil
		}
		// The frozen context is always its own last argument, and its identity is
		// cloned so no later layer can rewrite the captured request in place.
		expected, err := productionWriteExpectedContext(args, operation.arity-1)
		if err != nil {
			return productionReviewWriteRefusal(err), nil
		}
		path, ok := args[operation.pathPos].(string)
		if !ok || path == "" {
			return productionReviewWriteRefusal(security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")), nil
		}
		// The path argument must BE the frozen project path: a page cannot aim one
		// project's lease at another project's rows (reviewrevision.go:24).
		if path != expected["projectPath"] {
			return productionReviewWriteRefusal(security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")), nil
		}
		var input map[string]any
		var id, target int64
		if productionReviewWriteStructured(name) {
			input, ok = args[0].(map[string]any)
			if !ok || input == nil {
				return productionReviewWriteRefusal(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")), nil
			}
			if err := productionReviewWriteValidate(name, input); err != nil {
				return productionReviewWriteRefusal(err), nil
			}
		} else {
			// The scalar ids are read exactly like draftSafeInteger(args[i], 1):
			// a whole JavaScript-safe number, never a string coercion.
			id, ok = productionWriteInteger(args[0], 1)
			if !ok {
				return productionReviewWriteRefusal(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")), nil
			}
			if name == productionWriteRevisionMarkMergedName {
				target, ok = productionWriteInteger(args[1], 1)
				if !ok {
					return productionReviewWriteRefusal(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")), nil
				}
			}
		}
		// The live write lease, the permit and the handle identity are proven
		// here. A refusal leaves the release nil, so nothing is unlocked twice.
		db, release, err := w.borrowWriteDatabase(ctx, caller, expected)
		if release != nil {
			defer release()
		}
		if err != nil {
			return productionReviewWriteRefusal(err), nil
		}
		if db == nil || release == nil {
			return productionReviewWriteRefusal(errors.New("PRODUCTION_DURABLE_INVALID")), nil
		}
		repository := store.NewReviewRevisions(db)
		var result any
		switch name {
		case productionWriteReviewCreateName:
			result, err = repository.CreateReview(ctx, input)
		case productionWriteRevisionCreateName:
			result, err = repository.CreateRevision(ctx, input)
		case productionWriteRevisionReplacePendingName:
			result, err = repository.ReplacePending(ctx, input)
		case productionWriteRevisionMergeName:
			result, err = repository.MergeRevision(ctx, input)
		case productionWriteRevisionMarkMergedName:
			err = repository.MarkMerged(ctx, id, target)
		case productionWriteRevisionMarkDiscardedName:
			err = repository.MarkDiscarded(ctx, id)
		default:
			return productionReviewWriteRefusal(errors.New("PRODUCTION_MUTATION_OPERATION_UNKNOWN")), nil
		}
		if err != nil {
			return productionReviewWriteRefusal(err), nil
		}
		// The envelopes are the family binder's, unchanged
		// (reviewrevision.go:47-48). The rows they name are only ever reported
		// here; what really landed is what the author database returns. The
		// binder's reviewRevisionWire (:66) only rewrites []byte into a Buffer
		// projection, and none of these six results carries one - the store
		// returns integers, strings and booleans only - so they pass through
		// exactly as the binder would send them.
		if name == productionWriteRevisionMergeName {
			return map[string]any{"success": true, "receipt": result}, nil
		}
		response := map[string]any{"success": true}
		if saved, ok := result.(map[string]any); ok {
			for _, key := range []string{"id", "reviewIndex", "revisionIndex"} {
				if value, ok := saved[key]; ok {
					response[key] = value
				}
			}
		}
		return response, nil
	}
}

// recoveryOperation builds the handler of one recovery candidate write. Argument
// order, the strict three-key context check and every refusal code mirror
// recovery.go:48-105 for the same name.
func (w *productionWrites) recoveryOperation(operation productionWriteOperation) transport.Operation {
	name := operation.name
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if w == nil || w.durable == nil {
			return productionRecoveryWriteRefusal(errors.New("PRODUCTION_MUTATION_UNAVAILABLE")), nil
		}
		if len(args) != operation.arity {
			return productionRecoveryWriteRefusal(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")), nil
		}
		// Reject extra frozen-context fields before the shared parser clones only
		// the three accepted identity fields and loses that evidence
		// (recovery.go:64-69).
		raw, ok := args[operation.arity-1].(map[string]any)
		if !ok || len(raw) != 3 {
			return productionRecoveryWriteRefusal(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")), nil
		}
		expected, err := productionWriteExpectedContext(args, operation.arity-1)
		if err != nil {
			return productionRecoveryWriteRefusal(err), nil
		}
		for _, value := range expected {
			text, ok := value.(string)
			if !ok || text == "" || !utf8.ValidString(text) {
				return productionRecoveryWriteRefusal(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")), nil
			}
		}
		path, ok := args[operation.pathPos].(string)
		if !ok || path == "" || !utf8.ValidString(path) {
			return productionRecoveryWriteRefusal(security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")), nil
		}
		if path != expected["projectPath"] {
			return productionRecoveryWriteRefusal(security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")), nil
		}
		var input map[string]any
		var id, text string
		switch name {
		case productionWriteRecoveryRecordName:
			input, err = productionRecoveryWriteInput(args[0], expected["projectId"])
			if err != nil {
				return productionRecoveryWriteRefusal(err), nil
			}
		case productionWriteRecoveryUpdateName, productionWriteRecoveryResolveName:
			id, ok = productionRecoveryWriteString(args[0])
			if !ok {
				return productionRecoveryWriteRefusal(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")), nil
			}
			text, ok = productionRecoveryWriteString(args[1])
			if !ok {
				return productionRecoveryWriteRefusal(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")), nil
			}
			if name == productionWriteRecoveryResolveName && text != "continued" && text != "discarded" {
				return productionRecoveryWriteRefusal(errors.New("RECOVERY_CANDIDATE_ACTION_INVALID")), nil
			}
		}
		db, release, err := w.borrowWriteDatabase(ctx, caller, expected)
		if release != nil {
			defer release()
		}
		if err != nil {
			return productionRecoveryWriteRefusal(err), nil
		}
		if db == nil || release == nil {
			return productionRecoveryWriteRefusal(errors.New("PRODUCTION_DURABLE_INVALID")), nil
		}
		repository := store.NewRecovery(db)
		var candidate map[string]any
		switch name {
		case productionWriteRecoveryRecordName:
			candidate, err = repository.Record(ctx, input)
		case productionWriteRecoveryUpdateName:
			candidate, err = repository.UpdatePending(ctx, id, text)
		case productionWriteRecoveryResolveName:
			err = repository.Resolve(ctx, id, text)
		}
		if err != nil {
			return productionRecoveryWriteRefusal(err), nil
		}
		if name == productionWriteRecoveryResolveName {
			return map[string]any{"success": true}, nil
		}
		return map[string]any{"success": true, "candidate": candidate}, nil
	}
}

// productionWriteExpectedContext is draftExpectedContext's contract
// (internal/transport/drafts.go:164-178): the frozen context is a map carrying
// non-empty projectId, leaseId and projectPath strings, and only those three are
// cloned forward. Anything else is a malformed request, never authority.
func productionWriteExpectedContext(args []any, index int) (map[string]any, error) {
	if index >= 0 && index < len(args) {
		if raw, ok := args[index].(map[string]any); ok {
			id, idOK := raw["projectId"].(string)
			lease, leaseOK := raw["leaseId"].(string)
			path, pathOK := raw["projectPath"].(string)
			if idOK && leaseOK && pathOK && id != "" && lease != "" && path != "" {
				return map[string]any{"projectId": id, "leaseId": lease, "projectPath": path}, nil
			}
		}
	}
	return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
}

// productionWriteInteger is draftSafeInteger's minimum-bounded reading
// (internal/transport/drafts.go:183) over the lane's existing JavaScript-safe
// number helper (production_importrun_reads_linux.go:297-319): a whole number,
// never a string, a boolean, null or a fraction.
func productionWriteInteger(value any, minimum int64) (int64, bool) {
	number, ok := productionImportRunReadInteger(value)
	if !ok || number < minimum {
		return 0, false
	}
	return number, true
}

// productionReviewWriteStructured reports which names carry a structured input
// map in position 0 and which carry scalar ids (reviewrevision.go:26).
func productionReviewWriteStructured(name string) bool {
	switch name {
	case productionWriteReviewCreateName, productionWriteRevisionCreateName,
		productionWriteRevisionReplacePendingName, productionWriteRevisionMergeName:
		return true
	}
	return false
}

// productionReviewWriteValidate copies the transport's own input gate for these
// six names (internal/transport/reviewrevision.go:56-65). It exists so a
// malformed input is refused with the same code whichever binder answers, not so
// this lane can re-admit anything: every field it accepts is still validated
// against the author database by the store, which is the only authority on
// whether the frozen source is current.
func productionReviewWriteValidate(name string, input map[string]any) error {
	invalid := func() error { return security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS") }
	// These authorities are not implemented on this lane: their ABSENCE is
	// required, not their falsiness.
	for _, key := range []string{"reviewCycleId", "generationRunHandle", "authorRecovery"} {
		if _, exists := input[key]; exists {
			return security.NewHTTPError(http.StatusConflict, "REVIEW_CYCLE_OWNER_REQUIRED")
		}
	}
	if name == productionWriteRevisionMergeName {
		for _, key := range []string{"revisionId", "targetDraftId"} {
			if _, ok := productionWriteInteger(input[key], 1); !ok {
				return invalid()
			}
		}
		if _, ok := productionWriteInteger(input["wordCount"], 0); !ok {
			return invalid()
		}
		for _, key := range []string{"expectedDraftContent", "mergedContent"} {
			if _, ok := input[key].(string); !ok {
				return invalid()
			}
		}
		return nil
	}
	if _, ok := productionWriteInteger(input["baseDraftId"], 1); !ok {
		return invalid()
	}
	if _, ok := input["content"].(string); !ok {
		return invalid()
	}
	if strings.HasPrefix(name, "db:revision-") {
		kind, ok := input["revisionType"].(string)
		if !ok || (kind != "refine" && kind != "review-fix") {
			return invalid()
		}
		if _, ok := productionWriteInteger(input["wordCount"], 0); !ok {
			return invalid()
		}
		if value, exists := input["userPrompt"]; exists && value != nil {
			if _, ok := value.(string); !ok {
				return invalid()
			}
		}
		if value, exists := input["reviewSourceId"]; exists && value != nil {
			if _, ok := productionWriteInteger(value, 1); !ok {
				return invalid()
			}
		}
	}
	// A missing expected source is the domain's SOURCE_DRAFT_CHANGED, not an HTTP
	// shape failure: the store refuses it because there is no baseline to check.
	value, exists := input["expectedSource"]
	if !exists || value == nil {
		return nil
	}
	source, ok := value.(map[string]any)
	if !ok {
		return store.ErrSourceDraftChanged
	}
	for _, key := range []string{"id", "chapterNumber", "version"} {
		if _, ok := productionWriteInteger(source[key], 1); !ok {
			return store.ErrSourceDraftChanged
		}
	}
	for _, key := range []string{"status", "content"} {
		if _, ok := source[key].(string); !ok {
			return store.ErrSourceDraftChanged
		}
	}
	return nil
}

// productionRecoveryWriteInput copies recoveryRecordInput's allowlist gate for
// db:recovery-candidate-record (internal/transport/recovery.go:165-256). It is
// where the renderer's own projectId is REFUSED instead of trusted or looked up:
// projectId is not an accepted key, and the identity the frozen three-key context
// captured is the only one injected into the row.
func productionRecoveryWriteInput(raw any, projectID any) (map[string]any, error) {
	invalid := security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	request, ok := raw.(map[string]any)
	if !ok || request == nil {
		return nil, invalid
	}
	out := make(map[string]any, len(request)+1)
	for key, value := range request {
		switch key {
		case "runId", "stepId", "chapterTitle", "visibleText", "failureCode", "failureReason", "replacesCandidateId":
			if _, ok := productionRecoveryWriteString(value); !ok {
				return nil, invalid
			}
		case "chapterNumber", "source", "sourceDraft":
		default:
			return nil, invalid
		}
		out[key] = value
	}
	for _, key := range []string{"runId", "stepId", "chapterTitle", "visibleText", "failureCode", "failureReason"} {
		if _, ok := productionRecoveryWriteString(out[key]); !ok {
			return nil, invalid
		}
	}
	if _, ok := productionWriteInteger(out["chapterNumber"], 1); !ok {
		return nil, errors.New("RECOVERY_CANDIDATE_CHAPTER_INVALID")
	}
	source, ok := out["source"].(map[string]any)
	if !ok || source == nil {
		return nil, invalid
	}
	copySource := make(map[string]any, len(source))
	for key, value := range source {
		switch key {
		case "chapterNumber":
			if _, ok := productionWriteInteger(value, 1); !ok {
				return nil, invalid
			}
		case "title", "role", "purpose", "keyEvents", "suspenseHook", "userGuidance":
			if _, ok := productionRecoveryWriteString(value); !ok {
				return nil, invalid
			}
		case "characters":
			items, ok := value.([]any)
			if !ok {
				return nil, invalid
			}
			cloned := make([]any, len(items))
			for i, item := range items {
				if _, ok := productionRecoveryWriteString(item); !ok {
					return nil, invalid
				}
				cloned[i] = item
			}
			value = cloned
		default:
			return nil, invalid
		}
		copySource[key] = value
	}
	for _, key := range []string{"title", "role", "purpose", "keyEvents"} {
		if _, ok := productionRecoveryWriteString(copySource[key]); !ok {
			return nil, invalid
		}
	}
	if _, ok := productionWriteInteger(copySource["chapterNumber"], 1); !ok {
		return nil, invalid
	}
	if _, ok := copySource["characters"]; !ok {
		return nil, invalid
	}
	out["source"] = copySource
	// The captured draft identity is optional (a nil means "no draft"), but a
	// present one is exactly two whole numbers: id and version.
	identity, exists := out["sourceDraft"]
	if !exists {
		return nil, errors.New("RECOVERY_CANDIDATE_SOURCE_DRAFT_INVALID")
	}
	if identity != nil {
		draft, ok := identity.(map[string]any)
		if !ok || len(draft) != 2 {
			return nil, errors.New("RECOVERY_CANDIDATE_SOURCE_DRAFT_INVALID")
		}
		if _, ok := productionWriteInteger(draft["id"], 1); !ok {
			return nil, errors.New("RECOVERY_CANDIDATE_SOURCE_DRAFT_INVALID")
		}
		if _, ok := productionWriteInteger(draft["version"], 1); !ok {
			return nil, errors.New("RECOVERY_CANDIDATE_SOURCE_DRAFT_INVALID")
		}
		out["sourceDraft"] = map[string]any{"id": draft["id"], "version": draft["version"]}
	}
	injected, ok := projectID.(string)
	if !ok || injected == "" {
		return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
	}
	out["projectId"] = injected
	return out, nil
}

// productionRecoveryWriteString is recoveryString's contract
// (internal/transport/recovery.go:160-163): a UTF-8 string, not a coercion.
func productionRecoveryWriteString(value any) (string, bool) {
	text, ok := value.(string)
	if !ok || !utf8.ValidString(text) {
		return "", false
	}
	return text, true
}

// productionWriteSymbolicCode bounds what a refusal may surface: one uppercase
// symbolic constant, never a wrapped path or a raw SQLite sentence. It is the same
// expression the family mappers and the singleton read adapter rely on
// (internal/transport/recovery.go:260-292,
// production_singleton_reads_linux.go:354-358).
var productionWriteSymbolicCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// productionReviewWriteRefusal is the review family's mutation envelope
// (internal/transport/reviewrevision.go:22): a 200 business result whose error
// text names the code, with SOURCE_DRAFT_CHANGED carrying the second spelling the
// renderer matches on.
func productionReviewWriteRefusal(err error) map[string]any {
	code := productionReviewWriteCode(err)
	result := map[string]any{"success": false, "error": "Error: " + code}
	if code == productionWriteSourceDraftChanged {
		result["error"] = productionWriteSourceChangedPrefix + code
		result["errorCode"] = code
	}
	return result
}

// productionReviewWriteCode keeps a symbolic refusal - the lease fence's
// PROJECT_SESSION_NOT_OWNED/PROJECT_SESSION_STALE, the store's own
// REVISION_TARGET_CHANGED/SOURCE_DRAFT_CHANGED/NOT_PENDING - and replaces
// everything else, including a wrapped SQL rollback sentence, with the family's
// fallback. An HTTPError's code is symbolic by construction; it is checked rather
// than trusted so a future non-symbolic code cannot leak through.
func productionReviewWriteCode(err error) string {
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		if productionWriteSymbolicCode.MatchString(public.Code) {
			return public.Code
		}
		return productionWriteReviewFallbackCode
	}
	if err != nil && productionWriteSymbolicCode.MatchString(err.Error()) {
		return err.Error()
	}
	return productionWriteReviewFallbackCode
}

// productionRecoveryWriteRefusal is the recovery family's mutation envelope
// (internal/transport/recovery.go:49-60).
func productionRecoveryWriteRefusal(err error) map[string]any {
	return map[string]any{"success": false, "error": "Error: " + productionRecoveryWriteCode(err)}
}

// productionRecoveryWriteCode is the recovery family's finite public vocabulary
// (internal/transport/recovery.go:260-292). A cancellation keeps its own code, an
// HTTPError is rebuilt from its code, and any diagnostic outside the list is
// replaced by the family's fallback.
func productionRecoveryWriteCode(err error) string {
	if errors.Is(err, context.Canceled) {
		return "REQUEST_CANCELED"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "REQUEST_DEADLINE_EXCEEDED"
	}
	code := ""
	var public *security.HTTPError
	if errors.As(err, &public) {
		if public == nil {
			return productionWriteRecoveryFallbackCode
		}
		code = public.Code
	} else if err != nil {
		code = err.Error()
	}
	switch code {
	case "RECOVERY_CANDIDATE_IDENTITY_INVALID", "RECOVERY_CANDIDATE_INTEGRITY_FAILED",
		"RECOVERY_CANDIDATE_CHAPTER_INVALID", "RECOVERY_CANDIDATE_SOURCE_MISMATCH",
		"RECOVERY_CANDIDATE_SOURCE_DRAFT_INVALID", "RECOVERY_CANDIDATE_TEXT_EMPTY",
		"RECOVERY_CANDIDATE_REPLACEMENT_INVALID", "RECOVERY_CANDIDATE_NOT_PENDING",
		"RECOVERY_CANDIDATE_SOURCE_CHANGED", "RECOVERY_CANDIDATE_ACTION_INVALID",
		"RECOVERY_CANDIDATE_PROVIDER_UNAVAILABLE", "RECOVERY_CANDIDATE_INPUT_UNSUPPORTED", "INVALID_ARGUMENTS",
		"PROJECT_CONTEXT_REQUIRED", "PROJECT_CONTEXT_CHANGED", "PROJECT_SESSION_REQUIRED",
		"PROJECT_SESSION_CONTEXT_REQUIRED", "PROJECT_SESSION_MANAGER_CLOSED",
		"PROJECT_SESSION_STALE", "PROJECT_SESSION_ROOT_MISMATCH", "PROJECT_SESSION_NOT_OWNED",
		"PROJECT_OPEN_IN_ANOTHER_TAB", "PROJECT_REQUEST_IN_FLIGHT", "PROJECT_NOT_OPEN",
		"PROJECT_ROOT_CHANGED", "PROJECT_SOURCE_CHANGED", "PROJECT_HANDLE_CLOSED",
		"PORTABLE_AUTHORITY_UNSUPPORTED", "PORTABLE_RUNTIME_FROZEN":
		return code
	}
	return productionWriteRecoveryFallbackCode
}
