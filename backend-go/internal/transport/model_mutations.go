package transport

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// BindModelSave accepts browser model profiles only after the trusted global
// migration capture and an administrator supplied egress policy are present.
// The endpoint is checked again at provider dispatch through egress.Client.Do.
// Deletion needs a separate two-file reference transaction and is not bound.
func (s *Server) BindModelSave(models *globalconfig.Service, startup *globalconfig.Startup, policy *egress.Client) error {
	if models == nil || startup == nil || policy == nil {
		return errors.New("MODEL_SAVE_AUTHORITY_REQUIRED")
	}
	return s.Bind("llm:save-model", func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 1 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "MODEL_INVALID")
		}
		candidate, ok := args[0].(map[string]any)
		if !ok {
			return nil, security.NewHTTPError(http.StatusBadRequest, "MODEL_INVALID")
		}
		base, ok := candidate["baseUrl"].(string)
		if !ok {
			return nil, security.NewHTTPError(http.StatusBadRequest, "MODEL_ENDPOINT_INVALID")
		}
		switch err := policy.CheckBaseURL(base); {
		case errors.Is(err, egress.ErrBaseURLInvalid):
			return nil, security.NewHTTPError(http.StatusBadRequest, "MODEL_ENDPOINT_INVALID")
		case errors.Is(err, egress.ErrNotAllowed):
			return nil, security.NewHTTPError(http.StatusForbidden, "ENDPOINT_NOT_ALLOWED")
		case err != nil:
			return nil, security.NewHTTPError(http.StatusInternalServerError, "MODEL_ENDPOINT_POLICY_UNAVAILABLE")
		}
		key, ok := candidate["apiKey"].(string)
		if !ok {
			return nil, security.NewHTTPError(http.StatusBadRequest, "MODEL_INVALID")
		}
		// Register even a rejected submitted key before any diagnostic can be
		// emitted through the shared public response boundary.
		if _, err := s.public.RegisterSecret(key); err != nil {
			return nil, security.NewHTTPError(http.StatusBadRequest, "MODEL_CREDENTIAL_INVALID")
		}
		if startup.State(startupCaller(caller), true).State != "ready" {
			return nil, security.NewHTTPError(http.StatusConflict, "GLOBAL_DATA_NOT_READY")
		}
		if err := ctx.Err(); err != nil {
			return nil, security.NewHTTPError(http.StatusConflict, "MODEL_CONFIG_CANCELLED")
		}
		if mapping, present := candidate["reasoningMapping"]; present && !validModelReasoningMapping(mapping, candidate["protocol"]) {
			return map[string]any{"success": false, "error": "Error: INVALID_REASONING_MAPPING"}, nil
		}
		if err := models.SaveModel(globalconfig.ModelRecord(candidate)); err != nil {
			if errors.Is(err, globalconfig.ErrCredentialEndpointChanged) {
				return nil, security.NewHTTPError(http.StatusBadRequest, "CREDENTIAL_ENDPOINT_CHANGED")
			}
			if errors.Is(err, globalconfig.ErrModelInvalid) {
				return map[string]any{"success": false, "error": "Error: LLM_MODEL_INVALID"}, nil
			}
			return map[string]any{"success": false, "error": "Error: LLM_MODEL_CONFIG_WRITE_FAILED"}, nil
		}
		return map[string]any{"success": true}, nil
	})
}

var reasoningEfforts = map[string]bool{"off": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true}
var reasoningWire = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

// Mirrors the finite isReasoningMapping source contract. JSONBody uses
// json.Number, so number validity is checked through the existing safe-number
// helper rather than accepting an imprecise float conversion.
func validModelReasoningMapping(value any, protocol any) bool {
	protocolName, protocolOK := protocol.(string)
	if !protocolOK || protocolName != "openai" && protocolName != "gemini" {
		return false
	}
	mapping, ok := value.(map[string]any)
	if !ok || len(mapping) < 3 || len(mapping) > 4 {
		return false
	}
	for key := range mapping {
		if key != "adapter" && key != "supportedEfforts" && key != "providerValues" && key != "requestAliases" {
			return false
		}
	}
	adapter, ok := mapping["adapter"].(string)
	if !ok {
		return false
	}
	isGemini := adapter == "gemini-thinking-budget" || adapter == "gemini-thinking-level"
	if adapter != "openai-reasoning-effort" && adapter != "openai-thinking-budget" && adapter != "deepseek-v4-thinking" && adapter != "siliconflow-v4-thinking" && !isGemini {
		return false
	}
	if (protocolName == "gemini") != isGemini {
		return false
	}
	efforts, ok := mapping["supportedEfforts"].([]any)
	if !ok || len(efforts) == 0 || len(efforts) > len(reasoningEfforts) {
		return false
	}
	values, ok := mapping["providerValues"].(map[string]any)
	if !ok {
		return false
	}
	supported := make(map[string]bool, len(efforts))
	for _, value := range efforts {
		effort, ok := value.(string)
		if !ok || !reasoningEfforts[effort] || supported[effort] {
			return false
		}
		supported[effort] = true
	}
	for key := range values {
		if !supported[key] {
			return false
		}
	}
	for effort := range supported {
		wire, present := values[effort]
		if !present {
			return false
		}
		switch adapter {
		case "openai-reasoning-effort":
			text, ok := wire.(string)
			if !ok || !reasoningWire.MatchString(text) || effort == "off" && text != "off" && text != "none" && text != "disabled" {
				return false
			}
		case "openai-thinking-budget", "gemini-thinking-budget":
			budget, ok := safeModelNumber(wire)
			if !ok || budget < 0 || effort == "off" && budget != 0 || adapter == "openai-thinking-budget" && effort != "off" && budget == 0 {
				return false
			}
		case "gemini-thinking-level":
			text, ok := wire.(string)
			if !ok || (text != "minimal" && text != "low" && text != "medium" && text != "high") || (effort == "off") != (text == "minimal") {
				return false
			}
		case "deepseek-v4-thinking":
			text, ok := wire.(string)
			if !ok || effort == "off" && text != "disabled" || effort != "off" && text != "enabled" && text != "low" && text != "high" && text != "max" {
				return false
			}
		case "siliconflow-v4-thinking":
			text, ok := wire.(string)
			if !ok || (effort != "high" && effort != "max") || text != effort {
				return false
			}
		}
	}
	if raw, present := mapping["requestAliases"]; present {
		aliases, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		for requested, effectiveRaw := range aliases {
			effective, ok := effectiveRaw.(string)
			if !ok || !reasoningEfforts[requested] || !supported[effective] || requested == "off" && effective != "off" {
				return false
			}
		}
	}
	return true
}

func safeModelNumber(value any) (int64, bool) {
	// Decode side accepts exact decimal JSON integers only; values beyond the
	// ECMAScript safe-integer boundary fail closed.
	text := ""
	switch value := value.(type) {
	case interface{ String() string }:
		text = value.String()
	case float64:
		if value < 0 || value > 9007199254740991 || value != float64(int64(value)) {
			return 0, false
		}
		return int64(value), true
	default:
		return 0, false
	}
	if text == "" || strings.ContainsAny(text, ".eE+") || len(text) > 16 {
		return 0, false
	}
	var result int64
	for _, digit := range text {
		if digit < '0' || digit > '9' {
			return 0, false
		}
		result = result*10 + int64(digit-'0')
		if result > 9007199254740991 {
			return 0, false
		}
	}
	return result, true
}
