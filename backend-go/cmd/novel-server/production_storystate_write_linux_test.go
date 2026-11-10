//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// This file proves the eleven write operations production_storystate_write_linux.go
// binds: one subtest per operation, each writing over the real HTTP handler and
// then reading the result back through a connection of the test's own - never
// through the lane's admitted handle and never from the write receipt alone - so
// the assertion is on the persisted author database, not on a 200 envelope. On
// top of that it proves the fence: a caller without the live write lease is
// refused for every family and leaves the admitted file byte-identical, a write
// that passes the fence and fails inside its own transaction also leaves it
// byte-identical, and db:plot-tree-save keeps failing closed because its refusal
// is a capability gate rather than a missing migration.
const (
	productionStoryStateWriteOpenToken = "storystate-write-open-1"
	// productionStoryStateWriteFinalizationID is the outbox identity chapter 1 is
	// finalized under, so a thread confirmation has frozen prose to cite.
	productionStoryStateWriteFinalizationID = "ci-storystate-finalization-1"
	// The evidence is a substring of productionEditorSeed after the store's
	// whitespace compaction, so it is real prose from the frozen snapshot.
	productionStoryStateWriteEvidence = "他推开门"
	productionStoryStateWritePlanTitle = "线索：门后的信"
	productionStoryStateWritePlanTitleChanged = "线索：门后的信·改"
	productionStoryStateWritePlanIntent = "作者决定回收时机"
	productionStoryStateWritePlanIntentChanged = "作者改期"
	productionStoryStateWriteExemptionKey = "stable:fact:door"
	productionStoryStateWriteExemptionReason = "作者说明"
	productionStoryStateWriteExemptionReasonChanged = "作者再次确认"
	productionStoryStateWriteRunSource = "ci-storystate-write-source"
	productionStoryStateWriteCallPurpose = "chapter-draft"
	// productionStoryStateWritePlotSnapshot is a stored plot tree snapshot for
	// db:plot-tree-clear to blank. Its exact shape is not load-bearing: the
	// before/after assertion is on the stored column, and the bound read is only
	// asserted for the absence of a snapshot after the clear.
	productionStoryStateWritePlotSnapshot = `{"version":1,"generatedAt":"2026-01-02T03:04:05.678Z","writingLanguage":"zh-CN",` +
		`"sourceRevision":"a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1",` +
		`"tracks":[{"id":"main","title":"合成轨道","role":"main","startChapter":1,"endChapter":1,` +
		`"summary":"轨道摘要","events":[{"status":"occurred","chapterNumber":1,"summary":"事件摘要",` +
		`"sources":[{"type":"finalized-chapter","draftId":1,"chapterNumber":1}]}]}]}`
)

// productionStoryStateWriteRoot is the editor-read fixture plus exactly the rows
// these eleven writes need: an empty narrative/exemption/post-process/llm ledger,
// a finalized chapter 1 with its frozen outbox row, and a stored plot tree
// snapshot. The fixture is checkpointed into the admitted file, and its bytes are
// returned as the baseline a refused or failed write has to leave untouched.
func productionStoryStateWriteRoot(t *testing.T) (root, projectPath string, contract admission.SchemaContract, baseline []byte) {
	t.Helper()
	root, projectPath, contract, _, _ = productionEditorReadsRoot(t)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	statements := []struct {
		query string
		args  []any
	}{
		// The three families start empty, so every readback below counts exactly
		// the rows these writes commit.
		{"DELETE FROM narrative_thread_confirmations", nil},
		{"DELETE FROM narrative_thread_plans", nil},
		{"DELETE FROM consistency_exemptions", nil},
		{"DELETE FROM post_process_steps", nil},
		{"DELETE FROM post_process_runs", nil},
		{"DELETE FROM llm_calls", nil},
		// Chapter 1 is the finalized chapter a thread confirmation cites: the store
		// re-reads this outbox row and binds the evidence to its snapshot prose.
		{"UPDATE drafts SET status='finalized' WHERE id=1", nil},
		{`INSERT INTO finalization_outbox (finalization_id, draft_id, chapter_number, chapter_title,
 content_hash, content_revision, content_snapshot, target_file_name)
 VALUES (?, 1, 1, ?, ?, 1, ?, '第一章 门.md')`,
			[]any{productionStoryStateWriteFinalizationID, productionEditorChapter,
				strings.Repeat("a1", 32), productionEditorSeed}},
		{"UPDATE project_core SET plot_tree_snapshot=? WHERE id='main'", []any{productionStoryStateWritePlotSnapshot}},
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			_ = db.Close()
			t.Fatalf("seed %q: %v", statement.query, err)
		}
	}
	productionEditorCheckpoint(t, db, database)
	if baseline, err = os.ReadFile(database); err != nil {
		t.Fatal(err)
	}
	return root, projectPath, contract, baseline
}

