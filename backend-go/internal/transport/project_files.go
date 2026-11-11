package transport

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"reflect"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// projectFileMaxBytes bounds one browser file body held for a single request.
// It matches projectfs.BrowserFileMaxBytes so the transport refusal and the
// filesystem refusal agree on the same ceiling.
const projectFileMaxBytes = projectfs.BrowserFileMaxBytes

// ProjectFileProvider lends the admitted project tree for one file operation
// and never a host path. Every method re-observes the caller's frozen project
// lease, so a request whose lease was replaced cannot read or write another
// author's manuscript.
//
// The path argument is the browser's path RELATIVE to the admitted project
// root. It is not a host path and it is not a desktop external-file grant id:
// this binder replaces that grant model instead of reusing it.
type ProjectFileProvider interface {
	ReadProjectFile(context.Context, Caller, any, string, int64) ([]byte, error)
	WriteProjectFile(context.Context, Caller, any, string, []byte) error
	ProjectPathExists(context.Context, Caller, any, string) (bool, error)
	MakeProjectDirectory(context.Context, Caller, any, string) error
}

// BindProjectFiles serves the four fs:* names a browser can honestly be given:
// fs:read-file, fs:write-file, fs:mkdir and fs:check-exists.
//
// The wire arity is copied from the desktop fs-controller
// (electron/controllers/fs-controller.ts:222-317) and from the fs binder this
// lane already runs, BindEditorNavigation (editor_nav_reads.go:60-92):
//
//	fs:read-file    [relativePath, claimedRoot, context]          arity 3
//	fs:mkdir        [relativePath, claimedRoot, context]          arity 3
//	fs:check-exists [relativePath, claimedRoot, context]          arity 3
//	fs:write-file   [relativePath, content, claimedRoot, context] arity 4
//
// The frozen three-key session context is the LAST wire argument in every one
// of them, exactly as the renderer appends it (src/services/ipc-client.ts:120)
// and exactly as draftExpectedContext expects it. The second-to-last argument
// is the claimed project root the renderer froze; it must equal the path inside
// the frozen context, so a page cannot aim one project's lease at another
// project's tree.
//
// TWO deliberate departures from the desktop contract, both stricter:
//
//  1. The path is RELATIVE. The desktop accepted an absolute host path and
//     confined it with a realpath comparison; a browser-exposed server that did
//     the same would let any page name /etc/passwd and depend on one comparison
//     being right. projectfs.BrowserRelativePath refuses absolute paths,
//     traversal, hidden components and storage-reserved names before this
//     binder borrows a project at all.
//  2. The refusal envelope is the family's own. fs:read-file, fs:write-file and
//     fs:mkdir answered with `{success:false,...}` on the desktop
//     (fs-controller.ts:95-127), so they answer with that shape here too; only
//     fs:check-exists, which returned a bare boolean and threw on refusal,
//     answers with a transport error. The error strings are symbolic codes
//     because the lane has no i18n table and because a code cannot leak a host
//     path.
//
// fs:read-json, fs:list-dir, fs:write-json and the three fs:grant-* names are
// NOT bound here. fs:read-json and fs:list-dir already have their own binders;
// the grant names are refused because there is no server-issued external grant
// in a browser; fs:write-json stays at GO_OPERATION_NOT_MIGRATED because its
// only observed caller writes the .ai-novel storage marker that this confined
// surface deliberately keeps out of a browser's reach.
func (s *Server) BindProjectFiles(provider ProjectFileProvider) error {
	if provider == nil {
		return errors.New("PROJECT_FILE_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("PROJECT_FILE_PROVIDER_REQUIRED")
		}
	}
	operations := []struct {
		name             string
		arity, rootIndex int
	}{
		{"fs:read-file", 3, 1},
		{"fs:mkdir", 3, 1},
		{"fs:check-exists", 3, 1},
		{"fs:write-file", 4, 2},
	}
	handlers := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		name, arity, rootIndex := operation.name, operation.arity, operation.rootIndex
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			// fs:check-exists threw on the desktop and returns a bare boolean,
			// so its refusal must stay a transport error; the other three keep
			// the family's business envelope.
			fail := func(code string) (any, error) {
				if name == "fs:check-exists" {
					return nil, security.NewHTTPError(projectFileStatus(code), code)
				}
				return projectFileFailure(name, code), nil
			}
			if len(args) != arity {
				return fail("INVALID_ARGUMENTS")
			}
			// The frozen session context is last; anything malformed there is a
			// session refusal, not a file refusal.
			expected, err := draftExpectedContext(args, arity-1)
			if err != nil {
				return fail("PROJECT_SESSION_REQUIRED")
			}
			root, ok := expected["projectPath"].(string)
			if !ok || !editorCanonicalPath(root) {
				return fail("PROJECT_SESSION_REQUIRED")
			}
			claimed, ok := args[rootIndex].(string)
			if !ok || !editorCanonicalPath(claimed) {
				return fail("PROJECT_CONTEXT_REQUIRED")
			}
			if claimed != root {
				return fail("PROJECT_CONTEXT_CHANGED")
			}
			path, ok := args[0].(string)
			if !ok {
				return fail("PROJECT_FILE_PATH_REFUSED")
			}
			// The shipped renderer builds an ABSOLUTE path from the project root
			// (`${projectPath}/.ai-novel/skills`, writing-skill-bindings and the
			// prompt catalog in src). Accept exactly that form by making it
			// relative to the frozen root this request already proved; every
			// other absolute path stays refused, and a traversal still fails the
			// relative-path filter below.
			if strings.HasPrefix(path, root+"/") {
				path = strings.TrimPrefix(path, root+"/")
			}
			// Validate the browser path before the provider borrows anything, so
			// an absolute path or a traversal never reaches a filesystem call.
			if _, err := projectfs.BrowserRelativePath(path); err != nil {
				return fail("PROJECT_FILE_PATH_REFUSED")
			}
			if ctx == nil {
				return fail("FS_PROJECT_FILE_FAILED")
			}
			switch name {
			case "fs:read-file":
				data, err := provider.ReadProjectFile(ctx, caller, expected, path, projectFileMaxBytes)
				if err != nil {
					return fail(projectFileCode(err, "FS_READ_FILE_FAILED"))
				}
				return map[string]any{"success": true, "content": string(data)}, nil
			case "fs:write-file":
				content, ok := args[1].(string)
				if !ok {
					return fail("INVALID_ARGUMENTS")
				}
				if int64(len(content)) > projectFileMaxBytes {
					return fail("PROJECT_FILE_TOO_LARGE")
				}
				if err := provider.WriteProjectFile(ctx, caller, expected, path, []byte(content)); err != nil {
					// A replacement that may have landed is reported as
					// commitState "unknown", never as "not_committed": the
					// browser must re-read instead of assuming it can retry.
					state := "not_committed"
					if errors.Is(err, projectfs.ErrBrowserCommitUnknown) {
						state = "unknown"
					}
					return map[string]any{"success": false, "commitState": state, "error": projectFileCode(err, "FS_WRITE_FILE_FAILED")}, nil
				}
				return map[string]any{"success": true, "commitState": "committed"}, nil
			case "fs:mkdir":
				if err := provider.MakeProjectDirectory(ctx, caller, expected, path); err != nil {
					return fail(projectFileCode(err, "FS_MKDIR_FAILED"))
				}
				return map[string]any{"success": true}, nil
			case "fs:check-exists":
				present, err := provider.ProjectPathExists(ctx, caller, expected, path)
				if err != nil {
					return fail(projectFileCode(err, "FS_CHECK_EXISTS_FAILED"))
				}
				return present, nil
			}
			return fail("INVALID_ARGUMENTS")
		}
	}
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

