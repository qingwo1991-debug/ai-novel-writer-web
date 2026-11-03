//go:build linux

package globalconfig

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func modelTxnFixture(t *testing.T) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	config := []byte(`{"defaultModelId":"writer","defaultEmbeddingModelId":"writer","stageModelIds":{"planning":"writer","drafting":"other","unknown":"writer"},"untouched":{"x":1}}`)
	models := []byte(`[{"id":"writer","apiKey":"private-key","provider":"openai","protocol":"openai","baseUrl":"https://example.invalid/v1"},{"id":"other","apiKey":"another-key","provider":"openai","protocol":"openai","baseUrl":"https://example.invalid/v1"}]`)
	if err := os.WriteFile(filepath.Join(root, "config.json"), config, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "models.json"), models, 0600); err != nil {
		t.Fatal(err)
	}
	service, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return service, root
}

func TestDeleteModelAndReferencesRecoversBothDocuments(t *testing.T) {
	service, root := modelTxnFixture(t)
	result, err := service.DeleteModelAndReferences("writer")
	if err != nil {
		t.Fatal(err)
	}
	if result.DefaultModelID != nil || result.DefaultEmbeddingModelID != nil || len(result.StageModelIDs) != 1 || result.StageModelIDs["drafting"] != "other" {
		t.Fatal("reference projection wrong", result)
	}
	value, err := service.Get()
	if err != nil {
		t.Fatal(err)
	}
	config := value.(map[string]any)
	if config["defaultModelId"] != nil || config["defaultEmbeddingModelId"] != nil || config["untouched"] == nil {
		t.Fatal("config reference update failed", config)
	}
	stages := config["stageModelIds"].(map[string]any)
	if len(stages) != 1 || stages["drafting"] != "other" {
		t.Fatal("stage references not cleared", stages)
	}
	models, err := service.ListPrivateModels()
	if err != nil || len(models) != 1 || models[0]["id"] != "other" || models[0]["apiKey"] != "another-key" {
		t.Fatal("model deletion damaged other credential", err)
	}
	if _, err := os.Lstat(filepath.Join(root, modelDeleteJournalName)); !os.IsNotExist(err) {
		t.Fatal("journal not retired", err)
	}
}

func TestModelDeleteJournalRollsForwardPartialCommitAndPreservesThirdPartyConflict(t *testing.T) {
	service, root := modelTxnFixture(t)
	oldConfig, err := os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	oldModels, err := os.ReadFile(filepath.Join(root, "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	newConfig := []byte(`{"defaultModelId":null,"defaultEmbeddingModelId":null,"stageModelIds":{},"untouched":{"x":1}}`)
	newModels := []byte(`[{"id":"other","apiKey":"another-key","provider":"openai","protocol":"openai","baseUrl":"https://example.invalid/v1"}]`)
	journal := modelDeleteJournal{Version: 1, ModelID: "writer", ConfigOld: oldConfig, ConfigNew: newConfig, ModelsOld: oldModels, ModelsNew: newModels}
	if err := service.locked(func(fd int) error { return writeModelDeleteJournal(fd, journal) }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), newConfig, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(); err != nil {
		t.Fatal("partial commit did not recover", err)
	}
	actual, err := os.ReadFile(filepath.Join(root, "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, newModels) {
		t.Fatal("models did not roll forward")
	}
	if _, err := os.Lstat(filepath.Join(root, modelDeleteJournalName)); !os.IsNotExist(err) {
		t.Fatal("completed journal remained", err)
	}

	if err := service.locked(func(fd int) error { return writeModelDeleteJournal(fd, journal) }); err != nil {
		t.Fatal(err)
	}
	thirdParty := []byte(`{"defaultModelId":"third-party"}`)
	if err := os.WriteFile(filepath.Join(root, "config.json"), thirdParty, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(); !errors.Is(err, ErrModelDeleteJournalConflict) {
		t.Fatal("conflicting config accepted", err)
	}
	still, err := os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(still, thirdParty) {
		t.Fatal("conflicting config overwritten")
	}
}

func TestModelDeleteJournalCorruptionBlocksAllServiceActions(t *testing.T) {
	service, root := modelTxnFixture(t)
	journal := filepath.Join(root, modelDeleteJournalName)
	if err := os.WriteFile(journal, []byte(`{"version":1}`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(); !errors.Is(err, ErrModelDeleteJournalInvalid) {
		t.Fatal("unsafe journal accepted by read", err)
	}
	if err := service.SaveModel(ModelRecord{"id": "new", "provider": "openai", "protocol": "openai", "baseUrl": "https://example.invalid/v1", "apiKey": "secret"}); !errors.Is(err, ErrModelDeleteJournalInvalid) {
		t.Fatal("unsafe journal accepted by write", err)
	}
	models, err := os.ReadFile(filepath.Join(root, "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(models, []byte(`"id":"new"`)) {
		t.Fatal("write bypassed journal gate")
	}
}

func TestModelStageAssignmentsUseOriginalJSTrimSet(t *testing.T) {
	assignments := stageAssignments(map[string]any{
		"planning": "\uFEFF writer \uFEFF",
		"drafting": "\u0085",
		"review":   " \t ",
		"unknown":  "writer",
	})
	if len(assignments) != 2 || assignments["planning"] != "writer" || assignments["drafting"] != "\u0085" {
		t.Fatal("stage parsing changed JavaScript trim semantics", assignments)
	}
}
