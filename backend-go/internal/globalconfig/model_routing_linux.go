//go:build linux

package globalconfig

import (
	"bytes"
	"encoding/json"
	"errors"
)

// The three model-routing preference names (llm:get/set-stage-model,
// llm:get/set-default-model, llm:get/set-default-embedding-model) are reads and
// single-document writes of config.json. They never dispatch to a provider, so
// they need neither an egress policy nor an execution lease. Their validation
// order is the original's: llm:set-stage-model resolves the model against
// models.json BEFORE it touches config.json, and a damaged document on either
// side is refused rather than silently replaced.

// ErrConfigCorrupt mirrors the original loadGlobalConfigForUpdate refusal: a
// routing write never overwrites a config document it cannot parse.
var ErrConfigCorrupt = errors.New("LLM_GLOBAL_CONFIG_CORRUPT")

// ErrStageModelRequestInvalid mirrors the original's explicit
// LLM_STAGE_MODEL_REQUEST_INVALID throw for a stage this vocabulary does not
// name. Transport normally refuses that shape before the call; this keeps the
// service honest on its own.
var ErrStageModelRequestInvalid = errors.New("LLM_STAGE_MODEL_REQUEST_INVALID")

// ErrStageModelUnavailable mirrors LLM_STAGE_MODEL_UNAVAILABLE: the request
// named an id that is not a saved generation profile.
var ErrStageModelUnavailable = errors.New("LLM_STAGE_MODEL_UNAVAILABLE")

// modelStages is the original MODEL_STAGES vocabulary
// (src/shared/model-stage-routing.ts: the four writing-skill stages plus
// finalization). Only these keys are ever read from or written into
// stageModelIds; assignStageModel and parseStageModelIds both funnel through it.
var modelStages = []string{"planning", "drafting", "review", "refinement", "finalization"}

// IsModelStage reports whether the name is one of the original MODEL_STAGES.
// The transport uses it for the request-shape refusal.
func IsModelStage(stage string) bool {
	for _, known := range modelStages {
		if known == stage {
			return true
		}
	}
	return false
}

// configDocumentRead is the tolerant original readJsonFile projection: a
// missing document is the defaults, and an unparseable one is projected the
// same way instead of being reported or replaced.
func configDocumentRead(fd int) (map[string]any, error) {
	data, missing, err := readConfig(fd)
	if err != nil {
		return nil, err
	}
	if missing {
		return DefaultConfig(), nil
	}
	value, decodeErr := decode(data)
	if decodeErr != nil {
		return DefaultConfig(), nil
	}
	if document, ok := value.(map[string]any); ok {
		return document, nil
	}
	return DefaultConfig(), nil
}

// configDocumentWrite is the strict original loadGlobalConfigForUpdate: an
// unparseable document is a refusal, never a defaults rewrite. A JSON null
// lands here too, so a routing write can never assign into a nil document.
func configDocumentWrite(fd int) (map[string]any, error) {
	data, missing, err := readConfig(fd)
	if err != nil {
		return nil, err
	}
	if missing {
		return DefaultConfig(), nil
	}
	value, decodeErr := decode(data)
	if decodeErr != nil {
		return nil, ErrConfigCorrupt
	}
	document, ok := value.(map[string]any)
	if !ok {
		return nil, ErrConfigCorrupt
	}
	return document, nil
}

// writeConfigDocument commits a routing document through the same atomic
// temp-file replacement the other global-config writes use.
func writeConfigDocument(fd int, document map[string]any) error {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return ErrInvalidPatch
	}
	data := bytes.TrimSuffix(output.Bytes(), []byte("\n"))
	if len(data) > MaxConfigBytes {
		return ErrTooLarge
	}
	return atomicWrite(fd, data)
}

// routingValue returns one raw top-level value of config.json, nil when the
// document does not carry it. It is the shared body of the two default reads.
func (s *Service) routingValue(key string) (any, error) {
	var value any
	err := s.locked(func(fd int) error {
		document, err := configDocumentRead(fd)
		if err != nil {
			return err
		}
		value = document[key]
		return nil
	})
	if err != nil {
		return nil, err
	}
	return value, nil
}

