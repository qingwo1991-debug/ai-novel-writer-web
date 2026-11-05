package transport

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// The durable lane's remaining draft operations: three plain SQLite mutations
// and three authoritative-export reads. Every one of them is served from the
// SAME borrowed author repository the draft reads already use (DraftProvider,
// backend-go/internal/transport/drafts.go:18-30), so a lane that also binds
// BindDrafts cannot collide: none of these six names is in that binder's row
// set.
//
// Arity and the path index are counted against the inventoried channel
// declarations in src/shared/ipc-channels.ts, with the frozen context ALWAYS
// the last wire argument and its own argument, never inside an input object:
//
//	db:draft-create                   (params 0, path 1, context 2)  :1023
//	db:draft-update-status            (id 0, status 1, wordCount 2, path 3, context 4) :1099
//	db:draft-delete                   (id 0, path 1, context 2)      :1101
//	db:draft-authority-sequence       (path 0, context 1)            :1031
//	db:draft-export-snapshot          (path 0, context 1)            :1032
//	db:draft-export-authority-current (receipt 0, path 1, context 2) :1033
//
// The two path-only reads still carry the path as a typed argument and the
// context beside it, exactly like db:get-latest-summary
// (backend-go/internal/transport/summary.go:38-46).
//
// What is deliberately NOT here: db:draft-import-finalized-batch (the import
// pipeline), db:blueprint-clear-all and db:project-clear-generated-data (both
// retire generation runs through repositories this lane does not own) and
// db:project-core-commit-generated (an owner-bound generation write).