// productionStoryStateWriteReadback opens a connection of the test's own onto the
// admitted author database. It never touches the lane's handle and never writes,
// so what it reads is what the committed file plus its WAL actually hold.
func productionStoryStateWriteReadback(t *testing.T, projectPath string) *sql.DB {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	db, err := sql.Open("sqlite3", "file:"+database+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// productionStoryStateWriteScan reads one row through that separate connection.
func productionStoryStateWriteScan(t *testing.T, db *sql.DB, query string, args []any, into ...any) {
	t.Helper()
	if err := db.QueryRow(query, args...).Scan(into...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// productionStoryStateWritePlans reads the thread list the way the narrative read
// binder does - the repository over a connection of the test's own - so a plan,
// its status and its confirmed events are asserted as persisted state.
func productionStoryStateWritePlans(t *testing.T, projectPath string) []map[string]any {
	t.Helper()
	plans, err := store.NewNarrative(productionStoryStateWriteReadback(t, projectPath)).List(context.Background())
	if err != nil {
		t.Fatalf("narrative list readback: %v", err)
	}
	if plans == nil {
		t.Fatal("narrative list readback returned no slice")
	}
	return plans
}

// productionStoryStateWriteCall performs one operation and returns its result
// object. A 501 fails immediately: every name this test claims is bound.
func productionStoryStateWriteCall(t *testing.T, client *productionDurableClient, operation string, args []any) map[string]any {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%s is still GO_OPERATION_NOT_MIGRATED in the production lane: %s", operation, response.Body.String())
	}
	if response.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d %s", operation, response.Code, response.Body.String())
	}
	var wire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || wire.Result == nil {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	return wire.Result
}

// productionStoryStateWriteBusinessRefusal asserts the mutation envelope this
// family's binder commits to: HTTP 200, success=false, and "Error: CODE" - or,
// when wantCode is empty (db:log-llm-call), a bare success=false with no code at
// all. The code is the family's own sanitized refusal, which is what the browser
// already reads for a refused mutation.
func productionStoryStateWriteBusinessRefusal(t *testing.T, client *productionDurableClient, label, operation string, args []any, wantCode string) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%s: %s is unbound, so it cannot refuse by lease: %s", label, operation, response.Body.String())
	}
	if response.Code != http.StatusOK {
		t.Fatalf("%s: %s answered HTTP %d instead of its mutation envelope: %s",
			label, operation, response.Code, response.Body.String())
	}
	var wire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || wire.Result == nil {
		t.Fatalf("%s: %s: %v %s", label, operation, err, response.Body.String())
	}
	if wire.Result["success"] != false {
		t.Fatalf("%s: %s answered success without the live write lease: %s", label, operation, response.Body.String())
	}
	coded, present := wire.Result["error"]
	if wantCode == "" {
		if present {
			t.Fatalf("%s: %s invented a refusal code for db:log-llm-call: %s", label, operation, response.Body.String())
		}
		return
	}
	if coded != "Error: "+wantCode {
		t.Fatalf("%s: %s refused with %v instead of %q: %s",
			label, operation, coded, "Error: "+wantCode, response.Body.String())
	}
}

// productionStoryStateWriteTransportRefusal asserts the other refusal shape this
// batch keeps: db:narrative-thread-event-confirm and db:plot-tree-clear answer a
// transport error carrying their family's own symbolic code, not a business
// result.
func productionStoryStateWriteTransportRefusal(t *testing.T, client *productionDurableClient, label, operation string, args []any, wantCode string) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%s: %s is unbound, so it cannot refuse by lease: %s", label, operation, response.Body.String())
	}
	if response.Code == http.StatusOK {
		t.Fatalf("%s: %s answered 200 without the live write lease: %s", label, operation, response.Body.String())
	}
	var wire struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		t.Fatalf("%s: %s: %v %s", label, operation, err, response.Body.String())
	}
	if wire.Error.Code != wantCode {
		t.Fatalf("%s: %s refused with %q instead of %q: %s",
			label, operation, wire.Error.Code, wantCode, response.Body.String())
	}
}

