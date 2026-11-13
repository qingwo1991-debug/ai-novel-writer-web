package transport

import (
	"context"
	"errors"
	"math"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// ProjectCoreProvider borrows ONLY an admitted project for exact cookie+tab+lease.
// It must hold ownership throughout the repository call. No request path opens.
type ProjectCoreProvider interface {
	BeginProjectCore(context.Context, Caller, any) (*store.ProjectCore, func(), error)
}
type ProjectCoreProviderFunc func(context.Context, Caller, any) (*store.ProjectCore, func(), error)

func (f ProjectCoreProviderFunc) BeginProjectCore(ctx context.Context, caller Caller, expected any) (*store.ProjectCore, func(), error) {
	if f == nil {
		return nil, nil, errors.New("PROJECT_CORE_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, expected)
}

// BindProjectCore deliberately does not bind generated commits or recovery. Plain
// synopsis CAS requires every original source baseline; owner-bearing requests
// fail closed until generation/recovery authority and idempotency are migrated.
func (s *Server) BindProjectCore(provider ProjectCoreProvider) error {
	if provider == nil {
		return errors.New("PROJECT_CORE_PROVIDER_REQUIRED")
	}
	names := []string{"db:project-core-get", "db:project-core-update", "db:project-core-synopsis-commit"}
	handlers := make(map[string]Operation, len(names))
	for _, name := range names {
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			update := name == "db:project-core-update"
			fail := func(err error) (any, error) {
				if update {
					return map[string]any{"success": false, "error": "Error: " + projectCoreErrorCode(err)}, nil
				}
				return nil, err
			}
			arity, pathIndex := 3, 1
			if name == "db:project-core-get" {
				arity, pathIndex = 2, 0
			}
			if len(args) != arity {
				return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
			}
			expected, err := draftExpectedContext(args, len(args)-1)
			if err != nil {
				return fail(err)
			}
			path, ok := args[pathIndex].(string)
			if !ok || path == "" {
				return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED"))
			}
			if path != expected["projectPath"] {
				return fail(security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED"))
			}
			var data map[string]any
			if name != "db:project-core-get" {
				data, ok = args[0].(map[string]any)
				if !ok || data == nil {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
				if name == "db:project-core-synopsis-commit" {
					if err := validateSynopsisRequest(data); err != nil {
						return fail(err)
					}
				}
			}
			repository, release, err := provider.BeginProjectCore(ctx, caller, expected)
			if release != nil {
				defer release()
			}
			if err != nil {
				return fail(err)
			}
			if repository == nil || release == nil {
				return fail(errors.New("PROJECT_CORE_PROVIDER_UNAVAILABLE"))
			}
			switch name {
			case "db:project-core-get":
				result, err := repository.Get(ctx)
				if err != nil {
					return fail(errors.New("PROJECT_CORE_READ_FAILED"))
				}
				return projectCoreWire(result), nil
			case "db:project-core-update":
				if err := repository.Update(ctx, data); err != nil {
					return fail(err)
				}
				return map[string]any{"success": true}, nil
			default:
				changed, err := repository.CommitSynopsis(ctx, data)
				if err != nil {
					return fail(errors.New(projectCoreErrorCode(err)))
				}
				if !changed {
					return map[string]any{"success": false, "error": "PROJECT_CORE_SYNOPSIS_CHANGED"}, nil
				}
				return map[string]any{"success": true}, nil
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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

// Node Buffer.toJSON, not Go's base64 encoding of []byte. SQLite's flexible
// storage classes can preserve a legacy BLOB even in a nominal TEXT column.
func projectCoreWire(result map[string]any) map[string]any {
	if result == nil {
		return nil
	}
	wire := make(map[string]any, len(result))
	for key, value := range result {
		if bytes, ok := value.([]byte); ok {
			data := make([]int, len(bytes))
			for i, b := range bytes {
				data[i] = int(b)
			}
			wire[key] = map[string]any{"type": "Buffer", "data": data}
		} else {
			wire[key] = value
		}
	}
	return wire
}
func projectCoreErrorCode(err error) string {
	var securityError *security.HTTPError
	if errors.As(err, &securityError) {
		return securityError.Code
	}
	if err == nil {
		return "PROJECT_CORE_OPERATION_FAILED"
	}
	switch err.Error() {
	case "PROJECT_CORE_CHARACTERS_ARCH_READ_ONLY", "GENERATION_OWNER_REQUIRED", "PROJECT_NOT_OPEN", "PROJECT_CORE_INVALID_BINDING", "PROJECT_CORE_PROVIDER_UNAVAILABLE":
		return err.Error()
	}
	return "PROJECT_CORE_OPERATION_FAILED"
}
func validateSynopsisRequest(data map[string]any) error {
	if _, ok := data["generationRunHandle"]; ok {
		return security.NewHTTPError(http.StatusConflict, "GENERATION_OWNER_REQUIRED")
	}
	if _, ok := data["authorRecovery"]; ok {
		return security.NewHTTPError(http.StatusConflict, "GENERATION_OWNER_REQUIRED")
	}
	if _, ok := data["synopsis"].(string); !ok {
		return security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	expected, ok := data["expected"].(map[string]any)
	if !ok {
		return security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	for _, key := range []string{"synopsis", "premise", "charactersArch", "worldbuilding", "genre", "writingLanguage", "plotStructure", "narrativePov", "globalGuidance"} {
		value, exists := expected[key]
		if !exists {
			return security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		if value == nil {
			continue
		}
		if _, ok := value.(string); !ok {
			return security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
	}
	for _, key := range []string{"totalChapters", "wordsPerChapter"} {
		value, exists := expected[key]
		if !exists {
			return security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		if value == nil {
			continue
		}
		number, ok := value.(float64)
		if !ok || math.IsInf(number, 0) || math.IsNaN(number) {
			return security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
	}
	return nil
}
