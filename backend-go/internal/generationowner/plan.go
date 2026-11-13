package generationowner

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"regexp"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/llm"
)

const estimatorVersion = "utf8-bytes-plus-chat-framing-v1"
const safetyMargin int64 = 512

type Task struct {
	Purpose        string          `json:"purpose"`
	Output         string          `json:"output"`
	Messages       []llm.Message   `json:"messages"`
	ReasoningStage string          `json:"reasoningStage,omitempty"`
	PromptBudget   json.RawMessage `json:"promptBudget,omitempty"`
	BudgetDemand   *Demand         `json:"budgetDemand,omitempty"`
}
type Demand struct {
	Kind        string `json:"kind"`
	Language    string `json:"writingLanguage"`
	Units       int64  `json:"requestedUnits,omitempty"`
	Segmentable *bool  `json:"segmentable,omitempty"`
	Items       int64  `json:"requestedItems,omitempty"`
}

var purposePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9:_-]{0,127}$`)

// ParseTask refuses unknown fields recursively, including provider URL/key,
// sampling overrides, reasoning directives, tool protocols and browser budgets.
func ParseTask(raw []byte) (Task, error) {
	var task Task
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&task) != nil {
		return Task{}, failure("GENERATION_SEMANTIC_TASK_INVALID")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return Task{}, failure("GENERATION_SEMANTIC_TASK_INVALID")
	}
	var shape map[string]any
	if json.Unmarshal(raw, &shape) != nil {
		return Task{}, failure("GENERATION_SEMANTIC_TASK_INVALID")
	}
	if messages, ok := shape["messages"].([]any); ok {
		for _, value := range messages {
			message, ok := value.(map[string]any)
			if !ok {
				return Task{}, failure("GENERATION_SEMANTIC_TASK_INVALID")
			}
			if _, ok := message["content"].(string); !ok {
				return Task{}, failure("GENERATION_SEMANTIC_TASK_INVALID")
			}
		}
	}
	if err := AssertTask(task); err != nil {
		return Task{}, err
	}
	return task, nil
}
func AssertTask(t Task) error {
	if !purposePattern.MatchString(t.Purpose) || (t.Output != "visible-text" && t.Output != "structured-data") || len(t.Messages) == 0 || len(t.Messages) > 1024 {
		return failure("GENERATION_SEMANTIC_TASK_INVALID")
	}
	if t.ReasoningStage != "" && t.ReasoningStage != "drafting" && t.ReasoningStage != "planning" && t.ReasoningStage != "review" && t.ReasoningStage != "general" {
		return failure("GENERATION_SEMANTIC_TASK_INVALID")
	}
	for _, m := range t.Messages {
		if m.Role != "system" && m.Role != "user" && m.Role != "assistant" || !utf8.ValidString(m.Content) {
			return failure("GENERATION_SEMANTIC_TASK_INVALID")
		}
	}
	if d := t.BudgetDemand; d != nil {
		if d.Language != "zh-CN" && d.Language != "en-US" {
			return failure("GENERATION_SEMANTIC_TASK_INVALID")
		}
		if d.Kind == "draft-units" {
			if !positive(d.Units) || d.Segmentable == nil || d.Items != 0 {
				return failure("GENERATION_SEMANTIC_TASK_INVALID")
			}
		} else if d.Kind == "structured-items" {
			if !positive(d.Items) || d.Units != 0 || d.Segmentable != nil {
				return failure("GENERATION_SEMANTIC_TASK_INVALID")
			}
		} else {
			return failure("GENERATION_SEMANTIC_TASK_INVALID")
		}
	}
	return nil
}

type UsagePolicy struct {
	Estimator string `json:"estimatorVersion"`
	Safety    int64  `json:"safetyMarginTokens"`
	Reasoning string `json:"reasoning"`
	CanBound  bool   `json:"canBoundTotalLiability"`
}
type Plan struct {
	BudgetDecision map[string]any `json:"budgetDecision,omitempty"`
	Options        llm.Options    `json:"options"`
	UsagePolicy    UsagePolicy    `json:"usagePolicy"`
	TrustedUsage   bool           `json:"trustedUsage"`
	Reserved       int64          `json:"reservedTokens"`
	Input          int64          `json:"inputUpperBoundTokens"`
	Reasoning      int64          `json:"reasoningUpperBoundTokens"`
	Output         int64          `json:"requestedOutputTokens"`
}
type PreflightError struct{ Decision map[string]any }

func (e *PreflightError) Error() string { return "TASK_BUDGET_SCOPE_SPLIT_REQUIRED" }

// BuildPlan ports the physical planning and S07 semantic sizing for the finite
// admitted model subset. No proof of feature support comes from stored flags.
// It is a pure planning seam, not reservation or dispatch authorization.
func BuildPlan(model Profile, receipt Receipt, task Task, rootOutput int64) (Plan, error) {
	if err := AssertTask(task); err != nil {
		return Plan{}, err
	}
	if !positive(rootOutput) {
		return Plan{}, failure("GENERATION_PLANNING_BUDGET_INVALID")
	}
	contextTokens, modelOutput, err := admittedCapacity(model)
	if err != nil {
		return Plan{}, err
	}
	if math.IsNaN(model.Temperature) || math.IsInf(model.Temperature, 0) {
		return Plan{}, failure("MODEL_PROFILE_INVALID")
	}
	hints := hints(model)
	userOutput := int64(0)
	if positive(model.MaxTokens) {
		userOutput = model.MaxTokens
	}
	if hints.Output > 0 {
		if userOutput > 0 {
			userOutput = min(userOutput, hints.Output)
		} else {
			userOutput = hints.Output
		}
	}
	if userOutput == 0 {
		return Plan{}, failure("MODEL_OUTPUT_LIMIT_INVALID")
	}
	output := min(rootOutput, userOutput)
	if modelOutput > 0 {
		output = min(output, modelOutput)
	}
	input := int64(32)
	for _, m := range task.Messages {
		input += int64(len(m.Content) + len(m.Role) + 32)
		if input > safeInteger-output-safetyMargin {
			return Plan{}, failure("GENERATION_PLANNING_BUDGET_INVALID")
		}
	}
	endpoint, _ := normalizedEndpoint(model.BaseURL)
	openai := model.Protocol == "openai" && (endpoint == "https://api.openai.com" || endpoint == "https://api.openai.com/v1")
	policy := UsagePolicy{estimatorVersion, safetyMargin, "unknown", false}
	if openai && contextTokens > 0 && modelOutput > 0 {
		policy.Reasoning = "included-in-completion"
		policy.CanBound = true
	}
	options := llm.Options{Temperature: &model.Temperature, MaxTokens: &output}
	if openai {
		options.OutputTokenParameter = "max_completion_tokens"
	}
	if task.Output == "structured-data" && receipt.CapabilityEvidence.StructuredOutput != nil && *receipt.CapabilityEvidence.StructuredOutput {
		options.ResponseFormat = map[string]any{"type": "json_object"}
	}
	plan := Plan{Options: options, UsagePolicy: policy, TrustedUsage: true, Reserved: input + output + safetyMargin, Input: input, Output: output}
	if task.BudgetDemand == nil {
		return plan, nil
	}
	d := task.BudgetDemand
	stage := task.ReasoningStage
	if stage == "" {
		if task.Output == "visible-text" {
			stage = "drafting"
		} else {
			stage = "planning"
		}
	}
	requested := d.Units
	requestedOutput := int64(0)
	selected := requested
	if d.Kind == "draft-units" {
		numerator, denominator := int64(12), int64(5)
		if d.Language == "en-US" {
			numerator, denominator = 3, 2
		}
		if requested > (safeInteger-512)*denominator/numerator {
			return Plan{}, failure("TASK_BUDGET_INPUT_INVALID")
		}
		requestedOutput = 512 + (requested*numerator+denominator-1)/denominator
	} else {
		requested = d.Items
		perItem := int64(512)
		if d.Language == "en-US" {
			perItem = 384
		}
		if stage == "drafting" || stage == "planning" {
			perItem = 3072
			if d.Language == "en-US" {
				perItem = 2048
			}
		}
		if requested > (safeInteger-512)/perItem {
			return Plan{}, failure("TASK_BUDGET_INPUT_INVALID")
		}
		requestedOutput = 512 + requested*perItem
		available := (output - 512) / perItem
		if available < 1 {
			available = 1
		}
		selected = min(requested, available)
	}
	reasons := []map[string]any{{"code": "task-demand", "valueTokens": requestedOutput, "selected": true}, {"code": "input-upper-bound", "valueTokens": input, "selected": true}, {"code": "safety-margin", "valueTokens": safetyMargin, "selected": true}}
	if contextTokens == 0 || modelOutput == 0 {
		reasons = append(reasons, map[string]any{"code": "model-capability-unknown", "selected": true})
	}
	if !policy.CanBound {
		reasons = append(reasons, map[string]any{"code": "liability-bound-unknown", "selected": true})
	}
	if modelOutput > 0 {
		reasons = append(reasons, map[string]any{"code": "model-output-cap", "valueTokens": modelOutput, "selected": modelOutput == output})
	}
	reasons = append(reasons, map[string]any{"code": "root-output-cap", "valueTokens": rootOutput, "selected": rootOutput == output}, map[string]any{"code": "user-output-cap", "valueTokens": userOutput, "selected": userOutput == output})
	decision := "ready"
	reservedOutput := output
	if selected < requested {
		decision = "split-required"
		perItem := int64(512)
		if d.Language == "en-US" {
			perItem = 384
		}
		if stage == "drafting" || stage == "planning" {
			perItem = 3072
			if d.Language == "en-US" {
				perItem = 2048
			}
		}
		reservedOutput = min(output, 512+selected*perItem)
		reasons = append(reasons, map[string]any{"code": "scope-split", "valueTokens": reservedOutput, "selected": true})
	} else {
		reasons = append(reasons, map[string]any{"code": "output-allocation-ceiling", "valueTokens": reservedOutput, "selected": true})
	}
	budget := map[string]any{"policyVersion": "s07-task-budget-v1", "outputEstimatorVersion": "semantic-units-sizing-v1", "inputEstimatorVersion": estimatorVersion, "decision": decision, "stage": stage, "writingLanguage": d.Language, "demandKind": d.Kind, "requestedQuantity": requested, "selectedQuantity": selected, "remainingQuantity": requested - selected, "requestedOutputTokens": requestedOutput, "reservedOutputTokens": reservedOutput, "reasoningUpperBoundTokens": int64(0), "reservationLiabilityTokens": input + reservedOutput + safetyMargin, "inputUpperBoundTokens": input, "reasons": reasons}
	if decision != "ready" {
		return Plan{}, &PreflightError{budget}
	}
	plan.BudgetDecision = budget
	return plan, nil
}

// Profiles with a manual reasoning mapping, official unported catalog families,
// tools, planning owner intent, root scaling and composition remain unsupported.
// Do not remove the finite admission check to make a capability request succeed.
