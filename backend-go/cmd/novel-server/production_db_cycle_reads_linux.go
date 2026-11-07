//go:build linux

package main

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves db:review-cycle-list-pending-rechecks, the one name of the
// review-cycle family the editor's draft store asks for while rebuilding lost
// recovery work (src/stores/draft-store.ts:183).
//
// It is a plain read. The store list behind it is
// store.ListPendingReviewRechecks (internal/store/reviewcycle_read_tx.go), which
// is the reference's own query
//
//	SELECT rc.cycle_id,rc.review_id,rc.merged_hash,
//	    d.id,d.chapter_number,d.version,d.status,c.body
//	  FROM review_cycles rc JOIN revisions v ON v.id=rc.revision_id
//	  JOIN drafts d ON d.id=v.merged_to_draft_id JOIN contents c ON c.id=d.content_id
//	  WHERE rc.revision_status='merge-committed' AND rc.recheck_count=0 AND d.status='revised'
//	  ORDER BY d.chapter_number,d.id,rc.cycle_id
//
// (electron/repositories/review-cycle-repository.ts:507-512) over the same four
// tables, with the same projection, bounded by 200 rows and ordered exactly as
// the reference orders it. It opens no transaction and commits nothing.
//
// The wire shape is PendingReviewRecheck (src/shared/review-cycle.ts:36-43): the
// flat rows are nested into sourceDraft, and disposition is the only value this
// lane can prove, "required".
//
// # The one gate this lane does not port
//
// The reference keeps a candidate only when isCurrentMergedCycle re-proves the
// merged hash against the current base draft and planRecheck returns
// "required" (electron/repositories/review-cycle-repository.ts:518-521). The
// store ports the first proof (reviewCycleCurrentMerge) but not the second: the
// evidence classifier and the confirmation projection behind planRecheck have no
// owner in this package yet. A cycle whose findings' evidence did not change is
// therefore listed here where the reference would omit it. That is the exact
// risk this name carries; it can add a recheck the reference would skip, never
// lose one the reference reports, and it can never invent a cycle row because
// every row comes from the reference's own WHERE clause.
//
// The borrow is productionDurableOpen.borrowAuthorDatabase
// (production_durable_linux.go:313): the request's frozen three-key context is
// parsed, the caller's cookie/tab identity is named, the project path must be
// one the startup inventory issued a catalog key for, and authorruntime
// re-proves the lease, the admission permit and the handle identity before any
// SQL runs. Because this read writes nothing, its release closure is a
// re-verification rather than a write acknowledgement.

const (
	productionDBCyclePendingRechecksName = "db:review-cycle-list-pending-rechecks"

	// The wire shape, read off src/shared/ipc-channels.ts:1173 and the handler at
	// electron/controllers/db-controller.ts:1177-1181: (project path, context),
	// the frozen three-key context LAST, no scalar leading argument.
	productionDBCyclePendingRechecksArity     = 2
	productionDBCyclePendingRechecksPathIndex = 0
)

// productionDBCycleReads is the adapter for the one name. It is stateless: each
// request borrows the admitted author database through the lane's bridge and
// serializes the rows while the borrow is still held.
type productionDBCycleReads struct {
	durable *productionDurableOpen
}

func newProductionDBCycleReads(durable *productionDurableOpen) *productionDBCycleReads {
	return &productionDBCycleReads{durable: durable}
}

// Bind installs the one read with server.Bind, the exported single-name binder:
// the review-cycle family binder publishes the whole review/revision surface,
// which this lane already serves name by name, so the family binder would fail
// with OPERATION_ALREADY_BOUND. A nil adapter or server is refused instead of
// being published as a handler that can only fail.
func (r *productionDBCycleReads) Bind(server *transport.Server) error {
	if r == nil || r.durable == nil || server == nil {
		return errors.New("PRODUCTION_DB_CYCLE_BINDER_INVALID")
	}
	return server.Bind(productionDBCyclePendingRechecksName, r.pendingRechecks)
}

// pendingRechecks serves db:review-cycle-list-pending-rechecks. The arity check,
// the context parse and the path comparison are the review family binder's own
// (internal/transport/reviewrevision.go:23), so the renderer sees the same
// contract. The borrow happens only after every argument is valid, and the rows
// are copied into the wire shape before the release closure runs.
func (r *productionDBCycleReads) pendingRechecks(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if r == nil || r.durable == nil {
		return nil, errors.New("PRODUCTION_DB_CYCLE_UNAVAILABLE")
	}
	if len(args) != productionDBCyclePendingRechecksArity {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	expected, err := projectsession.ParseContext(args[productionDBCyclePendingRechecksArity-1])
	if err != nil {
		return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
	}
	path, ok := args[productionDBCyclePendingRechecksPathIndex].(string)
	if !ok || path == "" || !utf8.ValidString(path) {
		return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
	}
	if path != expected.ProjectPath {
		return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
	}
	if ctx == nil {
		return nil, errors.New("REVIEW_REVISION_OPERATION_FAILED")
	}
	db, release, err := r.durable.borrowAuthorDatabase(ctx, caller, args[productionDBCyclePendingRechecksArity-1])
	if release != nil {
		defer release()
	}
	if err != nil {
		return nil, productionDBCycleFailure(err)
	}
	if db == nil || release == nil {
		return nil, errors.New("REVIEW_REVISION_PROVIDER_UNAVAILABLE")
	}
	rows, err := store.ListPendingReviewRechecks(ctx, db)
	if err != nil {
		return nil, productionDBCycleFailure(err)
	}
	return productionDBCycleWire(rows), nil
}

// productionDBCycleWire is the reference's row-to-PendingReviewRecheck mapping
// (electron/repositories/review-cycle-repository.ts:521-523): the four cycle
// facts stay flat, the draft becomes sourceDraft, and the status is the literal
// the query already required. A nil row set becomes an empty array, because the
// renderer iterates the answer and the family answers [] rather than null when
// nothing is pending.
func productionDBCycleWire(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"cycleId":      row["cycleId"],
			"reviewId":     row["reviewId"],
			"rootActionId": row["rootActionId"],
			"mergedHash":   row["mergedHash"],
			"disposition":  "required",
			"sourceDraft": map[string]any{
				"id":            row["id"],
				"chapterNumber": row["chapterNumber"],
				"version":       row["version"],
				"status":        row["status"],
				"content":       row["content"],
			},
		})
	}
	return out
}

// productionDBCycleFailure keeps a refusal symbolic. It follows the review
// family's mapper (internal/transport/reviewrevision.go:52-55): a public HTTP
// error is rebuilt, a symbolic store or borrow code survives, a cancellation or
// deadline keeps its own code, and everything else becomes the family fallback
// instead of a raw SQLite sentence.
func productionDBCycleFailure(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return errors.New("REQUEST_CANCELED")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("REQUEST_DEADLINE_EXCEEDED")
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return security.NewHTTPError(public.Status, public.Code)
	}
	if productionDBCycleSymbolic(err.Error()) {
		return errors.New(err.Error())
	}
	return errors.New("REVIEW_REVISION_OPERATION_FAILED")
}

// productionDBCycleSymbolic reports whether a message is one uppercase symbolic
// constant. The family's own mapper accepts a finite allowlist; the shape check
// here is deliberately narrower than a substring match and still preserves every
// store sentinel and every lease refusal this borrow can produce, while a
// wrapped path or a SQLite sentence can never travel as a code.
func productionDBCycleSymbolic(text string) bool {
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
