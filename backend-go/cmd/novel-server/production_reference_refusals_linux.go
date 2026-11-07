//go:build linux

package main

import (
	"context"
	"errors"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file binds the two `db:*` names the DESKTOP's own controller refuses, as
// faithful refusals rather than a 501 fail-closed.
//
// The reference is explicit about both:
//   - db:plot-tree-save      electron/controllers/db-controller.ts:886 refuses every
//     renderer-generated write with GENERATION_GRAPH_ADMISSION_REQUIRED
//     (internal/transport/plottree.go:20-22).
//   - db:blueprint-update-notes  electron/controllers/db-controller.ts:636 refuses
//     with GENERATION_FINALIZATION_ADMISSION_REQUIRED
//     (internal/transport/blueprints.go:26).
//
// A 501 says "this lane has not migrated the name"; the browser then cannot tell a
// migrated-and-working name from a name whose only correct answer is a refusal. These
// two are the second kind, so they are bound now and answer the reference's own code,
// after the same arity, context and path checks their serving neighbours perform, and
// after the same borrow fence - a caller without a live lease gets the same
// PROJECT_NOT_OPEN the serving neighbours give, not the refusal.

const (
	productionReferenceRefusalPlotTree   = "db:plot-tree-save"
	productionReferenceRefusalBlueprint  = "db:blueprint-update-notes"
)

// productionReferenceRefusalArgs is the shared argument check the two binders' own
// tables spell out: arity and path index differ per name, the frozen three-key context
// is the trailing argument, and the path must equal the context's projectPath.
func productionReferenceRefusalArgs(args []any, arity, pathIndex int) error {
	if len(args) != arity {
		return security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	expected, err := projectsession.ParseContext(args[len(args)-1])
	if err != nil {
		return security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
	}
	path, ok := args[pathIndex].(string)
	if !ok || path == "" {
		return security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
	}
	if path != expected.ProjectPath {
		return security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
	}
	return nil
}

func bindProductionReferenceRefusals(durable *productionDurableOpen) func(*transport.Server) error {
	return func(server *transport.Server) error {
		if durable == nil || server == nil {
			return errors.New("PRODUCTION_REFERENCE_REFUSAL_BINDER_INVALID")
		}
		if err := server.Bind(productionReferenceRefusalPlotTree, func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
			if err := productionReferenceRefusalArgs(args, 4, 2); err != nil {
				return nil, err
			}
			_, release, err := durable.BeginPlotTree(ctx, caller, args[len(args)-1])
			if release != nil {
				defer release()
			}
			if err != nil {
				return nil, err
			}
			return map[string]any{"success": false, "error": "Error: GENERATION_GRAPH_ADMISSION_REQUIRED"}, nil
		}); err != nil {
			return err
		}
		return server.Bind(productionReferenceRefusalBlueprint, func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
			if err := productionReferenceRefusalArgs(args, 4, 2); err != nil {
				return nil, err
			}
			_, release, err := durable.BeginBlueprints(ctx, caller, args[len(args)-1])
			if release != nil {
				defer release()
			}
			if err != nil {
				return nil, err
			}
			return map[string]any{"success": false, "error": "Error: GENERATION_FINALIZATION_ADMISSION_REQUIRED"}, nil
		})
	}
}
