package transport

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// DraftProvider borrows an already admitted, caller-owned project repository.
// BeginDrafts must validate the entire frozen context against the caller's
// session AND tab; a supplied project path is never authorization. The returned
// release holds that project/snapshot stable until the operation has finished.
// Providers must not open arbitrary request paths, migrate, or switch projects.
// Keeping this adapter here avoids a projectsession -> transport import cycle.
type DraftProvider interface {
	BeginDrafts(context.Context, Caller, any) (*store.Drafts, func(), error)
}

type DraftProviderFunc func(context.Context, Caller, any) (*store.Drafts, func(), error)

func (f DraftProviderFunc) BeginDrafts(ctx context.Context, caller Caller, expected any) (*store.Drafts, func(), error) {
	if f == nil {
		return nil, nil, errors.New("DRAFT_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, expected)
}

const draftConflictMessage = "草稿已被其他编辑修改或已变为只读。请保留当前编辑内容，重新载入后比较，不要直接覆盖。"

// BindDrafts binds only the four existing reads and the atomic author-save path.
// Match ipc-client's wire format: legacy typed arguments, then the frozen
// browser context. The legacy expectedProjectPath is checked for consistency,
// never used as authorization. Save's acknowledged expectedContent is index 4.
// There is no path-only or implicit-current fallback or project:open stub.
func (s *Server) BindDrafts(provider DraftProvider) error {
	if provider == nil {
		return errors.New("DRAFT_PROVIDER_REQUIRED")
	}
	operations := []struct {
		name      string
		pathIndex int
		arity     int
	}{
		{"db:draft-list", 1, 3},
		{"db:draft-list-all", 0, 2},
		{"db:draft-get-meta", 1, 3},
		{"db:draft-get-full", 1, 3},
		{"db:draft-get-latest", 1, 3},
		{"db:draft-get-finalized", 1, 3},
		{"db:draft-get-max-finalized-chapter", 0, 2},
		{"db:draft-next-version", 1, 3},
		{"db:draft-update-content", 3, 6},
	}
	handlers := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		name, pathIndex, arity := operation.name, operation.pathIndex, operation.arity
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			mutating := name == "db:draft-update-content"
			fail := func(err error) (any, error) {
				if mutating {
					return draftSaveFailure(err), nil
				}
				return nil, err
			}
			expected, err := draftExpectedContext(args, len(args)-1)
			if err != nil {
				return fail(err)
			}
			// A missing/non-string save baseline is a content conflict, not an
			// invitation to obtain a fresh baseline from the repository.
			if len(args) != arity && !(mutating && len(args) == arity-1) {
				return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
			}
			path, ok := args[pathIndex].(string)
			if !ok || path == "" {
				return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED"))
			}
			if path != expected["projectPath"] {
				return fail(security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED"))
			}
			var id, wordCount int64
			var content string
			if name != "db:draft-list-all" && name != "db:draft-get-max-finalized-chapter" {
				var ok bool
				id, ok = draftSafeInteger(args[0], 1)
				if !ok {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
			}
			if mutating {
				var ok bool
				content, ok = args[1].(string)
				if !ok {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
				wordCount, ok = draftSafeInteger(args[2], 0)
				if !ok {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
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
			case "db:draft-list":
				return repository.ListByChapter(ctx, id)
			case "db:draft-list-all":
				return repository.ListAll(ctx)
			case "db:draft-get-meta":
				return repository.GetMeta(ctx, id)
			case "db:draft-get-full":
				return repository.GetFull(ctx, id)
			case "db:draft-get-latest":
				return repository.GetLatestByChapter(ctx, id)
			case "db:draft-get-finalized":
				return repository.GetFinalizedByChapter(ctx, id)
			case "db:draft-get-max-finalized-chapter":
				return repository.GetMaxFinalizedChapter(ctx)
			case "db:draft-next-version":
				return repository.GetNextVersion(ctx, id)
			default:
				var baseline *string
				if len(args) == arity {
					if text, ok := args[4].(string); ok {
						baseline = &text
					}
				}
				if err := repository.UpdateContentIfCurrent(ctx, id, content, wordCount, baseline); err != nil {
					return fail(err)
				}
				return map[string]any{"success": true}, nil
			}
		}
	}
	// Preflight the entire group before installing any handler, preserving the
	// same inventoried-only and no-replacement guarantees as Bind.
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, operation := range operations {
		if _, ok := s.registry.Lookup(operation.name); !ok {
			return errors.New("OPERATION_NOT_EXPOSED")
		}
		if _, ok := s.handlers[operation.name]; ok {
			return errors.New("OPERATION_ALREADY_BOUND")
		}
	}
	for name, handler := range handlers {
		s.handlers[name] = handler
	}
	return nil
}

func draftExpectedContext(args []any, index int) (map[string]any, error) {
	if index >= 0 && index < len(args) {
		if raw, ok := args[index].(map[string]any); ok {
			id, idOK := raw["projectId"].(string)
			lease, leaseOK := raw["leaseId"].(string)
			path, pathOK := raw["projectPath"].(string)
			if idOK && leaseOK && pathOK && id != "" && lease != "" && path != "" {
				// Copy the identity: neither the provider nor another layer can
				// rewrite the captured request's context in place.
				return map[string]any{"projectId": id, "leaseId": lease, "projectPath": path}, nil
			}
		}
	}
	return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
}

// JSONBody currently exposes JSON.parse-compatible float64s. Also accept
// json.Number for callers retaining decoder.UseNumber; never coerce strings,
// booleans, null, fractions, infinities, or integers outside JS's safe range.
func draftSafeInteger(value any, minimum int64) (int64, bool) {
	var number float64
	switch value := value.(type) {
	case float64:
		number = value
	case json.Number:
		// Do not round a fractional token to an integer while checking it.
		// Limit token length before rational parsing to keep this helper bounded.
		if len(value) > 64 || !json.Valid([]byte(value)) {
			return 0, false
		}
		number, err := value.Float64()
		if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || number < float64(minimum) || number > 9007199254740991 || math.Trunc(number) != number {
			return 0, false
		}
		if offset := strings.IndexAny(string(value), "eE"); offset >= 0 {
			exponent, err := strconv.Atoi(string(value)[offset+1:])
			if err != nil || exponent < -64 || exponent > 64 {
				return 0, false
			}
		}
		exact, ok := new(big.Rat).SetString(string(value))
		if !ok || !exact.IsInt() || !exact.Num().IsInt64() {
			return 0, false
		}
		integer := exact.Num().Int64()
		return integer, integer >= minimum && integer <= 9007199254740991
	default:
		return 0, false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number < float64(minimum) || number > 9007199254740991 || math.Trunc(number) != number {
		return 0, false
	}
	return int64(number), true
}

func draftSaveFailure(err error) map[string]any {
	if errors.Is(err, store.ErrDraftContentConflict) {
		return map[string]any{"success": false, "errorCode": "DRAFT_CONTENT_CONFLICT", "error": draftConflictMessage}
	}
	// Match the original Error: SYMBOLIC_CODE business result without exposing
	// SQLite diagnostics, project paths, or arbitrary provider error messages.
	code := err.Error()
	var public *security.HTTPError
	if errors.As(err, &public) {
		code = public.Code
	}
	if len(code) < 2 {
		code = "REQUEST_FAILED"
	}
	for i, c := range code {
		if !(c >= 'A' && c <= 'Z') && !(i > 0 && (c >= '0' && c <= '9' || c == '_')) {
			code = "REQUEST_FAILED"
			break
		}
	}
	log.Printf("novel-server: mutation refused (%s)", code)
	return map[string]any{"success": false, "error": "Error: " + code}
}
