package transport

import (
	"context"
	"errors"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// BindModelRouting exposes the original's six model-routing preference names
// (llm:get/set-stage-model, llm:get/set-default-model and
// llm:get/set-default-embedding-model) from the same pinned private global root
// BindModelReads uses. None of them dispatches to a provider: each is a read or
// a single-document write of config.json, so this family needs neither the
// administrator egress policy nor a generation lease.
//
// The execution family deliberately stays unbound. llm:generate,
// llm:generate-stream and llm:test-connection only take effect through a live
// provider round trip, llm:discover-models only through an outbound /models
// request, llm:cancel only against a stream this lane cannot start, and the two
// execution-lease names only through an in-flight lease registry no bound
// operation here can populate. Binding them would mean inventing a provider.
func (s *Server) BindModelRouting(models *globalconfig.Service, startup *globalconfig.Startup) error {
	handlers, err := modelRoutingHandlers(models, startup)
	if err != nil {
		return err
	}
	return s.bindOperations(handlers)
}

// modelRoutingReadNames are the three routing names that only read a committed
// config document. None of them stores a preference, so none of them can move
// the pinned global generation a read-only lane serves.
var modelRoutingReadNames = [...]string{"llm:get-stage-models", "llm:get-default-model", "llm:get-default-embedding-model"}

// BindModelRoutingReads installs only the three routing reads above from the
// same pinned private global root BindModelRouting uses. A lane that serves the
// author's committed generation must not receive the three setters: each one
// rewrites config.json in place, which is a global-config write this lane has
// never owned (config:set is unbound for exactly that reason), so they keep
// failing closed at GO_OPERATION_NOT_MIGRATED instead of being installed.
func (s *Server) BindModelRoutingReads(models *globalconfig.Service, startup *globalconfig.Startup) error {
	handlers, err := modelRoutingHandlers(models, startup)
	if err != nil {
		return err
	}
	reads := make(map[string]Operation, len(modelRoutingReadNames))
	for _, name := range modelRoutingReadNames {
		reads[name] = handlers[name]
	}
	return s.bindOperations(reads)
}

// modelRoutingHandlers is the single implementation shared by both binders, so
// the read half can never drift from the six-name family.
func modelRoutingHandlers(models *globalconfig.Service, startup *globalconfig.Startup) (map[string]Operation, error) {
	if models == nil || startup == nil {
		return nil, errors.New("MODEL_ROUTING_PROVIDER_REQUIRED")
	}
	handlers := map[string]Operation{}
	handlers["llm:get-stage-models"] = func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 0 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		if startup.State(startupCaller(caller), true).State != "ready" {
			return nil, security.NewHTTPError(http.StatusConflict, "GLOBAL_DATA_NOT_READY")
		}
		if err := ctx.Err(); err != nil {
			return nil, security.NewHTTPError(http.StatusConflict, "MODEL_CONFIG_CANCELLED")
		}
		stages, err := models.StageModels()
		if err != nil {
			return nil, globalConfigPublicError(err)
		}
		return stages, nil
	}
	handlers["llm:set-stage-model"] = func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 1 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		if startup.State(startupCaller(caller), true).State != "ready" {
			return nil, security.NewHTTPError(http.StatusConflict, "GLOBAL_DATA_NOT_READY")
		}
		if err := ctx.Err(); err != nil {
			return nil, security.NewHTTPError(http.StatusConflict, "MODEL_CONFIG_CANCELLED")
		}
		// The original destructures `{ stage, modelId }` out of an object
		// argument and refuses everything else. A missing modelId key is
		// `undefined`, which is neither null nor a string, so it is refused
		// too: present-with-null clears the stage, absent does not.
		request, ok := args[0].(map[string]any)
		if !ok {
			return modelRoutingRefusal("LLM_STAGE_MODEL_REQUEST_INVALID"), nil
		}
		stage, ok := request["stage"].(string)
		if !ok || !globalconfig.IsModelStage(stage) {
			return modelRoutingRefusal("LLM_STAGE_MODEL_REQUEST_INVALID"), nil
		}
		raw, present := request["modelId"]
		if !present {
			return modelRoutingRefusal("LLM_STAGE_MODEL_REQUEST_INVALID"), nil
		}
		var modelID *string
		if raw != nil {
			text, ok := raw.(string)
			if !ok {
				return modelRoutingRefusal("LLM_STAGE_MODEL_REQUEST_INVALID"), nil
			}
			modelID = &text
		}
		stages, err := models.AssignStageModel(stage, modelID)
		if err != nil {
			return modelRoutingRefusal(modelRoutingFailureCode(err)), nil
		}
		return map[string]any{"success": true, "stageModelIds": stages}, nil
	}
	handlers["llm:get-default-model"] = func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 0 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		if startup.State(startupCaller(caller), true).State != "ready" {
			return nil, security.NewHTTPError(http.StatusConflict, "GLOBAL_DATA_NOT_READY")
		}
		if err := ctx.Err(); err != nil {
			return nil, security.NewHTTPError(http.StatusConflict, "MODEL_CONFIG_CANCELLED")
		}
		value, err := models.DefaultModel()
		if err != nil {
			return nil, globalConfigPublicError(err)
		}
		return value, nil
	}
	handlers["llm:set-default-model"] = func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 1 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		if startup.State(startupCaller(caller), true).State != "ready" {
			return nil, security.NewHTTPError(http.StatusConflict, "GLOBAL_DATA_NOT_READY")
		}
		if err := ctx.Err(); err != nil {
			return nil, security.NewHTTPError(http.StatusConflict, "MODEL_CONFIG_CANCELLED")
		}
		// A default id is stored exactly as submitted, null included: the
		// original performs no reference or capability validation here.
		if err := models.SetDefaultModel(args[0]); err != nil {
			return modelRoutingRefusal(modelRoutingFailureCode(err)), nil
		}
		return map[string]any{"success": true}, nil
	}
	handlers["llm:get-default-embedding-model"] = func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 0 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		if startup.State(startupCaller(caller), true).State != "ready" {
			return nil, security.NewHTTPError(http.StatusConflict, "GLOBAL_DATA_NOT_READY")
		}
		if err := ctx.Err(); err != nil {
			return nil, security.NewHTTPError(http.StatusConflict, "MODEL_CONFIG_CANCELLED")
		}
		value, err := models.DefaultEmbeddingModel()
		if err != nil {
			return nil, globalConfigPublicError(err)
		}
		return value, nil
	}
	handlers["llm:set-default-embedding-model"] = func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 1 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		if startup.State(startupCaller(caller), true).State != "ready" {
			return nil, security.NewHTTPError(http.StatusConflict, "GLOBAL_DATA_NOT_READY")
		}
		if err := ctx.Err(); err != nil {
			return nil, security.NewHTTPError(http.StatusConflict, "MODEL_CONFIG_CANCELLED")
		}
		if err := models.SetDefaultEmbeddingModel(args[0]); err != nil {
			return modelRoutingRefusal(modelRoutingFailureCode(err)), nil
		}
		return map[string]any{"success": true}, nil
	}
	return handlers, nil
}

