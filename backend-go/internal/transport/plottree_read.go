package transport

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// BindPlotTreeReads binds only the single plot tree read that the durable lane
// serves from its admitted author database. BindPlotTree registers the whole
// family; this binder takes exactly the read, so the two mutations are bound
// elsewhere: db:plot-tree-save stays unbound because the original refuses it
// unconditionally, and db:plot-tree-clear is bound by BindPlotTreeClear.
//
// Arity, path index and wire normalization mirror BindPlotTree exactly:
// db:plot-tree-read is arity 2 with the project path at index 0
// (plottree.go:18, `arity,pathIndex:=2,0`), and the frozen three-key session
// context is the LAST argument (plottree.go:18,
// `expected,err:=draftExpectedContext(args,len(args)-1)`), so the read really
// arrives as (project path, frozen context). The store read is SELECT-only:
// PlotTree.Read opens a read-only transaction over plotRead's queries and never
// writes, so unlike the save/clear names it can be bound on this lane.
//
// The provider is the same seam BindPlotTree already uses: it lends one
// admitted repository per request and never a database handle, so a lane that
// takes this binder answers the read with the original's shape instead of an
// error.
func (s *Server) BindPlotTreeReads(provider PlotTreeProvider) error {
	if provider == nil {
		return errors.New("PLOT_TREE_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("PLOT_TREE_PROVIDER_REQUIRED")
		}
	}
	operations := []struct {
		name             string
		arity, pathIndex int
	}{
		{"db:plot-tree-read", 2, 0},
	}
	handlers := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		name, arity, pathIndex := operation.name, operation.arity, operation.pathIndex
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			if len(args) != arity {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			expected, err := draftExpectedContext(args, arity-1)
			if err != nil {
				return nil, err
			}
			path, ok := args[pathIndex].(string)
			if !ok || path == "" || !utf8.ValidString(path) {
				return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
			}
			if path != expected["projectPath"] {
				return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
			}
			// The read carries no leading argument in front of the path, so
			// there is nothing else to validate before the borrow.
			if ctx == nil {
				return nil, errors.New("PLOT_TREE_READ_FAILED")
			}
			repository, release, err := provider.BeginPlotTree(ctx, caller, expected)
			if release != nil {
				defer release()
			}
			if err != nil {
				return nil, plotTreeReadError(err)
			}
			if repository == nil || release == nil {
				return nil, errors.New("PLOT_TREE_PROVIDER_UNAVAILABLE")
			}
			result, err := repository.Read(ctx)
			if err != nil {
				return nil, plotTreeReadError(err)
			}
			return reviewRevisionWire(result), nil
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

// plotTreeReadError keeps this read failure symbolic. A borrow error may wrap a
// filesystem path or raw SQLite text, so only the finite vocabulary below is
// ever reflected; nothing else reaches the browser verbatim. plotTreeCode is
// the original transport binder's own finite mapping, so a store error keeps
// exactly the code BindPlotTree would have returned for the same read.
func plotTreeReadError(err error) error {
	if errors.Is(err, context.Canceled) {
		return errors.New("REQUEST_CANCELED")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("REQUEST_DEADLINE_EXCEEDED")
	}
	var public *security.HTTPError
	if errors.As(err, &public) {
		if public == nil {
			return errors.New("PLOT_TREE_READ_FAILED")
		}
		// Rebuild so a provider's own message never becomes public text.
		switch public.Code {
		case "INVALID_ARGUMENTS", "PROJECT_CONTEXT_REQUIRED", "PROJECT_CONTEXT_CHANGED", "PROJECT_SESSION_REQUIRED":
			return security.NewHTTPError(public.Status, public.Code)
		}
		return errors.New("PLOT_TREE_READ_FAILED")
	}
	return errors.New(plotTreeCode(err))
}