// productionStoryStateWriteArgs builds one well-formed request per name, so a
// refusal can only come from the lease fence and never from arity or argument
// validation. The identities it names (plan 1, run ci-run-refused) are shaped
// correctly on purpose: a reachable store would accept them.
func productionStoryStateWriteArgs(t *testing.T, operation string, session map[string]any, projectPath string) []any {
	t.Helper()
	plan := func() map[string]any {
		return map[string]any{
			"title": productionStoryStateWritePlanTitle, "type": "伏笔",
			"authorIntent":       productionStoryStateWritePlanIntent,
			"targetStartChapter": float64(1), "targetEndChapter": float64(3),
		}
	}
	switch operation {
	case "db:narrative-thread-plan-create":
		return []any{plan(), projectPath, session}
	case "db:narrative-thread-plan-update":
		return []any{float64(1), plan(), projectPath, session}
	case "db:narrative-thread-plan-delete":
		return []any{float64(1), projectPath, session}
	case "db:consistency-exemption-save":
		return []any{productionStoryStateWriteExemptionKey, productionStoryStateWriteExemptionReason, projectPath, session}
	case "db:consistency-exemption-revoke":
		return []any{productionStoryStateWriteExemptionKey, projectPath, session}
	case "db:narrative-thread-event-confirm":
		return []any{map[string]any{
			"type": "planted", "evidence": productionStoryStateWriteEvidence, "reason": "作者确认",
			"draftId": float64(1), "planId": float64(1),
		}, projectPath, session}
	case "db:plot-tree-clear":
		return []any{projectPath, session}
	case "db:post-process-create-run":
		return []any{map[string]any{
			"triggerSourceType": "chapter_postprocess",
			"triggerSourceId":   "ci-storystate-refused",
			"sourceLabel":       "合成章节",
			"steps": []any{
				map[string]any{"key": "consistency", "label": "一致性", "critical": true},
			},
		}, projectPath, session}
	case "db:post-process-mark-step-ok":
		return []any{"ci-run-refused", "consistency", projectPath, session}
	case "db:post-process-mark-step-failed":
		return []any{"ci-run-refused", "consistency", "合成失败", projectPath, session}
	case "db:log-llm-call":
		return []any{map[string]any{
			"modelId": productionTestModelID, "modelName": "ci-story-model",
			"purpose": productionStoryStateWriteCallPurpose, "durationMs": float64(1), "success": false,
		}, projectPath, session}
	}
	t.Fatalf("no request shape for %s", operation)
	return nil
}

