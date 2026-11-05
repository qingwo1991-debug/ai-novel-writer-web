package transport

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// ProjectLifecycleProvider owns admission, database lifetime, and the entire
// cookie+tab+lease handshake. Keeping Caller and any at this boundary lets the
// runtime implement it without transport importing a runtime implementation.
//
// OpenProject must resolve targets ONLY against server-injected catalog keys or
// caller-owned grants, never by opening an arbitrary request path. The renderer
// path is response metadata, not rollback authority. It must register and fence
// concurrent open requests per Caller, stage a candidate before switching, and
// retain the prior admitted owner/database/lease when opening B from A fails.
// It returns the original full Open DTO, including requestToken, project,
// activeProjectPath, databaseRestored, and dbReady from observed runtime state.
//
// CloseProject must validate the frozen context against Caller and actually
// close the admitted database before acknowledging. SaveProject receives an
// allowlisted project_core patch, NOT ProjectData; it must retain the same owner
// lock across context validation and the database update. Neither method may
// open a path or derive authority from projectId/path alone.
// ProjectRuntimeContext must report actual database readiness, not ownership
// bookkeeping or a fabricated ready flag.
type ProjectLifecycleProvider interface {
	OpenProject(context.Context, Caller, any, string, any) (any, error)
	CloseProject(context.Context, Caller, string, any) (any, error)
	ProjectRuntimeContext(context.Context, Caller) (any, error)
	SaveProject(context.Context, Caller, string, map[string]any, any) (any, error)
}

// ProjectCatalogProvider is optional: do not bind recent operations until a
// provider really implements them. Memory-only catalog navigation is allowed,
// but must not be advertised as durable recent-project persistence. Persistent
// recent metadata belongs in a server-owned atomically replaced file.
type ProjectCatalogProvider interface {
	RecentProjects(context.Context, Caller) (any, error)
	RemoveRecentProject(context.Context, Caller, string) (any, error)
}

// ProjectRecentRemoveProvider drops one recent-shelf navigation record. The
// argument is the identifier project:recent-list returned, never a new host
// path: the provider resolves it against its own trusted catalog and must not
// delete the project directory.
//
// The method is deliberately not spelled the way ProjectCatalogProvider spells
// it. A provider that also implements ProjectCatalogProvider is bound by
// BindProjects, and a shared method name would let a lane that adds this one
// silently satisfy that optional interface and double-bind project:recent-list
// (OPERATION_ALREADY_BOUND at startup).
type ProjectRecentRemoveProvider interface {
	RemoveRecentNavigation(context.Context, Caller, string) (any, error)
}