// modelRoutingRefusal is the original's `{ success: false, error: String(error) }`
// business result: HTTP 200 with the thrown symbolic error, which is exactly
// what String(new Error('CODE')) produces.
func modelRoutingRefusal(code string) map[string]any {
	return map[string]any{"success": false, "error": "Error: " + code}
}

// modelRoutingFailureCode maps a private store refusal onto the original's
// symbolic throw. Filesystem failures have no symbolic equivalent in the
// original (they surface as raw fs errors), so they report the write failure
// this lane already uses for llm:save-model.
func modelRoutingFailureCode(err error) string {
	for _, known := range []struct {
		err  error
		code string
	}{
		{globalconfig.ErrStageModelRequestInvalid, "LLM_STAGE_MODEL_REQUEST_INVALID"},
		{globalconfig.ErrStageModelUnavailable, "LLM_STAGE_MODEL_UNAVAILABLE"},
		{globalconfig.ErrModelCorrupt, "LLM_MODEL_CONFIG_CORRUPT"},
		{globalconfig.ErrConfigCorrupt, "LLM_GLOBAL_CONFIG_CORRUPT"},
		{globalconfig.ErrUnsafePath, "GLOBAL_CONFIG_UNSAFE_PATH"},
		{globalconfig.ErrUnsafeFile, "GLOBAL_CONFIG_UNSAFE_FILE"},
		{globalconfig.ErrTooLarge, "GLOBAL_CONFIG_TOO_LARGE"},
	} {
		if errors.Is(err, known.err) {
			return known.code
		}
	}
	return "GLOBAL_CONFIG_WRITE_FAILED"
}