// projectFileFailure is the family's business refusal envelope. The desktop
// returned these exact shapes (fs-controller.ts:101-125), so a browser caller
// keeps reading `success`/`commitState`/`error` and never has to treat a
// refusal as a transport failure.
func projectFileFailure(name, code string) map[string]any {
	switch name {
	case "fs:read-file":
		return map[string]any{"success": false, "content": "", "error": code}
	case "fs:write-file":
		return map[string]any{"success": false, "commitState": "not_committed", "error": code}
	}
	return map[string]any{"success": false, "error": code}
}

// projectFileCode maps a provider failure to one symbolic code. The mapping is
// finite on purpose: a wrapped host path, a SQLite message or an os error text
// must never reach the browser.
func projectFileCode(err error, fallback string) string {
	switch {
	case err == nil:
		return fallback
	case errors.Is(err, projectfs.ErrBrowserPathRefused):
		return "PROJECT_FILE_PATH_REFUSED"
	case errors.Is(err, projectfs.ErrBrowserCommitUnknown):
		return "PROJECT_FILE_COMMIT_UNKNOWN"
	case errors.Is(err, projectfs.ErrTooLarge):
		return "PROJECT_FILE_TOO_LARGE"
	case errors.Is(err, projectfs.ErrUnsafeFile), errors.Is(err, projectfs.ErrUnsafeDirectory):
		return "PROJECT_FILE_UNSAFE"
	case errors.Is(err, projectfs.ErrChanged):
		return "PROJECT_FILE_CHANGED"
	case errors.Is(err, projectfs.ErrClosed):
		return "PROJECT_HANDLE_CLOSED"
	case errors.Is(err, fs.ErrNotExist):
		return "PROJECT_FILE_NOT_FOUND"
	case errors.Is(err, context.Canceled):
		return "REQUEST_CANCELED"
	case errors.Is(err, context.DeadlineExceeded):
		return "REQUEST_DEADLINE_EXCEEDED"
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil && public.Code != "" {
		return public.Code
	}
	switch err.Error() {
	case "PROJECT_SESSION_STALE", "PROJECT_SESSION_CONTEXT_REQUIRED", "PROJECT_SESSION_REQUIRED",
		"PROJECT_SESSION_NOT_OWNED", "PROJECT_NOT_OPEN", "SYNTHETIC_DURABLE_CLOSED":
		return err.Error()
	}
	return fallback
}

// projectFileStatus is the HTTP status for the one channel that answers with a
// transport error. It follows the code family the rest of this package uses.
func projectFileStatus(code string) int {
	switch code {
	case "INVALID_ARGUMENTS", "PROJECT_SESSION_REQUIRED", "PROJECT_CONTEXT_REQUIRED", "PROJECT_FILE_PATH_REFUSED":
		return http.StatusBadRequest
	case "PROJECT_CONTEXT_CHANGED", "PROJECT_SESSION_STALE", "PROJECT_FILE_CHANGED":
		return http.StatusConflict
	case "PROJECT_FILE_NOT_FOUND":
		return http.StatusNotFound
	case "PROJECT_FILE_TOO_LARGE":
		return http.StatusRequestEntityTooLarge
	case "PROJECT_FILE_UNSAFE":
		return http.StatusForbidden
	case "REQUEST_CANCELED", "REQUEST_DEADLINE_EXCEEDED":
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}