// BindProjects preserves the existing frontend protocol. Only supported
// operations are bound; create/delete/import/dialog workflows are not stubs.
func (s *Server) BindProjects(provider ProjectLifecycleProvider) error {
	if provider == nil {
		return errors.New("PROJECT_LIFECYCLE_PROVIDER_REQUIRED")
	}
	handlers := map[string]Operation{
		"project:open": func(ctx context.Context, caller Caller, args []any) (any, error) {
			if len(args) != 3 {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			target, valid := projectOpenTarget(args[0])
			token, tokenOK := args[1].(string)
			if !valid || !tokenOK || token == "" {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			if args[2] != nil {
				if path, ok := args[2].(string); !ok || path == "" {
					return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
				}
			}
			// Never generate a success DTO or restored/ready state here. The
			// provider alone can observe admission and rollback outcomes.
			return provider.OpenProject(ctx, caller, target, token, args[2])
		},
		"db:close": func(ctx context.Context, caller Caller, args []any) (any, error) {
			if len(args) != 2 {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			expected, err := draftExpectedContext(args, 1)
			if err != nil {
				return nil, err
			}
			path, ok := args[0].(string)
			if !ok || path == "" {
				return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
			}
			if path != expected["projectPath"] {
				return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
			}
			return provider.CloseProject(ctx, caller, path, expected)
		},
		"project:get-runtime-context": func(ctx context.Context, caller Caller, args []any) (any, error) {
			if len(args) != 0 {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			return provider.ProjectRuntimeContext(ctx, caller)
		},
	}
	for _, operation := range []string{"project:save", "project:update-config"} {
		updateConfig := operation == "project:update-config"
		handlers[operation] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			id, patch, expected, err := projectSaveArguments(args, updateConfig)
			if err != nil {
				return projectSaveFailure(err), nil
			}
			result, err := provider.SaveProject(ctx, caller, id, patch, expected)
			if err != nil {
				return projectSaveFailure(err), nil
			}
			return result, nil
		}
	}
	if catalog, ok := provider.(ProjectCatalogProvider); ok {
		handlers["project:recent-list"] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			if len(args) != 0 {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			return catalog.RecentProjects(ctx, caller)
		}
		handlers["project:recent-remove"] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			if len(args) != 1 {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			key, ok := args[0].(string)
			if !ok || key == "" {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			return catalog.RemoveRecentProject(ctx, caller, key)
		}
	}
	// Binding is all-or-nothing, matching the other transport domain adapters.
	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range handlers {
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

func projectOpenTarget(raw any) (any, bool) {
	if key, ok := raw.(string); ok {
		// A string is an opaque server-catalog key. It may look like a path
		// for frontend compatibility, but the provider must look it up.
		return key, key != ""
	}
	grant, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	id, ok := grant["grantId"].(string)
	if !ok || id == "" {
		return nil, false
	}
	// displayName is UI metadata; do not forward caller-controlled paths or
	// grant operation/owner overrides to a provider.
	result := map[string]any{"grantId": id}
	if name, ok := grant["displayName"].(string); ok {
		result["displayName"] = name
	}
	return result, true
}

func projectSaveArguments(args []any, updateConfig bool) (string, map[string]any, map[string]any, error) {
	fail := func(code string) (string, map[string]any, map[string]any, error) {
		return "", nil, nil, security.NewHTTPError(http.StatusBadRequest, code)
	}
	if len(args) != 4 {
		return fail("INVALID_ARGUMENTS")
	}
	expected, err := draftExpectedContext(args, 3)
	if err != nil {
		return "", nil, nil, err
	}
	id, ok := args[0].(string)
	if !ok || id == "" || id != expected["projectId"] {
		return fail("PROJECT_SESSION_IDENTITY_MISMATCH")
	}
	data, ok := args[1].(map[string]any)
	if !ok || data == nil {
		return fail("INVALID_ARGUMENTS")
	}
	for key, value := range map[string]any{"id": id, "sessionLease": expected["leaseId"]} {
		if actual, present := data[key]; present && actual != value {
			return fail("PROJECT_SESSION_IDENTITY_MISMATCH")
		}
	}
	path, ok := args[2].(string)
	if !ok || path == "" {
		return fail("PROJECT_CONTEXT_REQUIRED")
	}
	if path != expected["projectPath"] {
		return fail("PROJECT_CONTEXT_CHANGED")
	}
	if actual, present := data["path"]; present {
		if actual != path {
			return fail("PROJECT_SESSION_ROOT_MISMATCH")
		}
	} else if !updateConfig {
		return fail("PROJECT_CONTEXT_REQUIRED")
	}
	patch := make(map[string]any)
	if raw, present := data["novelConfig"]; present && raw != nil {
		config, ok := raw.(map[string]any)
		if !ok {
			return fail("INVALID_ARGUMENTS")
		}
		for _, key := range []string{"genre", "subGenre", "targetAudience", "totalChapters", "wordsPerChapter", "creativeStrategy", "narrativeThreadDormantChapterThreshold", "plotStructure", "goldenFinger", "globalGuidance", "coreOutline", "worldSetting", "protagonistProfile"} {
			if value, exists := config[key]; exists {
				patch[key] = value
			}
		}
		if value, exists := config["narrativePOV"]; exists {
			patch["narrativePov"] = value
		}
		// Match the TS controllers: update-config does not update language;
		// project:save resolves invalid/empty language to zh-CN.
		if value, exists := config["writingLanguage"]; exists && !updateConfig {
			language := "zh-CN"
			if value == "en-US" {
				language = "en-US"
			}
			patch["writingLanguage"] = language
		}
		for _, key := range []string{"writingStyle", "referenceWorks"} {
			value := config[key]
			if value == nil {
				value = ""
			}
			patch[key] = value
		}
	}
	if !updateConfig {
		if name, present := data["name"]; present && name != nil && name != "" {
			patch["projectName"] = name
		}
		if value, present := data["characterStates"]; present {
			patch["characterStates"] = value
		}
	}
	// Match accepted SQLite bindings rather than forwarding nested objects to
	// a provider. SQL diagnostics and raw paths must never become error DTOs.
	for _, value := range patch {
		switch value.(type) {
		case nil, string, float64:
		default:
			return fail("PROJECT_CORE_INVALID_BINDING")
		}
	}
	return id, patch, expected, nil
}

func projectSaveFailure(err error) map[string]any {
	code := "PROJECT_SAVE_FAILED"
	var public *security.HTTPError
	if errors.As(err, &public) {
		code = public.Code
	} else if err != nil {
		candidate := err.Error()
		if strings.HasPrefix(candidate, "PROJECT_") && len(candidate) <= 100 {
			valid := true
			for _, char := range candidate {
				if char != '_' && (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
					valid = false
				}
			}
			if valid {
				code = candidate
			}
		}
	}
	return map[string]any{"success": false, "error": "Error: " + code}
}

// BindProjectRecentRemove binds the one project-family name BindProjects leaves
// unbound unless its provider happens to implement the optional
// ProjectCatalogProvider: project:recent-remove.
//
// Arity is copied from the family, not invented: the original handler takes
// exactly one argument (project-controller.ts:896) and no frozen session
// context, because dropping a shelf entry is navigation metadata rather than an
// author-database write (operations.json routes it as global). The success
// envelope is the family's `{success: true}` and its refusal is reported at
// HTTP 200 as `{success: false, error: "Error: CODE"}`, the same shape
// projectSaveFailure already returns for this family. The original reflects
// String(error) there; this binder keeps the failure symbolic instead, because
// a catalog error can carry a host path.
func (s *Server) BindProjectRecentRemove(provider ProjectRecentRemoveProvider) error {
	if provider == nil {
		return errors.New("PROJECT_RECENT_PROVIDER_REQUIRED")
	}
	return s.Bind("project:recent-remove", func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 1 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		target, ok := args[0].(string)
		if !ok || target == "" {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		result, err := provider.RemoveRecentNavigation(ctx, caller, target)
		if err != nil || result == nil {
			return map[string]any{"success": false, "error": "Error: PROJECT_RECENT_REMOVE_FAILED"}, nil
		}
		return result, nil
	})
}
