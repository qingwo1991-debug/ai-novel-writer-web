package main

import (
	"encoding/json"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/graphgen"
)

// syntheticGraphOperations maps a graph context kind onto the operation the
// frozen manifest must record. The three kinds are the only ones the browser
// can request (src/shared/graph-generation.ts).
var syntheticGraphOperations = map[string]string{
	"plot":  "plot-tree-snapshot",
	"plan":  "narrative-thread-plan-candidate",
	"event": "narrative-thread-event-candidate",
}

// syntheticGraphContext verifies the context a graph run was frozen with and
// rebuilds the task it was created for. Both `begin` and `read` need exactly
// this, so it is one function rather than two copies of the same twelve checks.
//
// It is deliberately strict about hashes: the context hash and the task hash are
// recomputed here and compared with the stored ones, and the stored task is also
// compared against a freshly built one. Any mismatch means the run was frozen
// from state that no longer reconstructs, and a projection built on top of it
// would describe a task the provider never received.
func syntheticGraphContext(manifest map[string]any, projectID string) (map[string]any, graphgen.Task, error) {
	invalid := errors.New("GENERATION_GRAPH_CONTEXT_INVALID")
	contextValue := syntheticMap(manifest["graphGenerationContext"])
	if contextValue == nil {
		return nil, graphgen.Task{}, invalid
	}
	kind := syntheticString(contextValue["kind"])
	operation, known := syntheticGraphOperations[kind]
	if !known || syntheticString(manifest["operation"]) != operation {
		return nil, graphgen.Task{}, invalid
	}
	input := syntheticMap(contextValue["input"])
	if input == nil || syntheticString(input["kind"]) != kind {
		return nil, graphgen.Task{}, invalid
	}
	hash, err := syntheticGraphDigest(contextValue)
	if err != nil || hash != syntheticString(manifest["graphGenerationContextHash"]) {
		return nil, graphgen.Task{}, invalid
	}
	if syntheticString(contextValue["projectId"]) != projectID {
		return nil, graphgen.Task{}, invalid
	}
	originEpoch := syntheticString(contextValue["originEpoch"])
	if originEpoch == "" || syntheticString(manifest["graphGenerationOriginEpoch"]) != originEpoch {
		return nil, graphgen.Task{}, invalid
	}
	key := syntheticString(contextValue["key"])
	if key == "" || syntheticString(contextValue["createdAt"]) == "" ||
		syntheticString(manifest["graphGenerationKey"]) != key {
		return nil, graphgen.Task{}, invalid
	}
	// The input is frozen twice: inside the context and beside it. They must be
	// the same document, otherwise the identity the caller holds is not the one
	// the run was admitted with.
	manifestInput, err := syntheticGraphDigest(manifest["graphGenerationInput"])
	if err != nil {
		return nil, graphgen.Task{}, invalid
	}
	inputDigest, err := syntheticGraphDigest(input)
	if err != nil || manifestInput != inputDigest {
		return nil, graphgen.Task{}, invalid
	}
	task, err := syntheticGraphTask(kind, contextValue)
	if err != nil {
		return nil, graphgen.Task{}, err
	}
	storedTask := syntheticMap(manifest["graphGenerationTask"])
	if storedTask == nil {
		return nil, graphgen.Task{}, errors.New("GENERATION_GRAPH_TASK_INVALID")
	}
	storedHash, err := syntheticGraphDigest(storedTask)
	if err != nil || storedHash != syntheticString(manifest["graphGenerationTaskHash"]) {
		return nil, graphgen.Task{}, errors.New("GENERATION_GRAPH_TASK_INVALID")
	}
	builtHash, err := syntheticGraphDigest(task)
	if err != nil || builtHash != storedHash {
		return nil, graphgen.Task{}, errors.New("GENERATION_GRAPH_TASK_INVALID")
	}
	return contextValue, task, nil
}

