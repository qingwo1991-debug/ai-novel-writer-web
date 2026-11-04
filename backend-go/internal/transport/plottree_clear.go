package transport

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// BindPlotTreeClear binds exactly db:plot-tree-clear, the mutation that
// BindPlotTreeReads deliberately leaves out. Clearing blanks the stored
// snapshot with one store statement (store.PlotTree.Clear updates
// project_core.plot_tree_snapshot to the empty string), so unlike
// db:plot-tree-save it needs no source-revision CAS and no generation graph
// admission, and the original binder already serves it as a plain write.
//
// Arity, path index and the committed envelope mirror BindPlotTree's row for
// this name exactly (plottree.go:22 states `arity,pathIndex:=2,0` for every
// name except save): the request is (project path, frozen three-key context),
// and the result is {"success":true}.
//
// The refusal envelope is NOT the family's `Error: CODE` business result:
// BindPlotTree catches only db:plot-tree-save (plottree.go:19), so a refused
// clear stays a transport error carrying plotTreeCode's symbolic code, which
// the boundary renders as HTTP 500 with {"error":{"code":CODE}} and no
// `Error: ` prefix. That shape is preserved.
func (s *Server) BindPlotTreeClear(provider PlotTreeProvider) error {
	if provider == nil {
		return errors.New("PLOT_TREE_PROVIDER_REQUIRED")
	}
	return s.Bind("db:plot-tree-clear", func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 2 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		expected, err := draftExpectedContext(args, 1)
		if err != nil {
			return nil, err
		}
		path, ok := args[0].(string)
		if !ok || path == "" || !utf8.ValidString(path) {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
		}
		if path != expected["projectPath"] {
			return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
		}
		if ctx == nil {
			return nil, errors.New("PLOT_TREE_OPERATION_FAILED")
		}
		repository, release, err := provider.BeginPlotTree(ctx, caller, expected)
		if release != nil {
			defer release()
		}
		if err != nil {
			return nil, plotTreeClearFailure(err)
		}
		if repository == nil || release == nil {
			return nil, errors.New("PLOT_TREE_PROVIDER_UNAVAILABLE")
		}
		if err := repository.Clear(ctx); err != nil {
			return nil, plotTreeClearFailure(err)
		}
		return map[string]any{"success": true}, nil
	})
}

// plotTreeClearFailure mirrors BindPlotTree's `fail` for this name: a public
// HTTP error passes through as itself (a caller-visible 400/409), and every
// other failure becomes a bare symbolic error through plotTreeCode's finite
// vocabulary, never the wrapped store text, which may carry SQL or a path.
func plotTreeClearFailure(err error) error {
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return public
	}
	return errors.New(plotTreeCode(err))
}
