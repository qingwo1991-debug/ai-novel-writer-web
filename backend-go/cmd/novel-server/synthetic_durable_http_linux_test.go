//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// syntheticDurableBlueprintTitle is the authored blueprint title the durable
// lane must read back from the author's own database.
const syntheticDurableBlueprintTitle = "合成蓝图标题"

// syntheticDurableFinalChapterTitle and syntheticDurableFinalEvidence are the
// frozen finalization outbox facts db:narrative-thread-event-confirm must bind
// to: the evidence only resolves through the outbox prose, so both live in one
// seeded row instead of being constants a stub could echo back.
const (
	syntheticDurableFinalChapterTitle = "第二章 合成定稿"
	syntheticDurableFinalEvidence     = "定稿证据"
)

// TestSyntheticDurableHTTPOpenSaveCloseReopenPersists drives the real browser
// project lifecycle protocol over the durable Go author runtime. It opens the
// one real synthetic project, rejects a stale frozen context before any write,
// saves a new projectName plus novelConfig, reads the live frozen context, and
// confirms the committed value survives db:close and a fresh project:open.
// It also asserts the three families wired beside the project lifecycle:
// config/startup/model settings read back from the lane's private root,
// db:plot-tree-clear blanking a real stored snapshot, and
// db:narrative-thread-event-confirm binding evidence to frozen outbox prose.
func TestSyntheticDurableHTTPOpenSaveCloseReopenPersists(t *testing.T) {
	oracle := os.Getenv("GO_SQLITE_CONTRACT")
	if oracle == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original schema oracle required")
		}
		t.Skip("GitHub original schema oracle required")
	}
	contractFile, err := os.Open(oracle)
	if err != nil {
		t.Fatal(err)
	}
	contract, parseErr := admission.ParseContract(contractFile)
	closeErr := contractFile.Close()
	if parseErr != nil || closeErr != nil {
		t.Fatal(parseErr, closeErr)
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	durableDir := filepath.Join(root, "durable-projects")
	if err := os.Mkdir(durableDir, 0700); err != nil {
		t.Fatal(err)
	}
	// The prompt template and installed-skill reads are served from the lane's
	// pinned private config root, so seed one real prompt and one real installed
	// skill under it. Both reads must return that content and must never echo
	// the absolute root back to the browser.
	globalRoot := filepath.Join(root, "durable-global")
	const promptKey = "durable-global-prompt"
	const skillName = "public-skill"
	const skillContent = "Synthetic installed skill content."
	promptDirectory := filepath.Join(globalRoot, "prompts")
	skillDirectory := filepath.Join(globalRoot, "skills", skillName)
	if err := os.MkdirAll(promptDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(skillDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	promptJSON := `{"key":"` + promptKey + `","content":"合成全局提示词","writingLanguage":"zh-CN"}`
	if err := os.WriteFile(filepath.Join(promptDirectory, promptKey+".zh-CN.json"), []byte(promptJSON), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDirectory, "SKILL.md"), []byte("---\nname: "+skillName+"\n---\n"+skillContent+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	handler, cleanup, err := migrationHandlerWithDurable(migrationRegistry(t), "127.0.0.1:16082", "", durableDir, syntheticDurableConfig{
		Schema: contract, Baseline: filepath.Join(filepath.Dir(oracle), syntheticBaselineDatabaseName),
		GlobalRoot: globalRoot, ModelOrigins: []string{"https://example.invalid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	projectPath := filepath.Join(durableDir, syntheticDurableProjectName)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	if _, err := os.Stat(database); err != nil {
		t.Fatalf("real synthetic project.db missing: %v", err)
	}
	// Seed the real current-schema rows this lane must serve: one working draft
	// body (the CAS target) and two characters. This runs before project:open so
	// the durable write admission fingerprints exactly the seeded database.
	draftID, originalBody := seedSyntheticDurableRows(t, database)
	// One authored blueprint row is seeded too, so the durable blueprint read
	// proves it reads the author's own database instead of a constant shape.
	seedSyntheticDurableBlueprint(t, database)
	// Two author-manuscript import runs are seeded with their frozen chapters and
	// sources, so the bound import-run bookkeeping advances real rows instead of
	// inventing them.
	seedSyntheticDurableImportRuns(t, database)
	bootRequest := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	bootRequest.Header.Set("X-Novel-User", "synthetic-author")
	boot := httptest.NewRecorder()
	handler.ServeHTTP(boot, bootRequest)
	if boot.Code != http.StatusOK || len(boot.Result().Cookies()) != 1 {
		t.Fatal(boot.Code, boot.Body.String())
	}
	var bootWire struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(boot.Body.Bytes(), &bootWire); err != nil || bootWire.CSRFToken == "" {
		t.Fatal(err, boot.Body.String())
	}
	cookie := boot.Result().Cookies()[0]
	const tab = "client_1234567890"
	rpc := func(operation string, args []any) *httptest.ResponseRecorder {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"args": args})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/operations/"+operation, bytes.NewReader(payload))
		request.Header.Set("X-Novel-User", "synthetic-author")
		request.Header.Set("X-Client-Id", tab)
		request.Header.Set("X-CSRF-Token", bootWire.CSRFToken)
		request.Header.Set("Origin", "http://127.0.0.1:16082")
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	// config:get, config:set, startup:get-state, startup:appearance-ack and the
	// three llm model names are served from the lane's pinned private global
	// root, so they are exercised before any project is opened: none of them
	// takes a project context. Values are read back both over the real handler
	// and from the private files they commit.
	const durableModelID = "durable-model"
	const durableModelKey = "sk-durable-private-key"
	configRead := func() map[string]any {
		t.Helper()
		response := rpc("config:get", []any{})
		var wire struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK || wire.Result == nil {
			t.Fatalf("config:get decode failed: %d %s %v", response.Code, response.Body.String(), err)
		}
		if strings.Contains(response.Body.String(), globalRoot) {
			t.Fatalf("config:get leaked the private root: %s", response.Body.String())
		}
		return wire.Result
	}
	initialConfig := configRead()
	if initialConfig["theme"] != "dark" || initialConfig["editorFontSize"] != float64(16) {
		t.Fatalf("config:get did not return the original defaults: %v", initialConfig)
	}
	configured := rpc("config:set", []any{map[string]any{"theme": "light", "editorFontSize": 18}})
	var configuredWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(configured.Body.Bytes(), &configuredWire); err != nil || configured.Code != http.StatusOK || configuredWire.Result["success"] != true {
		t.Fatalf("config:set failed: %d %s %v", configured.Code, configured.Body.String(), err)
	}
	if stored := configRead(); stored["theme"] != "light" || stored["editorFontSize"] != float64(18) {
		t.Fatalf("config:set was not persisted: %v", stored)
	}
	if raw, err := os.ReadFile(filepath.Join(globalRoot, "config.json")); err != nil || !bytes.Contains(raw, []byte(`"light"`)) {
		t.Fatalf("config:set did not commit config.json: %v", err)
	}
	// A proxy field is refused by presence (403), a non-public field is refused
	// before the write (400), and neither refusal changes the committed config.
	if proxySet := rpc("config:set", []any{map[string]any{"proxy": map[string]any{"enabled": false}}}); proxySet.Code != http.StatusForbidden || !strings.Contains(proxySet.Body.String(), "PROXY_SERVER_ADMIN_ONLY") {
		t.Fatalf("config:set accepted a proxy field: %d %s", proxySet.Code, proxySet.Body.String())
	}
	if deniedSet := rpc("config:set", []any{map[string]any{"binding": "0.0.0.0"}}); deniedSet.Code != http.StatusBadRequest || !strings.Contains(deniedSet.Body.String(), "CONFIG_FIELD_DENIED") {
		t.Fatalf("config:set accepted a private field: %d %s", deniedSet.Code, deniedSet.Body.String())
	}
	if stored := configRead(); stored["theme"] != "light" {
		t.Fatalf("a refused config:set changed the stored config: %v", stored)
	}
	startupState := func() map[string]any {
		t.Helper()
		response := rpc("startup:get-state", []any{})
		var wire struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK || wire.Result == nil {
			t.Fatalf("startup:get-state decode failed: %d %s %v", response.Code, response.Body.String(), err)
		}
		return wire.Result
	}
	readyState := startupState()
	if readyState["state"] != "ready" || readyState["globalGeneration"] != "synthetic-model-config-v1" {
		t.Fatalf("startup:get-state mismatch: %v", readyState)
	}
	if _, present := readyState["appearanceRevisionFloor"]; present {
		t.Fatalf("startup:get-state reported an unacknowledged appearance floor: %v", readyState)
	}
	appearanceAck := func(args map[string]any) (*httptest.ResponseRecorder, bool) {
		t.Helper()
		response := rpc("startup:appearance-ack", []any{args})
		var wire struct {
			Result bool `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK {
			t.Fatalf("startup:appearance-ack decode failed: %d %s %v", response.Code, response.Body.String(), err)
		}
		return response, wire.Result
	}
	if _, acknowledged := appearanceAck(map[string]any{
		"storageKey": "ai-novel-writer-appearance-v3", "profileRevision": 3,
		"globalGeneration": "synthetic-model-config-v1",
	}); !acknowledged {
		t.Fatal("startup:appearance-ack refused the frozen generation")
	}
	if ackState := startupState(); ackState["appearanceRevisionFloor"] != float64(3) {
		t.Fatalf("startup:appearance-ack was not retained: %v", ackState)
	}
	// An acknowledgement for another generation is a false result at 200 and
	// leaves the retained floor exactly where it was.
	if _, acknowledged := appearanceAck(map[string]any{
		"storageKey": "ai-novel-writer-appearance-v3", "profileRevision": 4,
		"globalGeneration": "another-generation",
	}); acknowledged {
		t.Fatal("startup:appearance-ack accepted another generation")
	}
	if ackState := startupState(); ackState["appearanceRevisionFloor"] != float64(3) {
		t.Fatalf("a refused appearance acknowledgement changed the floor: %v", ackState)
	}
	modelsList := func() []map[string]any {
		t.Helper()
		response := rpc("llm:list-models", []any{})
		var wire struct {
			Result []map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK {
			t.Fatalf("llm:list-models decode failed: %d %s %v", response.Code, response.Body.String(), err)
		}
		if strings.Contains(response.Body.String(), globalRoot) || strings.Contains(response.Body.String(), durableModelKey) {
			t.Fatalf("llm:list-models leaked private configuration: %s", response.Body.String())
		}
		return wire.Result
	}
	if emptyModels := modelsList(); len(emptyModels) != 0 {
		t.Fatalf("llm:list-models should start empty: %v", emptyModels)
	}
	savedModel := rpc("llm:save-model", []any{map[string]any{
		"id": durableModelID, "provider": "custom", "protocol": "openai",
		"baseUrl": "https://example.invalid/v1", "modelName": "durable-story",
		"temperature": 0.5, "maxTokens": 1024, "purposes": []any{"write"},
		"apiKey": durableModelKey,
	}})
	var savedModelWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(savedModel.Body.Bytes(), &savedModelWire); err != nil || savedModel.Code != http.StatusOK || savedModelWire.Result["success"] != true {
		t.Fatalf("llm:save-model failed: %d %s %v", savedModel.Code, savedModel.Body.String(), err)
	}
	savedModels := modelsList()
	if len(savedModels) != 1 || savedModels[0]["id"] != durableModelID || savedModels[0]["hasCredential"] != true || savedModels[0]["apiKey"] != "" {
		t.Fatalf("llm:list-models did not return the saved profile: %v", savedModels)
	}
	if raw, err := os.ReadFile(filepath.Join(globalRoot, "models.json")); err != nil || !bytes.Contains(raw, []byte(durableModelID)) {
		t.Fatalf("llm:save-model did not commit models.json: %v", err)
	}
	// A default that points at the profile makes the deletion observable in the
	// other private file: llm:delete-model must clear that reference.
	setDefault := rpc("config:set", []any{map[string]any{"defaultModelId": durableModelID}})
	if err := json.Unmarshal(setDefault.Body.Bytes(), &configuredWire); err != nil || setDefault.Code != http.StatusOK || configuredWire.Result["success"] != true {
		t.Fatalf("config:set defaultModelId failed: %d %s %v", setDefault.Code, setDefault.Body.String(), err)
	}
	if pointed := configRead(); pointed["defaultModelId"] != durableModelID {
		t.Fatalf("config:set did not persist defaultModelId: %v", pointed)
	}
	deletedModel := rpc("llm:delete-model", []any{durableModelID})
	var deletedModelWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(deletedModel.Body.Bytes(), &deletedModelWire); err != nil || deletedModel.Code != http.StatusOK || deletedModelWire.Result["success"] != true {
		t.Fatalf("llm:delete-model failed: %d %s %v", deletedModel.Code, deletedModel.Body.String(), err)
	}
	if deletedModelWire.Result["defaultModelId"] != nil {
		t.Fatalf("llm:delete-model reported a surviving default: %v", deletedModelWire.Result)
	}
	if remaining := modelsList(); len(remaining) != 0 {
		t.Fatalf("llm:delete-model left the profile in the store: %v", remaining)
	}
	if raw, err := os.ReadFile(filepath.Join(globalRoot, "models.json")); err != nil || bytes.Contains(raw, []byte(durableModelID)) {
		t.Fatalf("llm:delete-model did not commit models.json: %v", err)
	}
	if cleared := configRead(); cleared["defaultModelId"] != nil {
		t.Fatalf("llm:delete-model did not clear the default reference: %v", cleared)
	}
	// Saving a model is the author's declaration of its endpoint, so a base URL
	// outside the startup policy is committed (and the production lane then
	// rebuilds its dispatch policy from that commit) instead of being refused.
	// The save must succeed and persist without echoing the credential.
	deniedModel := rpc("llm:save-model", []any{map[string]any{
		"id": "denied-model", "provider": "custom", "protocol": "openai",
		"baseUrl": "https://denied.example/v1", "modelName": "denied",
		"apiKey": "sk-durable-denied-key",
	}})
	if deniedModel.Code != http.StatusOK || !strings.Contains(deniedModel.Body.String(), `"success":true`) || strings.Contains(deniedModel.Body.String(), "sk-durable-denied-key") {
		t.Fatalf("llm:save-model refused a committed endpoint: %d %s", deniedModel.Code, deniedModel.Body.String())
	}
	if raw, err := os.ReadFile(filepath.Join(globalRoot, "models.json")); err != nil || !bytes.Contains(raw, []byte("denied-model")) {
		t.Fatalf("a committed model save did not persist a profile: %v", err)
	}
	// The six model-routing preference names are bound on this lane by
	// BindModelRouting: each reads or rewrites config.json alone, so every
	// effect below is asserted back both over the handler and from the private
	// file it commits. None of them is project scoped, so none appends a frozen
	// context argument.
	routingMap := func(operation string, args []any) map[string]any {
		t.Helper()
		response := rpc(operation, args)
		var wire struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK || wire.Result == nil {
			t.Fatalf("%s decode failed: %d %s %v", operation, response.Code, response.Body.String(), err)
		}
		if strings.Contains(response.Body.String(), globalRoot) || strings.Contains(response.Body.String(), durableModelKey) {
			t.Fatalf("%s leaked private configuration: %s", operation, response.Body.String())
		}
		return wire.Result
	}
	routingValue := func(operation string, args []any) any {
		t.Helper()
		response := rpc(operation, args)
		if response.Code != http.StatusOK {
			t.Fatalf("%s failed: %d %s", operation, response.Code, response.Body.String())
		}
		var wire struct {
			Result any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
			t.Fatalf("%s decode failed: %s %v", operation, response.Body.String(), err)
		}
		return wire.Result
	}
	if stages := routingMap("llm:get-stage-models", []any{}); len(stages) != 0 {
		t.Fatalf("llm:get-stage-models did not start empty: %v", stages)
	}
	if value := routingValue("llm:get-default-model", []any{}); value != nil {
		t.Fatalf("llm:get-default-model returned a default on a bare root: %v", value)
	}
	if value := routingValue("llm:get-default-embedding-model", []any{}); value != nil {
		t.Fatalf("llm:get-default-embedding-model returned a default on a bare root: %v", value)
	}
	if extra := rpc("llm:get-default-model", []any{"durable-extra-argument"}); extra.Code != http.StatusBadRequest || !strings.Contains(extra.Body.String(), "INVALID_ARGUMENTS") {
		t.Fatalf("llm:get-default-model accepted an argument: %d %s", extra.Code, extra.Body.String())
	}
	// Only a saved profile that carries the generation purpose can be routed to
	// a stage, exactly like availableGenerationModelId in the original: an
	// unknown id and a saved non-generation profile are both unavailable.
	const routingModelID = "durable-routing-model"
	const routingModelKey = "sk-durable-routing-key"
	const routingEmbeddingID = "durable-embedding-model"
	if saved := routingMap("llm:save-model", []any{map[string]any{
		"id": routingModelID, "provider": "custom", "protocol": "openai",
		"baseUrl": "https://example.invalid/v1", "modelName": "routing-story",
		"temperature": 0.5, "maxTokens": 1024, "purposes": []any{"generation"},
		"apiKey": routingModelKey,
	}}); saved["success"] != true {
		t.Fatalf("llm:save-model did not accept a generation profile: %v", saved)
	}
	if saved := routingMap("llm:save-model", []any{map[string]any{
		"id": routingEmbeddingID, "provider": "custom", "protocol": "openai",
		"baseUrl": "https://example.invalid/v1", "modelName": "routing-embedding",
		"temperature": 0.5, "maxTokens": 1024, "purposes": []any{"embedding"},
		"apiKey": "sk-durable-embedding-key",
	}}); saved["success"] != true {
		t.Fatalf("llm:save-model did not accept an embedding profile: %v", saved)
	}
	for name, candidate := range map[string]string{"unknown": "durable-unknown-model", "non-generation": routingEmbeddingID} {
		unavailable := routingMap("llm:set-stage-model", []any{map[string]any{"stage": "drafting", "modelId": candidate}})
		if unavailable["success"] != false || unavailable["error"] != "Error: LLM_STAGE_MODEL_UNAVAILABLE" {
			t.Fatalf("llm:set-stage-model accepted a %s model: %v", name, unavailable)
		}
	}
	if stages := routingMap("llm:get-stage-models", []any{}); len(stages) != 0 {
		t.Fatalf("a refused stage assignment changed the stored stages: %v", stages)
	}
	// The positive assignment is reported by the write, read back by the read,
	// and found in the private file it commits.
	assigned := routingMap("llm:set-stage-model", []any{map[string]any{"stage": "drafting", "modelId": routingModelID}})
	if assigned["success"] != true {
		t.Fatalf("llm:set-stage-model refused a saved generation profile: %v", assigned)
	}
	if stages, ok := assigned["stageModelIds"].(map[string]any); !ok || stages["drafting"] != routingModelID {
		t.Fatalf("llm:set-stage-model did not report the assignment: %v", assigned)
	}
	if stages := routingMap("llm:get-stage-models", []any{}); stages["drafting"] != routingModelID {
		t.Fatalf("llm:get-stage-models did not read the assignment back: %v", stages)
	}
	if raw, err := os.ReadFile(filepath.Join(globalRoot, "config.json")); err != nil || !bytes.Contains(raw, []byte(`"drafting": "`+routingModelID+`"`)) {
		t.Fatalf("llm:set-stage-model did not commit stageModelIds: %v %s", err, raw)
	}
	// A present null modelId clears the assignment; an absent one is refused
	// above, because the original reads a missing key as undefined.
	if cleared := routingMap("llm:set-stage-model", []any{map[string]any{"stage": "drafting", "modelId": nil}}); cleared["success"] != true {
		t.Fatalf("llm:set-stage-model refused a null modelId: %v", cleared)
	}
	if stages := routingMap("llm:get-stage-models", []any{}); len(stages) != 0 {
		t.Fatalf("llm:set-stage-model did not clear the stage: %v", stages)
	}
	if raw, err := os.ReadFile(filepath.Join(globalRoot, "config.json")); err != nil || bytes.Contains(raw, []byte(`"drafting"`)) {
		t.Fatalf("llm:set-stage-model left the assignment on disk: %v %s", err, raw)
	}
	for name, request := range map[string]any{
		"unknown stage":      map[string]any{"stage": "outline", "modelId": routingModelID},
		"missing modelId":    map[string]any{"stage": "drafting"},
		"non-string modelId": map[string]any{"stage": "drafting", "modelId": float64(7)},
		"non-object request": "drafting",
	} {
		invalid := routingMap("llm:set-stage-model", []any{request})
		if invalid["success"] != false || invalid["error"] != "Error: LLM_STAGE_MODEL_REQUEST_INVALID" {
			t.Fatalf("llm:set-stage-model accepted a %s request: %v", name, invalid)
		}
	}
	// Both default ids are stored verbatim and cleared by a null value, and the
	// generation default is visible through config:get on the same document.
	if set := routingMap("llm:set-default-model", []any{routingModelID}); set["success"] != true {
		t.Fatalf("llm:set-default-model failed: %v", set)
	}
	if value := routingValue("llm:get-default-model", []any{}); value != routingModelID {
		t.Fatalf("llm:get-default-model did not read the default back: %v", value)
	}
	if pointed := configRead(); pointed["defaultModelId"] != routingModelID {
		t.Fatalf("llm:set-default-model did not commit config.json: %v", pointed)
	}
	if clearedDefault := routingMap("llm:set-default-model", []any{nil}); clearedDefault["success"] != true {
		t.Fatalf("llm:set-default-model refused a null default: %v", clearedDefault)
	}
	if value := routingValue("llm:get-default-model", []any{}); value != nil {
		t.Fatalf("llm:set-default-model did not clear the default: %v", value)
	}
	if set := routingMap("llm:set-default-embedding-model", []any{routingEmbeddingID}); set["success"] != true {
		t.Fatalf("llm:set-default-embedding-model failed: %v", set)
	}
	if value := routingValue("llm:get-default-embedding-model", []any{}); value != routingEmbeddingID {
		t.Fatalf("llm:get-default-embedding-model did not read the default back: %v", value)
	}
	if raw, err := os.ReadFile(filepath.Join(globalRoot, "config.json")); err != nil || !bytes.Contains(raw, []byte(routingEmbeddingID)) {
		t.Fatalf("llm:set-default-embedding-model did not commit config.json: %v %s", err, raw)
	}
	if clearedEmbedding := routingMap("llm:set-default-embedding-model", []any{nil}); clearedEmbedding["success"] != true {
		t.Fatalf("llm:set-default-embedding-model refused a null default: %v", clearedEmbedding)
	}
	if value := routingValue("llm:get-default-embedding-model", []any{}); value != nil {
		t.Fatalf("llm:set-default-embedding-model did not clear the default: %v", value)
	}
	// The seven remaining llm names stay unbound on this lane. llm:generate,
	// llm:generate-stream and llm:test-connection only take effect through a
	// live provider round trip, llm:discover-models only through an outbound
	// /models request, llm:cancel only against a stream this lane cannot start,
	// and the two execution-lease names only through an in-flight lease registry
	// no bound operation here can populate. Faking any of them would mean
	// inventing a provider, so they keep failing closed with the honest
	// migration code.
	for _, unboundModel := range []string{
		"llm:begin-execution-lease", "llm:close-execution-lease", "llm:cancel",
		"llm:discover-models", "llm:generate", "llm:generate-stream", "llm:test-connection",
	} {
		unboundRec := rpc(unboundModel, []any{map[string]any{"modelId": routingModelID}})
		if unboundRec.Code != http.StatusNotImplemented || !strings.Contains(unboundRec.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("llm name %s was bound on the durable lane: %d %s", unboundModel, unboundRec.Code, unboundRec.Body.String())
		}
	}
	// The eight mcp names stay unbound on this lane. BindMCP needs a pristine
	// *mcp.Manager plus a host-captured administrator predicate
	// (transport/mcp.go:26), and this lane owns neither: it has no trusted MCP
	// config path, no runtime directory and no host enable grant, so building a
	// manager here would mean stubbing one that can never load a config or spawn
	// a server. The registry exposes all eight, so the 501 is the durable
	// binding, not a 404.
	for _, unboundMCP := range []string{
		"mcp:load-config", "mcp:connect", "mcp:disconnect", "mcp:disconnect-all",
		"mcp:list-tools", "mcp:list-resources", "mcp:call-tool", "mcp:get-servers-status",
	} {
		mcpRec := rpc(unboundMCP, []any{})
		if mcpRec.Code != http.StatusNotImplemented || !strings.Contains(mcpRec.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("mcp name %s was bound on the durable lane: %d %s", unboundMCP, mcpRec.Code, mcpRec.Body.String())
		}
	}

	// project:open returns the durable Open DTO backed by the author's own DB.
	opened := rpc("project:open", []any{projectPath, "durable-open-1", nil})
	var openWire struct {
		Result struct {
			Success           bool           `json:"success"`
			Project           map[string]any `json:"project"`
			RequestToken      string         `json:"requestToken"`
			ActiveProjectPath *string        `json:"activeProjectPath"`
			DatabaseRestored  bool           `json:"databaseRestored"`
			DBReady           bool           `json:"dbReady"`
		} `json:"result"`
	}
	if err := json.Unmarshal(opened.Body.Bytes(), &openWire); err != nil || opened.Code != http.StatusOK {
		t.Fatal("open decode failed", opened.Code, opened.Body.String(), err)
	}
	if !openWire.Result.Success || openWire.Result.Project == nil || openWire.Result.RequestToken != "durable-open-1" || !openWire.Result.DBReady || !openWire.Result.DatabaseRestored || openWire.Result.ActiveProjectPath == nil || *openWire.Result.ActiveProjectPath != projectPath {
		t.Fatal("open DTO malformed", opened.Body.String())
	}
	id, _ := openWire.Result.Project["id"].(string)
	name, _ := openWire.Result.Project["name"].(string)
	gotPath, _ := openWire.Result.Project["path"].(string)
	lease, _ := openWire.Result.Project["sessionLease"].(string)
	if id != syntheticDurableProjectID || gotPath != projectPath || name != "Synthetic durable novel" || lease == "" {
		t.Fatal("open project identity malformed", opened.Body.String())
	}
	expected := map[string]any{"projectId": id, "leaseId": lease, "projectPath": projectPath}
	// Editor reads run over the same durable runtime: project-core reflects the
	// author's own database right after the open admission.
	seededCore := rpc("db:project-core-get", []any{projectPath, expected})
	if seededCore.Code != http.StatusOK || !json.Valid(seededCore.Body.Bytes()) || !bytes.Contains(seededCore.Body.Bytes(), []byte(`"projectName":"Synthetic durable novel"`)) {
		t.Fatal("durable project-core read failed", seededCore.Code, seededCore.Body.String())
	}
	// The live frozen context matches the Open DTO exactly.
	contextRec := rpc("project:get-runtime-context", []any{})
	var contextWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(contextRec.Body.Bytes(), &contextWire); err != nil || contextRec.Code != http.StatusOK {
		t.Fatal("runtime context decode failed", contextRec.Code, contextRec.Body.String(), err)
	}
	if contextWire.Result["projectId"] != id || contextWire.Result["leaseId"] != lease || contextWire.Result["projectPath"] != projectPath {
		t.Fatal("frozen context mismatch", contextRec.Body.String())
	}
	// Character reads run directly on the admitted author database under the
	// same frozen cookie/tab/lease fence: both the full characters query and the
	// roster read return real rows written into this durable project.
	characterAll := rpc("db:character-get-all", []any{projectPath, expected})
	if characterAll.Code != http.StatusOK || !bytes.Contains(characterAll.Body.Bytes(), []byte(`"name":"甲"`)) {
		t.Fatal("durable character-get-all failed", characterAll.Code, characterAll.Body.String())
	}
	characterRoster := rpc("db:character-roster-read", []any{projectPath, expected})
	if characterRoster.Code != http.StatusOK || !bytes.Contains(characterRoster.Body.Bytes(), []byte(`"name":"乙"`)) || !bytes.Contains(characterRoster.Body.Bytes(), []byte(`"currentDerivedFields"`)) {
		t.Fatal("durable character-roster-read failed", characterRoster.Code, characterRoster.Body.String())
	}
	// db:character-roster-commit is bound on this lane by
	// BindCharacterRosterCommit: store.RosterAuthor runs the original author
	// pipeline (identity commit, prose and dynamic-state writes, projection
	// refresh, roster-meta bump and the readback gate) on the admitted author
	// database, under the same frozen cookie/tab/lease fence as every other
	// durable write. The lanes that serve only character reads bind nothing here,
	// so their 501 stays.
	var rosterRevision, rosterIdentityRevision int64
	durableQuery(t, database, "SELECT revision FROM character_roster_meta WHERE id='main'", &rosterRevision)
	durableQuery(t, database, "SELECT revision FROM character_identity_meta WHERE id='main'", &rosterIdentityRevision)
	rosterEntry := func(characterID, name, role string) map[string]any {
		return map[string]any{"characterId": characterID, "name": name, "role": role,
			"gender": "", "age": "", "appearance": "", "personality": "", "background": "",
			"abilities": "", "motivation": "", "arc": "", "notes": "",
			"relationships": []any{}}
	}
	const committedName = "甲改"
	const rosterOperation = "synthetic-durable-roster-commit-1"
	// A roster write is a business result at HTTP 200, exactly like the other
	// durable writes: {success:true,receipt} here and {success:false,error} for the
	// refusals below. A malformed envelope would be a 200 with neither shape, so
	// the decode fails loudly instead of guessing.
	rosterWrite := func(request map[string]any) map[string]any {
		t.Helper()
		response := rpc("db:character-roster-commit", []any{request, projectPath, expected})
		var wire struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK || wire.Result == nil {
			t.Fatalf("durable db:character-roster-commit decode failed: %d %s %v", response.Code, response.Body.String(), err)
		}
		if strings.Contains(response.Body.String(), projectPath) || strings.Contains(response.Body.String(), durableDir) {
			t.Fatalf("durable db:character-roster-commit leaked a filesystem path: %s", response.Body.String())
		}
		return wire.Result
	}
	committedRoster := rosterWrite(map[string]any{
		"intent": "manual_edit", "schemaVersion": 1, "operationId": rosterOperation,
		"expectedRevision": rosterRevision, "expectedIdentityRevision": rosterIdentityRevision,
		"entries": []any{rosterEntry("c-durable", committedName, "protagonist"),
			rosterEntry("c-durable-2", "乙", "supporting")},
	})
	if committedRoster["success"] != true || committedRoster["error"] != nil {
		t.Fatalf("durable db:character-roster-commit reported failure: %v", committedRoster)
	}
	rosterReceipt, _ := committedRoster["receipt"].(map[string]any)
	if rosterReceipt == nil || rosterReceipt["operationId"] != rosterOperation || rosterReceipt["idempotent"] != false ||
		rosterReceipt["revision"] != float64(rosterRevision+1) || rosterReceipt["payloadHash"] == nil {
		t.Fatalf("durable roster commit receipt malformed: %v", committedRoster)
	}
	rosterSnapshot, _ := rosterReceipt["snapshot"].(map[string]any)
	rosterSnapshotEntries, _ := rosterSnapshot["entries"].([]any)
	if rosterSnapshot == nil || rosterSnapshot["status"] != "ready" || len(rosterSnapshotEntries) != 2 {
		t.Fatalf("durable roster commit receipt snapshot malformed: %v", rosterReceipt)
	}
	if rosterCreated, _ := rosterReceipt["created"].([]any); len(rosterCreated) != 0 {
		t.Fatalf("a manual edit reported a created character: %v", rosterCreated)
	}
	// The commit is proved by the author's own rows, not only by its receipt: the
	// edited name, both meta revisions and the refreshed projection.
	var committedStoredName, committedState, committedArch string
	var committedIdentityRevision, committedRosterRevision int64
	durableQuery(t, database, "SELECT name FROM characters WHERE character_id='c-durable'", &committedStoredName)
	durableQuery(t, database, "SELECT revision FROM character_identity_meta WHERE id='main'", &committedIdentityRevision)
	durableQuery(t, database, "SELECT revision FROM character_roster_meta WHERE id='main'", &committedRosterRevision)
	durableQuery(t, database, "SELECT migration_state FROM character_roster_meta WHERE id='main'", &committedState)
	durableQuery(t, database, "SELECT characters_arch FROM project_core WHERE id='main'", &committedArch)
	if committedStoredName != committedName {
		t.Fatalf("durable roster commit did not persist the edited name: %q", committedStoredName)
	}
	if committedIdentityRevision != rosterIdentityRevision+1 || committedRosterRevision != rosterRevision+1 || committedState != "ready" {
		t.Fatalf("durable roster commit did not bump identity and roster meta: %d %d %q",
			committedIdentityRevision, committedRosterRevision, committedState)
	}
	if !strings.Contains(committedArch, committedName) {
		t.Fatalf("durable roster commit did not refresh the projection: %q", committedArch)
	}
	// The already bound read has to agree with the rows the commit wrote.
	rosterAfterCommit := rpc("db:character-roster-read", []any{projectPath, expected})
	if rosterAfterCommit.Code != http.StatusOK ||
		!bytes.Contains(rosterAfterCommit.Body.Bytes(), []byte(`"name":"`+committedName+`"`)) ||
		!bytes.Contains(rosterAfterCommit.Body.Bytes(), []byte(`"status":"ready"`)) {
		t.Fatal("durable character-roster-read did not reflect the committed roster", rosterAfterCommit.Code, rosterAfterCommit.Body.String())
	}
	// Two refusals, both at HTTP 200 as business results, and neither may write: a
	// stale roster revision is refused after the idempotency lookup, and a
	// duplicate character ID is refused while the request is normalized, before a
	// single statement runs.
	for _, refusal := range []struct {
		name    string
		request map[string]any
		code    string
	}{
		{"stale roster revision", map[string]any{
			"intent": "manual_edit", "schemaVersion": 1, "operationId": "synthetic-durable-roster-stale",
			"expectedRevision": rosterRevision, "expectedIdentityRevision": rosterIdentityRevision,
			"entries": []any{rosterEntry("c-durable", "不得写入", "protagonist"),
				rosterEntry("c-durable-2", "乙", "supporting")},
		}, "CHARACTER_ID_REVISION_CONFLICT"},
		{"duplicate character id", map[string]any{
			"intent": "manual_edit", "schemaVersion": 1, "operationId": "synthetic-durable-roster-duplicate",
			"expectedRevision": committedRosterRevision, "expectedIdentityRevision": committedIdentityRevision,
			"entries": []any{rosterEntry("c-durable", "不得写入", "protagonist"),
				rosterEntry("c-durable", "也不写入", "supporting")},
		}, "CHARACTER_ID_DUPLICATE"},
	} {
		refused := rosterWrite(refusal.request)
		if refused["success"] != false || refused["error"] != "Error: "+refusal.code {
			t.Fatalf("%s was not reported as a refusal: %v", refusal.name, refused)
		}
	}
	var refusedName, refusedState, refusedArch string
	var refusedIdentity, refusedRoster int64
	durableQuery(t, database, "SELECT name FROM characters WHERE character_id='c-durable'", &refusedName)
	durableQuery(t, database, "SELECT revision FROM character_identity_meta WHERE id='main'", &refusedIdentity)
	durableQuery(t, database, "SELECT revision FROM character_roster_meta WHERE id='main'", &refusedRoster)
	durableQuery(t, database, "SELECT migration_state FROM character_roster_meta WHERE id='main'", &refusedState)
	durableQuery(t, database, "SELECT characters_arch FROM project_core WHERE id='main'", &refusedArch)
	if refusedName != committedName || refusedIdentity != committedIdentityRevision ||
		refusedRoster != committedRosterRevision || refusedState != committedState || refusedArch != committedArch {
		t.Fatalf("a refused roster commit wrote: %q %q %d %d %q", refusedName, refusedState, refusedIdentity, refusedRoster, refusedArch)
	}
	// The four editor-inline names stay unbound on this lane. They are not
	// store writes and not plain reads either: the original binds each one to
	// the main generation owner (electron/controllers/generation-controller.ts:248-251
	// register 'editor-inline:begin' / 'read-recovery' / 'execute' / 'cancel',
	// every one arity 1 with a single request object, the handle carried as
	// request.handle for the last three), and that owner's editor admission,
	// frozen author-draft context (UTF-16 selection offsets over unsaved
	// manuscript text, electron/services/editor-inline-generation.ts:20-43) and
	// model dispatch have no Go port: internal/generationowner/binding.go:37-44
	// has no editor-inline source kind at all, so store.GenerationLedger cannot
	// open such a run, and no store method begins, executes, reads back or
	// cancels one. Binding them here would mean inventing the whole owner
	// pipeline in a transport file, so all four keep failing closed. The
	// registry exposes them (web/operations.json:188-191), so the 501 is the
	// durable binding, not a 404, and the refusals write nothing.
	for _, unboundEditor := range []struct {
		name string
		args []any
	}{
		{"editor-inline:begin", []any{map[string]any{
			"input": map[string]any{
				"action": "instruct", "documentText": "原始正文", "from": 0, "to": 4,
				"selectedText": "原始正文", "instruction": "改成第一人称",
			},
			"modelId": "synthetic-editor-model", "uiActionNonce": "synthetic-editor-nonce",
		}}},
		{"editor-inline:read-recovery", []any{map[string]any{"handle": map[string]any{
			"projectId": id, "epoch": lease, "rootActionId": "synthetic-editor-root-1", "runId": "synthetic-editor-run-1",
		}}}},
		{"editor-inline:execute", []any{map[string]any{"handle": map[string]any{
			"projectId": id, "epoch": lease, "rootActionId": "synthetic-editor-root-1", "runId": "synthetic-editor-run-1",
		}}}},
		{"editor-inline:cancel", []any{map[string]any{"handle": map[string]any{
			"projectId": id, "epoch": lease, "rootActionId": "synthetic-editor-root-1", "runId": "synthetic-editor-run-1",
		}}}},
	} {
		editorRec := rpc(unboundEditor.name, unboundEditor.args)
		if editorRec.Code != http.StatusNotImplemented || !strings.Contains(editorRec.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("editor-inline name %s was bound on the durable lane: %d %s", unboundEditor.name, editorRec.Code, editorRec.Body.String())
		}
		if strings.Contains(editorRec.Body.String(), projectPath) || strings.Contains(editorRec.Body.String(), durableDir) {
			t.Fatalf("refused %s leaked a filesystem path: %s", unboundEditor.name, editorRec.Body.String())
		}
	}
	// No refusal reached the generation ledger: the editor-inline owner is the
	// only writer of these rows and it never ran, so the run tables stay empty
	// if this freshly created project carries them at all.
	var editorLedgerTables int64
	durableQuery(t, database, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='generation_runs'", &editorLedgerTables)
	if editorLedgerTables != 0 {
		var editorRuns int64
		durableQuery(t, database, "SELECT COUNT(*) FROM generation_runs", &editorRuns)
		if editorRuns != 0 {
			t.Fatalf("a refused editor-inline name persisted %d generation run(s)", editorRuns)
		}
	}
	// The five post-process receipt and LLM ledger reads are served from the
	// same admitted author database under the same frozen lease. Absence is the
	// absent shape (null run, false aggregate) or an empty list, never an error.
	statsRec := rpc("db:get-llm-stats", []any{projectPath, expected})
	var statsWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(statsRec.Body.Bytes(), &statsWire); err != nil || statsRec.Code != http.StatusOK {
		t.Fatal("durable llm stats decode failed", statsRec.Code, statsRec.Body.String(), err)
	}
	for _, key := range []string{"totalCalls", "successfulCalls", "failedCalls", "knownUsageCalls", "totalTokens", "totalPromptTokens", "totalCompletionTokens"} {
		if _, ok := statsWire.Result[key]; !ok {
			t.Fatalf("durable llm stats missing %q: %s", key, statsRec.Body.String())
		}
	}
	if _, ok := statsWire.Result["totalCalls"].(float64); !ok {
		t.Fatal("durable llm stats totalCalls is not a number", statsRec.Body.String())
	}
	// An absent run and a zero history limit both resolve to a real empty list.
	for _, emptyRead := range []struct {
		name string
		args []any
	}{
		{"db:get-llm-history", []any{0, projectPath, expected}},
		{"db:post-process-get-steps", []any{"synthetic-durable-absent-run", projectPath, expected}},
	} {
		emptyRec := rpc(emptyRead.name, emptyRead.args)
		var emptyWire struct {
			Result []any `json:"result"`
		}
		if err := json.Unmarshal(emptyRec.Body.Bytes(), &emptyWire); err != nil || emptyRec.Code != http.StatusOK || emptyWire.Result == nil || len(emptyWire.Result) != 0 {
			t.Fatalf("durable %s should be an empty list: %d %s", emptyRead.name, emptyRec.Code, emptyRec.Body.String())
		}
	}
	for _, absentRead := range []struct {
		name string
		args []any
		want string
	}{
		{"db:post-process-get-latest-run", []any{"arch_extract", "synthetic-durable-absent-source", projectPath, expected}, "null"},
		{"db:post-process-is-all-passed", []any{"arch_extract", "synthetic-durable-absent-source", projectPath, expected}, "false"},
	} {
		absentRec := rpc(absentRead.name, absentRead.args)
		var absentWire struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(absentRec.Body.Bytes(), &absentWire); err != nil || absentRec.Code != http.StatusOK || string(absentWire.Result) != absentRead.want {
			t.Fatalf("durable %s absent shape mismatch: %d %s", absentRead.name, absentRec.Code, absentRec.Body.String())
		}
	}
	// The three narrative thread and consistency exemption reads are served
	// from the same admitted author database under the same frozen lease. The
	// synthetic project has no planned thread and no exemption, so both list
	// reads return a real empty list, never an error.
	for _, emptyRead := range []struct {
		name string
		args []any
	}{
		{"db:narrative-thread-list", []any{projectPath, expected}},
		{"db:narrative-thread-list-relevant", []any{map[string]any{"title": "开场钩子", "chapterNumber": 1, "characters": []any{"甲"}}, projectPath, expected}},
		{"db:consistency-exemption-list", []any{projectPath, expected}},
	} {
		narrativeRec := rpc(emptyRead.name, emptyRead.args)
		var narrativeWire struct {
			Result []any `json:"result"`
		}
		if err := json.Unmarshal(narrativeRec.Body.Bytes(), &narrativeWire); err != nil || narrativeRec.Code != http.StatusOK || narrativeWire.Result == nil || len(narrativeWire.Result) != 0 {
			t.Fatalf("durable %s should be an empty list: %d %s", emptyRead.name, narrativeRec.Code, narrativeRec.Body.String())
		}
	}
	// The three blueprint and character-sync reads are served from the same
	// admitted author database under the same frozen lease. db:blueprint-get
	// returns the seeded chapter object; both character-sync reads keep the
	// absent shape the original returns (an empty list and null) because this
	// project has no committed blueprint range, never an error.
	blueprintRec := rpc("db:blueprint-get", []any{1, projectPath, expected})
	var blueprintWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(blueprintRec.Body.Bytes(), &blueprintWire); err != nil || blueprintRec.Code != http.StatusOK {
		t.Fatal("durable blueprint-get decode failed", blueprintRec.Code, blueprintRec.Body.String(), err)
	}
	if blueprintWire.Result == nil || blueprintWire.Result["title"] != syntheticDurableBlueprintTitle || blueprintWire.Result["chapterNumber"] != float64(1) {
		t.Fatal("durable blueprint-get did not return the seeded chapter", blueprintRec.Body.String())
	}
	pendingRec := rpc("db:blueprint-character-sync-list-pending", []any{projectPath, expected})
	var pendingWire struct {
		Result []any `json:"result"`
	}
	if err := json.Unmarshal(pendingRec.Body.Bytes(), &pendingWire); err != nil || pendingRec.Code != http.StatusOK || pendingWire.Result == nil || len(pendingWire.Result) != 0 {
		t.Fatal("durable character-sync-list-pending should be an empty list", pendingRec.Code, pendingRec.Body.String(), err)
	}
	syncRec := rpc("db:blueprint-character-sync-get", []any{"synthetic-durable-absent-sync", projectPath, expected})
	var syncWire struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(syncRec.Body.Bytes(), &syncWire); err != nil || syncRec.Code != http.StatusOK || string(syncWire.Result) != "null" {
		t.Fatal("durable character-sync-get absent shape mismatch", syncRec.Code, syncRec.Body.String(), err)
	}
	// The single plot tree read is served from the same admitted author database
	// under the same frozen lease, and it is SELECT-only. This project has no
	// stored snapshot, so the read returns the original's shape with a present
	// null snapshot (never an error) over its real source facts.
	plotRec := rpc("db:plot-tree-read", []any{projectPath, expected})
	var plotWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(plotRec.Body.Bytes(), &plotWire); err != nil || plotRec.Code != http.StatusOK {
		t.Fatal("durable plot-tree-read decode failed", plotRec.Code, plotRec.Body.String(), err)
	}
	plot := plotWire.Result
	if plot == nil || plot["writingLanguage"] != "zh-CN" {
		t.Fatal("durable plot-tree-read facts malformed", plotRec.Body.String())
	}
	if revision, _ := plot["sourceRevision"].(string); len(revision) != 64 {
		t.Fatal("durable plot-tree-read sourceRevision malformed", plotRec.Body.String())
	}
	// The seeded chapter-1 blueprint is a real source fact, read back from the
	// author's own database instead of a constant shape.
	plotBlueprints, _ := plot["blueprints"].([]any)
	if len(plotBlueprints) != 1 {
		t.Fatal("durable plot-tree-read did not return the seeded blueprint", plotRec.Body.String())
	}
	if seededPlotBlueprint, _ := plotBlueprints[0].(map[string]any); seededPlotBlueprint == nil || seededPlotBlueprint["title"] != syntheticDurableBlueprintTitle || seededPlotBlueprint["chapterNumber"] != float64(1) {
		t.Fatal("durable plot-tree-read blueprint malformed", plotRec.Body.String())
	}
	// No finalized chapter and no narrative thread exist in this project, so
	// both source lists are real empty lists.
	for _, key := range []string{"finalizedChapters", "narrativeThreads"} {
		list, ok := plot[key].([]any)
		if !ok || len(list) != 0 {
			t.Fatalf("durable plot-tree-read %s should be an empty list: %s", key, plotRec.Body.String())
		}
	}
	if synopsis, _ := plot["synopsis"].(map[string]any); synopsis == nil {
		t.Fatal("durable plot-tree-read missing the synopsis object", plotRec.Body.String())
	} else if _, ok := synopsis["content"]; !ok {
		t.Fatal("durable plot-tree-read synopsis malformed", plotRec.Body.String())
	}
	// An absent snapshot keeps the original's shape: the key is present and
	// null, and the invalid-snapshot flag the store adds for unreadable text is
	// not set for this empty project.
	snapshot, ok := plot["snapshot"]
	if !ok || snapshot != nil {
		t.Fatal("durable plot-tree-read absent snapshot shape mismatch", plotRec.Body.String())
	}
	if _, ok := plot["storedSnapshotInvalid"]; ok {
		t.Fatal("durable plot-tree-read flagged an empty project snapshot as invalid", plotRec.Body.String())
	}
	if strings.Contains(plotRec.Body.String(), projectPath) {
		t.Fatal("durable plot-tree-read leaked the project path", plotRec.Body.String())
	}
	// db:plot-tree-save stays unbound on this lane: the original refuses it
	// unconditionally (plottree.go:22 returns GENERATION_GRAPH_ADMISSION_REQUIRED)
	// and there is no generation graph admission here, so the registry-exposed
	// name keeps failing closed at 501, not 404. db:plot-tree-clear is a plain
	// one-statement mutation and is bound, so it is exercised further down
	// against a real stored snapshot instead of being probed here.
	for _, unboundWrite := range []struct {
		name string
		args []any
	}{
		{"db:plot-tree-save", []any{map[string]any{"version": 1, "tracks": []any{}}, "synthetic-source-revision", projectPath, expected}},
	} {
		plotWriteRec := rpc(unboundWrite.name, unboundWrite.args)
		if plotWriteRec.Code != http.StatusNotImplemented || !strings.Contains(plotWriteRec.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("mutating plot tree name %s was bound on the durable lane: %d %s", unboundWrite.name, plotWriteRec.Code, plotWriteRec.Body.String())
		}
	}
	// The refused save left the author database untouched: no snapshot exists.
	if keptPlot := rpc("db:plot-tree-read", []any{projectPath, expected}); keptPlot.Code != http.StatusOK || !strings.Contains(keptPlot.Body.String(), `"snapshot":null`) {
		t.Fatal("unbound plot tree mutation changed the author database", keptPlot.Code, keptPlot.Body.String())
	}
	// The four mutating blueprint names BindBlueprintWrites binds (upsert,
	// upsert-many, delete, commit-range) write the same admitted author database
	// through the same borrow-and-refresh write path as the draft CAS and the
	// post-process and narrative writes. Every write is performed over the real
	// HTTP handler and read back through the already bound db:blueprint-get, so
	// what is asserted is the persisted state, not only the write receipt.
	// db:blueprint-update-notes is deliberately NOT bound: the original refuses
	// it unconditionally (electron/controllers/db-controller.ts:636), so the 501
	// below is the durable binding mirroring that refusal.
	blueprintWrite := func(operation string, args []any) map[string]any {
		t.Helper()
		response := rpc(operation, args)
		var wire struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK || wire.Result == nil {
			t.Fatalf("durable %s decode failed: %d %s %v", operation, response.Code, response.Body.String(), err)
		}
		if strings.Contains(response.Body.String(), projectPath) || strings.Contains(response.Body.String(), durableDir) {
			t.Fatalf("durable %s leaked a filesystem path: %s", operation, response.Body.String())
		}
		return wire.Result
	}
	// A failed write is a business result at HTTP 200 carrying the symbolic
	// code, never a success and never a bare transport error.
	requireBlueprintRefusal := func(operation string, args []any, want string) {
		t.Helper()
		refused := blueprintWrite(operation, args)
		if refused["success"] != false || refused["error"] != want {
			t.Fatalf("durable %s was not reported as a refusal: %v", operation, refused)
		}
	}
	// blueprintChapter reads one chapter back through the already bound read; a
	// nil row is the original's absent shape.
	blueprintChapter := func(chapter float64) map[string]any {
		t.Helper()
		response := rpc("db:blueprint-get", []any{chapter, projectPath, expected})
		var wire struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK {
			t.Fatalf("durable blueprint-get %v decode failed: %d %s %v", chapter, response.Code, response.Body.String(), err)
		}
		if strings.Contains(response.Body.String(), projectPath) || strings.Contains(response.Body.String(), durableDir) {
			t.Fatalf("durable blueprint-get %v leaked a filesystem path: %s", chapter, response.Body.String())
		}
		return wire.Result
	}
	const upsertedTitle = "单章写入标题"
	upserted := blueprintWrite("db:blueprint-upsert", []any{map[string]any{
		"chapterNumber": 2, "title": upsertedTitle, "role": "main", "purpose": "单章目的",
		"keyEvents": "单章事件", "characters": []any{"甲"}, "suspenseHook": "单章悬念",
		"userGuidance": "", "notes": "", "notesUpdatedAt": "",
	}, projectPath, expected})
	if upserted["success"] != true || upserted["error"] != nil {
		t.Fatalf("durable blueprint-upsert reported failure: %v", upserted)
	}
	if row := blueprintChapter(2); row == nil || row["title"] != upsertedTitle ||
		row["chapterNumber"] != float64(2) || row["role"] != "main" {
		t.Fatalf("durable blueprint-upsert was not persisted: %v", row)
	}
	// The batch write commits both chapters, and both are readable individually.
	manyWritten := blueprintWrite("db:blueprint-upsert-many", []any{[]any{
		map[string]any{"chapterNumber": 3, "title": "批量第三章", "role": "main", "characters": []any{"甲"}},
		map[string]any{"chapterNumber": 4, "title": "批量第四章", "role": "supporting", "characters": []any{"乙"}},
	}, projectPath, expected})
	if manyWritten["success"] != true || manyWritten["error"] != nil {
		t.Fatalf("durable blueprint-upsert-many reported failure: %v", manyWritten)
	}
	if row := blueprintChapter(3); row == nil || row["title"] != "批量第三章" {
		t.Fatalf("durable blueprint-upsert-many was not persisted: %v", row)
	}
	if row := blueprintChapter(4); row == nil || row["title"] != "批量第四章" || row["role"] != "supporting" {
		t.Fatalf("durable blueprint-upsert-many was not persisted: %v", row)
	}
	// The delete removes exactly the named chapter: it is gone from the read and
	// the chapters it did not name are untouched.
	deleted := blueprintWrite("db:blueprint-delete", []any{3, projectPath, expected})
	if deleted["success"] != true || deleted["error"] != nil {
		t.Fatalf("durable blueprint-delete reported failure: %v", deleted)
	}
	if row := blueprintChapter(3); row != nil {
		t.Fatalf("deleted chapter blueprint is still readable: %v", row)
	}
	if row := blueprintChapter(1); row == nil || row["title"] != syntheticDurableBlueprintTitle || row["notes"] != "作者摘要" {
		t.Fatalf("durable blueprint-delete changed an unnamed chapter: %v", row)
	}
	// db:blueprint-commit-range is one store call that owns the whole range
	// commit, including the character-sync operation row and the read-back proof
	// of every committed column, so the durable lane binds it as-is. The input
	// carries full blueprint objects because the store re-reads what it
	// committed and refuses on any mismatch.
	const rangeTitle = "范围提交第五章"
	committedRange := blueprintWrite("db:blueprint-commit-range", []any{map[string]any{
		"mode": "replace-range", "operationId": "durable-range-1",
		"startChapter": 5, "endChapter": 5,
		"blueprints": []any{map[string]any{
			"chapterNumber": 5, "title": rangeTitle, "role": "main", "purpose": "范围目的",
			"keyEvents": "范围事件", "characters": []any{"乙"}, "suspenseHook": "范围悬念",
			"userGuidance": "", "notes": "", "notesUpdatedAt": "",
		}},
	}, projectPath, expected})
	if committedRange["success"] != true || committedRange["error"] != nil {
		t.Fatalf("durable blueprint-commit-range reported failure: %v", committedRange)
	}
	receipt, _ := committedRange["receipt"].(map[string]any)
	if receipt == nil || receipt["operationId"] != "durable-range-1" || receipt["mode"] != "replace-range" || receipt["idempotent"] != false {
		t.Fatalf("durable blueprint-commit-range receipt malformed: %v", committedRange)
	}
	if chapters, _ := receipt["chapterNumbers"].([]any); len(chapters) != 1 || chapters[0] != float64(5) {
		t.Fatalf("durable blueprint-commit-range chapter numbers malformed: %v", committedRange)
	}
	if syncOperation, _ := receipt["characterSyncOperation"].(map[string]any); syncOperation == nil ||
		syncOperation["operationId"] != "blueprint-sync-durable-range-1" || syncOperation["status"] != "pending" {
		t.Fatalf("durable blueprint-commit-range did not record the character sync operation: %v", committedRange)
	}
	if row := blueprintChapter(5); row == nil || row["title"] != rangeTitle {
		t.Fatalf("durable blueprint-commit-range was not persisted: %v", row)
	}
	// A rejected write must not report success and must leave no partial row.
	// The batch is atomic: the store rejects the null chapter title on its second
	// insert and rolls the whole transaction back, so the first item is not
	// committed either.
	requireBlueprintRefusal("db:blueprint-upsert-many", []any{[]any{
		map[string]any{"chapterNumber": 6, "title": "回滚第六章"},
		map[string]any{"chapterNumber": 7, "title": nil},
	}, projectPath, expected}, "Error: BLUEPRINT_OPERATION_FAILED")
	for _, chapter := range []float64{6, 7} {
		if row := blueprintChapter(chapter); row != nil {
			t.Fatalf("refused blueprint-upsert-many left a partial row for chapter %v: %v", chapter, row)
		}
	}
	// A chapter key the store cannot bind is refused symbolically and deletes
	// nothing, and a range whose mode the store rejects is refused before any
	// write, leaving the committed chapter exactly as it was.
	requireBlueprintRefusal("db:blueprint-delete", []any{true, projectPath, expected}, "Error: BLUEPRINT_OPERATION_FAILED")
	requireBlueprintRefusal("db:blueprint-commit-range", []any{map[string]any{
		"mode": "append-by-chapter", "operationId": "durable-range-refused",
		"startChapter": 5, "endChapter": 5, "blueprints": []any{map[string]any{"chapterNumber": 5}},
	}, projectPath, expected}, "Error: BLUEPRINT_COMMIT_MODE_INVALID")
	if row := blueprintChapter(1); row == nil || row["title"] != syntheticDurableBlueprintTitle {
		t.Fatalf("refused blueprint writes changed the seeded chapter: %v", row)
	}
	if row := blueprintChapter(5); row == nil || row["title"] != rangeTitle {
		t.Fatalf("refused blueprint writes changed the committed range: %v", row)
	}
	// db:blueprint-update-notes stays unbound on this lane: the registry exposes
	// it, so the 501 is the durable binding, not a 404, and the refusal rewrites
	// nothing.
	notesRec := rpc("db:blueprint-update-notes", []any{1, map[string]any{"notes": "绑定"}, projectPath, expected})
	if notesRec.Code != http.StatusNotImplemented || !strings.Contains(notesRec.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("db:blueprint-update-notes was bound on the durable lane: %d %s", notesRec.Code, notesRec.Body.String())
	}
	if row := blueprintChapter(1); row == nil || row["notes"] != "作者摘要" {
		t.Fatalf("refused blueprint-update-notes changed the seeded chapter: %v", row)
	}
	// Exactly the committed writes are present: the seeded chapter, the single
	// upsert, the surviving batch chapter and the committed range. Nothing a
	// refused call touched exists.
	var blueprintTableRows int64
	durableQuery(t, database, "SELECT COUNT(*) FROM blueprints", &blueprintTableRows)
	if blueprintTableRows != 4 {
		t.Fatalf("durable blueprint writes left the wrong row count: %d", blueprintTableRows)
	}
	// The prompt template and installed-skill reads are served from the lane's
	// pinned private config root. Both names take no argument at all, so the
	// caller sends `{"args":[]}`; the seeded root proves the reads return the
	// real files instead of only a non-501, and that no host path is echoed.
	promptsRec := rpc("prompt:load-global", []any{})
	var promptsWire struct {
		Result struct {
			Templates   []map[string]any `json:"templates"`
			Diagnostics []map[string]any `json:"diagnostics"`
		} `json:"result"`
	}
	if err := json.Unmarshal(promptsRec.Body.Bytes(), &promptsWire); err != nil || promptsRec.Code != http.StatusOK {
		t.Fatal("durable prompt:load-global decode failed", promptsRec.Code, promptsRec.Body.String(), err)
	}
	if len(promptsWire.Result.Templates) != 1 || len(promptsWire.Result.Diagnostics) != 0 {
		t.Fatal("durable prompt:load-global returned the wrong receipts", promptsRec.Body.String())
	}
	if key, _ := promptsWire.Result.Templates[0]["key"].(string); key != promptKey || !strings.Contains(promptsRec.Body.String(), "合成全局提示词") {
		t.Fatal("durable prompt:load-global did not read the private root", promptsRec.Body.String())
	}
	if strings.Contains(promptsRec.Body.String(), globalRoot) {
		t.Fatal("durable prompt:load-global leaked the private root", promptsRec.Body.String())
	}
	skillsRec := rpc("skills:list-user", []any{})
	var skillsWire struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(skillsRec.Body.Bytes(), &skillsWire); err != nil || skillsRec.Code != http.StatusOK || len(skillsWire.Result) != 1 {
		t.Fatal("durable skills:list-user did not return the installed skill", skillsRec.Code, skillsRec.Body.String(), err)
	}
	skillWire := skillsWire.Result[0]
	if skillWire["name"] != skillName || skillWire["baseDir"] != "managed://skills/"+skillName ||
		skillWire["filePath"] != "managed://skills/"+skillName+"/SKILL.md" || !strings.Contains(skillsRec.Body.String(), skillContent) {
		t.Fatal("durable skills:list-user wire mismatch", skillsRec.Body.String())
	}
	if strings.Contains(skillsRec.Body.String(), globalRoot) {
		t.Fatal("durable skills:list-user leaked the private root", skillsRec.Body.String())
	}
	// The three mutating names in the prompt and installed-skill family are
	// bound by BindPromptSkillsWrites over that same pinned root. Each one is
	// asserted on the state the already bound reads return and, where the store
	// commits a file, on the file in the private root itself: a `success: true`
	// receipt alone would not prove the write reached the root.
	const authoredPromptKey = "durable-authored-prompt"
	prompts := func() []map[string]any {
		t.Helper()
		response := rpc("prompt:load-global", []any{})
		var wire struct {
			Result struct {
				Templates []map[string]any `json:"templates"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK {
			t.Fatalf("prompt:load-global readback failed: %d %s %v", response.Code, response.Body.String(), err)
		}
		return wire.Result.Templates
	}
	savedPrompt := rpc("prompt:save-global", []any{map[string]any{
		"key": authoredPromptKey, "content": "作者保存的全局提示词", "writingLanguage": "zh-CN",
	}})
	var promptWriteWire struct {
		Result struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal(savedPrompt.Body.Bytes(), &promptWriteWire); err != nil ||
		savedPrompt.Code != http.StatusOK || !promptWriteWire.Result.Success || promptWriteWire.Result.Error != "" {
		t.Fatalf("prompt:save-global failed: %d %s %v", savedPrompt.Code, savedPrompt.Body.String(), err)
	}
	if templates := prompts(); len(templates) != 2 {
		t.Fatalf("prompt:save-global did not add the authored template: %v", templates)
	}
	authoredFile := filepath.Join(promptDirectory, authoredPromptKey+".zh-CN.json")
	if raw, err := os.ReadFile(authoredFile); err != nil || !bytes.Contains(raw, []byte("作者保存的全局提示词")) {
		t.Fatalf("prompt:save-global did not commit the prompt file: %v", err)
	}
	// A non-object template is refused before the root is borrowed: these names
	// take no frozen context, so a malformed argument is a plain 400 and can
	// never be mistaken for a context mismatch.
	if badShape := rpc("prompt:save-global", []any{"not-a-template"}); badShape.Code != http.StatusBadRequest ||
		!strings.Contains(badShape.Body.String(), "INVALID_ARGUMENTS") {
		t.Fatalf("prompt:save-global accepted a non-object template: %d %s", badShape.Code, badShape.Body.String())
	}
	promptDeleted := rpc("prompt:delete-global", []any{authoredPromptKey, "zh-CN"})
	if err := json.Unmarshal(promptDeleted.Body.Bytes(), &promptWriteWire); err != nil ||
		promptDeleted.Code != http.StatusOK || !promptWriteWire.Result.Success || promptWriteWire.Result.Error != "" {
		t.Fatalf("prompt:delete-global failed: %d %s %v", promptDeleted.Code, promptDeleted.Body.String(), err)
	}
	if templates := prompts(); len(templates) != 1 || templates[0]["key"] != promptKey {
		t.Fatalf("prompt:delete-global left the wrong template set: %v", templates)
	}
	if _, err := os.Stat(authoredFile); !os.IsNotExist(err) {
		t.Fatalf("prompt:delete-global left the prompt file behind: %v", err)
	}
	// skills:uninstall-user removes the installed tree; the already bound
	// skills:list-user read and the directory itself both prove it is gone.
	uninstalled := rpc("skills:uninstall-user", []any{skillName})
	if err := json.Unmarshal(uninstalled.Body.Bytes(), &promptWriteWire); err != nil ||
		uninstalled.Code != http.StatusOK || !promptWriteWire.Result.Success || promptWriteWire.Result.Error != "" {
		t.Fatalf("skills:uninstall-user failed: %d %s %v", uninstalled.Code, uninstalled.Body.String(), err)
	}
	emptySkills := rpc("skills:list-user", []any{})
	var emptySkillsWire struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(emptySkills.Body.Bytes(), &emptySkillsWire); err != nil ||
		emptySkills.Code != http.StatusOK || len(emptySkillsWire.Result) != 0 {
		t.Fatalf("skills:uninstall-user did not remove the installed skill: %d %s %v", emptySkills.Code, emptySkills.Body.String(), err)
	}
	if _, err := os.Stat(skillDirectory); !os.IsNotExist(err) {
		t.Fatalf("skills:uninstall-user left the skill directory behind: %v", err)
	}
	// Each arity is the family's own: save-global takes exactly one argument,
	// delete-global exactly two and uninstall-user exactly one. A short call is
	// refused with INVALID_ARGUMENTS instead of being read as a frozen context.
	for _, wrongArity := range []struct {
		name string
		args []any
	}{
		{"prompt:save-global", []any{}},
		{"prompt:delete-global", []any{authoredPromptKey}},
		{"skills:uninstall-user", []any{}},
	} {
		arityRec := rpc(wrongArity.name, wrongArity.args)
		if arityRec.Code != http.StatusBadRequest || !strings.Contains(arityRec.Body.String(), "INVALID_ARGUMENTS") {
			t.Fatalf("%s accepted the wrong argument count: %d %s", wrongArity.name, arityRec.Code, arityRec.Body.String())
		}
	}
	// The ninth name in that family, db:narrative-thread-event-confirm, is bound
	// separately by BindNarrativeEventConfirm and is exercised at the end of
	// this test: store.ConfirmEvent binds the evidence to frozen finalization
	// outbox prose, so it needs a finalized chapter and its outbox row seeded
	// while no lease is admitted. The five plan and exemption writes are bound
	// by BindNarrativeWrites and are exercised further down once the draft CAS
	// has rotated the lease.
	// The four mutating names in the same family are bound by
	// BindPostHistoryWrites against the same admitted author database, so they
	// are exercised further down once the draft CAS has rotated the lease.
	// A stale frozen context is rejected before any write reaches the database.
	staleExpected := map[string]any{"projectId": id, "leaseId": "forged-lease", "projectPath": projectPath}
	stale := rpc("project:save", []any{id, map[string]any{"id": id, "sessionLease": "forged-lease", "path": projectPath, "name": "骗写"}, projectPath, staleExpected})
	var staleWire struct {
		Result struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stale.Body.Bytes(), &staleWire); err != nil || stale.Code != http.StatusOK {
		t.Fatal("stale save decode failed", stale.Code, stale.Body.String(), err)
	}
	if staleWire.Result.Success || !strings.Contains(staleWire.Result.Error, "PROJECT_SESSION_NOT_OWNED") {
		t.Fatal("stale frozen context was not rejected before the write", stale.Body.String())
	}
	if got := durableProjectName(t, database); got != "Synthetic durable novel" {
		t.Fatalf("stale save changed the author database: %q", got)
	}
	// project:save commits a new projectName to the author's own project.db.
	const savedName = "持久化新书名"
	saved := rpc("project:save", []any{id, map[string]any{
		"id": id, "sessionLease": lease, "path": projectPath, "name": savedName,
		"novelConfig": map[string]any{"writingLanguage": "zh-CN"},
	}, projectPath, expected})
	var saveWire struct {
		Result struct {
			Success bool           `json:"success"`
			Project map[string]any `json:"project"`
		} `json:"result"`
	}
	if err := json.Unmarshal(saved.Body.Bytes(), &saveWire); err != nil || saved.Code != http.StatusOK {
		t.Fatal("save decode failed", saved.Code, saved.Body.String(), err)
	}
	if !saveWire.Result.Success {
		t.Fatal("save reported failure", saved.Body.String())
	}
	if value, _ := saveWire.Result.Project["projectName"].(string); value != savedName {
		t.Fatalf("save result missing committed name: %q", saveWire.Result.Project)
	}
	if got := durableProjectName(t, database); got != savedName {
		t.Fatalf("save did not persist to the author database: %q", got)
	}
	// project:update-config commits a novelConfig field through the same write
	// path and the change is reflected in the author database.
	updated := rpc("project:update-config", []any{id, map[string]any{"novelConfig": map[string]any{"genre": "urban"}}, projectPath, expected})
	if err := json.Unmarshal(updated.Body.Bytes(), &saveWire); err != nil || updated.Code != http.StatusOK || !saveWire.Result.Success {
		t.Fatal("update-config failed", updated.Code, updated.Body.String(), err)
	}
	if got := durableProjectGenre(t, database); got != "urban" {
		t.Fatalf("update-config did not persist: %q", got)
	}
	// The durable lane's shared read surface serves editor reads from the same
	// author DB: the overview and project-core reads reflect the committed save
	// and config update before db:close.
	overviewRec := rpc("project:overview-current", []any{expected})
	var overviewWire struct {
		Result struct {
			State     string `json:"state"`
			ProjectID string `json:"projectId"`
			Revision  string `json:"revision"`
			Name      string `json:"name"`
		} `json:"result"`
	}
	if err := json.Unmarshal(overviewRec.Body.Bytes(), &overviewWire); err != nil || overviewRec.Code != http.StatusOK {
		t.Fatal("overview decode failed", overviewRec.Code, overviewRec.Body.String(), err)
	}
	if overviewWire.Result.State != "ready" || overviewWire.Result.ProjectID != id || overviewWire.Result.Name != savedName || len(overviewWire.Result.Revision) != 64 {
		t.Fatal("durable overview mismatch", overviewRec.Body.String())
	}
	coreRec := rpc("db:project-core-get", []any{projectPath, expected})
	if coreRec.Code != http.StatusOK || !json.Valid(coreRec.Body.Bytes()) || !bytes.Contains(coreRec.Body.Bytes(), []byte(`"projectName":"`+savedName+`"`)) || !bytes.Contains(coreRec.Body.Bytes(), []byte(`"genre":"urban"`)) {
		t.Fatal("durable project-core read missing committed data", coreRec.Code, coreRec.Body.String())
	}
	// The two mutating project core names BindProjectCoreWrites binds
	// (db:project-core-update and db:project-core-synopsis-commit) write that
	// same admitted author database through the same borrow-and-refresh permit
	// path as the draft CAS and the other write binders. Every write is
	// performed over the real HTTP handler and read back through the already
	// bound db:project-core-get, so what is asserted is the persisted row, not
	// only the write receipt.
	coreRow := func() map[string]any {
		t.Helper()
		response := rpc("db:project-core-get", []any{projectPath, expected})
		var wire struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK || wire.Result == nil {
			t.Fatalf("durable project-core-get readback failed: %d %s %v", response.Code, response.Body.String(), err)
		}
		if strings.Contains(response.Body.String(), projectPath) || strings.Contains(response.Body.String(), durableDir) {
			t.Fatalf("durable project-core-get readback leaked a filesystem path: %s", response.Body.String())
		}
		return wire.Result
	}
	// coreWrite sends a mutation and returns the DTO itself, so a refusal can be
	// asserted instead of being fatal'd by read style decoding. The call sites
	// pass the frozen context as an element of args, exactly like rpc does.
	coreWrite := func(operation string, args []any) map[string]any {
		t.Helper()
		response := rpc(operation, args)
		var wire struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK {
			t.Fatalf("durable %s decode failed: %d %s %v", operation, response.Code, response.Body.String(), err)
		}
		if strings.Contains(response.Body.String(), projectPath) || strings.Contains(response.Body.String(), durableDir) {
			t.Fatalf("durable %s leaked a filesystem path: %s", operation, response.Body.String())
		}
		if wire.Result != nil {
			return wire.Result
		}
		var flat map[string]any
		_ = json.Unmarshal(response.Body.Bytes(), &flat)
		return flat
	}
	// A successful update is reported as success and every named field it
	// carried is what the bound read returns afterwards.
	updatedCore := coreWrite("db:project-core-update", []any{map[string]any{
		"premise": "合成前提", "worldbuilding": "合成世界观", "totalChapters": 130,
	}, projectPath, expected})
	if updatedCore["success"] != true || updatedCore["error"] != nil {
		t.Fatalf("durable project-core-update reported failure: %v", updatedCore)
	}
	if row := coreRow(); row["premise"] != "合成前提" || row["worldbuilding"] != "合成世界观" ||
		row["totalChapters"] != float64(130) || row["projectName"] != savedName {
		t.Fatalf("durable project-core-update was not persisted: %v", row)
	}
	// A refused update is all-or-nothing: the read-only charactersArch key
	// rejects the whole patch before any SQL runs, so the premise it carried is
	// not written either.
	refusedCore := coreWrite("db:project-core-update", []any{map[string]any{
		"charactersArch": nil, "premise": "不得写入",
	}, projectPath, expected})
	if refusedCore["success"] != false || refusedCore["error"] != "Error: PROJECT_CORE_CHARACTERS_ARCH_READ_ONLY" {
		t.Fatalf("durable project-core-update was not reported as a refusal: %v", refusedCore)
	}
	// The synopsis CAS is one atomic update over the target body and all eleven
	// source baselines, so the baseline is taken from the bound read itself and
	// the committed body is asserted through that same read.
	const committedSynopsis = "合成梗概：一次提交"
	committedSynopsisWrite := coreWrite("db:project-core-synopsis-commit", []any{map[string]any{
		"synopsis": committedSynopsis, "expected": coreRow(),
	}, projectPath, expected})
	if committedSynopsisWrite["success"] != true || committedSynopsisWrite["error"] != nil {
		t.Fatalf("durable project-core-synopsis-commit reported failure: %v", committedSynopsisWrite)
	}
	if row := coreRow(); row["synopsis"] != committedSynopsis {
		t.Fatalf("durable project-core-synopsis-commit was not persisted: %v", row["synopsis"])
	}
	// One stale source baseline loses the CAS: the write is reported as a
	// refusal and the committed synopsis is untouched.
	staleBaseline := coreRow()
	staleBaseline["premise"] = "过期前提"
	staleCommit := coreWrite("db:project-core-synopsis-commit", []any{map[string]any{
		"synopsis": "不得写入的梗概", "expected": staleBaseline,
	}, projectPath, expected})
	if staleCommit["success"] != false || staleCommit["error"] != "PROJECT_CORE_SYNOPSIS_CHANGED" {
		t.Fatalf("stale synopsis CAS was not reported as a refusal: %v", staleCommit)
	}
	// An owner-bearing request is refused before the borrow, even with a null
	// owner handle, so no generation owner fence is bypassed.
	ownerCommit := coreWrite("db:project-core-synopsis-commit", []any{map[string]any{
		"synopsis": "不得写入的梗概", "expected": coreRow(), "generationRunHandle": nil,
	}, projectPath, expected})
	if ownerCommit["success"] != false || ownerCommit["error"] != "Error: GENERATION_OWNER_REQUIRED" {
		t.Fatalf("owner-bearing synopsis commit was not refused: %v", ownerCommit)
	}
	// A forged frozen context is refused before the borrow, so it cannot write.
	forgedCore := coreWrite("db:project-core-update", []any{map[string]any{"premise": "伪造前提"},
		projectPath, map[string]any{"projectId": id, "leaseId": "forged-lease", "projectPath": projectPath}})
	forgedCoreError, _ := forgedCore["error"].(string)
	if forgedCore["success"] != false || !strings.HasPrefix(forgedCoreError, "Error: ") || strings.Contains(forgedCoreError, projectPath) {
		t.Fatalf("forged frozen context was not refused symbolically: %v", forgedCore)
	}
	// No refused write left partial state: the bound read and the database file
	// both hold exactly the committed synopsis and the committed premise.
	if row := coreRow(); row["synopsis"] != committedSynopsis || row["premise"] != "合成前提" {
		t.Fatalf("refused project core writes changed the persisted row: %v", row)
	}
	var storedCoreSynopsis, storedCorePremise string
	durableQuery(t, database, "SELECT synopsis FROM project_core WHERE id='main'", &storedCoreSynopsis)
	durableQuery(t, database, "SELECT premise FROM project_core WHERE id='main'", &storedCorePremise)
	if storedCoreSynopsis != committedSynopsis || storedCorePremise != "合成前提" {
		t.Fatalf("project core writes persisted the wrong row: %q / %q", storedCoreSynopsis, storedCorePremise)
	}
	// The durable lane commits a draft body CAS against the author's own
	// database: the stale-baseline rejection keeps the stored content unchanged,
	// then the matched CAS persists and rotates the frozen lease in place.
	var resultWire struct {
		Result map[string]any `json:"result"`
	}
	staleCAS := rpc("db:draft-update-content", []any{draftID, "replayed", 2, projectPath, "stale draft", expected})
	if err := json.Unmarshal(staleCAS.Body.Bytes(), &resultWire); err != nil || staleCAS.Code != http.StatusOK {
		t.Fatal("stale CAS decode failed", staleCAS.Code, staleCAS.Body.String(), err)
	}
	conflict, _ := resultWire.Result["errorCode"].(string)
	if success, _ := resultWire.Result["success"].(bool); success || conflict != "DRAFT_CONTENT_CONFLICT" {
		t.Fatal("stale draft CAS was not rejected", staleCAS.Body.String())
	}
	if got := durableDraftBody(t, database, draftID); got != originalBody {
		t.Fatalf("stale draft CAS changed stored content: %q", got)
	}
	const changedBody = "durable committed draft"
	committedDraft := rpc("db:draft-update-content", []any{draftID, changedBody, 5, projectPath, originalBody, expected})
	if err := json.Unmarshal(committedDraft.Body.Bytes(), &resultWire); err != nil || committedDraft.Code != http.StatusOK {
		t.Fatal("draft CAS decode failed", committedDraft.Code, committedDraft.Body.String(), err)
	}
	if success, _ := resultWire.Result["success"].(bool); !success {
		t.Fatal("draft CAS reported failure", committedDraft.Body.String())
	}
	session, _ := resultWire.Result["projectSession"].(map[string]any)
	fresh, _ := session["leaseId"].(string)
	if fresh == "" || fresh == lease || session["projectId"] != id || session["projectPath"] != projectPath {
		t.Fatal("draft CAS did not rotate the lease in place", committedDraft.Body.String())
	}
	freshSession := map[string]any{"projectId": id, "leaseId": fresh, "projectPath": projectPath}
	// The committed body is readable through the rotated lease both over the
	// real transport and by reading the SQLite file directly.
	full := rpc("db:draft-get-full", []any{draftID, projectPath, freshSession})
	if full.Code != http.StatusOK || !bytes.Contains(full.Body.Bytes(), []byte(`"content":"`+changedBody+`"`)) {
		t.Fatal("committed draft not readable over HTTP", full.Code, full.Body.String())
	}
	if got := durableDraftBody(t, database, draftID); got != changedBody {
		t.Fatalf("committed draft not visible in direct readback: %q", got)
	}
	if got := durableDraftWordCount(t, database, draftID); got != 5 {
		t.Fatalf("committed word count not persisted: %d", got)
	}
	// The rotated lease is the only live one: the pre-commit frozen context is
	// fenced out of every further operation.
	if old := rpc("db:draft-get-full", []any{draftID, projectPath, expected}); old.Code == http.StatusOK {
		t.Fatal("pre-commit lease still readable after the CAS", old.Body.String())
	}
	// Replaying the pre-commit baseline through the rotated lease is a stale
	// revision and must be rejected without touching the stored content.
	replayed := rpc("db:draft-update-content", []any{draftID, "again", 6, projectPath, originalBody, freshSession})
	if err := json.Unmarshal(replayed.Body.Bytes(), &resultWire); err != nil || replayed.Code != http.StatusOK {
		t.Fatal("replayed CAS decode failed", replayed.Code, replayed.Body.String(), err)
	}
	conflict, _ = resultWire.Result["errorCode"].(string)
	if success, _ := resultWire.Result["success"].(bool); success || conflict != "DRAFT_CONTENT_CONFLICT" {
		t.Fatal("replayed draft CAS was not rejected", replayed.Body.String())
	}
	if got := durableDraftBody(t, database, draftID); got != changedBody {
		t.Fatalf("replayed draft CAS changed stored content: %q", got)
	}
	// The finalization outbox is bound to that same admitted author database by
	// three names: finalization:commit freezes the author-confirmed prose and the
	// outbox row in one transaction, finalization:retry re-records the pending
	// projection, and db:finalization-link-knowledge-document binds a managed
	// document identity to the frozen row. The two service names carry the frozen
	// three-key context as their own last argument (their wire declaration has no
	// separate path), and the db name carries the required project path before it.
	const finalTitle = "定稿:标题?*中文"
	const finalBody = "定稿正文🐍 café\r\n第二行 <&>\u2028"
	finalSnapshot := func(content, baseline string) map[string]any {
		return map[string]any{
			"tabId": tab, "projectPath": projectPath, "projectSession": freshSession,
			"draftId": draftID, "chapterNumber": 1, "chapterTitle": finalTitle,
			"content": content, "expectedContent": baseline, "contentRevision": 1,
		}
	}
	finalizationRow := func(column string) string {
		t.Helper()
		var value string
		durableQuery(t, database, "SELECT "+column+" FROM finalization_outbox WHERE draft_id="+
			strconv.FormatInt(draftID, 10), &value)
		return value
	}
	digest := sha256.Sum256([]byte(finalBody))
	finalDigest := hex.EncodeToString(digest[:])
	committedFinal := coreWrite("finalization:commit", []any{finalSnapshot(finalBody, changedBody), freshSession})
	if committedFinal["committed"] != true || committedFinal["success"] != false ||
		committedFinal["publicationStatus"] != "pending" ||
		committedFinal["draftId"] != float64(draftID) ||
		committedFinal["error"] != "Error: FINALIZATION_PUBLISHER_UNAVAILABLE" {
		t.Fatalf("durable finalization:commit did not freeze an unpublished projection: %v", committedFinal)
	}
	finalID, _ := committedFinal["finalizationId"].(string)
	if len(finalID) != 36 || finalID[14] != '4' || committedFinal["contentHash"] != finalDigest {
		t.Fatalf("durable finalization:commit minted the wrong frozen facts: %v", committedFinal)
	}
	// The commit is a database fact, not an echo: the draft is finalized, the
	// confirmed prose is the stored body, and the outbox row holds the frozen
	// snapshot, the hash of that snapshot, the portable target name and the
	// acknowledged publication state. No publication receipt exists, because this
	// lane owns no publisher and never claims one.
	if got := durableDraftBody(t, database, draftID); got != finalBody {
		t.Fatalf("durable finalization:commit froze a different body: %q", got)
	}
	var finalizedStatus string
	durableQuery(t, database, "SELECT status FROM drafts WHERE id="+strconv.FormatInt(draftID, 10), &finalizedStatus)
	if finalizedStatus != "finalized" {
		t.Fatalf("durable finalization:commit did not finalize the draft: %q", finalizedStatus)
	}
	if row := finalizationRow("finalization_id"); row != finalID {
		t.Fatalf("durable finalization:commit persisted a different id: %q", row)
	}
	if row := finalizationRow("chapter_title"); row != finalTitle {
		t.Fatalf("durable finalization:commit persisted a different title: %q", row)
	}
	if row := finalizationRow("content_hash"); row != finalDigest {
		t.Fatalf("durable finalization:commit persisted a different hash: %q", row)
	}
	if row := finalizationRow("content_snapshot"); row != finalBody {
		t.Fatalf("durable finalization:commit persisted a different snapshot: %q", row)
	}
	if row := finalizationRow("target_file_name"); row != "第1章 定稿_标题__中文.txt" {
		t.Fatalf("durable finalization:commit did not derive the portable target name: %q", row)
	}
	if row := finalizationRow("publication_status"); row != "pending" {
		t.Fatalf("durable finalization:commit claimed a publication state: %q", row)
	}
	if row := finalizationRow("COALESCE(last_error,'')"); row != "Error: FINALIZATION_PUBLISHER_UNAVAILABLE" {
		t.Fatalf("durable finalization:commit hid the unpublished projection: %q", row)
	}
	var finalPublished sql.NullString
	durableQuery(t, database, "SELECT published_at FROM finalization_outbox WHERE draft_id="+
		strconv.FormatInt(draftID, 10), &finalPublished)
	if finalPublished.Valid && finalPublished.String != "" {
		t.Fatalf("durable finalization:commit minted a publication receipt: %q", finalPublished.String)
	}
	// Replaying the identical frozen request returns the original identity instead
	// of minting a second commit, and a different frozen request for the same
	// draft is the service's own conflict — the bare code, not the prefixed one.
	replayedFinal := coreWrite("finalization:commit", []any{finalSnapshot(finalBody, changedBody), freshSession})
	if replayedFinal["committed"] != true || replayedFinal["finalizationId"] != finalID ||
		replayedFinal["contentHash"] != finalDigest {
		t.Fatalf("durable finalization:commit did not replay the frozen identity: %v", replayedFinal)
	}
	conflictFinal := coreWrite("finalization:commit", []any{finalSnapshot("另一份正文", changedBody), freshSession})
	if conflictFinal["success"] != false || conflictFinal["committed"] != false ||
		conflictFinal["error"] != "FINALIZATION_COMMIT_CONFLICT" {
		t.Fatalf("durable finalization:commit did not refuse a conflicting request: %v", conflictFinal)
	}
	if got := durableDraftBody(t, database, draftID); got != finalBody {
		t.Fatalf("a refused finalization changed the frozen prose: %q", got)
	}
	if row := finalizationRow("content_snapshot"); row != finalBody {
		t.Fatalf("a refused finalization changed the frozen snapshot: %q", row)
	}
	// Snapshot refusals run before the borrow, so neither a malformed snapshot nor
	// a snapshot whose own session copy disagrees with the wire context writes.
	malformedFinal := coreWrite("finalization:commit", []any{map[string]any{"tabId": tab}, freshSession})
	if malformedFinal["success"] != false || malformedFinal["committed"] != false ||
		malformedFinal["error"] != "Error: FINALIZATION_SNAPSHOT_INVALID" {
		t.Fatalf("durable finalization:commit accepted a malformed snapshot: %v", malformedFinal)
	}
	mismatchedFinal := coreWrite("finalization:commit", []any{map[string]any{
		"tabId": tab, "projectPath": projectPath,
		"projectSession": map[string]any{"projectId": id, "leaseId": "forged-lease", "projectPath": projectPath},
		"draftId":        draftID, "chapterNumber": 1, "chapterTitle": finalTitle,
		"content": finalBody, "expectedContent": changedBody, "contentRevision": 1,
	}, freshSession})
	if mismatchedFinal["success"] != false || mismatchedFinal["committed"] != false ||
		mismatchedFinal["error"] != "Error: FINALIZATION_SNAPSHOT_SESSION_MISMATCH" {
		t.Fatalf("durable finalization:commit accepted a mismatched session: %v", mismatchedFinal)
	}
	// db:finalization-link-knowledge-document stores the trimmed managed document
	// identity on the frozen row and returns the record the store now holds.
	linkedFinal := coreWrite("db:finalization-link-knowledge-document", []any{draftID, "  kb:合成文档  ", projectPath, freshSession})
	if linkedFinal["success"] != true || linkedFinal["error"] != nil {
		t.Fatalf("durable finalization link reported failure: %v", linkedFinal)
	}
	linkedRecord, _ := linkedFinal["finalization"].(map[string]any)
	if linkedRecord == nil || linkedRecord["knowledgeDocumentId"] != "kb:合成文档" ||
		linkedRecord["finalizationId"] != finalID || linkedRecord["contentHash"] != finalDigest ||
		linkedRecord["contentSnapshot"] != finalBody || linkedRecord["publicationStatus"] != "pending" ||
		linkedRecord["chapterNumber"] != float64(1) || linkedRecord["draftId"] != float64(draftID) ||
		linkedRecord["publishedAt"] != nil {
		t.Fatalf("durable finalization link returned a malformed record: %v", linkedFinal)
	}
	if row := finalizationRow("COALESCE(knowledge_document_id,'')"); row != "kb:合成文档" {
		t.Fatalf("durable finalization link did not persist the document: %q", row)
	}
	// A blank identity is refused with the repository's prefix-carrying code, an
	// unknown draft with its not-found code, and a forged lease before the store is
	// reached; none of them rewrites the linked row.
	blankDocument := coreWrite("db:finalization-link-knowledge-document", []any{draftID, "\ufeff ", projectPath, freshSession})
	if blankDocument["success"] != false || blankDocument["error"] != "Error: FINALIZATION_COMMIT_KNOWLEDGE_DOCUMENT_REQUIRED" {
		t.Fatalf("durable finalization link accepted a blank document: %v", blankDocument)
	}
	unknownDraft := coreWrite("db:finalization-link-knowledge-document", []any{int64(999999), "kb:合成文档", projectPath, freshSession})
	if unknownDraft["success"] != false || unknownDraft["error"] != "Error: FINALIZATION_COMMIT_NOT_FOUND" {
		t.Fatalf("durable finalization link accepted an unknown draft: %v", unknownDraft)
	}
	forgedLink := coreWrite("db:finalization-link-knowledge-document", []any{draftID, "kb:伪造", projectPath,
		map[string]any{"projectId": id, "leaseId": "forged-lease", "projectPath": projectPath}})
	forgedLinkError, _ := forgedLink["error"].(string)
	if forgedLink["success"] != false || !strings.HasPrefix(forgedLinkError, "Error: ") {
		t.Fatalf("durable finalization link accepted a forged lease: %v", forgedLink)
	}
	if row := finalizationRow("COALESCE(knowledge_document_id,'')"); row != "kb:合成文档" {
		t.Fatalf("a refused finalization link rewrote the document: %q", row)
	}
	// The link name requires the project path as its own argument, so a call
	// without the frozen context is refused instead of reading through it.
	shortLink := coreWrite("db:finalization-link-knowledge-document", []any{draftID, "kb:合成文档", projectPath})
	if shortLink["success"] != false || shortLink["error"] != "Error: PROJECT_CONTEXT_REQUIRED" {
		t.Fatalf("durable finalization link accepted a request without a context: %v", shortLink)
	}
	// finalization:retry re-records the pending projection for the frozen id and
	// still mints no publication receipt: the committed facts come back from the
	// outbox, never from the request.
	retriedFinal := coreWrite("finalization:retry", []any{finalID, freshSession})
	if retriedFinal["committed"] != true || retriedFinal["success"] != false ||
		retriedFinal["finalizationId"] != finalID || retriedFinal["contentHash"] != finalDigest ||
		retriedFinal["draftId"] != float64(draftID) || retriedFinal["contentRevision"] != float64(1) ||
		retriedFinal["publicationStatus"] != "pending" ||
		retriedFinal["error"] != "Error: FINALIZATION_PUBLISHER_UNAVAILABLE" {
		t.Fatalf("durable finalization:retry did not replay the frozen projection: %v", retriedFinal)
	}
	if row := finalizationRow("publication_status"); row != "pending" {
		t.Fatalf("durable finalization:retry claimed a publication state: %q", row)
	}
	if row := finalizationRow("COALESCE(last_error,'')"); row != "Error: FINALIZATION_PUBLISHER_UNAVAILABLE" {
		t.Fatalf("durable finalization:retry hid the unpublished projection: %q", row)
	}
	missingID := coreWrite("finalization:retry", []any{"", freshSession})
	if missingID["success"] != false || missingID["committed"] != false ||
		missingID["error"] != "Error: FINALIZATION_COMMIT_ID_REQUIRED" {
		t.Fatalf("durable finalization:retry accepted a missing id: %v", missingID)
	}
	unknownID := coreWrite("finalization:retry", []any{"durable-missing-finalization", freshSession})
	if unknownID["success"] != false || unknownID["committed"] != false ||
		unknownID["error"] != "FINALIZATION_COMMIT_NOT_FOUND" {
		t.Fatalf("durable finalization:retry accepted an unknown id: %v", unknownID)
	}
	// The three authoritative-export reads are served from that same admitted
	// author database. After the commit above there is exactly one finalized
	// chapter, so the continuity verdict and the export snapshot are real facts
	// read back from the committed rows, not constants: the sequence must be
	// continuous at chapter 1, and its fingerprint must be the sha256 of the
	// original's payload shape over the frozen draft's body hash.
	fingerprintPayload := `[{"draftId":` + strconv.FormatInt(draftID, 10) +
		`,"chapterNumber":1,"version":1,"bodyHash":"` + finalDigest + `"}]`
	fingerprintSum := sha256.Sum256([]byte(fingerprintPayload))
	wantFingerprint := hex.EncodeToString(fingerprintSum[:])
	authorityRec := rpc("db:draft-authority-sequence", []any{projectPath, freshSession})
	var authorityWire struct {
		Result struct {
			Status                  string  `json:"status"`
			LastChapterNumber       int64   `json:"lastChapterNumber"`
			NextChapterNumber       int64   `json:"nextChapterNumber"`
			FirstGapChapterNumber   *int64  `json:"firstGapChapterNumber"`
			DuplicateChapterNumbers []int64 `json:"duplicateChapterNumbers"`
			AuthorityFingerprint    string  `json:"authorityFingerprint"`
		} `json:"result"`
	}
	if err := json.Unmarshal(authorityRec.Body.Bytes(), &authorityWire); err != nil || authorityRec.Code != http.StatusOK {
		t.Fatal("db:draft-authority-sequence decode failed", authorityRec.Code, authorityRec.Body.String(), err)
	}
	if got := authorityWire.Result; got.Status != "continuous" || got.LastChapterNumber != 1 ||
		got.NextChapterNumber != 2 || got.FirstGapChapterNumber != nil ||
		len(got.DuplicateChapterNumbers) != 0 || got.AuthorityFingerprint != wantFingerprint {
		t.Fatalf("db:draft-authority-sequence did not read the committed chapter: %+v", got)
	}
	exportRec := rpc("db:draft-export-snapshot", []any{projectPath, freshSession})
	var exportWire struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(exportRec.Body.Bytes(), &exportWire); err != nil || exportRec.Code != http.StatusOK {
		t.Fatal("db:draft-export-snapshot decode failed", exportRec.Code, exportRec.Body.String(), err)
	}
	if len(exportWire.Result) != 1 {
		t.Fatalf("db:draft-export-snapshot did not select exactly the committed chapter: %d %s",
			len(exportWire.Result), exportRec.Body.String())
	}
	exported := exportWire.Result[0]
	if exported["draftId"] != float64(draftID) || exported["chapterNumber"] != float64(1) ||
		exported["version"] != float64(1) || exported["finalizationId"] != finalID ||
		exported["contentHash"] != finalDigest || exported["title"] != finalTitle ||
		exported["content"] != finalBody {
		t.Fatalf("db:draft-export-snapshot returned a different snapshot: %v", exported)
	}
	// The receipt the snapshot itself describes is current; a receipt with one
	// altered hash, an empty receipt and a non-array receipt are all not current
	// and are answered as a bare false instead of an error.
	exportReceipt := []any{map[string]any{"draftId": draftID, "chapterNumber": 1,
		"version": 1, "finalizationId": finalID, "contentHash": finalDigest}}
	exportCurrent := func(receipt any) bool {
		t.Helper()
		response := rpc("db:draft-export-authority-current", []any{receipt, projectPath, freshSession})
		var wire struct {
			Result bool `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK {
			t.Fatalf("db:draft-export-authority-current decode failed: %d %s %v",
				response.Code, response.Body.String(), err)
		}
		return wire.Result
	}
	if !exportCurrent(exportReceipt) {
		t.Fatal("db:draft-export-authority-current refused the receipt its own snapshot describes")
	}
	tampered := []any{map[string]any{"draftId": draftID, "chapterNumber": 1, "version": 1,
		"finalizationId": finalID, "contentHash": strings.Repeat("0", 64)}}
	if exportCurrent(tampered) || exportCurrent([]any{}) || exportCurrent("not-a-receipt") {
		t.Fatal("db:draft-export-authority-current accepted a receipt that is not current")
	}
	// The three remaining draft mutations write the same admitted author
	// database. db:draft-create ignores the requested version and allocates
	// MAX(version)+1 inside its transaction, so the stored chapter starts at 1
	// even though 3 was asked for; the body is read back through the already
	// bound db:draft-get-full.
	const authorChapter = 5
	const authorBody = "第五章作者草稿🐍"
	createdDraft := coreWrite("db:draft-create", []any{map[string]any{
		"chapterNumber": authorChapter, "version": 3, "source": "write",
		"content": authorBody, "wordCount": 4,
	}, projectPath, freshSession})
	if createdDraft["success"] != true || createdDraft["error"] != nil {
		t.Fatalf("db:draft-create reported failure: %v", createdDraft)
	}
	createdNumber, _ := createdDraft["id"].(float64)
	if createdNumber < 1 {
		t.Fatalf("db:draft-create returned no draft id: %v", createdDraft)
	}
	createdID := int64(createdNumber)
	createdFull := rpc("db:draft-get-full", []any{createdNumber, projectPath, freshSession})
	var createdFullWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(createdFull.Body.Bytes(), &createdFullWire); err != nil || createdFull.Code != http.StatusOK {
		t.Fatal("db:draft-get-full decode failed", createdFull.Code, createdFull.Body.String(), err)
	}
	if got := createdFullWire.Result; got["id"] != createdNumber ||
		got["chapterNumber"] != float64(authorChapter) || got["version"] != float64(1) ||
		got["status"] != "draft" || got["source"] != "write" ||
		got["wordCount"] != float64(4) || got["content"] != authorBody ||
		got["dependenciesStale"] != false {
		t.Fatalf("db:draft-create did not persist the requested draft: %v", got)
	}
	var createdContentID sql.NullInt64
	durableQuery(t, database, "SELECT content_id FROM drafts WHERE id="+
		strconv.FormatInt(createdID, 10), &createdContentID)
	if !createdContentID.Valid || createdContentID.Int64 < 1 {
		t.Fatalf("db:draft-create stored no content row: %v", createdContentID)
	}
	// db:draft-update-status changes the working status and the word count and
	// leaves the content alone, which the bound read proves.
	statusWrite := coreWrite("db:draft-update-status", []any{createdNumber, "reviewed", 7, projectPath, freshSession})
	if statusWrite["success"] != true || statusWrite["error"] != nil {
		t.Fatalf("db:draft-update-status reported failure: %v", statusWrite)
	}
	statusMeta := rpc("db:draft-get-meta", []any{createdNumber, projectPath, freshSession})
	var statusMetaWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(statusMeta.Body.Bytes(), &statusMetaWire); err != nil || statusMeta.Code != http.StatusOK {
		t.Fatal("db:draft-get-meta decode failed", statusMeta.Code, statusMeta.Body.String(), err)
	}
	if got := statusMetaWire.Result; got["status"] != "reviewed" || got["wordCount"] != float64(7) ||
		got["contentId"] != float64(createdContentID.Int64) {
		t.Fatalf("db:draft-update-status was not persisted: %v", got)
	}
	// A working draft may not be promoted to a receipted status, and a receipted
	// draft may not change status at all: both refusals keep the family's
	// "Error: CODE" envelope.
	promoteWrite := coreWrite("db:draft-update-status", []any{createdNumber, "finalized", nil, projectPath, freshSession})
	if promoteWrite["success"] != false || promoteWrite["error"] != "Error: DRAFT_FINALIZATION_REQUIRES_COMMIT" {
		t.Fatalf("db:draft-update-status promoted a working draft: %v", promoteWrite)
	}
	receiptWrite := coreWrite("db:draft-update-status", []any{draftID, "draft", nil, projectPath, freshSession})
	if receiptWrite["success"] != false || receiptWrite["error"] != "Error: DRAFT_FINALIZED_READ_ONLY" {
		t.Fatalf("db:draft-update-status rewrote a receipted draft: %v", receiptWrite)
	}
	// db:draft-delete answers the one receipted code as errorCode with no error
	// string at all, and the frozen draft survives.
	deleteFinalRec := rpc("db:draft-delete", []any{draftID, projectPath, freshSession})
	var deleteFinalWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(deleteFinalRec.Body.Bytes(), &deleteFinalWire); err != nil || deleteFinalRec.Code != http.StatusOK {
		t.Fatal("db:draft-delete decode failed", deleteFinalRec.Code, deleteFinalRec.Body.String(), err)
	}
	if deleteFinalWire.Result["success"] != false ||
		deleteFinalWire.Result["errorCode"] != "FINALIZED_DRAFT_DELETE_REQUIRED" ||
		strings.Contains(deleteFinalRec.Body.String(), `"error":`) {
		t.Fatalf("db:draft-delete refused a finalized draft with the wrong envelope: %s", deleteFinalRec.Body.String())
	}
	var finalizedStill int64
	durableQuery(t, database, "SELECT COUNT(*) FROM drafts WHERE id="+
		strconv.FormatInt(draftID, 10), &finalizedStill)
	if finalizedStill != 1 {
		t.Fatal("db:draft-delete removed a finalized draft")
	}
	// The working draft is deletable, and both the draft row and its content row
	// are gone: db:draft-get-meta answers null and SQLite agrees.
	deleteRec := coreWrite("db:draft-delete", []any{createdNumber, projectPath, freshSession})
	if deleteRec["success"] != true || deleteRec["error"] != nil {
		t.Fatalf("db:draft-delete reported failure: %v", deleteRec)
	}
	goneRec := rpc("db:draft-get-meta", []any{createdNumber, projectPath, freshSession})
	var goneWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(goneRec.Body.Bytes(), &goneWire); err != nil || goneRec.Code != http.StatusOK || goneWire.Result != nil {
		t.Fatalf("db:draft-delete left the draft readable: %d %s %v", goneRec.Code, goneRec.Body.String(), err)
	}
	var draftsLeft, contentsLeft int64
	durableQuery(t, database, "SELECT COUNT(*) FROM drafts WHERE id="+
		strconv.FormatInt(createdID, 10), &draftsLeft)
	durableQuery(t, database, "SELECT COUNT(*) FROM contents WHERE id="+
		strconv.FormatInt(createdContentID.Int64, 10), &contentsLeft)
	if draftsLeft != 0 || contentsLeft != 0 {
		t.Fatalf("db:draft-delete left rows behind: drafts=%d contents=%d", draftsLeft, contentsLeft)
	}
	// A malformed dependency list and a stale but well-formed one are both
	// refused before any INSERT, so neither adds a draft or a content row.
	var draftsBefore int64
	durableQuery(t, database, "SELECT COUNT(*) FROM drafts", &draftsBefore)
	invalidDep := coreWrite("db:draft-create", []any{map[string]any{
		"chapterNumber": 6, "source": "write", "content": "不得写入", "wordCount": 1,
		"sourceDependencies": []any{map[string]any{"draftId": 1, "contentHash": "short"}},
	}, projectPath, freshSession})
	if invalidDep["success"] != false || invalidDep["error"] != "Error: GENERATION_DRAFT_DEPENDENCY_INVALID" {
		t.Fatalf("db:draft-create accepted a malformed dependency: %v", invalidDep)
	}
	staleDep := coreWrite("db:draft-create", []any{map[string]any{
		"chapterNumber": 6, "source": "write", "content": "不得写入", "wordCount": 1,
		"sourceDependencies": []any{map[string]any{"draftId": 987654, "contentHash": strings.Repeat("a", 64)}},
	}, projectPath, freshSession})
	if staleDep["success"] != false || staleDep["error"] != "Error: GENERATION_DRAFT_DEPENDENCY_STALE" {
		t.Fatalf("db:draft-create accepted a stale dependency: %v", staleDep)
	}
	var draftsAfter int64
	durableQuery(t, database, "SELECT COUNT(*) FROM drafts", &draftsAfter)
	if draftsAfter != draftsBefore {
		t.Fatalf("a refused db:draft-create wrote a draft: %d -> %d", draftsBefore, draftsAfter)
	}
	// The names this lane still does not serve keep failing closed at 501. The
	// import pipeline is a separate lane's job, db:project-core-commit-generated
	// needs a generation owner, db:blueprint-clear-all and
	// db:project-clear-generated-data both retire generation runs through
	// repositories this lane does not own, db:blueprint-character-sync-complete
	// must prove a character-proposal or roster receipt this lane cannot mint,
	// db:blueprint-update-notes and db:plot-tree-save are refused unconditionally
	// by the original itself, and db:review-cycle-list-pending-rechecks needs the
	// M03 receipt proof and recheck projection that are not ported. The registry
	// exposes all of them, so the 501 is the durable binding, not a 404.
	for _, remainingDraftName := range []string{
		"db:project-core-commit-generated", "db:project-clear-generated-data",
		"db:import-global-facts-commit",
		"db:import-run-prepare-inspection", "db:import-run-author-preview",
		"db:import-run-effect-receipt-prepare", "db:import-run-effect-receipt-commit", "db:draft-import-finalized-batch",
		"db:blueprint-character-sync-complete", "db:blueprint-update-notes",
		"db:blueprint-clear-all", "db:plot-tree-save", "db:review-cycle-list-pending-rechecks",
	} {
		unboundDraft := rpc(remainingDraftName, []any{map[string]any{}, projectPath, freshSession})
		if unboundDraft.Code != http.StatusNotImplemented ||
			!strings.Contains(unboundDraft.Body.String(), "GO_OPERATION_NOT_MIGRATED") ||
			strings.Contains(unboundDraft.Body.String(), projectPath) {
			t.Fatalf("%s was bound or leaked a path on the durable lane: %d %s",
				remainingDraftName, unboundDraft.Code, unboundDraft.Body.String())
		}
	}
	// The eight cloud-backup names stay unbound: every one of them needs the
	// safeStorage credential/binding store, a deployment-supplied WebDAV dispatch,
	// portable export/restore into the author's tree, or an in-flight backup
	// registry that no bound operation in this lane can populate. They keep failing
	// closed with the honest migration code instead of a fabricated success.
	for _, cloudOperation := range []string{
		"cloud-backup:connect", "cloud-backup:view", "cloud-backup:confirm-binding",
		"cloud-backup:backup", "cloud-backup:list", "cloud-backup:restore-copy",
		"cloud-backup:cancel", "cloud-backup:clear-credential",
	} {
		refused := rpc(cloudOperation, []any{map[string]any{"cloudBookId": "synthetic-cloud-book"}, freshSession})
		if refused.Code != http.StatusNotImplemented ||
			!strings.Contains(refused.Body.String(), "GO_OPERATION_NOT_MIGRATED") ||
			strings.Contains(refused.Body.String(), projectPath) || strings.Contains(refused.Body.String(), durableDir) {
			t.Fatalf("%s was not refused as unmigrated: %d %s", cloudOperation, refused.Code, refused.Body.String())
		}
	}
	// project:save still succeeds AFTER the draft CAS with the rotated lease:
	// the durable session was not downgraded by the commit.
	const savedNameAfterCAS = "草稿后书名"
	afterCAS := rpc("project:save", []any{id, map[string]any{
		"id": id, "sessionLease": fresh, "path": projectPath, "name": savedNameAfterCAS,
	}, projectPath, freshSession})
	if err := json.Unmarshal(afterCAS.Body.Bytes(), &saveWire); err != nil || afterCAS.Code != http.StatusOK {
		t.Fatal("save after draft CAS decode failed", afterCAS.Code, afterCAS.Body.String(), err)
	}
	if !saveWire.Result.Success {
		t.Fatal("save after draft CAS reported failure", afterCAS.Body.String())
	}
	if value, _ := saveWire.Result.Project["projectName"].(string); value != savedNameAfterCAS {
		t.Fatalf("save after draft CAS missing committed name: %q", saveWire.Result.Project)
	}
	if got := durableProjectName(t, database); got != savedNameAfterCAS {
		t.Fatalf("save after draft CAS did not persist: %q", got)
	}
	// The four mutating post-process and LLM ledger names are bound to the same
	// admitted author database through the same borrow-and-refresh write path as
	// the draft CAS: one run is created, one critical step is marked ok, one
	// optional step is marked failed and one LLM call is appended. Every write
	// must report success, and every committed row is read back through the
	// already bound post-process and ledger reads.
	mutate := func(operation string, args []any) map[string]any {
		t.Helper()
		response := rpc(operation, args)
		var wire struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK || wire.Result == nil {
			t.Fatalf("durable %s decode failed: %d %s %v", operation, response.Code, response.Body.String(), err)
		}
		return wire.Result
	}
	runInput := map[string]any{
		"triggerSourceType": "arch_extract",
		"triggerSourceId":   "synthetic-durable-source",
		"sourceLabel":       "合成跑批",
		"steps": []any{
			map[string]any{"key": "knowledge", "label": "知识投影", "critical": true},
			map[string]any{"key": "optional", "label": "可选投影", "critical": false},
		},
	}
	created := mutate("db:post-process-create-run", []any{runInput, projectPath, freshSession})
	if created["success"] != true || created["error"] != nil {
		t.Fatalf("durable create-run reported failure: %v", created)
	}
	runID, _ := created["id"].(string)
	if len(runID) != 36 || runID[14] != '4' {
		t.Fatalf("durable create-run did not mint a secure v4 run id: %q", runID)
	}
	for _, write := range []struct {
		name string
		args []any
	}{
		{"db:post-process-mark-step-ok", []any{runID, "knowledge", projectPath, freshSession}},
		{"db:post-process-mark-step-failed", []any{runID, "optional", "可选步骤失败", projectPath, freshSession}},
		{"db:log-llm-call", []any{map[string]any{
			"modelId": "synthetic-durable-model", "modelName": "Synthetic durable",
			"purpose": "write", "promptTokens": 12, "completionTokens": 34,
			"totalTokens": 46, "durationMs": 7, "success": true,
		}, projectPath, freshSession}},
	} {
		result := mutate(write.name, write.args)
		if result["success"] != true || result["error"] != nil {
			t.Fatalf("durable %s reported failure: %v", write.name, result)
		}
	}
	latest := rpc("db:post-process-get-latest-run", []any{"arch_extract", "synthetic-durable-source", projectPath, freshSession})
	var latestWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(latest.Body.Bytes(), &latestWire); err != nil || latest.Code != http.StatusOK {
		t.Fatal("durable post-process-get-latest-run decode failed", latest.Code, latest.Body.String(), err)
	}
	run := latestWire.Result
	if run == nil || run["id"] != runID || run["triggerSourceId"] != "synthetic-durable-source" ||
		run["sourceLabel"] != "合成跑批" || run["allCriticalPassed"] != true {
		t.Fatal("durable post-process-get-latest-run did not return the committed run", latest.Body.String())
	}
	persistedSteps := rpc("db:post-process-get-steps", []any{runID, projectPath, freshSession})
	var stepsWire struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(persistedSteps.Body.Bytes(), &stepsWire); err != nil || persistedSteps.Code != http.StatusOK || len(stepsWire.Result) != 2 {
		t.Fatal("durable post-process-get-steps did not return the two steps", persistedSteps.Code, persistedSteps.Body.String(), err)
	}
	knowledge, optional := stepsWire.Result[0], stepsWire.Result[1]
	if knowledge["stepKey"] != "knowledge" || knowledge["ok"] != true || knowledge["errorMsg"] != "" ||
		knowledge["attemptCount"] != float64(1) || knowledge["completedAt"] == "" {
		t.Fatal("durable knowledge step was not marked ok", persistedSteps.Body.String())
	}
	if optional["stepKey"] != "optional" || optional["ok"] != false || optional["errorMsg"] != "可选步骤失败" ||
		optional["attemptCount"] != float64(1) || optional["completedAt"] != "" {
		t.Fatal("durable optional step was not marked failed", persistedSteps.Body.String())
	}
	ledger := rpc("db:get-llm-history", []any{nil, projectPath, freshSession})
	var ledgerWire struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(ledger.Body.Bytes(), &ledgerWire); err != nil || ledger.Code != http.StatusOK || len(ledgerWire.Result) != 1 {
		t.Fatal("durable get-llm-history did not return the logged call", ledger.Code, ledger.Body.String(), err)
	}
	call := ledgerWire.Result[0]
	if call["modelId"] != "synthetic-durable-model" || call["purpose"] != "write" ||
		call["totalTokens"] != float64(46) || call["finishReason"] != "stop" {
		t.Fatal("durable logged llm call wire mismatch", ledger.Body.String())
	}
	for _, body := range []string{latest.Body.String(), persistedSteps.Body.String(), ledger.Body.String()} {
		if strings.Contains(body, projectPath) || strings.Contains(body, durableDir) {
			t.Fatal("durable post-process write readback leaked a filesystem path", body)
		}
	}
	// A refused create is reported as a refusal and leaves no partial row: the
	// store rejects the malformed step list before its transaction opens, so no
	// run header row exists and the readback is the absent shape.
	refusedRun := mutate("db:post-process-create-run", []any{map[string]any{
		"triggerSourceType": "arch_extract",
		"triggerSourceId":   "synthetic-durable-refused",
		"sourceLabel":       "拒绝",
		"steps":             []any{map[string]any{"key": "knowledge"}},
	}, projectPath, freshSession})
	if refusedRun["success"] != false || refusedRun["error"] != "Error: POST_HISTORY_INPUT_INVALID" {
		t.Fatalf("refused create-run was not reported as a refusal: %v", refusedRun)
	}
	absentRun := rpc("db:post-process-get-latest-run", []any{"arch_extract", "synthetic-durable-refused", projectPath, freshSession})
	var absentRunWire struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(absentRun.Body.Bytes(), &absentRunWire); err != nil || absentRun.Code != http.StatusOK || string(absentRunWire.Result) != "null" {
		t.Fatal("refused create-run left a partial run row", absentRun.Code, absentRun.Body.String(), err)
	}
	// A refused step update rolls its transaction back and keeps the committed
	// step rows exactly as they were.
	refusedStep := mutate("db:post-process-mark-step-ok", []any{runID, "absent", projectPath, freshSession})
	if refusedStep["success"] != false || refusedStep["error"] != "Error: POST_PROCESS_STEP_NOT_FOUND" {
		t.Fatalf("refused mark-step-ok was not reported as a refusal: %v", refusedStep)
	}
	// The ledger keeps its unusual refusal shape: a bare {"success":false} with
	// no error code at all.
	refusedCall := rpc("db:log-llm-call", []any{map[string]any{"modelId": "synthetic-durable-model"}, projectPath, freshSession})
	if refusedCall.Code != http.StatusOK || !bytes.Contains(refusedCall.Body.Bytes(), []byte(`"success":false`)) ||
		bytes.Contains(refusedCall.Body.Bytes(), []byte(`"error"`)) {
		t.Fatal("refused log-llm-call gained an error code", refusedCall.Code, refusedCall.Body.String())
	}
	// A forged frozen context is refused before the borrow, so it cannot write.
	forged := mutate("db:post-process-create-run", []any{map[string]any{
		"triggerSourceType": "arch_extract",
		"triggerSourceId":   "synthetic-durable-forged",
		"sourceLabel":       "伪造",
		"steps":             []any{},
	}, projectPath, map[string]any{"projectId": id, "leaseId": "forged-lease", "projectPath": projectPath}})
	forgedError, _ := forged["error"].(string)
	if forged["success"] != false || !strings.HasPrefix(forgedError, "Error: ") || strings.Contains(forgedError, projectPath) {
		t.Fatalf("forged frozen context was not refused symbolically: %v", forged)
	}
	// No refused write left a partial row anywhere: one run, two steps, one call.
	var runRows, stepRows, callRows int64
	durableQuery(t, database, "SELECT COUNT(*) FROM post_process_runs", &runRows)
	durableQuery(t, database, "SELECT COUNT(*) FROM post_process_steps", &stepRows)
	durableQuery(t, database, "SELECT COUNT(*) FROM llm_calls", &callRows)
	if runRows != 1 || stepRows != 2 || callRows != 1 {
		t.Fatalf("refused post-process writes left partial rows: %d runs, %d steps, %d calls", runRows, stepRows, callRows)
	}
	// The five mutating narrative plan and consistency exemption names are bound
	// by BindNarrativeWrites to the same admitted author database through the
	// same borrow-and-refresh write path as the draft CAS and the post-process
	// writes. Every write is performed over the real HTTP handler and then read
	// back through the already bound db:narrative-thread-list and
	// db:consistency-exemption-list, so what is asserted is the persisted state,
	// not only the write receipt.
	narrativePlans := func() []map[string]any {
		t.Helper()
		response := rpc("db:narrative-thread-list", []any{projectPath, freshSession})
		var wire struct {
			Result []map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK || wire.Result == nil {
			t.Fatalf("durable narrative-thread-list readback failed: %d %s %v", response.Code, response.Body.String(), err)
		}
		return wire.Result
	}
	exemptionRows := func() []map[string]any {
		t.Helper()
		response := rpc("db:consistency-exemption-list", []any{projectPath, freshSession})
		var wire struct {
			Result []map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK || wire.Result == nil {
			t.Fatalf("durable consistency-exemption-list readback failed: %d %s %v", response.Code, response.Body.String(), err)
		}
		return wire.Result
	}
	createdPlan := mutate("db:narrative-thread-plan-create", []any{map[string]any{
		"title": "合成线索", "type": "伏笔", "authorIntent": "作者决定回收时机",
		"targetStartChapter": 2, "targetEndChapter": 4,
	}, projectPath, freshSession})
	if createdPlan["success"] != true || createdPlan["error"] != nil {
		t.Fatalf("durable plan-create reported failure: %v", createdPlan)
	}
	createdPlanWire, _ := createdPlan["plan"].(map[string]any)
	if createdPlanWire == nil || createdPlanWire["title"] != "合成线索" || createdPlanWire["type"] != "伏笔" {
		t.Fatalf("durable plan-create returned a malformed plan: %v", createdPlan)
	}
	planID, _ := createdPlanWire["id"].(float64)
	if planID <= 0 {
		t.Fatalf("durable plan-create did not mint a plan id: %v", createdPlan)
	}
	// The created plan is visible through the already bound list read.
	if plans := narrativePlans(); len(plans) != 1 || plans[0]["title"] != "合成线索" || plans[0]["status"] != "planned" {
		t.Fatalf("created plan is not visible in narrative-thread-list: %v", plans)
	}
	// A plan update changes a persisted field: the retitled and re-ranged plan is
	// what the list read returns, not only what the write receipt echoes.
	updatedPlan := mutate("db:narrative-thread-plan-update", []any{planID, map[string]any{
		"title": "合成线索·改", "type": "伏笔", "authorIntent": "作者改期",
		"targetStartChapter": 2, "targetEndChapter": 5,
	}, projectPath, freshSession})
	if updatedPlan["success"] != true || updatedPlan["error"] != nil {
		t.Fatalf("durable plan-update reported failure: %v", updatedPlan)
	}
	if plans := narrativePlans(); len(plans) != 1 || plans[0]["title"] != "合成线索·改" ||
		plans[0]["authorIntent"] != "作者改期" || plans[0]["targetEndChapter"] != float64(5) {
		t.Fatalf("updated plan fields were not persisted: %v", plans)
	}
	// A plan delete removes the row: the list read no longer returns it.
	deletedPlan := mutate("db:narrative-thread-plan-delete", []any{planID, projectPath, freshSession})
	if deletedPlan["success"] != true || deletedPlan["error"] != nil {
		t.Fatalf("durable plan-delete reported failure: %v", deletedPlan)
	}
	if plans := narrativePlans(); len(plans) != 0 {
		t.Fatalf("deleted plan is still visible in narrative-thread-list: %v", plans)
	}
	// A saved exemption is visible through the already bound list read as an
	// active row with its authored reason.
	savedExemption := mutate("db:consistency-exemption-save", []any{"stable:durable-fact", "作者豁免理由", projectPath, freshSession})
	if savedExemption["success"] != true || savedExemption["error"] != nil {
		t.Fatalf("durable exemption-save reported failure: %v", savedExemption)
	}
	if exemptions := exemptionRows(); len(exemptions) != 1 || exemptions[0]["stableFactKey"] != "stable:durable-fact" ||
		exemptions[0]["reason"] != "作者豁免理由" || exemptions[0]["revoked"] != false {
		t.Fatalf("saved exemption is not visible in consistency-exemption-list: %v", exemptions)
	}
	// A revoked exemption is no longer active: the same row is read back with
	// revoked=true, so the exemption is gone from the consistency authority.
	revokedExemption := mutate("db:consistency-exemption-revoke", []any{"stable:durable-fact", projectPath, freshSession})
	if revokedExemption["success"] != true || revokedExemption["error"] != nil {
		t.Fatalf("durable exemption-revoke reported failure: %v", revokedExemption)
	}
	if exemptions := exemptionRows(); len(exemptions) != 1 || exemptions[0]["stableFactKey"] != "stable:durable-fact" ||
		exemptions[0]["revoked"] != true {
		t.Fatalf("revoked exemption is still active in consistency-exemption-list: %v", exemptions)
	}
	// A failed write is reported as a refusal and leaves no partial row. The
	// inverted plan range is rejected by the store before its transaction, so no
	// plan row exists and the earlier delete is not undone.
	refusedPlan := mutate("db:narrative-thread-plan-create", []any{map[string]any{
		"title": "越界线索", "type": "伏笔", "authorIntent": "作者越界",
		"targetStartChapter": 5, "targetEndChapter": 2,
	}, projectPath, freshSession})
	if refusedPlan["success"] != false || refusedPlan["error"] != "Error: NARRATIVE_THREAD_RANGE_INVALID" {
		t.Fatalf("refused plan-create was not reported as a refusal: %v", refusedPlan)
	}
	// An update of the deleted plan is a refusal too: the store reports zero
	// matched rows as NARRATIVE_THREAD_PLAN_NOT_FOUND and inserts nothing.
	refusedUpdate := mutate("db:narrative-thread-plan-update", []any{planID, map[string]any{
		"title": "复活线索", "type": "伏笔", "authorIntent": "作者复活",
		"targetStartChapter": 1, "targetEndChapter": 2,
	}, projectPath, freshSession})
	if refusedUpdate["success"] != false || refusedUpdate["error"] != "Error: NARRATIVE_THREAD_PLAN_NOT_FOUND" {
		t.Fatalf("refused plan-update was not reported as a refusal: %v", refusedUpdate)
	}
	// A blank exemption key is refused before the upsert, so the revoked row is
	// neither replaced nor reactivated.
	refusedExemption := mutate("db:consistency-exemption-save", []any{"  \n ", "作者乱写", projectPath, freshSession})
	if refusedExemption["success"] != false || refusedExemption["error"] != "Error: CONSISTENCY_EXEMPTION_KEY_INVALID" {
		t.Fatalf("refused exemption-save was not reported as a refusal: %v", refusedExemption)
	}
	if exemptions := exemptionRows(); len(exemptions) != 1 || exemptions[0]["reason"] != "作者豁免理由" || exemptions[0]["revoked"] != true {
		t.Fatalf("refused exemption-save changed the persisted exemption: %v", exemptions)
	}
	// No refused write left a partial row anywhere: the plan table is empty
	// (created then deleted) and the exemption table holds only the revoked row.
	var planRows, exemptionTableRows int64
	durableQuery(t, database, "SELECT COUNT(*) FROM narrative_thread_plans", &planRows)
	durableQuery(t, database, "SELECT COUNT(*) FROM consistency_exemptions", &exemptionTableRows)
	if planRows != 0 || exemptionTableRows != 1 {
		t.Fatalf("refused narrative writes left partial rows: %d plans, %d exemptions", planRows, exemptionTableRows)
	}
	// Neither readback echoes a filesystem path: the receipt vocabulary stays
	// symbolic even after a refused write.
	for _, readback := range []string{"db:narrative-thread-list", "db:consistency-exemption-list"} {
		response := rpc(readback, []any{projectPath, freshSession})
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), projectPath) || strings.Contains(response.Body.String(), durableDir) {
			t.Fatalf("durable %s readback leaked a filesystem path: %d %s", readback, response.Code, response.Body.String())
		}
	}
	// Recent navigation exposes only the one synthetic project, never a key.
	recentRec := rpc("project:recent-list", []any{})
	var recentWire struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(recentRec.Body.Bytes(), &recentWire); err != nil || recentRec.Code != http.StatusOK || len(recentWire.Result) != 1 || recentWire.Result[0]["path"] != projectPath || recentWire.Result[0]["key"] != nil {
		t.Fatal("durable recent navigation mismatch", recentRec.Code, recentRec.Body.String(), err)
	}
	// db:close closes the admitted database before acknowledging.
	closed := rpc("db:close", []any{projectPath, freshSession})
	if closed.Code != http.StatusOK {
		t.Fatal("db:close failed", closed.Code, closed.Body.String())
	}
	// The two names wired this round need source facts that only exist in the
	// author's own database, so they are seeded here, while no lease is
	// admitted: db:close released the write handle and its content identity
	// proof, and the next project:open fingerprints exactly what is on disk.
	// One stored plot tree snapshot gives db:plot-tree-clear something real to
	// blank, and one finalized chapter plus its frozen finalization outbox row
	// gives db:narrative-thread-event-confirm the evidence source it requires.
	finalDraftID := seedSyntheticDurableFinalizedDraft(t, database)
	seedSyntheticDurablePlotSnapshot(t, database, finalDraftID)
	// A fresh open reads back the committed name from the same project.db.
	reopened := rpc("project:open", []any{projectPath, "durable-open-2", nil})
	var reopenWire struct {
		Result struct {
			Success bool           `json:"success"`
			Project map[string]any `json:"project"`
		} `json:"result"`
	}
	if err := json.Unmarshal(reopened.Body.Bytes(), &reopenWire); err != nil || reopened.Code != http.StatusOK {
		t.Fatal("reopen decode failed", reopened.Code, reopened.Body.String(), err)
	}
	if !reopenWire.Result.Success || reopenWire.Result.Project == nil {
		t.Fatal("reopen failed", reopened.Body.String())
	}
	if value, _ := reopenWire.Result.Project["name"].(string); value != savedNameAfterCAS {
		t.Fatalf("reopen lost committed update: %q", value)
	}
	if value, _ := reopenWire.Result.Project["path"].(string); value != projectPath {
		t.Fatalf("reopen changed project path: %q", value)
	}
	reopenedLease, _ := reopenWire.Result.Project["sessionLease"].(string)
	if reopenedLease == "" {
		t.Fatal("reopen did not mint a session lease")
	}
	reopenedSession := map[string]any{"projectId": id, "leaseId": reopenedLease, "projectPath": projectPath}
	// db:plot-tree-clear is bound by BindPlotTreeClear. The read before it proves
	// the seeded snapshot really is stored and readable through the original
	// stored-snapshot validator; the read after it proves the clear committed,
	// and the file itself is queried so the assertion is on persisted state.
	seededPlot := rpc("db:plot-tree-read", []any{projectPath, reopenedSession})
	var seededPlotWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(seededPlot.Body.Bytes(), &seededPlotWire); err != nil || seededPlot.Code != http.StatusOK {
		t.Fatalf("durable plot-tree-read decode failed: %d %s %v", seededPlot.Code, seededPlot.Body.String(), err)
	}
	seededSnapshot, _ := seededPlotWire.Result["snapshot"].(map[string]any)
	if seededSnapshot == nil {
		t.Fatalf("the seeded plot tree snapshot was not readable: %s", seededPlot.Body.String())
	}
	if tracks, _ := seededSnapshot["tracks"].([]any); len(tracks) != 1 {
		t.Fatalf("the seeded plot tree snapshot shape changed: %s", seededPlot.Body.String())
	}
	cleared := rpc("db:plot-tree-clear", []any{projectPath, reopenedSession})
	var clearedWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(cleared.Body.Bytes(), &clearedWire); err != nil || cleared.Code != http.StatusOK || clearedWire.Result["success"] != true {
		t.Fatalf("db:plot-tree-clear failed: %d %s %v", cleared.Code, cleared.Body.String(), err)
	}
	if strings.Contains(cleared.Body.String(), projectPath) || strings.Contains(cleared.Body.String(), durableDir) {
		t.Fatalf("db:plot-tree-clear leaked a filesystem path: %s", cleared.Body.String())
	}
	clearedPlot := rpc("db:plot-tree-read", []any{projectPath, reopenedSession})
	var clearedPlotWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(clearedPlot.Body.Bytes(), &clearedPlotWire); err != nil || clearedPlot.Code != http.StatusOK {
		t.Fatalf("durable plot-tree-read after clear decode failed: %d %s %v", clearedPlot.Code, clearedPlot.Body.String(), err)
	}
	if snapshot, present := clearedPlotWire.Result["snapshot"]; !present || snapshot != nil {
		t.Fatalf("db:plot-tree-clear did not blank the stored snapshot: %s", clearedPlot.Body.String())
	}
	var storedSnapshot string
	durableQuery(t, database, "SELECT plot_tree_snapshot FROM project_core WHERE id='main'", &storedSnapshot)
	if storedSnapshot != "" {
		t.Fatalf("db:plot-tree-clear left a stored snapshot: %q", storedSnapshot)
	}
	// A forged lease is refused before the store is reached and changes nothing.
	// Like the original binder for this name, the refusal is a transport error
	// carrying a symbolic code, not the save name's `Error: CODE` result.
	forgedClear := rpc("db:plot-tree-clear", []any{projectPath, map[string]any{"projectId": id, "leaseId": "forged-lease", "projectPath": projectPath}})
	if forgedClear.Code == http.StatusOK || strings.Contains(forgedClear.Body.String(), projectPath) || strings.Contains(forgedClear.Body.String(), durableDir) {
		t.Fatalf("db:plot-tree-clear accepted a forged lease: %d %s", forgedClear.Code, forgedClear.Body.String())
	}
	// db:narrative-thread-event-confirm is bound by BindNarrativeEventConfirm and
	// writes against a real plan plus the seeded frozen finalization outbox row.
	// The receipt carries the committed event, and the already bound thread list
	// reads that confirmation back from the author's own database.
	confirmedPlan := mutate("db:narrative-thread-plan-create", []any{map[string]any{
		"title": "定稿线索", "type": "伏笔", "authorIntent": "作者决定回收时机",
		"targetStartChapter": 1, "targetEndChapter": 4,
	}, projectPath, reopenedSession})
	if confirmedPlan["success"] != true || confirmedPlan["error"] != nil {
		t.Fatalf("durable plan-create reported failure: %v", confirmedPlan)
	}
	confirmedPlanWire, _ := confirmedPlan["plan"].(map[string]any)
	if confirmedPlanWire == nil {
		t.Fatalf("durable plan-create returned a malformed plan: %v", confirmedPlan)
	}
	confirmedPlanID, _ := confirmedPlanWire["id"].(float64)
	if confirmedPlanID <= 0 {
		t.Fatalf("durable plan-create did not mint a plan id: %v", confirmedPlan)
	}
	confirmed := mutate("db:narrative-thread-event-confirm", []any{map[string]any{
		"type": "resolved", "evidence": syntheticDurableFinalEvidence, "reason": "作者确认回收",
		"draftId": finalDraftID, "planId": confirmedPlanID,
	}, projectPath, reopenedSession})
	if confirmed["success"] != true || confirmed["error"] != nil {
		t.Fatalf("durable event-confirm reported failure: %v", confirmed)
	}
	confirmedEvent, _ := confirmed["event"].(map[string]any)
	if confirmedEvent == nil || confirmedEvent["evidence"] != syntheticDurableFinalEvidence ||
		confirmedEvent["reason"] != "作者确认回收" || confirmedEvent["type"] != "resolved" ||
		confirmedEvent["chapterNumber"] != float64(2) || confirmedEvent["chapterTitle"] != syntheticDurableFinalChapterTitle {
		t.Fatalf("durable event-confirm returned a malformed event: %v", confirmed)
	}
	threads := func() []map[string]any {
		t.Helper()
		response := rpc("db:narrative-thread-list", []any{projectPath, reopenedSession})
		var wire struct {
			Result []map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK || wire.Result == nil {
			t.Fatalf("durable narrative-thread-list readback failed: %d %s %v", response.Code, response.Body.String(), err)
		}
		return wire.Result
	}
	confirmedThreads := threads()
	if len(confirmedThreads) != 1 || confirmedThreads[0]["title"] != "定稿线索" || confirmedThreads[0]["status"] != "resolved" {
		t.Fatalf("the confirmed plan was not read back: %v", confirmedThreads)
	}
	confirmedEvents, _ := confirmedThreads[0]["events"].([]any)
	if len(confirmedEvents) != 1 {
		t.Fatalf("the confirmed event was not read back: %v", confirmedThreads[0])
	}
	confirmedEventRow, _ := confirmedEvents[0].(map[string]any)
	if confirmedEventRow == nil || confirmedEventRow["evidence"] != syntheticDurableFinalEvidence ||
		confirmedEventRow["reason"] != "作者确认回收" || confirmedEventRow["type"] != "resolved" ||
		confirmedEventRow["draftId"] != float64(finalDraftID) {
		t.Fatalf("the confirmed event row mismatch: %v", confirmedEvents[0])
	}
	var confirmationRows int64
	durableQuery(t, database, "SELECT COUNT(*) FROM narrative_thread_confirmations", &confirmationRows)
	if confirmationRows != 1 {
		t.Fatalf("durable event-confirm did not persist exactly one confirmation: %d", confirmationRows)
	}
	// Evidence that is not inside the frozen outbox prose is refused before the
	// insert, with this name's transport-error envelope at HTTP 500 carrying the
	// symbolic code — never the `Error: ` prefix the exemption names use.
	outsideEvidence := rpc("db:narrative-thread-event-confirm", []any{map[string]any{
		"type": "planted", "evidence": "正文里没有这句", "reason": "理由",
		"draftId": finalDraftID, "planId": confirmedPlanID,
	}, projectPath, reopenedSession})
	if outsideEvidence.Code != http.StatusInternalServerError ||
		!strings.Contains(outsideEvidence.Body.String(), "NARRATIVE_THREAD_EVIDENCE_NOT_IN_SOURCE") ||
		strings.Contains(outsideEvidence.Body.String(), "Error: ") ||
		strings.Contains(outsideEvidence.Body.String(), projectPath) {
		t.Fatalf("durable event-confirm did not refuse unsupported evidence: %d %s", outsideEvidence.Code, outsideEvidence.Body.String())
	}
	// An unknown plan is refused with the same envelope and its own code, so the
	// finite mapping is exercised in both directions.
	unknownPlan := rpc("db:narrative-thread-event-confirm", []any{map[string]any{
		"type": "planted", "evidence": syntheticDurableFinalEvidence, "reason": "理由",
		"draftId": finalDraftID, "planId": float64(999999),
	}, projectPath, reopenedSession})
	if unknownPlan.Code != http.StatusInternalServerError || !strings.Contains(unknownPlan.Body.String(), "NARRATIVE_THREAD_PLAN_NOT_FOUND") {
		t.Fatalf("durable event-confirm did not refuse an unknown plan: %d %s", unknownPlan.Code, unknownPlan.Body.String())
	}
	// Both refusals left exactly the one committed confirmation row.
	if afterRefusals := threads(); len(afterRefusals) != 1 {
		t.Fatalf("a refused event-confirm changed the thread list: %v", afterRefusals)
	}
	durableQuery(t, database, "SELECT COUNT(*) FROM narrative_thread_confirmations", &confirmationRows)
	if confirmationRows != 1 {
		t.Fatalf("a refused event-confirm persisted a row: %d", confirmationRows)
	}
	// project:recent-remove is bound by BindProjectRecentRemove and is exercised
	// last, because it drops the only shelf entry this lane ever registers. The
	// already bound project:recent-list read proves the entry is gone, the
	// database file proves the catalog dropped navigation metadata rather than
	// author files, and a second removal is a symbolic refusal at HTTP 200 that
	// leaks no catalog internals.
	removed := rpc("project:recent-remove", []any{projectPath})
	var removedWire struct {
		Result struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal(removed.Body.Bytes(), &removedWire); err != nil ||
		removed.Code != http.StatusOK || !removedWire.Result.Success || removedWire.Result.Error != "" {
		t.Fatalf("project:recent-remove failed: %d %s %v", removed.Code, removed.Body.String(), err)
	}
	afterRemove := rpc("project:recent-list", []any{})
	var afterRemoveWire struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(afterRemove.Body.Bytes(), &afterRemoveWire); err != nil ||
		afterRemove.Code != http.StatusOK || len(afterRemoveWire.Result) != 0 {
		t.Fatalf("project:recent-remove did not drop the shelf entry: %d %s %v", afterRemove.Code, afterRemove.Body.String(), err)
	}
	if _, err := os.Stat(database); err != nil {
		t.Fatalf("project:recent-remove touched the project directory: %v", err)
	}
	removedAgain := rpc("project:recent-remove", []any{projectPath})
	var removedAgainWire struct {
		Result struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal(removedAgain.Body.Bytes(), &removedAgainWire); err != nil ||
		removedAgain.Code != http.StatusOK || removedAgainWire.Result.Success ||
		removedAgainWire.Result.Error != "Error: PROJECT_RECENT_REMOVE_FAILED" ||
		strings.Contains(removedAgain.Body.String(), projectPath) {
		t.Fatalf("a repeated project:recent-remove was not refused symbolically: %d %s %v", removedAgain.Code, removedAgain.Body.String(), err)
	}
}

// seedSyntheticDurableRows writes the real current-schema rows the durable
// lane must serve into the created synthetic project: one working draft body
// (the CAS target) and two characters. It runs before project:open so the write
// admission fingerprints exactly the seeded database.
func seedSyntheticDurableRows(t *testing.T, database string) (int64, string) {
	t.Helper()
	db, err := sql.Open("sqlite3", database)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	originalBody := "old durable draft"
	inserted, err := db.Exec("INSERT INTO contents(body) VALUES(?)", originalBody)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	contentID, err := inserted.LastInsertId()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	inserted, err = db.Exec("INSERT INTO drafts(chapter_number, version, status, content_id, word_count) VALUES(1, 1, 'draft', ?, 9)", contentID)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	draftID, err := inserted.LastInsertId()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO characters(character_id, name, role) VALUES('c-durable', '甲', 'protagonist')"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO characters(character_id, name, role) VALUES('c-durable-2', '乙', 'supporting')"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	// The roster commit refuses anything but a ready or empty roster, exactly like
	// the original author service, so the seeded facts are projected with the same
	// helper the commit itself runs (store.RefreshIdentityProjection). Seeding the
	// projection, not the status, keeps the readiness gate honest: the hashes and
	// the roster markdown are derived from the two characters above.
	if err := store.NewCharacters(db).RefreshIdentityProjection(context.Background()); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return draftID, originalBody
}

// seedSyntheticDurableBlueprint writes one real current-schema blueprint row
// into the created synthetic project. It runs before project:open so the write
// admission fingerprints exactly the seeded database.
func seedSyntheticDurableBlueprint(t *testing.T, database string) {
	t.Helper()
	db, err := sql.Open("sqlite3", database)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	_, execErr := db.Exec(`INSERT INTO blueprints(chapter_number, title, role, purpose, key_events,
		characters, suspense_hook, user_guidance, notes, notes_updated_at)
		VALUES(1, ?, 'main', '目的', '事件', ?, '悬念', '指导', '作者摘要', 'synthetic-fixed-author-clock')`,
		syntheticDurableBlueprintTitle, `["甲"]`)
	closeErr := db.Close()
	if execErr != nil || closeErr != nil {
		t.Fatal(execErr, closeErr)
	}
}

// The two import runs the durable lane advances. Both are author-manuscript
// runs, because a reference run cannot leave the knowledge stage without a real
// committed knowledge receipt for every chapter (store/importrun.go checkpoint
// validation), while the author plan needs only its own frozen chapter rows.
const (
	syntheticDurableImportProgressRun  = "durable-import-progress"
	syntheticDurableImportLifecycleRun = "durable-import-lifecycle"
	syntheticDurableImportOwner        = "synthetic-import-executor"
	syntheticDurableImportChapterOne   = "第一章：合成导入正文。"
	syntheticDurableImportChapterTwo   = "第二章：合成导入续章。"
)

// seedSyntheticDurableImportRuns writes two author-manuscript import runs with
// their frozen chapters and sources into the author's database while no lease is
// admitted. The "progress" run carries two chapters and one still-unfinished
// source, so both the checkpoint loop and the unfinished-source read see real
// rows; the "lifecycle" run is the one that is leased, cancelled and re-leased.
// It runs before project:open so the write admission fingerprints exactly the
// seeded database.
func seedSyntheticDurableImportRuns(t *testing.T, database string) {
	t.Helper()
	db, err := sql.Open("sqlite3", database)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	fingerprint := func(text string) string {
		sum := sha256.Sum256([]byte(text))
		return hex.EncodeToString(sum[:])
	}
	fail := func(err error) {
		_ = db.Close()
		t.Fatal(err)
	}
	type seededSource struct {
		index        int64
		id           string
		status       string
		chapterCount int64
		displayName  string
	}
	insertRun := func(runID string, chapters []string, sources []seededSource) {
		if _, err := db.Exec(`INSERT INTO import_runs(
			id, purpose, root_run_id, effect_namespace, source_fingerprint, manifest_fingerprint,
			locale, stage, status, total_chapters, manifest_chapter_count, completed_chapters
		) VALUES(?, 'author-manuscript', ?, ?, ?, ?, 'zh-CN', 'author-publish', 'ready', ?, ?, 0)`,
			runID, runID, "import:author-manuscript:"+runID, fingerprint(runID+"-source"),
			fingerprint(runID+"-manifest"), int64(len(chapters)), int64(len(chapters))); err != nil {
			fail(err)
		}
		for offset, content := range chapters {
			chapterNumber := int64(offset + 1)
			if _, err := db.Exec(`INSERT INTO import_run_chapters(
				run_id, chapter_number, source_id, source_chapter_number, title,
				content_fingerprint, content_size, content_snapshot
			) VALUES(?, ?, 'source-1', ?, ?, ?, ?, ?)`,
				runID, chapterNumber, chapterNumber, "第"+strconv.FormatInt(chapterNumber, 10)+"章",
				fingerprint(content), int64(len(content)), content); err != nil {
				fail(err)
			}
		}
		for _, source := range sources {
			display := `{"displayName":"` + source.displayName + `","mediaType":"text/plain","size":42}`
			if _, err := db.Exec(`INSERT INTO import_run_sources(
				run_id, source_index, source_id, source_fingerprint, display_json, status,
				manifest_fingerprint, chapter_count, content_size, word_count
			) VALUES(?, ?, ?, ?, ?, ?, '', ?, 42, 7)`,
				runID, source.index, source.id, fingerprint(runID+source.id), display,
				source.status, source.chapterCount); err != nil {
				fail(err)
			}
		}
	}
	insertRun(syntheticDurableImportProgressRun,
		[]string{syntheticDurableImportChapterOne, syntheticDurableImportChapterTwo},
		[]seededSource{
			{index: 1, id: "source-1", status: "completed", chapterCount: 2, displayName: "合成资料一.txt"},
			{index: 2, id: "source-2", status: "pending", chapterCount: 0, displayName: "合成资料二.txt"},
		})
	insertRun(syntheticDurableImportLifecycleRun,
		[]string{syntheticDurableImportChapterOne},
		[]seededSource{
			{index: 1, id: "source-1", status: "completed", chapterCount: 1, displayName: "合成资料三.txt"},
		})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// seedSyntheticDurableFinalizedDraft writes one finalized chapter with its
// frozen finalization outbox row into the author's database while no lease is
// admitted, and returns its draft id. store.ConfirmEvent requires exactly that
// pair: it refuses evidence that is not inside the outbox's frozen prose, so a
// draft row alone would prove nothing about the confirmation source.
func seedSyntheticDurableFinalizedDraft(t *testing.T, database string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite3", database)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	prose := "第二章正文：作者在此写下" + syntheticDurableFinalEvidence + "，随后收束。"
	inserted, err := db.Exec("INSERT INTO contents(body) VALUES(?)", prose)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	contentID, err := inserted.LastInsertId()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	inserted, err = db.Exec("INSERT INTO drafts(chapter_number, version, status, content_id, word_count) VALUES(2, 1, 'finalized', ?, 24)", contentID)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	draftID, err := inserted.LastInsertId()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_, execErr := db.Exec(`INSERT INTO finalization_outbox(finalization_id, draft_id, chapter_number,
		chapter_title, content_hash, content_revision, content_snapshot, target_file_name)
		VALUES('durable-finalized-1', ?, 2, ?, ?, 1, ?, 'synthetic.txt')`,
		draftID, syntheticDurableFinalChapterTitle, strings.Repeat("a1", 32), prose)
	closeErr := db.Close()
	if execErr != nil || closeErr != nil {
		t.Fatal(execErr, closeErr)
	}
	return draftID
}

// seedSyntheticDurablePlotSnapshot writes one valid stored plot tree snapshot
// into the author's database while no lease is admitted, so db:plot-tree-clear
// has a real stored snapshot to blank. The shape is the validator's own
// accepted stored form (store/plottree_validation.go:210): version 1, a
// readable generatedAt, zh-CN, and one track whose event supports itself from
// the seeded finalized chapter.
func seedSyntheticDurablePlotSnapshot(t *testing.T, database string, draftID int64) {
	t.Helper()
	db, err := sql.Open("sqlite3", database)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	snapshot := `{"version":1,"generatedAt":"2026-01-02T03:04:05.678Z","writingLanguage":"zh-CN",` +
		`"sourceRevision":"` + strings.Repeat("a1", 32) + `","tracks":[{"id":"main","title":"合成轨道",` +
		`"role":"main","startChapter":2,"endChapter":5,"summary":"轨道摘要",` +
		`"events":[{"status":"occurred","chapterNumber":2,"summary":"事件摘要",` +
		`"sources":[{"type":"finalized-chapter","draftId":` + strconv.FormatInt(draftID, 10) + `,"chapterNumber":2}]}]}]}`
	_, execErr := db.Exec("UPDATE project_core SET plot_tree_snapshot=? WHERE id='main'", snapshot)
	closeErr := db.Close()
	if execErr != nil || closeErr != nil {
		t.Fatal(execErr, closeErr)
	}
}

// durableDraftBody reads a draft body straight from the online database file.
func durableDraftBody(t *testing.T, database string, draftID int64) string {
	t.Helper()
	var value string
	durableQuery(t, database, "SELECT c.body FROM drafts d JOIN contents c ON c.id=d.content_id WHERE d.id="+strconv.FormatInt(draftID, 10), &value)
	return value
}

// durableDraftWordCount reads a draft's committed word count straight from the
// online database file.
func durableDraftWordCount(t *testing.T, database string, draftID int64) int64 {
	t.Helper()
	var value int64
	durableQuery(t, database, "SELECT word_count FROM drafts WHERE id="+strconv.FormatInt(draftID, 10), &value)
	return value
}

func durableProjectName(t *testing.T, database string) string {
	t.Helper()
	var value string
	durableQuery(t, database, "SELECT project_name FROM project_core WHERE id='main'", &value)
	return value
}

func durableProjectGenre(t *testing.T, database string) string {
	t.Helper()
	var value string
	durableQuery(t, database, "SELECT genre FROM project_core WHERE id='main'", &value)
	return value
}

func durableQuery(t *testing.T, database, query string, target any) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+database+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	err = db.QueryRow(query).Scan(target)
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
}
