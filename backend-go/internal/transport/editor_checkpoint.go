package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// EditorCheckpointProvider reads only the legacy partial architecture marker
// while the exact project lease is held. It cannot read arbitrary JSON files.
type EditorCheckpointProvider interface {
	ReadArchCheckpoint(context.Context, Caller, any) (json.RawMessage, bool, error)
}

type EditorCheckpointProviderFunc func(context.Context, Caller, any) (json.RawMessage, bool, error)

func (f EditorCheckpointProviderFunc) ReadArchCheckpoint(ctx context.Context, caller Caller, expected any) (json.RawMessage, bool, error) {
	if f == nil {
		return nil, false, errors.New("EDITOR_CHECKPOINT_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, expected)
}

func (s *Server) BindEditorCheckpoint(provider EditorCheckpointProvider) error {
	if provider == nil {
		return errors.New("EDITOR_CHECKPOINT_PROVIDER_REQUIRED")
	}
	return s.Bind("fs:read-json", func(ctx context.Context, caller Caller, args []any) (any, error) {
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
		if !pathOK || !rootOK || claimedRoot != root || path != filepath.Join(root, projectfs.StorageDirectory, "partial_arch.json") {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
		}
		wire, found, err := provider.ReadArchCheckpoint(ctx, caller, expected)
		if err != nil {
			return nil, editorNavigationError(err, "FS_READ_JSON_FAILED")
		}
		if !found {
			return map[string]any{"success": false, "data": nil}, nil
		}
		if !json.Valid(wire) {
			return nil, errors.New("FS_READ_JSON_FAILED")
		}
		return map[string]any{"success": true, "data": json.RawMessage(append([]byte(nil), wire...))}, nil
	})
}