// setRoutingValue assigns one raw top-level value in one locked
// read-modify-write. The original performs no validation on a default id: it
// stores whatever the caller supplied, including null.
func (s *Service) setRoutingValue(key string, value any) error {
	return s.locked(func(fd int) error {
		document, err := configDocumentWrite(fd)
		if err != nil {
			return err
		}
		document[key] = value
		return writeConfigDocument(fd, document)
	})
}

// StageModels returns the parsed stage assignments llm:get-stage-models
// projects, with the original readJsonFile fallback: a missing or damaged
// document yields no assignments and is left untouched.
func (s *Service) StageModels() (map[string]string, error) {
	result := map[string]string{}
	err := s.locked(func(fd int) error {
		document, err := configDocumentRead(fd)
		if err != nil {
			return err
		}
		result = stageAssignments(document["stageModelIds"])
		return nil
	})
	if err != nil {
		return map[string]string{}, err
	}
	return result, nil
}

// DefaultModel returns the raw defaultModelId of config.json (nil when absent).
func (s *Service) DefaultModel() (any, error) {
	return s.routingValue("defaultModelId")
}

// SetDefaultModel stores the raw defaultModelId of config.json.
func (s *Service) SetDefaultModel(value any) error {
	return s.setRoutingValue("defaultModelId", value)
}

// DefaultEmbeddingModel returns the raw defaultEmbeddingModelId (nil when absent).
func (s *Service) DefaultEmbeddingModel() (any, error) {
	return s.routingValue("defaultEmbeddingModelId")
}

// SetDefaultEmbeddingModel stores the raw defaultEmbeddingModelId.
func (s *Service) SetDefaultEmbeddingModel(value any) error {
	return s.setRoutingValue("defaultEmbeddingModelId", value)
}

// AssignStageModel sets or clears one stage assignment in a single locked
// read-modify-write and returns the committed assignments. A nil modelID clears
// the stage, exactly like the original's `modelId === null`; a non-nil one must
// name a saved generation profile, and that lookup happens first so a damaged
// models.json refuses the write instead of clearing or reassigning the stage.
func (s *Service) AssignStageModel(stage string, modelID *string) (map[string]string, error) {
	if !IsModelStage(stage) {
		return nil, ErrStageModelRequestInvalid
	}
	assigned := modelID
	var committed map[string]string
	err := s.locked(func(fd int) error {
		if assigned != nil {
			models, err := readModels(fd)
			if err != nil {
				return err
			}
			resolved, ok := availableGenerationModelID(models, *assigned)
			if !ok {
				return ErrStageModelUnavailable
			}
			assigned = &resolved
		}
		document, err := configDocumentWrite(fd)
		if err != nil {
			return err
		}
		next := stageAssignments(document["stageModelIds"])
		if assigned == nil {
			delete(next, stage)
		} else {
			next[stage] = *assigned
		}
		document["stageModelIds"] = next
		if err := writeConfigDocument(fd, document); err != nil {
			return err
		}
		committed = next
		return nil
	})
	if err != nil {
		return nil, err
	}
	return committed, nil
}

// availableGenerationModelID mirrors availableGenerationModelId: the id is
// trimmed, then it must match a saved profile that carries the generation
// purpose. A non-generation or unknown id is not routable to any stage.
func availableGenerationModelID(models []ModelRecord, candidate string) (string, bool) {
	id := trimModelStage(candidate)
	if id == "" {
		return "", false
	}
	for _, model := range models {
		if model["id"] != id {
			continue
		}
		purposes, ok := model["purposes"].([]any)
		if !ok {
			continue
		}
		for _, purpose := range purposes {
			if text, ok := purpose.(string); ok && text == "generation" {
				return id, true
			}
		}
	}
	return "", false
}
