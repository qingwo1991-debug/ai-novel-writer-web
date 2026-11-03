package transport

import (
	"context"
	"errors"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// BindModelReads exposes the original llm:list-models browser projection from
// an admitted private global model store. The store is never returned to the
// caller: credentials are registered with the gateway-wide sanitizer before
// any model metadata can be emitted. Mutating/model-execution operations need
// endpoint policy and provider validation and are deliberately separate.
func (s *Server) BindModelReads(models *globalconfig.Service, startup *globalconfig.Startup) error {
	if models == nil || startup == nil {
		return errors.New("MODEL_CONFIG_PROVIDER_REQUIRED")
	}
	return s.Bind("llm:list-models", func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 0 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		if startup.State(startupCaller(caller), true).State != "ready" {
			return nil, security.NewHTTPError(http.StatusConflict, "GLOBAL_DATA_NOT_READY")
		}
		if err := ctx.Err(); err != nil {
			return nil, security.NewHTTPError(http.StatusConflict, "MODEL_CONFIG_CANCELLED")
		}
		private, err := models.ListPrivateModels()
		if err != nil {
			return nil, security.NewHTTPError(http.StatusInternalServerError, "MODEL_CONFIG_UNAVAILABLE")
		}
		for _, model := range private {
			key, ok := model["apiKey"].(string)
			if !ok {
				return nil, security.NewHTTPError(http.StatusInternalServerError, "MODEL_CONFIG_UNAVAILABLE")
			}
			if _, err := s.public.RegisterSecret(key); err != nil {
				return nil, security.NewHTTPError(http.StatusInternalServerError, "PUBLIC_RESPONSE_SECRET_LIMIT")
			}
		}
		public := make([]any, 0, len(private))
		for _, model := range private {
			key := model["apiKey"].(string)
			value, ok := s.public.Sanitize(map[string]any(model)).(map[string]any)
			if !ok {
				return nil, security.NewHTTPError(http.StatusInternalServerError, "MODEL_CONFIG_UNAVAILABLE")
			}
			value["apiKey"] = ""
			value["hasCredential"] = key != ""
			public = append(public, value)
		}
		return public, nil
	})
}
