//go:build linux

package main

import (
	"bytes"
	"database/sql"
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
)

// syntheticDurableBlueprintTitle is the authored blueprint title the durable
// lane must read back from the author's own database.
const syntheticDurableBlueprintTitle = "合成蓝图标题"

// TestSyntheticDurableHTTPOpenSaveCloseReopenPersists drives the real browser
// project lifecycle protocol over the durable Go author runtime. It opens the
// one real synthetic project, rejects a stale frozen context before any write,
// saves a new projectName plus novelConfig, reads the live frozen context, and
// confirms the committed value survives db:close and a fresh project:open.
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
	// db:character-roster-commit is deliberately NOT bound on this lane. The
	// original binder only ever denies it (the manual_edit branch requires
	// author scope, every other roster commit the identity lane) and no store
	// method expresses a roster commit at all, so binding it would mean
	// inventing SQL in a transport file. The registry exposes the name, so the
	// 501 is the durable binding, not a 404, and the refused call leaves the
	// roster exactly as seeded — asserted through the bound read and the file.
	rosterCommit := rpc("db:character-roster-commit", []any{map[string]any{
		"intent":     "manual_edit",
		"characters": []any{map[string]any{"characterId": "c-durable", "name": "改造角色"}},
	}, projectPath, expected})
	if rosterCommit.Code != http.StatusNotImplemented || !strings.Contains(rosterCommit.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("db:character-roster-commit was bound on the durable lane: %d %s", rosterCommit.Code, rosterCommit.Body.String())
	}
	var rosterName string
	durableQuery(t, database, "SELECT name FROM characters WHERE character_id='c-durable'", &rosterName)
	if rosterName != "甲" {
		t.Fatalf("unbound roster commit changed the persisted character: %q", rosterName)
	}
	rosterAfterCommit := rpc("db:character-roster-read", []any{projectPath, expected})
	if rosterAfterCommit.Code != http.StatusOK || !bytes.Contains(rosterAfterCommit.Body.Bytes(), []byte(`"name":"甲"`)) ||
		!bytes.Contains(rosterAfterCommit.Body.Bytes(), []byte(`"name":"乙"`)) {
		t.Fatal("durable character-roster-read changed after a refused roster commit", rosterAfterCommit.Code, rosterAfterCommit.Body.String())
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
	// The two plot tree mutations stay unbound on this lane: the registry
	// exposes them, so the 501 is the durable binding, not a 404.
	for _, unboundWrite := range []struct {
		name string
		args []any
	}{
		{"db:plot-tree-save", []any{map[string]any{"version": 1, "tracks": []any{}}, "synthetic-source-revision", projectPath, expected}},
		{"db:plot-tree-clear", []any{projectPath, expected}},
	} {
		plotWriteRec := rpc(unboundWrite.name, unboundWrite.args)
		if plotWriteRec.Code != http.StatusNotImplemented || !strings.Contains(plotWriteRec.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("mutating plot tree name %s was bound on the durable lane: %d %s", unboundWrite.name, plotWriteRec.Code, plotWriteRec.Body.String())
		}
	}
	// Both refused mutations left the author database untouched.
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
	// The three mutating names in the prompt and installed-skill family stay
	// unbound on this lane: the registry exposes them, so the 501 is the durable
	// binding, not a 404. A refused uninstall must also leave the skill on disk.
	for _, unboundWrite := range []struct {
		name string
		args []any
	}{
		{"prompt:save-global", []any{map[string]any{"key": "public", "content": "no"}}},
		{"prompt:delete-global", []any{promptKey, "zh-CN"}},
		{"skills:uninstall-user", []any{skillName}},
	} {
		promptRec := rpc(unboundWrite.name, unboundWrite.args)
		if promptRec.Code != http.StatusNotImplemented || !strings.Contains(promptRec.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("mutating prompt/skill name %s was bound on the durable lane: %d %s", unboundWrite.name, promptRec.Code, promptRec.Body.String())
		}
	}
	if _, err := os.Stat(filepath.Join(skillDirectory, "SKILL.md")); err != nil {
		t.Fatal("unbound skills:uninstall-user removed the installed skill", err)
	}
	// The one mutating name in the narrative thread and consistency exemption
	// family that belongs to the graph admission gate (thread event
	// confirmation) stays unbound on this lane: the registry exposes it, so the
	// 501 is the durable binding, not a 404. The five plan and exemption writes
	// are bound by BindNarrativeWrites and are exercised further down once the
	// draft CAS has rotated the lease.
	for _, unboundWrite := range []struct {
		name string
		args []any
	}{
		{"db:narrative-thread-event-confirm", []any{map[string]any{"type": "planted", "evidence": "证据", "reason": "理由"}, projectPath, expected}},
	} {
		writeRec := rpc(unboundWrite.name, unboundWrite.args)
		if writeRec.Code != http.StatusNotImplemented || !strings.Contains(writeRec.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("mutating narrative name %s was bound on the durable lane: %d %s", unboundWrite.name, writeRec.Code, writeRec.Body.String())
		}
	}
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
	coreWrite := func(operation string, args []any) map[string]any {
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
	// coreWrite is the mutation counterpart of call: it returns the DTO the
	// handler produced, so a refusal can be inspected rather than fatal'd.
	coreWrite := func(operation string, args []any, path string, ctx any) map[string]any {
		t.Helper()
		response := rpc(operation, []any{args[0], path, ctx})
		var wire map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK {
			t.Fatalf("durable %s decode failed: %d %s %v", operation, response.Code, response.Body.String(), err)
		}
		return wire
	}
	// coreRow re-reads the project core row through the bound get.
	coreRow := func() map[string]any {
		t.Helper()
		return call("db:project-core-get", []any{projectPath, expected})
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
	if staleCommit["success"] != false || staleCommit["error"] != "Error: PROJECT_CORE_SYNOPSIS_CHANGED" {
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