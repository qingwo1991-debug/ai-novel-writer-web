package transport

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// GenerationProvider is the slice S1 durable generation chain. Every function
// receives the frozen browser project session (the exact 3-key expected map)
// plus the raw op arguments, and returns the already-projected DTO or an error.
// A nil function field leaves that group unbound so the server fails closed
// with the existing 501 GO_OPERATION_NOT_MIGRATED; the remaining generation
// family ops are never registered here.
type GenerationProvider struct {
	Begin                  func(context.Context, Caller, map[string]any, any) (any, error)
	BindMaterialDecision   func(context.Context, Caller, map[string]any, any) (any, error)
	Execute                func(context.Context, Caller, map[string]any, any) (any, error)
	Read                   func(context.Context, Caller, map[string]any, any) (any, error)
	List                   func(context.Context, Caller, map[string]any) (any, error)
	Pause                  func(context.Context, Caller, map[string]any, any) (any, error)
	Cancel                 func(context.Context, Caller, map[string]any, any) (any, error)
	Resume                 func(context.Context, Caller, map[string]any, any) (any, error)
	Restart                func(context.Context, Caller, map[string]any, any, any) (any, error)
	DiscardCandidate       func(context.Context, Caller, map[string]any, any, any) (any, error)
	RetryDraftShortOutline func(context.Context, Caller, map[string]any, any) (any, error)
}

// BindGeneration binds the eleven S1 names plus the two family cancels, which
// share the ordinary handle shape and the ordinary stop path. The browser IPC contract
// sends args = [request..., expectedContext], so each op's wire arity is its
// request arity plus one. Read ops surface security.HTTPError envelopes;
// mutating ops return the business DTO {"success":false,"error":"Error: CODE"}.
func (s *Server) BindGeneration(provider *GenerationProvider) error {
	if provider == nil {
		return errors.New("GENERATION_PROVIDER_REQUIRED")
	}
	operations := []struct {
		name     string
		arity    int
		mutating bool
		call     func(ctx context.Context, caller Caller, expected map[string]any, args []any) (any, error)
	}{
		{"generation:begin", 1, true, func(ctx context.Context, caller Caller, expected map[string]any, args []any) (any, error) {
			if provider.Begin == nil {
				return nil, errors.New("GENERATION_OPERATION_UNSUPPORTED")
			}
			return provider.Begin(ctx, caller, expected, args[0])
		}},
		{"generation:bind-material-decision", 1, true, func(ctx context.Context, caller Caller, expected map[string]any, args []any) (any, error) {
			if provider.BindMaterialDecision == nil {
				return nil, errors.New("GENERATION_OPERATION_UNSUPPORTED")
			}
			return provider.BindMaterialDecision(ctx, caller, expected, args[0])
		}},
		{"generation:execute", 1, true, func(ctx context.Context, caller Caller, expected map[string]any, args []any) (any, error) {
			if provider.Execute == nil {
				return nil, errors.New("GENERATION_OPERATION_UNSUPPORTED")
			}
			return provider.Execute(ctx, caller, expected, args[0])
		}},
		{"generation:read", 1, false, func(ctx context.Context, caller Caller, expected map[string]any, args []any) (any, error) {
			if provider.Read == nil {
				return nil, errors.New("GENERATION_OPERATION_UNSUPPORTED")
			}
			return provider.Read(ctx, caller, expected, args[0])
		}},
		{"generation:list", 0, false, func(ctx context.Context, caller Caller, expected map[string]any, args []any) (any, error) {
			if provider.List == nil {
				return nil, errors.New("GENERATION_OPERATION_UNSUPPORTED")
			}
			return provider.List(ctx, caller, expected)
		}},
		{"generation:pause", 1, true, func(ctx context.Context, caller Caller, expected map[string]any, args []any) (any, error) {
			if provider.Pause == nil {
				return nil, errors.New("GENERATION_OPERATION_UNSUPPORTED")
			}
			return provider.Pause(ctx, caller, expected, args[0])
		}},
		{"generation:cancel", 1, true, func(ctx context.Context, caller Caller, expected map[string]any, args []any) (any, error) {
			if provider.Cancel == nil {
				return nil, errors.New("GENERATION_OPERATION_UNSUPPORTED")
			}
			return provider.Cancel(ctx, caller, expected, args[0])
		}},
		// Cancelling a graph or finalization run is the same frozen-handle stop the
		// ordinary lane already performs, so it reuses the same call rather than a
		// parallel implementation.
		{"graph-generation:cancel", 1, true, func(ctx context.Context, caller Caller, expected map[string]any, args []any) (any, error) {
			if provider.Cancel == nil {
				return nil, errors.New("GENERATION_OPERATION_UNSUPPORTED")
			}
			handle, err := familyCancelHandle(args[0], "GENERATION_GRAPH_REQUEST_INVALID")
			if err != nil {
				return nil, err
			}
			return provider.Cancel(ctx, caller, expected, handle)
		}},
		{"finalization-generation:cancel", 1, true, func(ctx context.Context, caller Caller, expected map[string]any, args []any) (any, error) {
			if provider.Cancel == nil {
				return nil, errors.New("GENERATION_OPERATION_UNSUPPORTED")
			}
			handle, err := familyCancelHandle(args[0], "GENERATION_FINALIZATION_REQUEST_INVALID")
			if err != nil {
				return nil, err
			}
			return provider.Cancel(ctx, caller, expected, handle)
		}},
		{"generation:resume", 1, true, func(ctx context.Context, caller Caller, expected map[string]any, args []any) (any, error) {
			if provider.Resume == nil {
				return nil, errors.New("GENERATION_OPERATION_UNSUPPORTED")
			}
			return provider.Resume(ctx, caller, expected, args[0])
		}},
		{"generation:restart", 2, true, func(ctx context.Context, caller Caller, expected map[string]any, args []any) (any, error) {
			if provider.Restart == nil {
				return nil, errors.New("GENERATION_OPERATION_UNSUPPORTED")
			}
			return provider.Restart(ctx, caller, expected, args[0], args[1])
		}},
		{"generation:discard-candidate", 2, true, func(ctx context.Context, caller Caller, expected map[string]any, args []any) (any, error) {
			if provider.DiscardCandidate == nil {
				return nil, errors.New("GENERATION_OPERATION_UNSUPPORTED")
			}
			return provider.DiscardCandidate(ctx, caller, expected, args[0], args[1])
		}},
		{"generation:retry-draft-short-outline", 1, true, func(ctx context.Context, caller Caller, expected map[string]any, args []any) (any, error) {
			if provider.RetryDraftShortOutline == nil {
				return nil, errors.New("GENERATION_OPERATION_UNSUPPORTED")
			}
			return provider.RetryDraftShortOutline(ctx, caller, expected, args[0])
		}},
	}
	handlers := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		name, arity, mutating := operation.name, operation.arity, operation.mutating
		call := operation.call
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			fail := func(err error) (any, error) { return generationFailure(mutating, err) }
			if len(args) != arity+1 {
				return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
			}
			raw, ok := args[arity].(map[string]any)
			if !ok || len(raw) != 3 {
				return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
			}
			expected := make(map[string]any, 3)
			for _, key := range []string{"projectId", "leaseId", "projectPath"} {
				text, ok := raw[key].(string)
				if !ok || text == "" || !utf8.ValidString(text) {
					return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
				}
				expected[key] = text
			}
			if ctx == nil {
				return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
			}
			return call(ctx, caller, expected, args)
		}
	}
	for name, handler := range handlers {
		if err := s.Bind(name, handler); err != nil {
			return err
		}
	}
	return nil
}

