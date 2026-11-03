package transport

import (
	"context"
	"errors"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// BindModelDelete exposes the original model deletion result only after the
// trusted global store and its recoverable two-file transaction are available.
// The store owns reference clearing, journal recovery, and credential-bearing
// files. The browser receives only the resulting model IDs and a safe error.
func (s *Server) BindModelDelete(models *globalconfig.Service, startup *globalconfig.Startup) error {
	if models == nil || startup == nil {
		return errors.New("MODEL_DELETE_AUTHORITY_REQUIRED")
	}
	return s.Bind("llm:delete-model", func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 1 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		id, ok := args[0].(string)
		if !ok || id == "" {
			return map[string]any{"success": false, "error": "Error: LLM_MODEL_INVALID"}, nil
		}
		if startup.State(startupCaller(caller), true).State != "ready" {
			return nil, security.NewHTTPError(http.StatusConflict, "GLOBAL_DATA_NOT_READY")
		}
		if err := ctx.Err(); err != nil {
			return nil, security.NewHTTPError(http.StatusConflict, "MODEL_CONFIG_CANCELLED")
		}
		deleted, err := models.DeleteModelAndReferences(id)
		if err != nil {
			return map[string]any{"success": false, "error": modelDeletePublicError(err)}, nil
		}
		return map[string]any{
			"success":                 true,
			"defaultModelId":          deleted.DefaultModelID,
			"defaultEmbeddingModelId": deleted.DefaultEmbeddingModelID,
			"stageModelIds":           deleted.StageModelIDs,
		}, nil
	})
}

func modelDeletePublicError(err error) string {
	switch {
	case errors.Is(err, globalconfig.ErrModelInvalid):
		return "Error: LLM_MODEL_INVALID"
	case errors.Is(err, globalconfig.ErrModelCorrupt):
		return "Error: LLM_MODEL_CONFIG_CORRUPT"
	case errors.Is(err, globalconfig.ErrModelDeleteConfigCorrupt):
		return "Error: LLM_GLOBAL_CONFIG_CORRUPT"
	case errors.Is(err, globalconfig.ErrModelDeleteJournalInvalid):
		return "Error: LLM_MODEL_DELETE_JOURNAL_INVALID"
	case errors.Is(err, globalconfig.ErrModelDeleteJournalConflict):
		return "Error: LLM_MODEL_DELETE_JOURNAL_CONFLICT"
	default:
		return "Error: LLM_MODEL_DELETE_FAILED"
	}
}
