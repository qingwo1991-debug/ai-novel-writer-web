package main

import (
	"encoding/json"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/graphgen"
)

// syntheticGraphManifest builds a frozen graph manifest the way the lane will
// write it: the context and the task are stored together with their digests.
func syntheticGraphManifest(t *testing.T, mutate func(map[string]any)) map[string]any {
	t.Helper()
	sources := map[string]any{}
	sources["writingLanguage"] = "zh-CN"
	sources["synopsis"] = map[string]any{"content": "梗概"}
	blueprint := map[string]any{}
	blueprint["chapterNumber"] = float64(1)
	blueprint["title"] = "第一章"
	blueprint["purpose"] = "铺垫"
	blueprint["keyEvents"] = "开篇"
	sources["blueprints"] = []any{blueprint}
	sources["finalizedChapters"] = []any{}
	sources["narrativeThreads"] = []any{}
	sources["sourceRevision"] = "synthetic-source"
	sources["snapshot"] = nil
	contextValue := map[string]any{}
	contextValue["kind"] = "plot"
	contextValue["projectId"] = "project-1"
	contextValue["originEpoch"] = "epoch-1"
	contextValue["key"] = "plot:1"
	contextValue["createdAt"] = "2026-01-01T00:00:00.000Z"
	contextValue["writingLanguage"] = "zh-CN"
	contextValue["input"] = map[string]any{"kind": "plot"}
	contextValue["targetBaselineHash"] = syntheticHash("baseline")
	contextValue["sources"] = sources
	bundle := graphgen.PlotTreeSourceBundle{}
	if err := syntheticDecode(sources, &bundle); err != nil {
		t.Fatal(err)
	}
	task, err := graphgen.BuildPlotTreeTask(bundle)
	if err != nil {
		t.Fatal(err)
	}
	rawTask, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var taskValue map[string]any
	if err := json.Unmarshal(rawTask, &taskValue); err != nil {
		t.Fatal(err)
	}
	contextHash, err := syntheticGraphDigest(contextValue)
	if err != nil {
		t.Fatal(err)
	}
	taskHash, err := syntheticGraphDigest(taskValue)
	if err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{}
	manifest["operation"] = "plot-tree-snapshot"
	manifest["graphGenerationContext"] = contextValue
	manifest["graphGenerationContextHash"] = contextHash
	manifest["graphGenerationOriginEpoch"] = "epoch-1"
	manifest["graphGenerationKey"] = "plot:1"
	manifest["graphGenerationInput"] = map[string]any{"kind": "plot"}
	manifest["graphGenerationTask"] = taskValue
	manifest["graphGenerationTaskHash"] = taskHash
	if mutate != nil {
		mutate(manifest)
	}
	return manifest
}

func TestSyntheticGraphContextAcceptsAFrozenPlotRun(t *testing.T) {
	manifest := syntheticGraphManifest(t, nil)
	contextValue, task, err := syntheticGraphContext(manifest, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	if syntheticString(contextValue["key"]) != "plot:1" {
		t.Fatalf("context = %#v", contextValue)
	}
	if len(task.Messages) == 0 || task.Purpose == "" {
		t.Fatalf("task = %#v", task)
	}
}

func TestSyntheticGraphContextRefusesATamperedContext(t *testing.T) {
	// Every field the digest covers must actually be verified, so the tamper is
	// applied after the digests were computed.
	manifest := syntheticGraphManifest(t, nil)
	syntheticMap(manifest["graphGenerationContext"])["key"] = "plot:2"
	if _, _, err := syntheticGraphContext(manifest, "project-1"); err == nil ||
		err.Error() != "GENERATION_GRAPH_CONTEXT_INVALID" {
		t.Fatalf("err = %v, want GENERATION_GRAPH_CONTEXT_INVALID", err)
	}
}

func TestSyntheticGraphContextRefusesATamperedTask(t *testing.T) {
	manifest := syntheticGraphManifest(t, nil)
	syntheticMap(manifest["graphGenerationTask"])["purpose"] = "something else"
	if _, _, err := syntheticGraphContext(manifest, "project-1"); err == nil ||
		err.Error() != "GENERATION_GRAPH_TASK_INVALID" {
		t.Fatalf("err = %v, want GENERATION_GRAPH_TASK_INVALID", err)
	}
}

func TestSyntheticGraphContextRefusesAnotherProject(t *testing.T) {
	manifest := syntheticGraphManifest(t, nil)
	if _, _, err := syntheticGraphContext(manifest, "project-2"); err == nil ||
		err.Error() != "GENERATION_GRAPH_CONTEXT_INVALID" {
		t.Fatalf("err = %v, want GENERATION_GRAPH_CONTEXT_INVALID", err)
	}
}

func TestSyntheticGraphContextRefusesADifferentInput(t *testing.T) {
	// The input is frozen twice; a manifest whose copies disagree is not the
	// identity the run was admitted with.
	manifest := syntheticGraphManifest(t, func(manifest map[string]any) {
		manifest["graphGenerationInput"] = map[string]any{"kind": "plot", "chapterNumber": float64(9)}
	})
	if _, _, err := syntheticGraphContext(manifest, "project-1"); err == nil ||
		err.Error() != "GENERATION_GRAPH_CONTEXT_INVALID" {
		t.Fatalf("err = %v, want GENERATION_GRAPH_CONTEXT_INVALID", err)
	}
}
