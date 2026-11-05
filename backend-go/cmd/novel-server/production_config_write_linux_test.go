//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// Every fixture below is written inside one disposable t.TempDir(): the synthetic
// data root productionDurableDataRoot builds, its committed generation (the
// config.json and models.json documents the model and config writes replace), the
// prompt and installed-skill tree under that same generation, and the project tree
// the two fs writes are confined to. No real author data root is read or written.
const (
	productionWriteModelID       = "ci-written-model"
	productionWriteModelName     = "合成写入模型"
	productionWriteModelBaseURL  = "https://synthetic.invalid/v1"
	productionWriteModelSecret   = "synthetic-production-written-model-key"
	productionWritePromptKey     = "ci-authored-prompt"
	productionWritePromptBody    = "改写后的全局提示词：他推开门，门后有人。"
	productionWriteKeptPrompt    = "ci-kept-prompt"
	productionWriteSkillName     = "ci-installed-skill"
	productionWriteKeptSkill     = "ci-kept-skill"
	productionWriteDirPath       = "notes/chapter-1"
	productionWriteChapterPath   = "manuscript/chapter-2.md"
	productionWriteChapterBody   = "第二章：门后的人抬起头。"
	productionWriteNeighbourPath = "manuscript/chapter-1.md"
	productionWriteNeighbourBody = "第一章：门是锁着的。"
	productionWriteOutsideFile   = "written-through-the-escape.txt"
)

// productionWriteGeneration is the committed generation every global write
// replaces a document inside.
func productionWriteGeneration(root string) string {
	return filepath.Dir(productionCommittedConfigPath(root))
}

// productionWriteReadback opens a SEPARATE global config store over the committed
// generation: its own directory descriptors, its own flock and its own reads, so a
// value it reports is on disk and not in the lane's memory. It is opened only
// between requests, because the store serializes on a directory flock.
func productionWriteReadback(t *testing.T, root string) *globalconfig.Service {
	t.Helper()
	service, err := globalconfig.New(productionWriteGeneration(root))
	if err != nil {
		t.Fatalf("open a second global config connection: %v", err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Errorf("close the second global config connection: %v", err)
		}
	})
	return service
}