// TestProductionStoryStateWritesServeAdmittedAuthorDatabase is the write half of
// this lane: all eleven names answer over the real handler, and each one is read
// back through a separate connection - the plan and event rows through the
// narrative repository's own list read, the receipts and the ledger through plain
// SQL, the plot tree through the stored column and the already bound read. The
// subtests run in order on one admitted project because the create/update/delete
// cycle is a real lifecycle, not four independent inserts.
func TestProductionStoryStateWritesServeAdmittedAuthorDatabase(t *testing.T) {
	root, projectPath, contract, _ := productionStoryStateWriteRoot(t)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionEditorTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, productionStoryStateWriteOpenToken, projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	frozen := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}
	if frozen["leaseId"] == "" || frozen["leaseId"] == nil {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	// The plan and run identities the later subtests act on. The subtests below
	// run in declaration order, so each one asserts the state its predecessor
	// committed before it writes again.
	var planID, runID any

	t.Run("db:narrative-thread-plan-create", func(t *testing.T) {
		created := productionStoryStateWriteCall(t, author, "db:narrative-thread-plan-create", []any{map[string]any{
			"title": productionStoryStateWritePlanTitle, "type": "伏笔",
			"authorIntent":       productionStoryStateWritePlanIntent,
			"targetStartChapter": float64(1), "targetEndChapter": float64(3),
		}, projectPath, frozen})
		if created["success"] != true || created["error"] != nil {
			t.Fatalf("db:narrative-thread-plan-create reported failure: %v", created)
		}
		plan, ok := created["plan"].(map[string]any)
		if !ok || plan == nil || plan["title"] != productionStoryStateWritePlanTitle || plan["type"] != "伏笔" {
			t.Fatalf("db:narrative-thread-plan-create returned a malformed plan: %v", created)
		}
		id, ok := plan["id"].(float64)
		if !ok || id <= 0 {
			t.Fatalf("db:narrative-thread-plan-create did not mint a plan id: %v", plan)
		}
		planID = id
		plans := productionStoryStateWritePlans(t, projectPath)
		if len(plans) != 1 || plans[0]["id"] != int64(id) || plans[0]["title"] != productionStoryStateWritePlanTitle ||
			plans[0]["type"] != "伏笔" || plans[0]["authorIntent"] != productionStoryStateWritePlanIntent ||
			plans[0]["targetEndChapter"] != int64(3) || plans[0]["status"] != "planned" {
			t.Fatalf("the created plan was not committed to the author database: %v", plans)
		}
	})

	t.Run("db:narrative-thread-plan-update", func(t *testing.T) {
		if planID == nil {
			t.Fatal("the plan-create subtest committed no plan to update")
		}
		updated := productionStoryStateWriteCall(t, author, "db:narrative-thread-plan-update", []any{planID, map[string]any{
			"title": productionStoryStateWritePlanTitleChanged, "type": "伏笔",
			"authorIntent":       productionStoryStateWritePlanIntentChanged,
			"targetStartChapter": float64(1), "targetEndChapter": float64(5),
		}, projectPath, frozen})
		if updated["success"] != true || updated["error"] != nil {
			t.Fatalf("db:narrative-thread-plan-update reported failure: %v", updated)
		}
		plans := productionStoryStateWritePlans(t, projectPath)
		if len(plans) != 1 || plans[0]["title"] != productionStoryStateWritePlanTitleChanged ||
			plans[0]["authorIntent"] != productionStoryStateWritePlanIntentChanged ||
			plans[0]["targetStartChapter"] != int64(1) || plans[0]["targetEndChapter"] != int64(5) {
			t.Fatalf("the updated plan fields were not committed: %v", plans)
		}
	})

	t.Run("db:narrative-thread-event-confirm", func(t *testing.T) {
		if planID == nil {
			t.Fatal("the plan-create subtest committed no plan to confirm against")
		}
		db := productionStoryStateWriteReadback(t, projectPath)
		var finalized int64
		productionStoryStateWriteScan(t, db, "SELECT COUNT(*) FROM drafts WHERE id=1 AND status='finalized'", nil, &finalized)
		if finalized != 1 {
			t.Fatal("the fixture's finalized chapter is missing, so a confirmation could only fail")
		}
		confirmed := productionStoryStateWriteCall(t, author, "db:narrative-thread-event-confirm", []any{map[string]any{
			"type": "resolved", "evidence": productionStoryStateWriteEvidence, "reason": "作者确认回收",
			"draftId": float64(1), "planId": planID,
		}, projectPath, frozen})
		if confirmed["success"] != true || confirmed["error"] != nil {
			t.Fatalf("db:narrative-thread-event-confirm reported failure: %v", confirmed)
		}
		event, ok := confirmed["event"].(map[string]any)
		if !ok || event == nil {
			t.Fatalf("db:narrative-thread-event-confirm returned no event: %v", confirmed)
		}
		if event["type"] != "resolved" || event["evidence"] != productionStoryStateWriteEvidence ||
			event["reason"] != "作者确认回收" || event["chapterNumber"] != float64(1) ||
			event["chapterTitle"] != productionEditorChapter {
			t.Fatalf("db:narrative-thread-event-confirm returned the wrong event: %v", event)
		}
		// The confirmation row itself, then the thread list's own view of it.
		var rows int64
		productionStoryStateWriteScan(t, db,
			"SELECT COUNT(*) FROM narrative_thread_confirmations WHERE plan_id=? AND draft_id=1 AND event_type='resolved'",
			[]any{int64(planID.(float64))}, &rows)
		if rows != 1 {
			t.Fatalf("the confirmation row was not committed: %d rows", rows)
		}
		plans := productionStoryStateWritePlans(t, projectPath)
		if len(plans) != 1 || plans[0]["status"] != "resolved" {
			t.Fatalf("the confirmed thread did not become resolved: %v", plans)
		}
		events, ok := plans[0]["events"].([]map[string]any)
		if !ok || len(events) != 1 {
			t.Fatalf("the confirmation was not read back through the thread list: %v", plans[0])
		}
		if events[0]["evidence"] != productionStoryStateWriteEvidence || events[0]["reason"] != "作者确认回收" ||
			events[0]["type"] != "resolved" || events[0]["chapterNumber"] != int64(1) ||
			events[0]["chapterTitle"] != productionEditorChapter {
			t.Fatalf("the confirmed event row mismatch: %v", events[0])
		}
	})

	t.Run("db:narrative-thread-plan-delete", func(t *testing.T) {
		if planID == nil {
			t.Fatal("the plan-create subtest committed no plan to delete")
		}
		deleted := productionStoryStateWriteCall(t, author, "db:narrative-thread-plan-delete", []any{planID, projectPath, frozen})
		if deleted["success"] != true || deleted["error"] != nil {
			t.Fatalf("db:narrative-thread-plan-delete reported failure: %v", deleted)
		}
		if plans := productionStoryStateWritePlans(t, projectPath); len(plans) != 0 {
			t.Fatalf("the deleted plan is still readable: %v", plans)
		}
		db := productionStoryStateWriteReadback(t, projectPath)
		var plans, confirmations int64
		productionStoryStateWriteScan(t, db, "SELECT COUNT(*) FROM narrative_thread_plans", nil, &plans)
		productionStoryStateWriteScan(t, db, "SELECT COUNT(*) FROM narrative_thread_confirmations", nil, &confirmations)
		if plans != 0 || confirmations != 0 {
			t.Fatalf("the delete left rows behind: %d plans, %d confirmations", plans, confirmations)
		}
	})

	t.Run("db:consistency-exemption-save", func(t *testing.T) {
		saved := productionStoryStateWriteCall(t, author, "db:consistency-exemption-save",
			[]any{productionStoryStateWriteExemptionKey, productionStoryStateWriteExemptionReason, projectPath, frozen})
		if saved["success"] != true {
			t.Fatalf("db:consistency-exemption-save reported failure: %v", saved)
		}
		db := productionStoryStateWriteReadback(t, projectPath)
		var reason string
		var revoked int64
		productionStoryStateWriteScan(t, db,
			"SELECT reason, revoked FROM consistency_exemptions WHERE stable_fact_key=?",
			[]any{productionStoryStateWriteExemptionKey}, &reason, &revoked)
		if reason != productionStoryStateWriteExemptionReason || revoked != 0 {
			t.Fatalf("the saved exemption was not committed: %q revoked=%d", reason, revoked)
		}
		exemptions, err := store.NewNarrative(db).Exemptions(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(exemptions) != 1 || exemptions[0]["stableFactKey"] != productionStoryStateWriteExemptionKey ||
			exemptions[0]["reason"] != productionStoryStateWriteExemptionReason || exemptions[0]["revoked"] != false {
			t.Fatalf("the saved exemption was not read back: %v", exemptions)
		}
	})

	t.Run("db:consistency-exemption-revoke", func(t *testing.T) {
		revoked := productionStoryStateWriteCall(t, author, "db:consistency-exemption-revoke",
			[]any{productionStoryStateWriteExemptionKey, projectPath, frozen})
		if revoked["success"] != true {
			t.Fatalf("db:consistency-exemption-revoke reported failure: %v", revoked)
		}
		db := productionStoryStateWriteReadback(t, projectPath)
		var flag int64
		productionStoryStateWriteScan(t, db,
			"SELECT revoked FROM consistency_exemptions WHERE stable_fact_key=?",
			[]any{productionStoryStateWriteExemptionKey}, &flag)
		if flag != 1 {
			t.Fatalf("the revoked exemption was not committed: revoked=%d", flag)
		}
		exemptions, err := store.NewNarrative(db).Exemptions(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(exemptions) != 1 || exemptions[0]["revoked"] != true {
			t.Fatalf("the revoked exemption was not read back: %v", exemptions)
		}
		// The same upsert reactivates a revoked key, exactly as the original does.
		resaved := productionStoryStateWriteCall(t, author, "db:consistency-exemption-save",
			[]any{productionStoryStateWriteExemptionKey, productionStoryStateWriteExemptionReasonChanged, projectPath, frozen})
		if resaved["success"] != true {
			t.Fatalf("reactivating the exemption reported failure: %v", resaved)
		}
		var reason string
		productionStoryStateWriteScan(t, db,
			"SELECT reason, revoked FROM consistency_exemptions WHERE stable_fact_key=?",
			[]any{productionStoryStateWriteExemptionKey}, &reason, &flag)
		if flag != 0 || reason != productionStoryStateWriteExemptionReasonChanged {
			t.Fatalf("the reactivated exemption was not committed: %q revoked=%d", reason, flag)
		}
	})

	t.Run("db:plot-tree-clear", func(t *testing.T) {
		db := productionStoryStateWriteReadback(t, projectPath)
		var stored string
		productionStoryStateWriteScan(t, db, "SELECT plot_tree_snapshot FROM project_core WHERE id='main'", nil, &stored)
		if stored != productionStoryStateWritePlotSnapshot {
			t.Fatalf("the seeded plot tree snapshot was not stored: %q", stored)
		}
		cleared := productionStoryStateWriteCall(t, author, "db:plot-tree-clear", []any{projectPath, frozen})
		if cleared["success"] != true {
			t.Fatalf("db:plot-tree-clear reported failure: %v", cleared)
		}
		var blanked string
		productionStoryStateWriteScan(t, db, "SELECT plot_tree_snapshot FROM project_core WHERE id='main'", nil, &blanked)
		if blanked != "" {
			t.Fatalf("db:plot-tree-clear left a stored snapshot: %q", blanked)
		}
		var tree map[string]any
		author.decode(t, "db:plot-tree-read", []any{projectPath, frozen}, &tree)
		if snapshot, present := tree["snapshot"]; !present || snapshot != nil {
			t.Fatalf("the bound plot tree read still returns a snapshot after the clear: %v", tree["snapshot"])
		}
	})

	t.Run("db:post-process-create-run", func(t *testing.T) {
		created := productionStoryStateWriteCall(t, author, "db:post-process-create-run", []any{map[string]any{
			"triggerSourceType": "chapter_postprocess",
			"triggerSourceId":   productionStoryStateWriteRunSource,
			"sourceLabel":       "合成章节",
			"steps": []any{
				map[string]any{"key": "consistency", "label": "一致性", "critical": true},
				map[string]any{"key": "character", "label": "角色", "critical": false},
			},
		}, projectPath, frozen})
		if created["success"] != true || created["error"] != nil {
			t.Fatalf("db:post-process-create-run reported failure: %v", created)
		}
		id, ok := created["id"].(string)
		if !ok || id == "" {
			t.Fatalf("db:post-process-create-run returned no run id: %v", created)
		}
		runID = id
		db := productionStoryStateWriteReadback(t, projectPath)
		var kind, source, label string
		productionStoryStateWriteScan(t, db,
			"SELECT trigger_source_type, trigger_source_id, source_label FROM post_process_runs WHERE id=?",
			[]any{id}, &kind, &source, &label)
		if kind != "chapter_postprocess" || source != productionStoryStateWriteRunSource || label != "合成章节" {
			t.Fatalf("the created run was not committed: %q %q %q", kind, source, label)
		}
		// The step rows are part of the same transaction as the run row.
		steps, err := store.NewPostHistory(db).Steps(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) != 2 || steps[0]["stepKey"] != "consistency" || steps[1]["stepKey"] != "character" ||
			steps[0]["critical"] != true || steps[1]["critical"] != false ||
			steps[0]["ok"] != false || steps[1]["ok"] != false {
			t.Fatalf("the created run's steps were not committed: %v", steps)
		}
	})

	t.Run("db:post-process-mark-step-ok", func(t *testing.T) {
		if runID == nil {
			t.Fatal("the create-run subtest committed no run to mark")
		}
		marked := productionStoryStateWriteCall(t, author, "db:post-process-mark-step-ok",
			[]any{runID, "consistency", projectPath, frozen})
		if marked["success"] != true || marked["error"] != nil {
			t.Fatalf("db:post-process-mark-step-ok reported failure: %v", marked)
		}
		db := productionStoryStateWriteReadback(t, projectPath)
		var ok int64
		var completed string
		var attempts int64
		productionStoryStateWriteScan(t, db,
			"SELECT ok, completed_at, attempt_count FROM post_process_steps WHERE run_id=? AND step_key='consistency'",
			[]any{runID}, &ok, &completed, &attempts)
		if ok != 1 || completed == "" || attempts != 1 {
			t.Fatalf("mark-step-ok was not committed: ok=%d completed=%q attempts=%d", ok, completed, attempts)
		}
		// The only critical step passed, so the run's aggregate flag moved too.
		var allPassed int64
		productionStoryStateWriteScan(t, db, "SELECT all_critical_passed FROM post_process_runs WHERE id=?", []any{runID}, &allPassed)
		if allPassed != 1 {
			t.Fatalf("the run's aggregate flag was not recomputed: allCriticalPassed=%d", allPassed)
		}
		steps, err := store.NewPostHistory(db).Steps(context.Background(), runID.(string))
		if err != nil {
			t.Fatal(err)
		}
		if len(steps) != 2 || steps[0]["ok"] != true || steps[1]["ok"] != false {
			t.Fatalf("the marked step was not read back: %v", steps)
		}
	})

	t.Run("db:post-process-mark-step-failed", func(t *testing.T) {
		if runID == nil {
			t.Fatal("the create-run subtest committed no run to mark")
		}
		failed := productionStoryStateWriteCall(t, author, "db:post-process-mark-step-failed",
			[]any{runID, "character", "合成失败", projectPath, frozen})
		if failed["success"] != true || failed["error"] != nil {
			t.Fatalf("db:post-process-mark-step-failed reported failure: %v", failed)
		}
		db := productionStoryStateWriteReadback(t, projectPath)
		var ok int64
		var message string
		var attempts int64
		productionStoryStateWriteScan(t, db,
			"SELECT ok, error_msg, attempt_count FROM post_process_steps WHERE run_id=? AND step_key='character'",
			[]any{runID}, &ok, &message, &attempts)
		if ok != 0 || message != "合成失败" || attempts != 1 {
			t.Fatalf("mark-step-failed was not committed: ok=%d errorMsg=%q attempts=%d", ok, message, attempts)
		}
		// A non-critical failure leaves the aggregate flag alone.
		var allPassed int64
		productionStoryStateWriteScan(t, db, "SELECT all_critical_passed FROM post_process_runs WHERE id=?", []any{runID}, &allPassed)
		if allPassed != 1 {
			t.Fatalf("a non-critical failure reset the aggregate flag: allCriticalPassed=%d", allPassed)
		}
	})

	t.Run("db:log-llm-call", func(t *testing.T) {
		logged := productionStoryStateWriteCall(t, author, "db:log-llm-call", []any{map[string]any{
			"modelId": productionTestModelID, "modelName": "ci-story-model",
			"purpose": productionStoryStateWriteCallPurpose,
			"promptTokens": float64(12), "completionTokens": float64(5), "totalTokens": float64(17),
			"durationMs": float64(42), "success": true,
		}, projectPath, frozen})
		if logged["success"] != true {
			t.Fatalf("db:log-llm-call reported failure: %v", logged)
		}
		db := productionStoryStateWriteReadback(t, projectPath)
		var modelID, purpose string
		var total, success int64
		productionStoryStateWriteScan(t, db,
			"SELECT model_id, purpose, total_tokens, success FROM llm_calls ORDER BY id DESC LIMIT 1",
			nil, &modelID, &purpose, &total, &success)
		if modelID != productionTestModelID || purpose != productionStoryStateWriteCallPurpose ||
			total != 17 || success != 1 {
			t.Fatalf("the logged call was not committed: %q %q total=%d success=%d", modelID, purpose, total, success)
		}
		var rows int64
		productionStoryStateWriteScan(t, db, "SELECT COUNT(*) FROM llm_calls", nil, &rows)
		if rows != 1 {
			t.Fatalf("db:log-llm-call committed %d ledger rows; want exactly the one logged call", rows)
		}
	})
}

// TestProductionStoryStateWritesRefuseWithoutTheLiveLease is the fence proof: a
// stale lease on the caller's own tab, and a live lease held by a different
// account, are both refused for every one of the eleven names, in the envelope each
// binder already commits to - and the admitted author database is byte-identical
// afterwards, so a refusal really is a non-write. A sibling tab of the same account
// is the other half: it shares the account-keyed lease and its write really lands.
// db:plot-tree-save is asserted here too: it is a capability gate, so it must keep
// failing closed at 501 instead of acquiring a handler that could only refuse.
func TestProductionStoryStateWritesRefuseWithoutTheLiveLease(t *testing.T) {
	root, projectPath, contract, baseline := productionStoryStateWriteRoot(t)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionEditorTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, productionStoryStateWriteOpenToken, projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	frozen := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}
	if frozen["leaseId"] == "" || frozen["leaseId"] == nil {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	stale := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     "stale-lease",
	}
	// A different account does not share this project's lease: `frozen` is real, but
	// it belongs to the production-author account, so even the same tab is refused.
	// (A sibling tab of that account shares the lease and is served; see the two
	// tests below.)
	foreign := newProductionDurableClientAs(t, handler, "other-author", productionEditorOtherTab)

	t.Run("narrative plan and exemption writes", func(t *testing.T) {
		for _, operation := range []string{
			"db:narrative-thread-plan-create",
			"db:narrative-thread-plan-update",
			"db:narrative-thread-plan-delete",
			"db:consistency-exemption-save",
			"db:consistency-exemption-revoke",
		} {
			productionStoryStateWriteBusinessRefusal(t, author, "stale lease", operation,
				productionStoryStateWriteArgs(t, operation, stale, projectPath), "NARRATIVE_OPERATION_FAILED")
			productionStoryStateWriteBusinessRefusal(t, foreign, "other account", operation,
				productionStoryStateWriteArgs(t, operation, frozen, projectPath), "NARRATIVE_OPERATION_FAILED")
		}
	})

	t.Run("db:narrative-thread-event-confirm", func(t *testing.T) {
		for _, session := range []struct {
			label string
			args  map[string]any
		}{{"stale lease", stale}, {"other account", frozen}} {
			client := author
			if session.label == "other account" {
				client = foreign
			}
			productionStoryStateWriteTransportRefusal(t, client, session.label, "db:narrative-thread-event-confirm",
				productionStoryStateWriteArgs(t, "db:narrative-thread-event-confirm", session.args, projectPath),
				"NARRATIVE_OPERATION_FAILED")
		}
	})

	t.Run("db:plot-tree-clear", func(t *testing.T) {
		for _, session := range []struct {
			label string
			args  map[string]any
		}{{"stale lease", stale}, {"other account", frozen}} {
			client := author
			if session.label == "other account" {
				client = foreign
			}
			productionStoryStateWriteTransportRefusal(t, client, session.label, "db:plot-tree-clear",
				productionStoryStateWriteArgs(t, "db:plot-tree-clear", session.args, projectPath),
				"PLOT_TREE_OPERATION_FAILED")
		}
	})

	t.Run("post-process receipt and llm ledger writes", func(t *testing.T) {
		for _, operation := range []string{
			"db:post-process-create-run",
			"db:post-process-mark-step-ok",
			"db:post-process-mark-step-failed",
		} {
			productionStoryStateWriteBusinessRefusal(t, author, "stale lease", operation,
				productionStoryStateWriteArgs(t, operation, stale, projectPath), "POST_HISTORY_OPERATION_FAILED")
			productionStoryStateWriteBusinessRefusal(t, foreign, "other account", operation,
				productionStoryStateWriteArgs(t, operation, frozen, projectPath), "POST_HISTORY_OPERATION_FAILED")
		}
		// db:log-llm-call keeps the original's bare refusal shape: success=false
		// and no code at all, which must not gain one.
		productionStoryStateWriteBusinessRefusal(t, author, "stale lease", "db:log-llm-call",
			productionStoryStateWriteArgs(t, "db:log-llm-call", stale, projectPath), "")
		productionStoryStateWriteBusinessRefusal(t, foreign, "other account", "db:log-llm-call",
			productionStoryStateWriteArgs(t, "db:log-llm-call", frozen, projectPath), "")
	})

	t.Run("db:plot-tree-save", func(t *testing.T) {
		// The original refuses this name unconditionally inside its own transaction
		// (electron/controllers/db-controller.ts:886-894) because the plot tree is
		// written by the main generation transaction, and the transport binder
		// mirrors that at plottree.go:22. It is a capability gate, so nothing here
		// binds a handler for it and the lane's honest 501 stands.
		response := author.call(t, "db:plot-tree-save",
			[]any{map[string]any{"version": float64(1)}, "ci-source-revision", projectPath, frozen})
		if response.Code != http.StatusOK ||
			!strings.Contains(response.Body.String(), "GENERATION_GRAPH_ADMISSION_REQUIRED") {
			t.Fatalf("db:plot-tree-save answered %d %s, want the desktop's refusal",
				response.Code, response.Body.String())
		}
	})

	after, err := os.ReadFile(filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, baseline) {
		t.Fatal("a refused or unbound story-state write changed the admitted author database")
	}

	// The lease is keyed by the authenticated account now, so a sibling tab of the
	// same account shares it: a write it makes with the frozen lease is served and
	// really lands in the admitted database. The refusals above are what remains of
	// the per-tab fence - the different account, which is the security boundary.
	sibling := newProductionDurableClient(t, handler, productionEditorOtherTab)
	shared := productionStoryStateWriteCall(t, sibling, "db:log-llm-call",
		productionStoryStateWriteArgs(t, "db:log-llm-call", frozen, projectPath))
	if shared["success"] != true || shared["error"] != nil {
		t.Fatalf("a sibling tab of the same account was refused the shared lease: %v", shared)
	}
	var sharedRows int64
	productionStoryStateWriteScan(t, productionStoryStateWriteReadback(t, projectPath),
		"SELECT COUNT(*) FROM llm_calls", nil, &sharedRows)
	if sharedRows != 1 {
		t.Fatalf("the sibling tab's shared-lease write committed %d ledger rows; want 1", sharedRows)
	}
}

// TestProductionStoryStateWritesFailAtomicallyUnderTheLiveLease proves the other
// half of "a failed write must not change the file": a request that passes the
// lease fence and fails inside the store's own transaction - a missing plan, a
// missing step, evidence absent from the frozen snapshot - is refused and leaves
// the admitted author database byte-identical to the bytes captured before the
// attempts.
func TestProductionStoryStateWritesFailAtomicallyUnderTheLiveLease(t *testing.T) {
	root, projectPath, contract, baseline := productionStoryStateWriteRoot(t)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionEditorTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, productionStoryStateWriteOpenToken, projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	frozen := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}
	if frozen["leaseId"] == "" || frozen["leaseId"] == nil {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	before, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, baseline) {
		t.Fatal("opening the project changed the admitted author database before any write")
	}
	plan := func() map[string]any {
		return map[string]any{
			"title": productionStoryStateWritePlanTitle, "type": "伏笔",
			"authorIntent":       productionStoryStateWritePlanIntent,
			"targetStartChapter": float64(1), "targetEndChapter": float64(3),
		}
	}

	// A valid request naming a plan the database does not hold: the store's own
	// UPDATE changes zero rows and refuses with its documented code.
	productionStoryStateWriteBusinessRefusal(t, author, "missing plan", "db:narrative-thread-plan-update",
		[]any{float64(987654), plan(), projectPath, frozen}, "NARRATIVE_THREAD_PLAN_NOT_FOUND")
	// A range the store refuses before it opens a transaction.
	invalidRange := plan()
	invalidRange["targetStartChapter"] = float64(4)
	invalidRange["targetEndChapter"] = float64(2)
	productionStoryStateWriteBusinessRefusal(t, author, "inverted range", "db:narrative-thread-plan-create",
		[]any{invalidRange, projectPath, frozen}, "NARRATIVE_THREAD_RANGE_INVALID")
	// A step the run does not hold: the UPDATE changes zero rows.
	productionStoryStateWriteBusinessRefusal(t, author, "missing step", "db:post-process-mark-step-ok",
		[]any{"ci-run-absent", "consistency", projectPath, frozen}, "POST_PROCESS_STEP_NOT_FOUND")
	// Evidence outside the frozen outbox prose: refused after the source read and
	// before the confirmation insert, in this name's transport-error shape.
	productionStoryStateWriteTransportRefusal(t, author, "evidence outside the source", "db:narrative-thread-event-confirm",
		[]any{map[string]any{
			"type": "planted", "evidence": "不存在的证据", "reason": "作者确认",
			"draftId": float64(1), "planId": float64(1),
		}, projectPath, frozen}, "NARRATIVE_THREAD_EVIDENCE_NOT_IN_SOURCE")
	// A ledger row whose required text field is not text: refused before the
	// INSERT, in db:log-llm-call's bare shape.
	productionStoryStateWriteBusinessRefusal(t, author, "non-text model id", "db:log-llm-call",
		[]any{map[string]any{
			"modelId": float64(1), "modelName": "ci-story-model",
			"purpose": productionStoryStateWriteCallPurpose, "durationMs": float64(1), "success": true,
		}, projectPath, frozen}, "")

	after, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("a failed story-state write changed the admitted author database")
	}
	// And the rows themselves are absent, so the file comparison above is not the
	// only witness.
	db := productionStoryStateWriteReadback(t, projectPath)
	for _, table := range []string{
		"narrative_thread_plans", "narrative_thread_confirmations", "consistency_exemptions",
		"post_process_runs", "post_process_steps", "llm_calls",
	} {
		var rows int64
		productionStoryStateWriteScan(t, db, "SELECT COUNT(*) FROM "+table, nil, &rows)
		if rows != 0 {
			t.Fatalf("a failed story-state write committed %d rows into %s", rows, table)
		}
	}
}