// syntheticGraphTask rebuilds the provider task for one context kind.
func syntheticGraphTask(kind string, contextValue map[string]any) (graphgen.Task, error) {
	invalid := errors.New("GENERATION_GRAPH_CONTEXT_INVALID")
	writingLanguage := syntheticString(contextValue["writingLanguage"])
	switch kind {
	case "plot":
		sources := syntheticMap(contextValue["sources"])
		if sources == nil || sources["snapshot"] != nil ||
			syntheticString(sources["writingLanguage"]) != writingLanguage ||
			!syntheticHex64(syntheticString(contextValue["targetBaselineHash"])) {
			return graphgen.Task{}, invalid
		}
		bundle := graphgen.PlotTreeSourceBundle{}
		if err := syntheticDecode(sources, &bundle); err != nil {
			return graphgen.Task{}, invalid
		}
		task, err := graphgen.BuildPlotTreeTask(bundle)
		if err != nil {
			return graphgen.Task{}, invalid
		}
		return task, nil
	case "plan":
		blueprint := syntheticMap(contextValue["blueprint"])
		totalChapters := syntheticNumber(contextValue["totalChapters"])
		if blueprint == nil || totalChapters < 1 ||
			syntheticNumber(blueprint["chapterNumber"]) != syntheticNumber(syntheticMap(contextValue["input"])["chapterNumber"]) {
			return graphgen.Task{}, invalid
		}
		model := graphgen.Blueprint{}
		if err := syntheticDecode(blueprint, &model); err != nil {
			return graphgen.Task{}, invalid
		}
		task, err := graphgen.BuildNarrativeThreadPlanTask(graphgen.NarrativeThreadPlanTaskInput{
			WritingLanguage: writingLanguage,
			TotalChapters:   int(totalChapters),
			Blueprint:       model,
		})
		if err != nil {
			return graphgen.Task{}, invalid
		}
		return task, nil
	case "event":
		input := syntheticMap(contextValue["input"])
		plan := syntheticMap(contextValue["plan"])
		source := syntheticMap(contextValue["source"])
		content, contentIsText := contextValue["content"].(string)
		contentRevision := syntheticNumber(contextValue["contentRevision"])
		projectionGeneration := syntheticNumber(contextValue["projectionGeneration"])
		if input == nil || plan == nil || source == nil || !contentIsText ||
			syntheticNumber(plan["id"]) != syntheticNumber(input["planId"]) ||
			syntheticNumber(source["draftId"]) != syntheticNumber(input["draftId"]) ||
			syntheticNumber(source["chapterNumber"]) <= 0 ||
			syntheticString(source["finalizationId"]) == "" ||
			graphgen.TextHash(content) != syntheticString(source["contentHash"]) ||
			contentRevision < 0 || projectionGeneration < 0 {
			return graphgen.Task{}, invalid
		}
		view := graphgen.NarrativeThreadView{}
		if err := syntheticDecode(plan, &view); err != nil {
			return graphgen.Task{}, invalid
		}
		task, err := graphgen.BuildNarrativeThreadEventTask(graphgen.NarrativeThreadEventTaskInput{
			WritingLanguage:  writingLanguage,
			Plan:             view,
			DraftID:          int(syntheticNumber(source["draftId"])),
			ChapterNumber:    int(syntheticNumber(source["chapterNumber"])),
			FinalizedContent: content,
		})
		if err != nil {
			return graphgen.Task{}, invalid
		}
		return task, nil
	default:
		return graphgen.Task{}, invalid
	}
}

// syntheticGraphDigest is the lane's canonical hash: the value is normalised
// through encoding/json first, so a struct and the map it decodes to hash the
// same, and map keys are sorted. Graph runs are written by this lane only, so
// the digest never has to agree with the Node build's insertion-order hash.
func syntheticGraphDigest(value any) (string, error) {
	if value == nil {
		return syntheticHash("null"), nil
	}
	normalised, err := syntheticCanonical(value)
	if err != nil {
		return "", err
	}
	var generic any
	if err := json.Unmarshal([]byte(normalised), &generic); err != nil {
		return "", err
	}
	canonical, err := syntheticCanonical(generic)
	if err != nil {
		return "", err
	}
	return syntheticHash(canonical), nil
}

// syntheticDecode re-creates a typed value from the manifest's stored form.
func syntheticDecode(value any, target any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

// syntheticHex64 reports whether text is a lowercase 64-character hex digest.
func syntheticHex64(text string) bool {
	if len(text) != 64 {
		return false
	}
	for _, character := range text {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
