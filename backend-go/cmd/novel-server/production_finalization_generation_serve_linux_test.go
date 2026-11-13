//go:build linux

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/prompttemplate"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file proves the composition the served finalization begin/execute/commit
// rely on, without a loopback: the frozen seven-member context carries the
// shipped template asset byte for byte, the task renders the shipped body and
// contract, and the request validators refuse exactly the shapes the reference
// refuses. The served handlers themselves are wired in production_open_linux.go
// (newProductionFinalizationServe(owner).Bind(server)), which this change does
// not own; the binder below pins that the serve installs nothing when the owner
// is absent.

const (
	productionFinalizationServeSlotID  = "finalization-serve-owned"
	productionFinalizationServeDraft   = 7
	productionFinalizationServeChapter = 3
)

// productionFinalizationServeSlot builds one valid chapter_notes slot.
func productionFinalizationServeSlot(t *testing.T) productionFinalizationSlot {
	t.Helper()
	slot, err := productionFinalizationParseSlot(map[string]any{
		"source": map[string]any{
			"draftId":        float64(productionFinalizationServeDraft),
			"chapterNumber":  float64(productionFinalizationServeChapter),
			"finalizationId": productionFinalizationServeSlotID,
			"contentHash":    strings.Repeat("d", 64),
		},
		"stepKey": "chapter_notes",
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
	return slot
}

// productionFinalizationServeIdentity is a legacy identity, the shape whose
// serialization carries no provenance map.
func productionFinalizationServeIdentity(t *testing.T) *store.FinalizedCharacterContext {
	t.Helper()
	return &store.FinalizedCharacterContext{
		ProjectID:            "project",
		Epoch:                "lease",
		Source:               store.FinalizedCharacterSourceIdentity{DraftID: productionFinalizationServeDraft, FinalizationID: productionFinalizationServeSlotID, ChapterNumber: productionFinalizationServeChapter, ContentHash: strings.Repeat("d", 64)},
		Content:              "第三章正文：他推开门。",
		ProjectionGeneration: 0,
		IdentityRevision:     0,
		IdentityStatus:       store.FinalizedCharacterIdentityLegacy,
		SourceOrder:          store.FinalizedCharacterSourceOrder{ContinuityEpoch: "project:0", ChapterNumber: productionFinalizationServeChapter, AuthoritativeFinalizationRevision: 1},
		Characters:           []store.FinalizedCharacterContextCharacter{},
	}
}

// TestProductionFinalizationServeComposition proves the frozen context text
// carries the shipped template byte for byte and that no Go file restates it.
func TestProductionFinalizationServeComposition(t *testing.T) {
	template, err := prompttemplate.FinalizationTemplate(prompttemplate.FinalizationChapterNotesKey, prompttemplate.LanguageChinese)
	if err != nil {
		t.Fatalf("%v", err)
	}
	slot := productionFinalizationServeSlot(t)
	identity := productionFinalizationServeIdentity(t)
	baseline := productionFinalizationNotesBaseline{BlueprintExists: true, BlueprintNotes: "要点", ContinuityHash: strings.Repeat("c", 64)}
	contextText, err := productionFinalizationContextText(slot, identity, "第三章 门后有人", []string{}, prompttemplate.LanguageChinese, template.Template.Text, baseline)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(contextText, template.Template.Text) {
		t.Fatal("the frozen context text does not carry the shipped template text")
	}
	var decoded struct {
		Slot struct {
			StepKey string `json:"stepKey"`
		} `json:"slot"`
		WritingLanguage string `json:"writingLanguage"`
		Template        struct {
			Key string `json:"key"`
		} `json:"template"`
	}
	if err := json.Unmarshal([]byte(contextText), &decoded); err != nil {
		t.Fatalf("%v", err)
	}
	if decoded.Slot.StepKey != "chapter_notes" || decoded.WritingLanguage != prompttemplate.LanguageChinese || decoded.Template.Key != prompttemplate.FinalizationChapterNotesKey {
		t.Fatalf("the frozen context is not the reference's: %+v", decoded)
	}
	if !strings.HasPrefix(contextText, `{"slot":{"source":{"draftId":`) {
		t.Fatalf("the frozen context is not in the reference member order: %s", contextText)
	}
	taskText, err := productionFinalizationTaskText(slot, template, prompttemplate.LanguageChinese, identity, "第三章 门后有人")
	if err != nil {
		t.Fatalf("%v", err)
	}
	var task struct {
		Purpose  string `json:"purpose"`
		Output   string `json:"output"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(taskText), &task); err != nil {
		t.Fatalf("%v %s", err, taskText)
	}
	if task.Purpose != productionFinalizationOperationNotes || task.Output != "visible-text" || len(task.Messages) != 2 {
		t.Fatalf("the frozen task is not the reference's: %+v", task)
	}
	if !strings.Contains(task.Messages[0].Content, template.Template.SystemRole) ||
		!strings.Contains(task.Messages[0].Content, template.SystemContract) {
		t.Fatalf("the frozen system message is not the composed role: %s", task.Messages[0].Content)
	}
	if !strings.Contains(task.Messages[1].Content, identity.Content) || strings.Contains(task.Messages[1].Content, "{{") {
		t.Fatalf("the frozen user message is not the rendered body: %s", task.Messages[1].Content)
	}
	// The shipped body is not written in Go: the body line that names the frozen
	// chapter is taken from the asset, and no Go file in this package carries it.
	lines := strings.Split(template.Template.Content, "\n")
	if len(lines) < 3 || lines[2] == "" {
		t.Fatal("the shipped template body no longer has the section line this scan keys on")
	}
	marker := lines[2]
	for _, source := range productionEditorInlineWriteSources(t) {
		if productionSeamContains(source.body, marker) {
			t.Fatalf("%v writes the shipped template body in Go; it must come from the committed asset", source.name)
		}
	}
}

// TestProductionFinalizationServeRequestValidation pins the three request
// validators to the reference's closed shapes.
func TestProductionFinalizationServeRequestValidation(t *testing.T) {
	validSlot := map[string]any{
		"source": map[string]any{
			"draftId":        float64(productionFinalizationServeDraft),
			"chapterNumber":  float64(productionFinalizationServeChapter),
			"finalizationId": productionFinalizationServeSlotID,
			"contentHash":    strings.Repeat("d", 64),
		},
		"stepKey": "chapter_notes",
	}
	if _, _, _, err := productionFinalizationBeginRequest(map[string]any{"slot": validSlot, "modelId": "m"}); err != nil {
		t.Fatalf("a valid begin request was refused: %v", err)
	}
	widened := map[string]any{"slot": validSlot, "modelId": "m", "extra": true}
	if _, _, _, err := productionFinalizationBeginRequest(widened); err == nil {
		t.Fatal("a widened begin request was accepted")
	}
	if _, err := productionFinalizationExecuteRequest(map[string]any{"handle": map[string]any{
		"projectId": "p", "epoch": "e", "rootActionId": "r", "runId": "u"}}); err != nil {
		t.Fatalf("a valid execute request was refused: %v", err)
	}
	if _, err := productionFinalizationExecuteRequest(map[string]any{"handle": map[string]any{"projectId": "p"}}); err == nil {
		t.Fatal("a partial handle was accepted")
	}
	ref := map[string]any{"artifactId": "a", "revision": 1, "textHash": strings.Repeat("a", 64)}
	if _, _, err := productionFinalizationCommitRequest(map[string]any{"handle": map[string]any{
		"projectId": "p", "epoch": "e", "rootActionId": "r", "runId": "u"}, "artifact": ref}); err != nil {
		t.Fatalf("a valid commit request was refused: %v", err)
	}
	if _, _, err := productionFinalizationCommitRequest(map[string]any{"handle": map[string]any{
		"projectId": "p", "epoch": "e", "rootActionId": "r", "runId": "u"}, "artifact": map[string]any{"artifactId": "a"}}); err == nil {
		t.Fatal("an incomplete artifact was accepted")
	}
	if _, _, err := productionFinalizationCommitRequest(map[string]any{"handle": map[string]any{
		"projectId": "p", "epoch": "e", "rootActionId": "r", "runId": "u"}, "artifact": ref, "extra": true}); err == nil {
		t.Fatal("a widened commit request was accepted")
	}
}

// TestProductionFinalizationServeBinder pins the serve's fail-closed shape: a
// nil owner installs nothing, and a nil server is refused.
func TestProductionFinalizationServeBinder(t *testing.T) {
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
	if err := newProductionFinalizationServe(nil).Bind(server); err != nil {
		t.Fatalf("the serve binder failed with a nil owner: %v", err)
	}
	if names := server.BoundOperations(); len(names) != 0 {
		t.Fatalf("the serve binder installed %v with a nil owner, want nothing", names)
	}
	if err := newProductionFinalizationServe(nil).Bind(nil); err == nil {
		t.Fatal("a nil server was accepted")
	}
	for _, operation := range productionFinalizationGenerationOperations() {
		// Each name is inventoried, so an absent handler is exactly 501.
		fresh := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
		if err := fresh.Bind(operation, func(context.Context, transport.Caller, []any) (any, error) {
			return nil, nil
		}); err != nil {
			t.Fatalf("%s is not inventoried, so its unbound answer would not be 501: %v", operation, err)
		}
	}
}