// generationFailure maps one op error to the lane's wire envelope. Read ops
// surface security.HTTPError directly; mutating ops return the business DTO.
func generationFailure(mutating bool, err error) (any, error) {
	if !mutating {
		var public *security.HTTPError
		if errors.As(err, &public) && public != nil {
			return nil, security.NewHTTPError(public.Status, public.Code)
		}
		return nil, security.NewHTTPError(http.StatusInternalServerError, generationSymbolicCode(err))
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return draftSaveFailure(public), nil
	}
	return draftSaveFailure(err), nil
}

// generationSymbolicCode extracts a safe all-caps symbolic code from an op
// error, mirroring the business-DTO code rule; anything else is REQUEST_FAILED.
func generationSymbolicCode(err error) string {
	if err == nil {
		return "REQUEST_FAILED"
	}
	code := err.Error()
	var public *security.HTTPError
	if errors.As(err, &public) {
		code = public.Code
	}
	if len(code) < 2 {
		return "REQUEST_FAILED"
	}
	for i, c := range code {
		if !(c >= 'A' && c <= 'Z') && !(i > 0 && (c >= '0' && c <= '9' || c == '_')) {
			return "REQUEST_FAILED"
		}
	}
	return code
}

// familyCancelHandle unwraps the `{ handle }` envelope the graph and finalization
// families use. It is not the same wire shape as `generation:cancel`, which
// takes the handle itself: the renderer sends `{ handle: view.handle }` for the
// family channels (src/services/graph-generation.ts:40,
// src/services/finalization-generation.ts:27) and the Node owner rejects any
// request carrying a key other than `handle` before it looks at the run.
// Passing the envelope straight through would make every real call fail while a
// test that only checks "not 501" still passed.
func familyCancelHandle(value any, invalidCode string) (any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New(invalidCode)
	}
	for key := range object {
		if key != "handle" {
			return nil, errors.New(invalidCode)
		}
	}
	handle, present := object["handle"]
	if !present {
		return nil, errors.New(invalidCode)
	}
	return handle, nil
}
