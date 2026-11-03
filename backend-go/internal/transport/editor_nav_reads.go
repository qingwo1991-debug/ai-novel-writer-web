package transport

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// RecentNavigationProject is the browser's recent-project shape. A catalog
// key is intentionally absent; the server resolves paths against its trusted
// catalog before any later project open.
type RecentNavigationProject struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	UpdatedAt string `json:"updatedAt"`
	ProjectID string `json:"projectId,omitempty"`
}

// EditorNavigationProvider owns catalog lookup and a pinned project tree.
// ListDirectory must hold the exact caller's project lease for its whole read.
type EditorNavigationProvider interface {
	RecentProjects(context.Context, Caller) ([]RecentNavigationProject, error)
	ListDirectory(context.Context, Caller, any, string) ([]projectfs.VisibleNode, error)
}

// BindEditorNavigation adds the two read-only operations needed before the
// editor can populate its shelf and project tree. It grants no file writes.
func (s *Server) BindEditorNavigation(provider EditorNavigationProvider) error {
	if provider == nil {
		return errors.New("EDITOR_NAVIGATION_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("EDITOR_NAVIGATION_PROVIDER_REQUIRED")
		}
	}
	handlers := map[string]Operation{
		"project:recent-list": func(ctx context.Context, caller Caller, args []any) (any, error) {
			if len(args) != 0 {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			projects, err := provider.RecentProjects(ctx, caller)
			if err != nil {
				return nil, editorNavigationError(err, "PROJECT_RECENT_LIST_FAILED")
			}
			if projects == nil {
				projects = []RecentNavigationProject{}
			}
			return projects, nil
		},
		"fs:list-dir": func(ctx context.Context, caller Caller, args []any) (any, error) {
			if len(args) != 3 {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			expected, err := draftExpectedContext(args, 2)
			if err != nil {
				return nil, err
			}
			root, ok := expected["projectPath"].(string)
			if !ok || !editorCanonicalPath(root) {
				return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
			}
			path, pathOK := args[0].(string)
			claimedRoot, rootOK := args[1].(string)
			if !pathOK || !rootOK || !editorCanonicalPath(path) || !editorCanonicalPath(claimedRoot) {
				return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
			}
			if claimedRoot != root {
				return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
			}
			relative, err := filepath.Rel(root, path)
			if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
			}
			tree, err := provider.ListDirectory(ctx, caller, expected, relative)
			if err != nil {
				return nil, editorNavigationError(err, "FS_LIST_DIR_FAILED")
			}
			if tree == nil {
				tree = []projectfs.VisibleNode{}
			}
			return tree, nil
		},
	}
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

func editorCanonicalPath(path string) bool {
	return path != "" && len(path) <= 4096 && utf8.ValidString(path) &&
		!strings.ContainsAny(path, "\x00\\") && filepath.IsAbs(path) && filepath.Clean(path) == path
}

func editorNavigationError(err error, fallback string) error {
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
		case "PROJECT_SESSION_NOT_OWNED", "PROJECT_SESSION_CONTEXT_REQUIRED", "PROJECT_SESSION_STALE", "PROJECT_NOT_OPEN":
			return errors.New(err.Error())
		}
	}
	return errors.New(fallback)
}