// productionWriteConfigDocument reads config.json straight off disk.
func productionWriteConfigDocument(t *testing.T, root string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(productionCommittedConfigPath(root))
	if err != nil {
		t.Fatalf("read the committed config document: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("decode the committed config document: %v", err)
	}
	return document
}

// productionWriteModelIDs is the id set of the on-disk models.json document,
// read without any store: a delete has only happened when the file says so.
func productionWriteModelIDs(t *testing.T, root string) []string {
	t.Helper()
	raw, err := os.ReadFile(productionCommittedModelsPath(root))
	if err != nil {
		t.Fatalf("read the committed model document: %v", err)
	}
	var records []map[string]any
	if err := json.Unmarshal(raw, &records); err != nil {
		t.Fatalf("decode the committed model document: %v", err)
	}
	ids := make([]string, 0, len(records))
	for _, record := range records {
		id, _ := record["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// productionWriteResult performs one operation and returns its result payload. The
// lane answers a bound name at HTTP 200, so a transport status here means the name
// is not served the way this test claims.
func productionWriteResult(t *testing.T, client *productionDurableClient, operation string, args []any) any {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code != http.StatusOK {
		t.Fatalf("%s answered %d instead of the family's 200 envelope: %s", operation, response.Code, response.Body.String())
	}
	var wire struct {
		Result any `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	return wire.Result
}

// productionWriteObject is productionWriteResult for the object envelopes.
func productionWriteObject(t *testing.T, client *productionDurableClient, operation string, args []any) map[string]any {
	t.Helper()
	result, ok := productionWriteResult(t, client, operation, args).(map[string]any)
	if !ok {
		t.Fatalf("%s did not answer an object envelope", operation)
	}
	return result
}

// productionWriteSucceeded asserts one write answered `{"success":true}`.
func productionWriteSucceeded(t *testing.T, client *productionDurableClient, label, operation string, args []any) map[string]any {
	t.Helper()
	result := productionWriteObject(t, client, operation, args)
	if success, _ := result["success"].(bool); !success {
		t.Fatalf("%s %s did not commit: %v", label, operation, result)
	}
	return result
}

// productionWriteRefused asserts one write answered the family's business refusal
// envelope and returns the symbolic code it reported.
func productionWriteRefused(t *testing.T, client *productionDurableClient, label, operation string, args []any) string {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code != http.StatusOK {
		t.Fatalf("%s %s answered %d instead of the family's 200 envelope: %s",
			label, operation, response.Code, response.Body.String())
	}
	var wire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || wire.Result == nil {
		t.Fatalf("%s %s: %v %s", label, operation, err, response.Body.String())
	}
	if success, _ := wire.Result["success"].(bool); success {
		t.Fatalf("%s %s reported success: %s", label, operation, response.Body.String())
	}
	code, _ := wire.Result["error"].(string)
	if code == "" {
		t.Fatalf("%s %s refused without a symbolic code: %s", label, operation, response.Body.String())
	}
	return code
}

// productionWriteTransportRefused asserts one write answered a transport refusal
// with the exact status and code the gate promises.
func productionWriteTransportRefused(t *testing.T, client *productionDurableClient, operation string, args []any, wantStatus int, wantCode string) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code != wantStatus {
		t.Fatalf("%s answered %d instead of %d: %s", operation, response.Code, wantStatus, response.Body.String())
	}
	var wire struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	if wire.Error.Code != wantCode {
		t.Fatalf("%s refused with %q instead of %q: %s", operation, wire.Error.Code, wantCode, response.Body.String())
	}
}

// productionWriteLane opens the production lane over one disposable data root and
// returns its cleanup plus the browser tab used by every request. The cleanup is
// deferred by the caller, so a test that wants to observe the lane AFTER it
// released the writer lease can call it itself.
func productionWriteLane(t *testing.T, root string) (func(), *productionDurableClient) {
	t.Helper()
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatalf("open the production lane: %v", err)
	}
	return cleanup, newProductionDurableClient(t, handler, productionDurableTab)
}

// productionWriteOpenProject opens the fixture's admitted project under the
// caller's live lease and returns the frozen three-key session context the file
// writes carry.
func productionWriteOpenProject(t *testing.T, client *productionDurableClient, projectPath, requestToken string) map[string]any {
	t.Helper()
	var opened productionDurableOpenWire
	client.decode(t, "project:open", []any{projectPath, requestToken, projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	lease, _ := opened.Project["sessionLease"].(string)
	if lease == "" {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	return map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     lease,
	}
}

// TestProductionGlobalWritesPersistThroughTheirStores is the model, config and
// prompt write surface: one subtest per operation, each one asserting the store's
// own bytes - the committed models.json and config.json read back through a
// SEPARATE global config connection and through the already bound reads, and the
// prompt and skill files read off disk - plus the one refusal that matters
// (a delete does NOT refuse while a model is referenced: it clears the reference
// transactionally, and the test asserts exactly that instead of assuming a code).
func TestProductionGlobalWritesPersistThroughTheirStores(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	// The committed story profile gains the generation purpose the stage
	// assignment requires; nothing else about the document changes.
	productionCommitGenerationPurpose(t, root, productionTestModelID)

	generation := productionWriteGeneration(root)
	prompts := filepath.Join(generation, "prompts")
	skills := filepath.Join(generation, "skills")
	for _, directory := range []string{prompts, filepath.Join(skills, productionWriteSkillName), filepath.Join(skills, productionWriteKeptSkill)} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// A legacy single-file prompt for the key the save has to replace, a second
	// template no operation here may touch, and two installed skills.
	legacyPrompt := filepath.Join(prompts, productionWritePromptKey+".json")
	if err := os.WriteFile(legacyPrompt, []byte(`{"key":"`+productionWritePromptKey+`","writingLanguage":"zh-CN","content":"旧版提示词"}`), 0600); err != nil {
		t.Fatal(err)
	}
	keptPrompt := filepath.Join(prompts, productionWriteKeptPrompt+".zh-CN.json")
	keptBytes := []byte(`{"key":"` + productionWriteKeptPrompt + `","writingLanguage":"zh-CN","content":"保留的提示词"}`)
	if err := os.WriteFile(keptPrompt, keptBytes, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{productionWriteSkillName, productionWriteKeptSkill} {
		if err := os.WriteFile(filepath.Join(skills, name, "SKILL.md"), []byte("# "+name), 0600); err != nil {
			t.Fatal(err)
		}
	}

	cleanup, author := productionWriteLane(t, root)
	t.Cleanup(cleanup)
	frozen := productionWriteOpenProject(t, author, projectPath, "write-open-1")
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	seeded, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("llm:save-model", func(t *testing.T) {
		candidate := map[string]any{
			"id": productionWriteModelID, "name": productionWriteModelName,
			"provider": "custom", "protocol": "openai",
			"baseUrl": productionWriteModelBaseURL, "apiKey": productionWriteModelSecret,
			"modelName": "ci-written-model-wire",
			"purposes":  []any{"generation"},
		}
		response := productionWriteSucceeded(t, author, "new profile", "llm:save-model", []any{candidate})
		if _, leaked := response["apiKey"]; leaked {
			t.Fatalf("llm:save-model echoed a credential: %v", response)
		}
		// The bytes are on disk: a second connection reads them back, and the
		// document itself carries the profile with its credential withheld from
		// the browser only.
		stored, err := productionWriteReadback(t, root).ListPrivateModels()
		if err != nil {
			t.Fatalf("read the saved model back through a second connection: %v", err)
		}
		found := false
		for _, record := range stored {
			if record["id"] != productionWriteModelID {
				continue
			}
			found = true
			if record["baseUrl"] != productionWriteModelBaseURL || record["apiKey"] != productionWriteModelSecret {
				t.Fatalf("the saved profile was not persisted as submitted: %v", record)
			}
		}
		if !found {
			t.Fatalf("the saved profile is not in the committed model document: %v", productionWriteModelIDs(t, root))
		}
		if ids := productionWriteModelIDs(t, root); !containsString(ids, productionWriteModelID) || !containsString(ids, productionTestModelID) {
			t.Fatalf("llm:save-model did not add exactly one profile: %v", ids)
		}
	})

	t.Run("llm:set-default-model", func(t *testing.T) {
		productionWriteSucceeded(t, author, "default model", "llm:set-default-model", []any{productionWriteModelID})
		if got := productionWriteResult(t, author, "llm:get-default-model", []any{}); got != productionWriteModelID {
			t.Fatalf("llm:get-default-model returned %v instead of the written id", got)
		}
		value, err := productionWriteReadback(t, root).DefaultModel()
		if err != nil || value != productionWriteModelID {
			t.Fatalf("a second connection read %v (%v) instead of the written default model", value, err)
		}
		if document := productionWriteConfigDocument(t, root); document["defaultModelId"] != productionWriteModelID {
			t.Fatalf("config.json does not carry the written default model: %v", document["defaultModelId"])
		}
	})

	t.Run("llm:set-default-embedding-model", func(t *testing.T) {
		productionWriteSucceeded(t, author, "default embedding model", "llm:set-default-embedding-model", []any{productionWriteModelID})
		if got := productionWriteResult(t, author, "llm:get-default-embedding-model", []any{}); got != productionWriteModelID {
			t.Fatalf("llm:get-default-embedding-model returned %v instead of the written id", got)
		}
		value, err := productionWriteReadback(t, root).DefaultEmbeddingModel()
		if err != nil || value != productionWriteModelID {
			t.Fatalf("a second connection read %v (%v) instead of the written embedding model", value, err)
		}
		if document := productionWriteConfigDocument(t, root); document["defaultEmbeddingModelId"] != productionWriteModelID {
			t.Fatalf("config.json does not carry the written embedding model: %v", document["defaultEmbeddingModelId"])
		}
	})

	t.Run("llm:set-stage-model", func(t *testing.T) {
		result := productionWriteSucceeded(t, author, "stage assignment", "llm:set-stage-model",
			[]any{map[string]any{"stage": "finalization", "modelId": productionWriteModelID}})
		assigned, _ := result["stageModelIds"].(map[string]any)
		if assigned["finalization"] != productionWriteModelID {
			t.Fatalf("llm:set-stage-model did not report the assignment: %v", result)
		}
		stages, err := productionWriteReadback(t, root).StageModels()
		if err != nil || stages["finalization"] != productionWriteModelID {
			t.Fatalf("a second connection read %v (%v) instead of the assigned stage", stages, err)
		}
		// The committed drafting assignment is untouched by a finalization write.
		if stages["drafting"] != productionTestModelID {
			t.Fatalf("the stage write dropped the committed drafting assignment: %v", stages)
		}
		// An id that is not a saved generation profile is refused by the store,
		// not silently stored, and an unknown stage never reaches the document.
		if code := productionWriteRefused(t, author, "unknown model", "llm:set-stage-model",
			[]any{map[string]any{"stage": "drafting", "modelId": "ci-no-such-model"}}); code != "Error: LLM_STAGE_MODEL_UNAVAILABLE" {
			t.Fatalf("an unsaved model was not refused with LLM_STAGE_MODEL_UNAVAILABLE: %s", code)
		}
		if code := productionWriteRefused(t, author, "unknown stage", "llm:set-stage-model",
			[]any{map[string]any{"stage": "ci-no-such-stage", "modelId": productionWriteModelID}}); code != "Error: LLM_STAGE_MODEL_REQUEST_INVALID" {
			t.Fatalf("an unknown stage was not refused with LLM_STAGE_MODEL_REQUEST_INVALID: %s", code)
		}
		if stages, err := productionWriteReadback(t, root).StageModels(); err != nil || stages["drafting"] != productionTestModelID {
			t.Fatalf("a refused stage write changed the document: %v (%v)", stages, err)
		}
	})

	t.Run("llm:delete-model", func(t *testing.T) {
		// The profile is referenced by BOTH default documents and by the
		// finalization stage. This lane does not refuse such a delete: the store
		// runs the recoverable two-file transaction that clears every reference and
		// then removes the profile, and the answer reports the cleared values. The
		// refusal that does exist is an unusable id.
		if code := productionWriteRefused(t, author, "empty id", "llm:delete-model", []any{""}); code != "Error: LLM_MODEL_INVALID" {
			t.Fatalf("an empty model id was not refused with LLM_MODEL_INVALID: %s", code)
		}
		result := productionWriteSucceeded(t, author, "referenced profile", "llm:delete-model", []any{productionWriteModelID})
		if result["defaultModelId"] != nil || result["defaultEmbeddingModelId"] != nil {
			t.Fatalf("llm:delete-model did not report the cleared default references: %v", result)
		}
		if ids := productionWriteModelIDs(t, root); containsString(ids, productionWriteModelID) {
			t.Fatalf("llm:delete-model left the profile in the committed document: %v", ids)
		}
		remaining := productionWriteModelIDs(t, root)
		if !containsString(remaining, productionTestModelID) || !containsString(remaining, productionTestEmbeddingModelID) {
			t.Fatalf("llm:delete-model removed more than the named profile: %v", remaining)
		}
		service := productionWriteReadback(t, root)
		if stored, err := service.ListPrivateModels(); err != nil || len(stored) != len(remaining) {
			t.Fatalf("a second connection still reads %d profiles (%v); want %d", len(stored), err, len(remaining))
		}
		if value, err := service.DefaultModel(); err != nil || value != nil {
			t.Fatalf("the deleted profile is still the default model: %v (%v)", value, err)
		}
		if value, err := service.DefaultEmbeddingModel(); err != nil || value != nil {
			t.Fatalf("the deleted profile is still the default embedding model: %v (%v)", value, err)
		}
		if stages, err := service.StageModels(); err != nil || stages["finalization"] != "" {
			t.Fatalf("the deleted profile is still assigned to a stage: %v (%v)", stages, err)
		}
	})

	t.Run("config:set", func(t *testing.T) {
		productionWriteSucceeded(t, author, "ui preferences", "config:set",
			[]any{map[string]any{"theme": "dark", "editorFontSize": 19, "locale": "zh-CN"}})
		document := productionWriteConfigDocument(t, root)
		if document["theme"] != "dark" || document["locale"] != "zh-CN" {
			t.Fatalf("config.json does not carry the written preferences: %v", document)
		}
		if size, ok := document["editorFontSize"].(float64); !ok || size != 19 {
			t.Fatalf("config.json does not carry the written editor font size: %v", document["editorFontSize"])
		}
		// The already bound read projects the same document, so the write is the
		// one the rest of the lane serves.
		if read := productionWriteObject(t, author, "config:get", []any{}); read["theme"] != "dark" {
			t.Fatalf("config:get does not reflect the write: %v", read)
		}
		// The deployment extension the fixture committed is never dropped by a
		// patch that does not name it.
		if _, present := document["deploymentExtension"]; !present {
			t.Fatalf("config:set dropped the committed deployment extension: %v", document)
		}
		// The two denials that keep a browser out of deployment authority.
		productionWriteTransportRefused(t, author, "config:set", []any{map[string]any{"proxy": nil}}, http.StatusForbidden, "PROXY_SERVER_ADMIN_ONLY")
		productionWriteTransportRefused(t, author, "config:set", []any{map[string]any{"stageModelIds": map[string]any{}}}, http.StatusBadRequest, "CONFIG_FIELD_DENIED")
		productionWriteTransportRefused(t, author, "config:set", []any{map[string]any{"deploymentExtension": map[string]any{"hostRoot": "/srv/private"}}}, http.StatusBadRequest, "CONFIG_FIELD_DENIED")
		if document := productionWriteConfigDocument(t, root); !reflect.DeepEqual(document["stageModelIds"], map[string]any{"drafting": productionTestModelID}) {
			t.Fatalf("a refused config:set changed the routing document: %v", document["stageModelIds"])
		}
	})

	t.Run("prompt:save-global", func(t *testing.T) {
		template := map[string]any{
			"key": productionWritePromptKey, "writingLanguage": "zh-CN", "content": productionWritePromptBody,
		}
		productionWriteSucceeded(t, author, "template", "prompt:save-global", []any{template})
		path := filepath.Join(prompts, productionWritePromptKey+".zh-CN.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("the saved template is not on disk: %v", err)
		}
		var stored map[string]any
		if err := json.Unmarshal(raw, &stored); err != nil {
			t.Fatalf("the saved template is not a JSON object: %v %s", err, raw)
		}
		if stored["key"] != productionWritePromptKey || stored["writingLanguage"] != "zh-CN" ||
			stored["content"] != productionWritePromptBody {
			t.Fatalf("the saved template does not carry the submitted content: %s", raw)
		}
		// The legacy single-file prompt for the same key is the one the save
		// replaces; the neighbouring template is untouched.
		if _, err := os.Lstat(legacyPrompt); !os.IsNotExist(err) {
			t.Fatalf("the save left the legacy prompt file behind: %v", err)
		}
		if got, err := os.ReadFile(keptPrompt); err != nil || !bytes.Equal(got, keptBytes) {
			t.Fatalf("the save touched an unrelated template (%v): %s", err, got)
		}
		// The already bound read serves what was written.
		loaded := productionWriteObject(t, author, "prompt:load-global", []any{})
		templates, _ := loaded["templates"].([]any)
		found := false
		for _, item := range templates {
			template, ok := item.(map[string]any)
			if ok && template["key"] == productionWritePromptKey && template["content"] == productionWritePromptBody {
				found = true
			}
		}
		if !found {
			t.Fatalf("prompt:load-global does not serve the written template: %v", loaded)
		}
	})

	t.Run("prompt:delete-global", func(t *testing.T) {
		productionWriteSucceeded(t, author, "template", "prompt:delete-global",
			[]any{productionWritePromptKey, "zh-CN"})
		if _, err := os.Lstat(filepath.Join(prompts, productionWritePromptKey+".zh-CN.json")); !os.IsNotExist(err) {
			t.Fatalf("prompt:delete-global left the template behind: %v", err)
		}
		// Exactly one file went: the neighbour and the directory itself stay.
		if got, err := os.ReadFile(keptPrompt); err != nil || !bytes.Equal(got, keptBytes) {
			t.Fatalf("prompt:delete-global removed an unrelated template (%v): %s", err, got)
		}
		if info, err := os.Lstat(prompts); err != nil || !info.IsDir() {
			t.Fatalf("prompt:delete-global removed the prompt directory: %v", err)
		}
	})

	t.Run("skills:uninstall-user", func(t *testing.T) {
		productionWriteSucceeded(t, author, "installed skill", "skills:uninstall-user", []any{productionWriteSkillName})
		if _, err := os.Lstat(filepath.Join(skills, productionWriteSkillName)); !os.IsNotExist(err) {
			t.Fatalf("skills:uninstall-user left the skill directory behind: %v", err)
		}
		if _, err := os.Stat(filepath.Join(skills, productionWriteKeptSkill, "SKILL.md")); err != nil {
			t.Fatalf("skills:uninstall-user removed an unrelated skill: %v", err)
		}
	})

	t.Run("author-database-unchanged", func(t *testing.T) {
		after, err := os.ReadFile(database)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(seeded, after) {
			t.Fatal("a global-config write changed the admitted author database")
		}
		if frozen["leaseId"] == nil {
			t.Fatal("the frozen session context lost its lease")
		}
	})
}

// TestProductionFSWritesStayConfinedToTheProjectRoot is the file write surface:
// one subtest per operation, the write read back through the already bound read,
// one confinement refusal per write (an absolute path, a ".." traversal and a
// path through a planted escaping symlink), one stale-lease refusal per write, and
// the proof that NOTHING was written outside the project root - the outside tree
// is snapshotted before the first attempt and compared after the last one.
func TestProductionFSWritesStayConfinedToTheProjectRoot(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	// A neighbour chapter already in the project: the write below must create a
	// NEW file and leave this one byte-identical.
	neighbour := filepath.Join(projectPath, productionWriteNeighbourPath)
	if err := os.MkdirAll(filepath.Dir(neighbour), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(neighbour, []byte(productionWriteNeighbourBody), 0600); err != nil {
		t.Fatal(err)
	}
	// A real outside tree plus the two planted links inside the project: a file
	// link and a directory link. The directory link is the escape a write must
	// never walk through; the file link is the target a write must never follow.
	outside := t.TempDir()
	outsideSecret := filepath.Join(outside, "outside-secret.txt")
	if err := os.WriteFile(outsideSecret, []byte(productionFSOutsideBody), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideSecret, filepath.Join(projectPath, "escape.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(projectPath, "escape-dir")); err != nil {
		t.Fatal(err)
	}

	cleanup, author := productionWriteLane(t, root)
	defer cleanup()
	frozen := productionWriteOpenProject(t, author, projectPath, "fs-write-open-1")
	stale := map[string]any{"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": "stale-lease"}

	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	seeded, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	outsideBefore := productionFSSnapshot(t, outside)

	t.Run("fs:mkdir", func(t *testing.T) {
		productionWriteSucceeded(t, author, "new directory", "fs:mkdir", []any{productionWriteDirPath, projectPath, frozen})
		info, err := os.Lstat(filepath.Join(projectPath, productionWriteDirPath))
		if err != nil || !info.IsDir() {
			t.Fatalf("fs:mkdir did not create the directory: %v", err)
		}
		// Read back through the already bound read.
		result := productionWriteResult(t, author, "fs:check-exists", []any{productionWriteDirPath, projectPath, frozen})
		if exists, ok := result.(bool); !ok || !exists {
			t.Fatalf("fs:check-exists does not see the created directory: %v", result)
		}
	})

	t.Run("fs:write-file", func(t *testing.T) {
		productionWriteSucceeded(t, author, "new chapter", "fs:write-file",
			[]any{productionWriteChapterPath, productionWriteChapterBody, projectPath, frozen})
		raw, err := os.ReadFile(filepath.Join(projectPath, productionWriteChapterPath))
		if err != nil {
			t.Fatalf("fs:write-file did not create the file: %v", err)
		}
		if string(raw) != productionWriteChapterBody {
			t.Fatalf("fs:write-file wrote %q; want %q", raw, productionWriteChapterBody)
		}
		read := productionWriteObject(t, author, "fs:read-file", []any{productionWriteChapterPath, projectPath, frozen})
		if read["content"] != productionWriteChapterBody {
			t.Fatalf("fs:read-file does not serve the written bytes: %v", read)
		}
	})

	t.Run("confinement/fs:mkdir", func(t *testing.T) {
		absolute := filepath.Join(outside, "made-directory")
		for _, attempt := range []struct {
			label string
			path  string
			code  string
		}{
			{"absolute path", absolute, "PROJECT_FILE_PATH_REFUSED"},
			{"traversal", "../made-directory", "PROJECT_FILE_PATH_REFUSED"},
			{"escaping symlink", filepath.Join("escape-dir", "made-directory"), "PROJECT_FILE_UNSAFE"},
		} {
			if code := productionWriteRefused(t, author, attempt.label, "fs:mkdir",
				[]any{attempt.path, projectPath, frozen}); code != attempt.code {
				t.Fatalf("fs:mkdir refused a %s with %q instead of %q", attempt.label, code, attempt.code)
			}
		}
		if _, err := os.Lstat(absolute); !os.IsNotExist(err) {
			t.Fatalf("fs:mkdir created a directory outside the project root: %v", err)
		}
	})

	t.Run("confinement/fs:write-file", func(t *testing.T) {
		absolute := filepath.Join(outside, productionWriteOutsideFile)
		for _, attempt := range []struct {
			label string
			path  string
			code  string
		}{
			{"absolute path", absolute, "PROJECT_FILE_PATH_REFUSED"},
			{"traversal", "../" + productionWriteOutsideFile, "PROJECT_FILE_PATH_REFUSED"},
			{"escaping symlink", filepath.Join("escape-dir", productionWriteOutsideFile), "PROJECT_FILE_UNSAFE"},
		} {
			if code := productionWriteRefused(t, author, attempt.label, "fs:write-file",
				[]any{attempt.path, "outside body", projectPath, frozen}); code != attempt.code {
				t.Fatalf("fs:write-file refused a %s with %q instead of %q", attempt.label, code, attempt.code)
			}
		}
		if _, err := os.Lstat(absolute); !os.IsNotExist(err) {
			t.Fatalf("fs:write-file wrote outside the project root: %v", err)
		}
		// The final component is a symlink to the outside file. A replacement here
		// replaces the LINK inside the project; it must never follow it, so the
		// outside bytes stay sealed.
		productionWriteSucceeded(t, author, "symlink name", "fs:write-file",
			[]any{"escape.md", "replaced inside the project", projectPath, frozen})
		if raw, err := os.ReadFile(outsideSecret); err != nil || string(raw) != productionFSOutsideBody {
			t.Fatalf("fs:write-file followed a symlink out of the project root (%v): %s", err, raw)
		}
	})

	t.Run("stale-lease/fs:mkdir", func(t *testing.T) {
		if code := productionWriteRefused(t, author, "stale lease", "fs:mkdir",
			[]any{"stale-directory", projectPath, stale}); code != "PROJECT_SESSION_STALE" {
			t.Fatalf("fs:mkdir accepted a stale lease: %s", code)
		}
	})

	t.Run("stale-lease/fs:write-file", func(t *testing.T) {
		if code := productionWriteRefused(t, author, "stale lease", "fs:write-file",
			[]any{"manuscript/stale-chapter.md", "stale body", projectPath, stale}); code != "PROJECT_SESSION_STALE" {
			t.Fatalf("fs:write-file accepted a stale lease: %s", code)
		}
	})

	t.Run("nothing-written-outside", func(t *testing.T) {
		// Every refusal above ran against the same trees; neither an escaping write
		// nor a stale lease may have touched them.
		if _, err := os.Lstat(filepath.Join(projectPath, "stale-directory")); !os.IsNotExist(err) {
			t.Fatalf("a refused fs:mkdir created its directory: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(projectPath, "manuscript", "stale-chapter.md")); !os.IsNotExist(err) {
			t.Fatalf("a refused fs:write-file wrote its file: %v", err)
		}
		if after := productionFSSnapshot(t, outside); !reflect.DeepEqual(outsideBefore, after) {
			t.Fatalf("the file writes changed the outside tree: %v then %v", outsideBefore, after)
		}
	})

	t.Run("author-database-unchanged", func(t *testing.T) {
		after, err := os.ReadFile(database)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(seeded, after) {
			t.Fatal("a file write changed the admitted author database")
		}
	})
}

// TestProductionWritesRefuseWithoutTheLiveWriteLease closes the lane - which
// releases the exclusive writer lease the durable runtime owns - and then asserts
// that EVERY write this file binds still refuses symbolically and leaves the
// committed generation untouched. It is the same admission
// db:save-summary-snapshot and generation:begin pass: no lease, no write.
func TestProductionWritesRefuseWithoutTheLiveWriteLease(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	cleanup, author := productionWriteLane(t, root)
	frozen := productionWriteOpenProject(t, author, projectPath, "lease-open-1")

	configBefore, err := os.ReadFile(productionCommittedConfigPath(root))
	if err != nil {
		t.Fatal(err)
	}
	modelsBefore, err := os.ReadFile(productionCommittedModelsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	// The lease is released here, exactly as it is at shutdown. The project tree is
	// captured AFTER the close, so the baseline is the tree at rest rather than
	// whatever SQLite sidecar the live handle still held.
	cleanup()
	projectBefore := productionFSSnapshot(t, projectPath)

	// The three global writes this file owns itself answer the gate's own code.
	for _, operation := range []string{
		productionModelStageSetName, productionModelDefaultSetName,
		productionModelEmbeddingSetName, productionConfigSetName,
	} {
		productionWriteTransportRefused(t, author, operation,
			productionWriteLeaseArguments(operation, frozen), http.StatusServiceUnavailable, productionFSClosedCode)
	}
	// The shipped model binders keep their own envelope: a closed store refuses
	// with its own symbolic `Error: CODE` at 200, never with a success.
	for _, operation := range []string{productionModelSaveName, productionModelDeleteName} {
		code := productionWriteRefused(t, author, "closed lane", operation, productionWriteLeaseArguments(operation, frozen))
		if !strings.HasPrefix(code, "Error: ") {
			t.Fatalf("%s refused with %q instead of the family's symbolic error", operation, code)
		}
	}
	// The prompt binder surfaces a provider failure as a transport refusal, so the
	// closed lane keeps its own symbolic code and the committed prompt tree is
	// never opened.
	for _, operation := range []string{"prompt:save-global", "prompt:delete-global", "skills:uninstall-user"} {
		productionWriteTransportRefused(t, author, operation, productionWriteLeaseArguments(operation, frozen),
			http.StatusInternalServerError, "PROMPT_SKILLS_READ_FAILED")
	}
	if code := productionWriteRefused(t, author, "closed lane", "fs:mkdir",
		[]any{"closed-directory", projectPath, frozen}); code != productionFSClosedCode {
		t.Fatalf("fs:mkdir wrote without the live lease: %s", code)
	}
	if code := productionWriteRefused(t, author, "closed lane", "fs:write-file",
		[]any{"manuscript/closed-chapter.md", "closed body", projectPath, frozen}); code != productionFSClosedCode {
		t.Fatalf("fs:write-file wrote without the live lease: %s", code)
	}

	// Nothing moved: the committed generation, the prompt tree and the author
	// database are byte-identical, and the project tree is exactly as captured.
	if after, err := os.ReadFile(productionCommittedConfigPath(root)); err != nil || !bytes.Equal(configBefore, after) {
		t.Fatalf("a write without the live lease changed config.json (%v)", err)
	}
	if after, err := os.ReadFile(productionCommittedModelsPath(root)); err != nil || !bytes.Equal(modelsBefore, after) {
		t.Fatalf("a write without the live lease changed models.json (%v)", err)
	}
	if after := productionFSSnapshot(t, projectPath); !reflect.DeepEqual(projectBefore, after) {
		t.Fatalf("a write without the live lease changed the project tree: %v then %v", projectBefore, after)
	}
	if _, err := os.Lstat(filepath.Join(productionWriteGeneration(root), "prompts")); !os.IsNotExist(err) {
		t.Fatalf("a write without the live lease created the prompt tree: %v", err)
	}
}

// productionWriteLeaseArguments is one well-formed request per bound write name,
// used by the shutdown test to prove the admission - and not the arity - is what
// refuses.
func productionWriteLeaseArguments(operation string, frozen map[string]any) []any {
	switch operation {
	case productionModelSaveName:
		return []any{map[string]any{
			"id": "ci-closed-lane-model", "provider": "custom", "protocol": "openai",
			"baseUrl": productionWriteModelBaseURL, "apiKey": productionWriteModelSecret,
		}}
	case productionModelDeleteName:
		return []any{productionTestModelID}
	case productionModelStageSetName:
		return []any{map[string]any{"stage": "drafting", "modelId": productionTestModelID}}
	case productionModelDefaultSetName, productionModelEmbeddingSetName:
		return []any{productionTestModelID}
	case productionConfigSetName:
		return []any{map[string]any{"theme": "light"}}
	case "prompt:save-global":
		return []any{map[string]any{
			"key": productionWritePromptKey, "writingLanguage": "zh-CN", "content": productionWritePromptBody,
		}}
	case "prompt:delete-global":
		return []any{productionWritePromptKey, "zh-CN"}
	case "skills:uninstall-user":
		return []any{productionWriteSkillName}
	}
	return []any{frozen}
}

// containsString reports whether the list carries the value.
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
