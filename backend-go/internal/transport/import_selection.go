package transport

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/importsource"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// ImportSelectionRequest contains display intent only. It cannot name a file.
type ImportSelectionRequest struct {
	RunID   string
	Purpose importsource.Purpose
	Locale  string
}

// ImportSelection is supplied by a trusted upload or file-selection owner
// after it has admitted the exact caller and frozen project context. Sources
// are owned readers, never paths supplied by the browser.
type ImportSelection struct {
	ProjectID string
	LeaseID   string
	Sources   []importsource.CapturedSource
	Canceled  bool
}

type ImportSelectionProvider interface {
	BeginImportSelection(context.Context, Caller, ImportSelectionRequest, map[string]any) (ImportSelection, func(), error)
}

type ImportSelectionProviderFunc func(context.Context, Caller, ImportSelectionRequest, map[string]any) (ImportSelection, func(), error)

func (f ImportSelectionProviderFunc) BeginImportSelection(ctx context.Context, caller Caller, request ImportSelectionRequest, expected map[string]any) (ImportSelection, func(), error) {
	if f == nil {
		return ImportSelection{}, nil, errors.New("IMPORT_SELECTION_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, request, expected)
}

// BindImportSelection admits only the original author-manuscript source
// inspection response. Reference selection requires a durable parsing run,
// which an in-memory inspection cannot represent. No commit or author preview
// operation is bound here; the latter needs target-draft conflict reads.
func (s *Server) BindImportSelection(store *importsource.Store, provider ImportSelectionProvider) error {
	if store == nil || provider == nil {
		return errors.New("IMPORT_SELECTION_OWNER_REQUIRED")
	}
	return s.Bind("dialog:select-novel-files", func(ctx context.Context, caller Caller, args []any) (any, error) {
		fail := func(err error) (any, error) { return nil, importSelectionError(err) }
		if len(args) != 2 {
			return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		raw, ok := args[0].(map[string]any)
		if !ok || len(raw) != 4 {
			return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		contextRaw, ok := args[1].(map[string]any)
		if !ok || len(contextRaw) != 3 {
			return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
		}
		expected, err := draftExpectedContext(args, 1)
		if err != nil {
			return fail(err)
		}
		if !validImportText(expected["projectId"].(string), 256) || !validImportText(expected["leaseId"].(string), 256) || !validImportText(expected["projectPath"].(string), 4096) {
			return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
		}
		runID, runOK := raw["runId"].(string)
		purpose, purposeOK := raw["purpose"].(string)
		locale, localeOK := raw["locale"].(string)
		path, pathOK := raw["expectedProjectPath"].(string)
		if !runOK || !validImportText(runID, 128) || !purposeOK || !localeOK || !pathOK || !validImportText(path, 4096) || path != expected["projectPath"] || (locale != "zh-CN" && locale != "en-US") {
			return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		if purpose != string(importsource.AuthorManuscript) {
			if purpose == string(importsource.Reference) {
				return fail(security.NewHTTPError(http.StatusNotImplemented, "IMPORT_DURABLE_PARSING_REQUIRED"))
			}
			return fail(security.NewHTTPError(http.StatusBadRequest, "IMPORT_PURPOSE_INVALID"))
		}
		if ctx == nil {
			return fail(errors.New("IMPORT_SELECTION_FAILED"))
		}
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		request := ImportSelectionRequest{RunID: runID, Purpose: importsource.AuthorManuscript, Locale: locale}
		selection, release, err := provider.BeginImportSelection(ctx, caller, request, expected)
		if release != nil {
			defer release()
		}
		if err != nil {
			return fail(err)
		}
		if release == nil || selection.ProjectID != expected["projectId"] || selection.LeaseID != expected["leaseId"] {
			return fail(errors.New("IMPORT_SELECTION_AUTHORITY_CHANGED"))
		}
		if selection.Canceled {
			return nil, nil
		}
		binding := importsource.Binding{Caller: caller.SessionID + ":" + caller.ClientID,
			Generation: selection.ProjectID + ":" + selection.LeaseID, Purpose: request.Purpose}
		summary, err := store.Inspect(binding, selection.Sources)
		if err != nil {
			return fail(err)
		}
		return map[string]any{"success": true, "inspection": map[string]any{
			"inspectionId": summary.InspectionID, "purpose": summary.Purpose,
			"sourceCount": summary.SourceCount, "sourceDisplayNames": summary.SourceDisplayNames,
			"chapterCount": summary.ChapterCount, "totalWords": summary.TotalWords,
			"totalBytes": summary.TotalBytes, "preview": summary.Preview,
		}}, nil
	})
}

func validImportText(value string, limit int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= limit && utf8.ValidString(value) && !strings.ContainsRune(value, 0)
}

func importSelectionError(err error) error {
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return public
	}
	if errors.Is(err, context.Canceled) {
		return errors.New("REQUEST_CANCELED")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("REQUEST_DEADLINE_EXCEEDED")
	}
	if err != nil {
		switch err.Error() {
		case "IMPORT_SOURCE_COUNT_EXCEEDED", "IMPORT_SOURCE_BYTES_EXCEEDED", "IMPORT_CHAPTER_COUNT_EXCEEDED", "IMPORT_SOURCE_SIZE_INVALID", "IMPORT_INSPECTION_LIMIT", "AUTHOR_MANUSCRIPT_DUPLICATE_CHAPTER", "IMPORT_INSPECTION_EXPIRED":
			return errors.New(err.Error())
		}
	}
	return errors.New("IMPORT_SELECTION_FAILED")
}
