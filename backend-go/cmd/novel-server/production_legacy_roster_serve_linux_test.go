//go:build linux

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/prompttemplate"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file proves the composition the served legacy-roster begin/read/cancel
// rely on, without a loopback: the frozen task carries the shipped system prompt
// and contract byte for byte, the frozen context round-trips the preserved
// markdown, and the request validators refuse exactly the shapes the reference
// refuses. The served handlers themselves are wired in production_open_linux.go
// (newProductionLegacyRosterServe(owner).Bind(server)), which this change does
// not own; the binder below pins that the serve installs nothing when the owner
// is absent, and the real-HTTP success/replay/refusal tests run once that binder
// line is applied.

// productionLegacyRosterServeSource builds a pending-repair source the context
// and task freeze from, reusing the preserved text the read-source fixtures pin.
func productionLegacyRosterServeSource() *productionLegacyRosterSource {
	return &productionLegacyRosterSource{
		Snapshot: map[string]any{
			"schemaVersion":  float64(1),
			"revision":       float64(0),
			"migrationState": "legacy_markdown_pending",
			"status":         "legacy_repair_required",
			"entries":        []any{},
			"legacyMarkdown": productionLegacyRosterPreservedText,
		},
		RawLegacy:        productionLegacyRosterPreservedText,
		LegacyHash:       productionLegacyRosterTextHash(productionLegacyRosterPreservedText),
		IdentityRevision: 0,
		ActiveIDs:        []string{},
		FactsHash:        strings.Repeat("a", 64),
		Genre:            "fantasy",
		WritingLanguage:  productionLegacyRosterLanguageZhCN,
	}
}

// TestProductionLegacyRosterServeComposition proves the frozen task carries the
// shipped system prompt and contract byte for byte, that the frozen context
// round-trips the preserved markdown, and that no Go file restates the prose.
func TestProductionLegacyRosterServeComposition(t *testing.T) {
	prompt, err := prompttemplate.LegacyRosterTemplate()
	if err != nil {
		t.Fatalf("%v", err)
	}
	source := productionLegacyRosterServeSource()

	contextText, err := productionLegacyRosterContextText("project", "epoch", "key", "2024-01-01T00:00:00.000Z", source)
	if err != nil {
		t.Fatalf("%v", err)
	}
	decoded, err := productionLegacyRosterDecodeContext([]byte(contextText))
	if err != nil {
		t.Fatalf("%v", err)
	}
	decodedSource := syntheticMap(decoded["source"])
	if syntheticString(decodedSource["rawLegacy"]) != productionLegacyRosterPreservedText {
		t.Fatal("the frozen context did not round-trip the preserved markdown")
	}
	if syntheticString(decoded["key"]) != "key" || syntheticString(decoded["originEpoch"]) != "epoch" {
		t.Fatalf("the frozen context lost its identity members: %+v", decoded)
	}

	taskText, err := productionLegacyRosterTaskText(source.RawLegacy, source.Genre)
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
	if task.Purpose != productionLegacyRosterOperation || task.Output != "structured-data" || len(task.Messages) != 2 {
		t.Fatalf("the frozen task is not the reference's: %+v", task)
	}
	if task.Messages[0].Role != "system" || task.Messages[1].Role != "user" {
		t.Fatalf("the frozen task messages are not system/user: %+v", task.Messages)
	}
	if task.Messages[0].Content != prompt.SystemPrompt {
		t.Fatal("the frozen system message is not the shipped system prompt, byte for byte")
	}
	if !strings.Contains(task.Messages[1].Content, prompt.Contract) {
		t.Fatal("the frozen user message does not carry the shipped contract")
	}
	if !strings.Contains(task.Messages[1].Content, productionLegacyRosterPreservedText) ||
		!strings.Contains(task.Messages[1].Content, "fantasy") {
		t.Fatal("the frozen user message does not carry the preserved markdown and genre")
	}

	// The shipped prose is not written in Go: the first non-empty line of the
	// system prompt is taken from the asset, and no Go file in this package
	// carries it.
	lines := strings.Split(prompt.SystemPrompt, "\n")
	if len(lines) < 2 || lines[1] == "" {
		t.Fatal("the shipped system prompt no longer has the first line this scan keys on")
	}
	marker := lines[1]
	for _, sourceFile := range productionEditorInlineWriteSources(t) {
		if strings.Contains(sourceFile.body, marker) {
			t.Fatalf("%v restates the shipped roster prompt in Go; it must come from the committed asset", sourceFile.name)
		}
	}
}

// TestProductionLegacyRosterServeRequestValidation pins the two request
// validators to the reference's closed shapes.
func TestProductionLegacyRosterServeRequestValidation(t *testing.T) {
	if _, _, err := productionLegacyRosterBeginRequest(map[string]any{"modelId": "m", "uiActionNonce": "n"}); err != nil {
		t.Fatalf("a valid begin request was refused: %v", err)
	}
	if _, _, err := productionLegacyRosterBeginRequest(map[string]any{"modelId": "m", "uiActionNonce": "n", "extra": true}); err == nil {
		t.Fatal("a widened begin request was accepted")
	}
	if _, _, err := productionLegacyRosterBeginRequest(map[string]any{"modelId": "m", "uiActionNonce": "   "}); err == nil {
		t.Fatal("a blank nonce was accepted")
	}
	if _, _, err := productionLegacyRosterBeginRequest(map[string]any{"modelId": "", "uiActionNonce": "n"}); err == nil {
		t.Fatal("an empty model id was accepted")
	}

	validHandle := map[string]any{"projectId": "p", "epoch": "e", "rootActionId": "r", "runId": "u"}
	if _, err := productionLegacyRosterHandleRequest(map[string]any{"handle": validHandle}); err != nil {
		t.Fatalf("a valid read/cancel request was refused: %v", err)
	}
	if _, err := productionLegacyRosterHandleRequest(map[string]any{"handle": validHandle, "extra": true}); err == nil {
		t.Fatal("a widened read/cancel request was accepted")
	}
	if _, err := productionLegacyRosterHandleRequest(map[string]any{"handle": map[string]any{"projectId": "p"}}); err == nil {
		t.Fatal("a partial handle was accepted")
	}
	if _, err := productionLegacyRosterHandleRequest(map[string]any{"projectId": "p"}); err == nil {
		t.Fatal("a request without the handle key was accepted")
	}
}

// TestProductionLegacyRosterServeBinder pins the serve's fail-closed shape: a
// nil owner installs nothing, and a nil server is refused.
func TestProductionLegacyRosterServeBinder(t *testing.T) {
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
	if err := newProductionLegacyRosterServe(nil).Bind(server); err != nil {
		t.Fatalf("the serve binder failed with a nil owner: %v", err)
	}
	if names := server.BoundOperations(); len(names) != 0 {
		t.Fatalf("the serve binder installed %v with a nil owner, want nothing", names)
	}
	if err := newProductionLegacyRosterServe(nil).Bind(nil); err == nil {
		t.Fatal("a nil server was accepted")
	}
	for _, operation := range productionLegacyRosterOperations() {
		// Each name is inventoried, so an absent handler is exactly 501.
		fresh := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
		if err := fresh.Bind(operation, func(context.Context, transport.Caller, []any) (any, error) {
			return nil, nil
		}); err != nil {
			t.Fatalf("%s is not inventoried, so its unbound answer would not be 501: %v", operation, err)
		}
	}
}