// BindDraftAuthorWrites binds the three self-contained author draft mutations.
// They write the admitted author database and never rotate the frozen lease,
// because the original changes no session fact for a create, a status change or
// a delete. A refusal is always a business result and a commit is a success.
func (s *Server) BindDraftAuthorWrites(provider DraftProvider) error {
	if provider == nil {
		return errors.New("DRAFT_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("DRAFT_PROVIDER_REQUIRED")
		}
	}
	operations := []struct {
		name             string
		arity, pathIndex int
	}{
		{"db:draft-create", 3, 1},
		{"db:draft-update-status", 5, 3},
		{"db:draft-delete", 3, 1},
	}
	names := make([]string, 0, len(operations))
	handlers := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		name, arity, pathIndex := operation.name, operation.arity, operation.pathIndex
		names = append(names, name)
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			// draftSaveFailure is this family's own mutation envelope: the
			// original reports every refusal as {success:false,error:String(err)}
			// and String(new Error('CODE')) is "Error: CODE". The helper also
			// refuses to reflect anything but a symbolic code, so a wrapped path
			// or raw SQLite text never reaches the browser.
			fail := func(err error) (any, error) {
				if name == "db:draft-delete" && errors.Is(err, store.ErrFinalizedDraftDeleteRequired) {
					// The original answers this ONE code as {success:false,
					// errorCode} with no error string at all
					// (electron/controllers/db-controller.ts:946-953), so the
					// documented code field must not gain an "Error: " twin.
					return map[string]any{"success": false, "errorCode": "FINALIZED_DRAFT_DELETE_REQUIRED"}, nil
				}
				return draftAuthorWriteFailure(err), nil
			}
			if len(args) != arity {
				return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
			}
			expected, err := draftExpectedContext(args, arity-1)
			if err != nil {
				return fail(err)
			}
			path, ok := args[pathIndex].(string)
			if !ok || path == "" || !utf8.ValidString(path) {
				return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED"))
			}
			if path != expected["projectPath"] {
				return fail(security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED"))
			}
			var id int64
			var status string
			var wordCount *int64
			var params store.DraftCreateParams
			switch name {
			case "db:draft-create":
				input, ok := args[0].(map[string]any)
				if !ok || input == nil {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
				chapterNumber, ok := draftSafeInteger(input["chapterNumber"], 1)
				if !ok {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
				source, ok := input["source"].(string)
				if !ok || (source != "write" && source != "rewrite") {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
				content, ok := input["content"].(string)
				if !ok || !utf8.ValidString(content) {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
				count, ok := draftSafeInteger(input["wordCount"], 0)
				if !ok {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
				// The raw sourceDependencies value is handed to the store so its
				// own parseDependencies validates the same JSON bytes the original
				// stringifies; nil is the original's `?? []`.
				params = store.DraftCreateParams{ChapterNumber: chapterNumber, Source: source,
					Content: content, WordCount: count, Dependencies: input["sourceDependencies"]}
			case "db:draft-update-status":
				id, ok = draftSafeInteger(args[0], 1)
				if !ok {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
				status, ok = args[1].(string)
				if !ok || status == "" || !utf8.ValidString(status) {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
				// `wordCount: number | undefined` reaches the wire as null when
				// the renderer passes undefined (src/stores/draft-store.ts:310-314),
				// so nil means "leave the stored count alone".
				if args[2] != nil {
					count, ok := draftSafeInteger(args[2], 0)
					if !ok {
						return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
					}
					wordCount = &count
				}
			default:
				id, ok = draftSafeInteger(args[0], 1)
				if !ok {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
			}
			if ctx == nil {
				return fail(errors.New("DRAFT_OPERATION_FAILED"))
			}
			repository, release, err := provider.BeginDrafts(ctx, caller, expected)
			if release != nil {
				defer release()
			}
			if err != nil {
				return fail(err)
			}
			if repository == nil || release == nil {
				return fail(errors.New("DRAFT_PROVIDER_UNAVAILABLE"))
			}
			switch name {
			case "db:draft-create":
				created, err := repository.Create(ctx, params)
				if err != nil {
					return fail(err)
				}
				return map[string]any{"success": true, "id": created}, nil
			case "db:draft-update-status":
				if err := repository.UpdateStatus(ctx, id, status, wordCount); err != nil {
					return fail(err)
				}
				return map[string]any{"success": true}, nil
			default:
				if err := repository.Delete(ctx, id); err != nil {
					return fail(err)
				}
				return map[string]any{"success": true}, nil
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.installDraftAuthorHandlers(names, handlers)
}

// BindDraftAuthorityReads binds the three authoritative-export reads. All three
// are pure reads over drafts, contents and finalization_outbox, so they borrow
// the same author repository as the other draft reads and never rotate a lease.
// Their refusal is a transport error carrying the original's own export code,
// not an `Error: CODE` business result: the original returns a bare array, a
// bare object and a bare boolean, so there is no result object to carry a code.
func (s *Server) BindDraftAuthorityReads(provider DraftProvider) error {
	if provider == nil {
		return errors.New("DRAFT_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("DRAFT_PROVIDER_REQUIRED")
		}
	}
	operations := []struct {
		name             string
		arity, pathIndex int
	}{
		{"db:draft-authority-sequence", 2, 0},
		{"db:draft-export-snapshot", 2, 0},
		{"db:draft-export-authority-current", 3, 1},
	}
	names := make([]string, 0, len(operations))
	handlers := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		name, arity, pathIndex := operation.name, operation.arity, operation.pathIndex
		names = append(names, name)
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			if len(args) != arity {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			expected, err := draftExpectedContext(args, arity-1)
			if err != nil {
				return nil, draftAuthorityReadFailure(err)
			}
			path, ok := args[pathIndex].(string)
			if !ok || path == "" || !utf8.ValidString(path) {
				return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
			}
			if path != expected["projectPath"] {
				return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
			}
			// A receipt that is not an array of well-formed current facts is
			// simply not current: the original returns false instead of throwing,
			// and these shape rules are its own
			// (electron/repositories/finalization-repository.ts:211-231).
			var receipt []store.DraftExportAuthorityItem
			if name == "db:draft-export-authority-current" {
				var shaped bool
				receipt, shaped = draftExportAuthorityReceipt(args[0])
				if !shaped {
					return false, nil
				}
			}
			if ctx == nil {
				return nil, errors.New("DRAFT_READ_FAILED")
			}
			repository, release, err := provider.BeginDrafts(ctx, caller, expected)
			if release != nil {
				defer release()
			}
			if err != nil {
				return nil, draftAuthorityReadFailure(err)
			}
			if repository == nil || release == nil {
				return nil, errors.New("DRAFT_READ_FAILED")
			}
			switch name {
			case "db:draft-authority-sequence":
				result, err := repository.AuthoritySequence(ctx)
				if err != nil {
					return nil, draftAuthorityReadFailure(err)
				}
				return reviewRevisionWire(result), nil
			case "db:draft-export-snapshot":
				result, err := repository.ExportSnapshot(ctx)
				if err != nil {
					return nil, draftAuthorityReadFailure(err)
				}
				return reviewRevisionWire(result), nil
			default:
				matched, err := repository.ExportAuthorityCurrent(ctx, receipt)
				if err != nil {
					return nil, draftAuthorityReadFailure(err)
				}
				return matched, nil
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.installDraftAuthorHandlers(names, handlers)
}

// installDraftAuthorHandlers preflights the whole group before installing any
// handler, preserving the inventoried-only and no-replacement guarantees of
// Bind. The caller holds s.mu.
func (s *Server) installDraftAuthorHandlers(names []string, handlers map[string]Operation) error {
	for _, name := range names {
		if _, ok := s.registry.Lookup(name); !ok {
			return errors.New("OPERATION_NOT_EXPOSED")
		}
		if _, ok := s.handlers[name]; ok {
			return errors.New("OPERATION_ALREADY_BOUND")
		}
	}
	for name, handler := range handlers {
		s.handlers[name] = handler
	}
	return nil
}

// draftAuthorWriteFailure is the finite refusal vocabulary of the three author
// mutations. The original surfaces String(err) verbatim, so the codes the
// renderer is documented to interpret are preserved and everything else - a
// wrapped filesystem path, a raw SQLite diagnostic, an unexpected driver error -
// degrades to REQUEST_FAILED. The result is always a business result, never a
// transport failure, because the original answers these three with an object
// (electron/controllers/db-controller.ts:730-746, :911-929, :940-960).
func draftAuthorWriteFailure(err error) map[string]any {
	code := ""
	if err != nil {
		code = err.Error()
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		code = public.Code
	}
	switch code {
	case "INVALID_ARGUMENTS", "PROJECT_CONTEXT_REQUIRED", "PROJECT_CONTEXT_CHANGED",
		"PROJECT_SESSION_REQUIRED", "PROJECT_SESSION_STALE", "PROJECT_SESSION_NOT_OWNED",
		"PROJECT_SESSION_ROOT_MISMATCH", "PROJECT_NOT_OPEN",
		"REQUEST_CANCELED", "REQUEST_DEADLINE_EXCEEDED",
		// The store's own refusals, which are the original's thrown codes.
		"GENERATION_DRAFT_DEPENDENCY_INVALID", "GENERATION_DRAFT_DEPENDENCY_STALE",
		"DRAFT_FINALIZED_READ_ONLY", "DRAFT_FINALIZATION_REQUIRES_COMMIT",
		"FINALIZED_DRAFT_DELETE_REQUIRED", "SUPERSEDED_DRAFT_DELETE_FORBIDDEN",
		// This lane's own two guards, reachable only through a nil provider or a
		// caller-supplied nil context.
		"DRAFT_PROVIDER_UNAVAILABLE", "DRAFT_OPERATION_FAILED":
		return draftSaveFailure(errors.New(code))
	}
	return draftSaveFailure(errors.New("REQUEST_FAILED"))
}

// draftExportAuthorityReceipt validates one FinalizedDraftExportAuthorityReceipt
// and returns the comparable facts. Anything the original's shape check rejects
// is reported as shaped=false, never as an error, so the caller answers false.
func draftExportAuthorityReceipt(raw any) ([]store.DraftExportAuthorityItem, bool) {
	items, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	receipt := make([]store.DraftExportAuthorityItem, 0, len(items))
	seenChapters := make(map[int64]bool)
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok || entry == nil {
			return nil, false
		}
		draftID, ok := draftSafeInteger(entry["draftId"], 1)
		if !ok {
			return nil, false
		}
		chapterNumber, ok := draftSafeInteger(entry["chapterNumber"], 1)
		if !ok || seenChapters[chapterNumber] {
			return nil, false
		}
		version, ok := draftSafeInteger(entry["version"], 1)
		if !ok {
			return nil, false
		}
		// The key must be PRESENT and either null or a non-blank string: an
		// absent key is the original's `undefined`, which its check rejects.
		value, present := entry["finalizationId"]
		if !present {
			return nil, false
		}
		var finalizationID *string
		if value != nil {
			text, ok := value.(string)
			if !ok || finalizationTrim(text) == "" {
				return nil, false
			}
			finalizationID = &text
		}
		contentHash, ok := entry["contentHash"].(string)
		if !ok || !draftLowerHexDigest(contentHash) {
			return nil, false
		}
		seenChapters[chapterNumber] = true
		receipt = append(receipt, store.DraftExportAuthorityItem{DraftID: draftID,
			ChapterNumber: chapterNumber, Version: version,
			FinalizationID: finalizationID, ContentHash: contentHash})
	}
	return receipt, true
}

// draftLowerHexDigest is the original's /^[a-f0-9]{64}$/ test.
func draftLowerHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, digit := range []byte(value) {
		if !(digit >= 'a' && digit <= 'f') && !(digit >= '0' && digit <= '9') {
			return false
		}
	}
	return true
}

// draftAuthorityReadFailure keeps the export reads' refusal symbolic. The
// original throws these six export codes and the browser shows them verbatim,
// so the finite allowlist below is the only vocabulary that may reach the wire;
// everything else degrades to DRAFT_READ_FAILED and no path or SQLite text is
// ever reflected. A public HTTPError keeps its own status and code.
func draftAuthorityReadFailure(err error) error {
	if errors.Is(err, context.Canceled) {
		return errors.New("REQUEST_CANCELED")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("REQUEST_DEADLINE_EXCEEDED")
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return public
	}
	code := ""
	if err != nil {
		code = err.Error()
	}
	switch code {
	case "FINALIZATION_EXPORT_DUPLICATE_CHAPTER", "FINALIZATION_EXPORT_SNAPSHOT_INVALID",
		"FINALIZATION_EXPORT_CONTENT_EMPTY", "FINALIZATION_EXPORT_IDENTITY_MISMATCH",
		"FINALIZATION_EXPORT_CONTENT_MISMATCH", "FINALIZATION_EXPORT_SUMMARY_MISMATCH",
		"PROJECT_NOT_OPEN", "PROJECT_SESSION_STALE", "PROJECT_SESSION_NOT_OWNED",
		"PROJECT_SESSION_ROOT_MISMATCH":
		return errors.New(code)
	}
	return errors.New("DRAFT_READ_FAILED")
}
