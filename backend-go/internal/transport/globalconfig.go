package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// BindGlobalConfig captures a trusted, already anchored app-data service and a
// startup snapshot admitted by the backend migration owner. Neither is obtained
// from an operation argument or inferred from directory existence. This binding
// is intentionally not installed by scratch-only main.
func (s *Server) BindGlobalConfig(config *globalconfig.Service, startup *globalconfig.Startup) error {
	if config == nil || startup == nil {
		return errors.New("GLOBAL_CONFIG_PROVIDER_REQUIRED")
	}
	return s.bindOperations(globalConfigHandlers(config, startup, true))
}

// BindGlobalConfigReads publishes only the read and acknowledgement half of the
// same family. It exists for a lane admitted read-only over a real author data
// directory: config:set writes the author's committed global generation, so it
// keeps failing closed at GO_OPERATION_NOT_MIGRATED until a lane owns that
// write. Publication stays atomic, and the shared handler bodies below keep the
// two surfaces from drifting apart.
func (s *Server) BindGlobalConfigReads(config *globalconfig.Service, startup *globalconfig.Startup) error {
	if config == nil || startup == nil {
		return errors.New("GLOBAL_CONFIG_PROVIDER_REQUIRED")
	}
	return s.bindOperations(globalConfigHandlers(config, startup, false))
}

func globalConfigHandlers(config *globalconfig.Service, startup *globalconfig.Startup, writable bool) map[string]Operation {
	handlers := map[string]Operation{}
	handlers["config:get"] = func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 0 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		if startup.State(startupCaller(caller), true).State != "ready" {
			return nil, security.NewHTTPError(http.StatusConflict, "GLOBAL_DATA_NOT_READY")
		}
		if err := ctx.Err(); err != nil {
			return nil, errors.New("GLOBAL_CONFIG_CANCELLED")
		}
		value, err := config.Get()
		if err != nil {
			return nil, globalConfigPublicError(err)
		}
		return publicConfig(value), nil
	}
	if writable {
		handlers["config:set"] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			if len(args) != 1 {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			input, ok := args[0].(map[string]any)
			if !ok {
				return nil, security.NewHTTPError(http.StatusBadRequest, "CONFIG_INVALID")
			}
			for key := range input {
				if (!configPublicFields[key] || key == "stageModelIds") && key != "proxy" {
					return nil, security.NewHTTPError(http.StatusBadRequest, "CONFIG_FIELD_DENIED")
				}
			}
			// Presence, not truthiness: null/false/0/empty string cannot clear deployment
			// proxy settings. Browser authors have no proxy administration capability.
			if _, present := input["proxy"]; present {
				return nil, security.NewHTTPError(http.StatusForbidden, "PROXY_SERVER_ADMIN_ONLY")
			}
			if startup.State(startupCaller(caller), true).State != "ready" {
				return nil, security.NewHTTPError(http.StatusConflict, "GLOBAL_DATA_NOT_READY")
			}
			if err := ctx.Err(); err != nil {
				return nil, errors.New("GLOBAL_CONFIG_CANCELLED")
			}
			result := config.Set(input)
			if !result.Success {
				return map[string]any{"success": false, "error": "GLOBAL_CONFIG_WRITE_FAILED"}, nil
			}
			return map[string]any{"success": true}, nil
		}
	}
	handlers["startup:get-state"] = func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 0 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		return startup.State(startupCaller(caller), true), nil
	}
	handlers["startup:appearance-ack"] = func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 1 {
			return false, nil
		}
		return startup.AppearanceAck(startupCaller(caller), true, args[0]), nil
	}
	return handlers
}

var configPublicFields = map[string]bool{
	"theme": true, "locale": true, "autoOpenNextChapterAfterFinalize": true,
	"editorFontSize": true, "editorFontFamily": true, "autoSaveInterval": true,
	"defaultModelId": true, "defaultEmbeddingModelId": true, "outlineTargetUnits": true,
	"blueprintTargetUnits": true, "stageModelIds": true,
}

// Deliberately project UI preferences only. The private JSON may contain saved
// models, arbitrary extension fields, host paths, proxy username/password, etc.
// Filtering those only by credential field names would not be sufficient.
func publicConfig(value any) map[string]any {
	result := map[string]any{}
	object, ok := value.(map[string]any)
	if !ok {
		return result
	}
	for key := range configPublicFields {
		item, present := object[key]
		if !present {
			continue
		}
		if key == "stageModelIds" {
			mapping, ok := item.(map[string]any)
			if !ok {
				continue
			}
			clean := map[string]any{}
			for stage, id := range mapping {
				if text, ok := id.(string); ok && len(stage) <= 128 && len(text) <= 256 {
					clean[stage] = text
				}
			}
			result[key] = clean
			continue
		}
		// Never return arbitrary nested extension objects under an allowed key.
		if item == nil {
			result[key] = nil
			continue
		}
		if number, ok := item.(json.Number); ok {
			result[key] = number
			continue
		}
		kind := reflect.TypeOf(item).Kind()
		if kind == reflect.String || kind == reflect.Bool || kind == reflect.Float64 || kind == reflect.Float32 || kind == reflect.Int || kind == reflect.Int64 {
			result[key] = item
		}
	}
	return result
}
func startupCaller(caller Caller) string {
	// Length-delimited components avoid delimiter collisions while preserving tab
	// isolation even for the same principal with two authenticated cookies.
	if caller.Principal == "" || caller.SessionID == "" || caller.ClientID == "" {
		return ""
	}
	return caller.SessionID + ":" + caller.ClientID
}
func globalConfigPublicError(err error) error {
	for _, known := range []error{globalconfig.ErrUnsafePath, globalconfig.ErrUnsafeFile, globalconfig.ErrTooLarge, globalconfig.ErrIO, globalconfig.ErrClosed, globalconfig.ErrInvalidPatch} {
		if errors.Is(err, known) {
			return known
		}
	}
	return errors.New("GLOBAL_CONFIG_READ_FAILED")
}

// Register a family atomically; a collision must never publish a partial family.
func (s *Server) bindOperations(handlers map[string]Operation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, handler := range handlers {
		if _, ok := s.registry.Lookup(name); !ok {
			return errors.New("OPERATION_NOT_EXPOSED")
		}
		if handler == nil {
			return errors.New("OPERATION_HANDLER_REQUIRED")
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
