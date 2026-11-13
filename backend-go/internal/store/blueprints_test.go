package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every SQL test receives a private original TypeScript baseline byte-copy.
// No hand-maintained replacement schema or shared fixture mutation is allowed.
func blueprintFixture(t *testing.T) (*sql.DB, *Blueprints, string) {
	t.Helper()
	db, _, path := narrativeFixture(t)
	mustExec(t, db, `DELETE FROM blueprint_character_sync_operations; DELETE FROM blueprint_commit_operations; DELETE FROM blueprints`)
	return db, NewBlueprints(db), path
}
func blueprintData(chapter int) map[string]any {
	return map[string]any{"chapterNumber": chapter, "title": fmt.Sprintf("第%d章 暗门😀", chapter), "role": "转折",
		"purpose": "目的\u2028<>&", "keyEvents": "甲\u2029乙", "characters": []any{"阿青", "😀"},
		"suspenseHook": "疑问？", "userGuidance": "", "notes": "", "notesUpdatedAt": ""}
}
func blueprintRequest(id, mode string, start, end int) map[string]any {
	items := make([]map[string]any, 0)
	for chapter := start; chapter <= end; chapter++ {
		items = append(items, blueprintData(chapter))
	}
	return map[string]any{"operationId": id, "mode": mode, "startChapter": start, "endChapter": end, "blueprints": items}
}
func blueprintCopy(input map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range input {
		out[key] = value
	}
	return out
}
func blueprintAssertError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error=%v want=%v", err, want)
	}
}

func TestBlueprintNilAndValidationPrecedence(t *testing.T) {
	ctx := context.Background()
	for _, b := range []*Blueprints{nil, NewBlueprints(nil), {}} {
		if rows, err := b.All(ctx); rows != nil || !errors.Is(err, ErrProjectNotOpen) {
			t.Fatalf("all %v %v", rows, err)
		}
		if row, err := b.Get(ctx, true); row != nil || !errors.Is(err, ErrProjectNotOpen) {
			t.Fatalf("get %v %v", row, err)
		}
		blueprintAssertError(t, b.Upsert(ctx, nil), ErrProjectNotOpen)
		blueprintAssertError(t, b.UpsertMany(ctx, nil), ErrProjectNotOpen)
		blueprintAssertError(t, b.Delete(ctx, true), ErrProjectNotOpen)
		if row, err := b.CommitRange(ctx, nil); row != nil || !errors.Is(err, ErrProjectNotOpen) {
			t.Fatalf("commit %v %v", row, err)
		}
		if rows, err := b.PendingSync(ctx); rows != nil || !errors.Is(err, ErrProjectNotOpen) {
			t.Fatalf("pending %v %v", rows, err)
		}
		if row, err := b.GetSync(ctx, "\ufeff "); row != nil || !errors.Is(err, ErrBlueprintCharacterSyncOperationIDRequired) {
			t.Fatalf("id before DB %v %v", row, err)
		}
		if row, err := b.GetSync(ctx, "valid"); row != nil || !errors.Is(err, ErrProjectNotOpen) {
			t.Fatalf("sync %v %v", row, err)
		}
	}
	_, b, _ := blueprintFixture(t)
	request := blueprintRequest(" ", "bad", 0, 0)
	_, err := b.CommitRange(ctx, request)
	blueprintAssertError(t, err, ErrBlueprintOperationIDRequired)
	request["operationId"] = "id"
	_, err = b.CommitRange(ctx, request)
	blueprintAssertError(t, err, ErrBlueprintCommitModeInvalid)
	request["mode"] = "full"
	_, err = b.CommitRange(ctx, request)
	blueprintAssertError(t, err, ErrBlueprintCommitRangeInvalid)
	request = blueprintRequest("id", "full", 2, 2)
	_, err = b.CommitRange(ctx, request)
	blueprintAssertError(t, err, ErrBlueprintCommitRangeInvalid)
	request = blueprintRequest("id", "replace-range", 2, 3)
	request["blueprints"] = []map[string]any{blueprintData(2), blueprintData(2)}
	_, err = b.CommitRange(ctx, request)
	blueprintAssertError(t, err, ErrBlueprintCommitRangeIncomplete)
	request["generationRunHandle"] = nil
	_, err = b.CommitRange(ctx, request)
	blueprintAssertError(t, err, ErrBlueprintCommitRangeIncomplete)
	for _, value := range []any{true, "2", nil, 1.5} {
		request = blueprintRequest("id", "replace-range", 2, 2)
		request["startChapter"] = value
		_, err = b.CommitRange(ctx, request)
		blueprintAssertError(t, err, ErrBlueprintCommitRangeInvalid)
	}
	// These two bounds are deliberate native hardening, not Number.isInteger parity.
	for _, bounds := range [][2]any{{1, 10001}, {float64(9007199254740992), float64(9007199254740992)}} {
		request = blueprintRequest("id", "replace-range", 1, 1)
		request["startChapter"], request["endChapter"] = bounds[0], bounds[1]
		_, err = b.CommitRange(ctx, request)
		blueprintAssertError(t, err, ErrBlueprintCommitRangeInvalid)
	}
}

func TestBlueprintCRUDProjectionBindingsAndJSONValues(t *testing.T) {
	db, b, _ := blueprintFixture(t)
	ctx := context.Background()
	rows, err := b.All(ctx)
	if err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("empty %v %v", rows, err)
	}
	for _, chapter := range []int{3, 1, 2} {
		if err := b.Upsert(ctx, blueprintData(chapter)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err = b.All(ctx)
	if err != nil || len(rows) != 3 || rows[0]["chapterNumber"] != int64(1) || rows[2]["chapterNumber"] != int64(3) {
		t.Fatalf("order %v %v", rows, err)
	}
	row, err := b.Get(ctx, json.Number("2"))
	if err != nil || row["title"] != "第2章 暗门😀" || len(row) != 10 {
		t.Fatalf("UseNumber projection %v %v", row, err)
	}
	if !reflect.DeepEqual(row["characters"], []any{"阿青", "😀"}) {
		t.Fatalf("characters %v", row)
	}
	if row, err := b.Get(ctx, "404"); row != nil || err != nil {
		t.Fatalf("missing %v %v", row, err)
	}
	const bindError = "SQLite3 can only bind numbers, strings, bigints, buffers, and null"
	for _, bad := range []any{true, false} {
		if _, err := b.Get(ctx, bad); err == nil || err.Error() != bindError {
			t.Fatalf("get binding %v %v", bad, err)
		}
		if err := b.Delete(ctx, bad); err == nil || err.Error() != bindError {
			t.Fatalf("delete binding %v %v", bad, err)
		}
		input := blueprintData(4)
		input["role"] = bad
		if err := b.Upsert(ctx, input); err == nil || err.Error() != bindError {
			t.Fatalf("upsert binding %v %v", bad, err)
		}
	}
	mustExec(t, db, `UPDATE blueprints SET role=NULL,purpose=NULL,key_events=NULL,suspense_hook=NULL,
  user_guidance=NULL,notes=NULL,notes_updated_at=NULL,characters=NULL WHERE chapter_number=1`)
	row, err = b.Get(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"role", "purpose", "keyEvents", "suspenseHook", "userGuidance", "notes", "notesUpdatedAt"} {
		if row[key] != "" {
			t.Fatalf("NULL %s %v", key, row)
		}
	}
	if !reflect.DeepEqual(row["characters"], []any{}) {
		t.Fatalf("SQL NULL characters %v", row)
	}
	for _, test := range []struct {
		raw  any
		want any
	}{
		{"null", nil}, {"{\"name\":\"阿青\"}", map[string]any{"name": "阿青"}}, {"7", json.Number("7")},
		{"true", true}, {`"文字😀"`, "文字😀"}, {"not JSON", []any{}}, {"[] trailing", []any{}}, {"", []any{}},
	} {
		mustExec(t, db, "UPDATE blueprints SET characters=? WHERE chapter_number=1", test.raw)
		row, err = b.Get(ctx, 1)
		if err != nil || !reflect.DeepEqual(row["characters"], test.want) {
			t.Fatalf("raw %v => %v %v", test.raw, row, err)
		}
	}
	mustExec(t, db, "UPDATE blueprints SET purpose=CAST('raw' AS BLOB),characters=CAST('null' AS BLOB) WHERE chapter_number=1")
	row, err = b.Get(ctx, 1)
	if err != nil || !reflect.DeepEqual(row["purpose"], []byte("raw")) || row["characters"] != nil {
		t.Fatalf("storage classes %v %v", row, err)
	}
	for _, value := range []any{nil, "scalar", true, json.Number("12"), map[string]any{"name": "阿青"}, []string{"甲", "乙"}} {
		input := blueprintData(5)
		input["characters"] = value
		if err := b.Upsert(ctx, input); err != nil {
			t.Fatal(err)
		}
		row, err = b.Get(ctx, 5)
		if err != nil {
			t.Fatal(err)
		}
		expected, err := blueprintJSON(value)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := blueprintJSON(row["characters"])
		if err != nil || expected != actual {
			t.Fatalf("JSON roundtrip %s %s %v", expected, actual, err)
		}
	}
	input := blueprintData(6)
	delete(input, "characters")
	delete(input, "role")
	if err := b.Upsert(ctx, input); err != nil {
		t.Fatal(err)
	}
	var rawCharacters, rawRole any
	if err := db.QueryRow("SELECT characters,role FROM blueprints WHERE chapter_number=6").Scan(&rawCharacters, &rawRole); err != nil {
		t.Fatal(err)
	}
	if rawCharacters != nil || rawRole != nil {
		t.Fatalf("missing JS undefined fields should bind NULL %v %v", rawCharacters, rawRole)
	}
	if err := b.Delete(ctx, json.Number("6")); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx, -123); err != nil {
		t.Fatalf("missing delete no-op %v", err)
	}
}

func TestBlueprintBulkRollbackAndTimestamps(t *testing.T) {
	db, b, _ := blueprintFixture(t)
	ctx := context.Background()
	input := blueprintData(1)
	if err := b.Upsert(ctx, input); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "UPDATE blueprints SET created_at='created-fixed',updated_at='old' WHERE chapter_number=1")
	input["title"] = "changed"
	if err := b.Upsert(ctx, input); err != nil {
		t.Fatal(err)
	}
	var created, updated string
	if err := db.QueryRow("SELECT created_at,updated_at FROM blueprints WHERE chapter_number=1").Scan(&created, &updated); err != nil {
		t.Fatal(err)
	}
	if created != "created-fixed" || updated == "old" {
		t.Fatalf("timestamps %s %s", created, updated)
	}
	bad := blueprintData(3)
	bad["title"] = nil
	if err := b.UpsertMany(ctx, []map[string]any{blueprintData(2), bad}); err == nil || !strings.Contains(err.Error(), "NOT NULL") {
		t.Fatalf("bulk failure %v", err)
	}
	if narrativeCount(t, db, "blueprints") != 1 {
		t.Fatal("bulk first item survived failure")
	}
	if err := b.UpsertMany(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if err := b.UpsertMany(ctx, []map[string]any{blueprintData(2), blueprintData(3)}); err != nil {
		t.Fatal(err)
	}
}

func TestBlueprintCommitAuthorRetentionReplayAndFull(t *testing.T) {
	db, b, _ := blueprintFixture(t)
	ctx := context.Background()
	old := blueprintData(2)
	old["notes"], old["notesUpdatedAt"], old["userGuidance"] = "作者旧笔记", "clock-old", "作者指导"
	if err := b.Upsert(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := b.Upsert(ctx, blueprintData(8)); err != nil {
		t.Fatal(err)
	}
	request := blueprintRequest("  op-id  ", "replace-range", 2, 3)
	items := request["blueprints"].([]map[string]any)
	items[0]["notesUpdatedAt"] = "must-not-overwrite-companion"
	items[0]["relationshipHints"] = map[string]any{"from": "阿青", "to": "阿白"}
	items[0]["newCharacterCandidates"] = []any{map[string]any{"name": "阿青", "reason": "反复出场"}}
	// Request order is hash/signature significant; snapshot order is chapter ASC.
	request["blueprints"] = []map[string]any{items[1], items[0]}
	receipt, err := b.CommitRange(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if receipt["idempotent"] != false || receipt["operationId"] != "  op-id  " || len(receipt["payloadHash"].(string)) != 64 {
		t.Fatalf("receipt %v", receipt)
	}
	snapshot := receipt["snapshot"].([]map[string]any)
	if snapshot[0]["chapterNumber"] != int64(2) || snapshot[0]["notes"] != "作者旧笔记" || snapshot[0]["notesUpdatedAt"] != "clock-old" || snapshot[0]["userGuidance"] != "作者指导" || snapshot[0]["relationshipHints"] == nil {
		t.Fatalf("retained %v", snapshot)
	}
	if items[0]["notes"] != "" || items[0]["notesUpdatedAt"] != "must-not-overwrite-companion" {
		t.Fatal("mutated input")
	}
	frozen := receipt["characterSyncInput"].([]any)
	if frozen[0].(map[string]any)["chapterNumber"] != 3 {
		t.Fatalf("input order lost %v", frozen)
	}
	if narrativeCount(t, db, "blueprint_commit_operations") != 1 || narrativeCount(t, db, "blueprint_character_sync_operations") != 1 || narrativeCount(t, db, "blueprints") != 3 {
		t.Fatal("atomic facts absent or replace-range deleted outside")
	}
	// Replay is read-only but returns CURRENT editable rows and FROZEN sync extras.
	edit := blueprintData(2)
	edit["title"] = "作者后来修改"
	if err := b.Upsert(ctx, edit); err != nil {
		t.Fatal(err)
	}
	replay, err := b.CommitRange(ctx, request)
	if err != nil || replay["idempotent"] != true || replay["payloadHash"] != receipt["payloadHash"] {
		t.Fatalf("replay %v %v", replay, err)
	}
	snapshot = replay["snapshot"].([]map[string]any)
	if snapshot[0]["title"] != "作者后来修改" || !reflect.DeepEqual(snapshot[0]["relationshipHints"], items[0]["relationshipHints"]) {
		t.Fatalf("replay changed original semantics %v", snapshot)
	}
	changed := blueprintRequest("  op-id  ", "replace-range", 2, 3)
	_, err = b.CommitRange(ctx, changed)
	blueprintAssertError(t, err, ErrBlueprintOperationConflict)
	if err := b.Delete(ctx, 3); err != nil {
		t.Fatal(err)
	}
	_, err = b.CommitRange(ctx, request)
	blueprintAssertError(t, err, ErrBlueprintCommitRangeIncomplete)
	// Full commits remove every outside row but preserve selected author groups.
	full := blueprintRequest("full-op", "full", 1, 2)
	fullItems := full["blueprints"].([]map[string]any)
	fullItems[1]["notes"], fullItems[1]["notesUpdatedAt"] = "新笔记", "new-clock"
	if _, err := b.CommitRange(ctx, full); err != nil {
		t.Fatal(err)
	}
	rows, err := b.All(ctx)
	if err != nil || len(rows) != 2 || rows[1]["notes"] != "新笔记" || rows[1]["notesUpdatedAt"] != "new-clock" {
		t.Fatalf("full %v %v", rows, err)
	}
}

func TestBlueprintStrictReadbackRollbackAndAuthority(t *testing.T) {
	db, b, _ := blueprintFixture(t)
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	if err := b.Upsert(ctx, blueprintData(9)); err != nil {
		t.Fatal(err)
	}
	request := blueprintRequest("strict", "full", 1, 1)
	items := request["blueprints"].([]map[string]any)
	items[0]["purpose"] = nil
	_, err := b.CommitRange(ctx, request)
	blueprintAssertError(t, err, ErrBlueprintCommitReadBackFailed)
	if narrativeCount(t, db, "blueprints") != 1 || narrativeCount(t, db, "blueprint_commit_operations") != 0 {
		t.Fatal("strict readback did not roll back deletion/insert")
	}
	request = blueprintRequest("strict", "full", 1, 1)
	mustExec(t, db, `CREATE TRIGGER blueprint_mutate AFTER INSERT ON blueprints
  WHEN NEW.chapter_number=1 BEGIN UPDATE blueprints SET title='trigger mutation' WHERE chapter_number=1; END`)
	_, err = b.CommitRange(ctx, request)
	blueprintAssertError(t, err, ErrBlueprintCommitReadBackFailed)
	if narrativeCount(t, db, "blueprints") != 1 {
		t.Fatal("trigger mutation survived rollback")
	}
	mustExec(t, db, "DROP TRIGGER blueprint_mutate")
	receipt, err := b.CommitRange(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"generationRunHandle", "authorRecovery"} {
		owned := blueprintCopy(request)
		owned[key] = nil
		_, err = b.CommitRange(ctx, owned)
		blueprintAssertError(t, err, ErrGenerationOwnerRequired)
	}
	if narrativeCount(t, db, "blueprint_commit_operations") != 1 {
		t.Fatal("authority rejection changed ledger")
	}
	mustExec(t, db, `UPDATE blueprint_character_sync_operations SET status='completed',completion_receipt='{}' WHERE operation_id=?`, "blueprint-sync-strict")
	_, err = b.GetSync(ctx, "blueprint-sync-strict")
	blueprintAssertError(t, err, ErrGenerationOwnerRequired)
	_, err = b.CommitRange(ctx, request)
	blueprintAssertError(t, err, ErrGenerationOwnerRequired)
	if receipt["idempotent"] != false {
		t.Fatal("original receipt mutated")
	}
}

func TestBlueprintCommitLedgerSyncTriggerRollbackAndPoolRecovery(t *testing.T) {
	for _, action := range []string{"ABORT", "ROLLBACK"} {
		for _, table := range []string{"blueprint_commit_operations", "blueprint_character_sync_operations"} {
			t.Run(action+"-"+table, func(t *testing.T) {
				db, b, _ := blueprintFixture(t)
				db.SetMaxOpenConns(1)
				ctx := context.Background()
				if err := b.Upsert(ctx, blueprintData(9)); err != nil {
					t.Fatal(err)
				}
				mustExec(t, db, fmt.Sprintf(`CREATE TRIGGER blueprint_block AFTER INSERT ON %s
   BEGIN SELECT RAISE(%s,'blueprint blocked'); END`, table, action))
				request := blueprintRequest("blocked", "full", 1, 2)
				if _, err := b.CommitRange(ctx, request); err == nil || !strings.Contains(err.Error(), "blueprint blocked") {
					t.Fatalf("trigger %v", err)
				}
				if narrativeCount(t, db, "blueprints") != 1 || narrativeCount(t, db, "blueprint_commit_operations") != 0 || narrativeCount(t, db, "blueprint_character_sync_operations") != 0 {
					t.Fatal("partial transaction survived")
				}
				row, err := b.Get(ctx, 9)
				if err != nil || row == nil {
					t.Fatalf("outside row deletion survived %v %v", row, err)
				}
				mustExec(t, db, "DROP TRIGGER blueprint_block")
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				if _, err := b.CommitRange(canceled, request); err == nil {
					t.Fatal("cancelled commit succeeded")
				}
				recovery, stop := context.WithTimeout(ctx, 5*time.Second)
				defer stop()
				if _, err := b.CommitRange(recovery, request); err != nil {
					t.Fatalf("pool recovery %v", err)
				}
			})
		}
	}
}

func TestBlueprintPendingBackfillParsingAndErrorPrecedence(t *testing.T) {
	db, b, _ := blueprintFixture(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO blueprint_commit_operations(operation_id,payload_hash,mode,start_chapter,end_chapter,character_sync_input)
  VALUES('b','opaque-b','replace-range',2,2,'[]'),('a','opaque-a','replace-range',1,1,'[]')`)
	pending, err := b.PendingSync(ctx)
	if err != nil || len(pending) != 2 || pending[0]["operationId"] != "blueprint-sync-a" || pending[0]["status"] != "pending" {
		t.Fatalf("backfill %v %v", pending, err)
	}
	if narrativeCount(t, db, "blueprint_character_sync_operations") != 2 {
		t.Fatal("repair missing")
	}
	row, err := b.GetSync(ctx, "blueprint-sync-a")
	if err != nil || row["blueprintCommitPayloadHash"] != "opaque-a" || len(row) != 9 {
		t.Fatalf("normal sync fields %v %v", row, err)
	}
	if row, err := b.GetSync(ctx, "missing"); row != nil || err != nil {
		t.Fatalf("missing %v %v", row, err)
	}
	mustExec(t, db, `UPDATE blueprint_character_sync_operations SET created_at='2000',completed_at='raw-clock' WHERE operation_id='blueprint-sync-b'`)
	pending, err = b.PendingSync(ctx)
	if err != nil || pending[0]["operationId"] != "blueprint-sync-b" || pending[0]["completedAt"] != "raw-clock" {
		t.Fatalf("raw completed clock/order %v %v", pending, err)
	}
	for _, test := range []struct {
		completion    any
		status, input string
		want          error
	}{
		{"bad", "pending", "bad", ErrBlueprintCharacterSyncReceiptInvalid},
		{"null", "pending", "bad", ErrBlueprintCharacterSyncReceiptInvalid},
		{"[]", "pending", "bad", ErrBlueprintCharacterSyncReceiptInvalid},
		{"{}", "pending", "bad", ErrBlueprintCharacterSyncStateInvalid},
		{nil, "completed", "bad", ErrBlueprintCharacterSyncStateInvalid},
		{nil, "pending", "bad", ErrBlueprintCharacterSyncInputInvalid},
		{nil, "pending", "null", ErrBlueprintCharacterSyncInputInvalid},
		{nil, "pending", "{}", ErrBlueprintCharacterSyncInputInvalid},
		{"{}", "completed", "bad", ErrBlueprintCharacterSyncInputInvalid},
		{"{}", "completed", "[]", ErrGenerationOwnerRequired},
	} {
		mustExec(t, db, "UPDATE blueprint_character_sync_operations SET completion_receipt=?,status=?,character_sync_input=? WHERE operation_id='blueprint-sync-a'", test.completion, test.status, test.input)
		_, err := b.GetSync(ctx, "blueprint-sync-a")
		blueprintAssertError(t, err, test.want)
	}
	// Pending listing also rejects corrupt pending completion state (not silently
	// skipping the row). Completed rows are not listed and need no owner proof.
	mustExec(t, db, `UPDATE blueprint_character_sync_operations SET status='pending',completion_receipt='{}' WHERE operation_id='blueprint-sync-a'`)
	_, err = b.PendingSync(ctx)
	blueprintAssertError(t, err, ErrBlueprintCharacterSyncStateInvalid)
}

func TestBlueprintBackfillStatementAtomicityAndNoDDL(t *testing.T) {
	db, b, _ := blueprintFixture(t)
	ctx := context.Background()
	mustExec(t, db, `INSERT INTO blueprint_commit_operations(operation_id,payload_hash,mode,start_chapter,end_chapter,character_sync_input)
  VALUES('a','hash','replace-range',1,1,'[]'),('b','hash','replace-range',2,2,'[]');
  CREATE TRIGGER blueprint_backfill_block AFTER INSERT ON blueprint_character_sync_operations
  WHEN NEW.blueprint_commit_operation_id='b' BEGIN SELECT RAISE(ABORT,'backfill blocked'); END`)
	if _, err := b.PendingSync(ctx); err == nil || !strings.Contains(err.Error(), "backfill blocked") {
		t.Fatalf("repair error %v", err)
	}
	if narrativeCount(t, db, "blueprint_character_sync_operations") != 0 {
		t.Fatal("partially repaired ledger")
	}
	mustExec(t, db, "DROP TRIGGER blueprint_backfill_block")
	before, err := rrMaps(ctx, db, "SELECT type,name,sql FROM sqlite_master ORDER BY type,name")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.PendingSync(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := rrMaps(ctx, db, "SELECT type,name,sql FROM sqlite_master ORDER BY type,name")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("repository issued DDL %v", err)
	}
}

func TestBlueprintCanonicalASCIIAndBoundaries(t *testing.T) {
	value := map[string]any{"z": 1, "Z": 2, "chapterNumber": json.Number("3"), "2": "two", "10": "ten", "a": true, "A": false}
	canonical, err := blueprintCanonical(value)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := plotJSON(canonical)
	if err != nil || encoded != `{"2":"two","10":"ten","a":true,"A":false,"chapterNumber":3,"z":1,"Z":2}` {
		t.Fatalf("canonical ASCII/index order %s %v", encoded, err)
	}
	request := blueprintRequest("hash", "replace-range", 1, 1)
	first, err := blueprintPayloadHash(request)
	if err != nil {
		t.Fatal(err)
	}
	request["operationId"] = "different"
	second, err := blueprintPayloadHash(request)
	if err != nil || first != second {
		t.Fatalf("operation ID was hashed %s %s %v", first, second, err)
	}
	for _, key := range []string{"角色", "user-guidance", "with space", ""} {
		request = blueprintRequest("hash", "replace-range", 1, 1)
		request["blueprints"].([]map[string]any)[0]["relationshipHints"] = map[string]any{key: "value"}
		_, err = blueprintPayloadHash(request)
		blueprintAssertError(t, err, ErrBlueprintCommitCanonicalKeyUnsupported)
	}
	// Unicode VALUES are ordinary supported BlueprintData, not key collation.
	request = blueprintRequest("hash", "replace-range", 1, 1)
	request["blueprints"].([]map[string]any)[0]["title"] = "中文😀\u2028\u2029<>&"
	if _, err := blueprintPayloadHash(request); err != nil {
		t.Fatal(err)
	}
}

func TestBlueprintReplayLedgerParsingAndCorruptSyncPrecedence(t *testing.T) {
	db, b, _ := blueprintFixture(t)
	ctx := context.Background()
	request := blueprintRequest("replay", "replace-range", 1, 1)
	if _, err := b.CommitRange(ctx, request); err != nil {
		t.Fatal(err)
	}
	var original string
	if err := db.QueryRow("SELECT character_sync_input FROM blueprint_commit_operations WHERE operation_id='replay'").Scan(&original); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"bad", "null", "{}"} {
		mustExec(t, db, "UPDATE blueprint_commit_operations SET character_sync_input=? WHERE operation_id='replay'", raw)
		_, err := b.CommitRange(ctx, request)
		blueprintAssertError(t, err, ErrBlueprintCommitReceiptInvalid)
	}
	// Payload conflict precedes receipt decoding and never repairs editable rows.
	conflict := blueprintRequest("replay", "replace-range", 1, 1)
	conflict["blueprints"].([]map[string]any)[0]["title"] = "different"
	_, err := b.CommitRange(ctx, conflict)
	blueprintAssertError(t, err, ErrBlueprintOperationConflict)
	mustExec(t, db, "UPDATE blueprint_commit_operations SET character_sync_input=? WHERE operation_id='replay'", original)
	mustExec(t, db, "UPDATE blueprint_character_sync_operations SET character_sync_input='bad' WHERE operation_id='blueprint-sync-replay'")
	_, err = b.CommitRange(ctx, request)
	blueprintAssertError(t, err, ErrBlueprintCharacterSyncInputInvalid)
	// Current exact-range failure precedes sync JSON decoding on replay.
	if err := b.Delete(ctx, 1); err != nil {
		t.Fatal(err)
	}
	_, err = b.CommitRange(ctx, request)
	blueprintAssertError(t, err, ErrBlueprintCommitRangeIncomplete)
	if err := b.Upsert(ctx, blueprintData(1)); err != nil {
		t.Fatal(err)
	}
	// Missing ledger-bound sync is lazily backfilled even on replay.
	mustExec(t, db, "DELETE FROM blueprint_character_sync_operations WHERE operation_id='blueprint-sync-replay'")
	receipt, err := b.CommitRange(ctx, request)
	if err != nil || receipt["idempotent"] != true || narrativeCount(t, db, "blueprint_character_sync_operations") != 1 {
		t.Fatalf("repair on replay %v %v", receipt, err)
	}
}

func TestBlueprintNotesCompanionGroupAndNativeArrays(t *testing.T) {
	_, b, _ := blueprintFixture(t)
	ctx := context.Background()
	old := blueprintData(1)
	old["notes"], old["notesUpdatedAt"], old["userGuidance"] = "old", "old-clock", "old-guide"
	if err := b.Upsert(ctx, old); err != nil {
		t.Fatal(err)
	}
	request := blueprintRequest("author", "replace-range", 1, 1)
	item := request["blueprints"].([]map[string]any)[0]
	item["notes"], item["notesUpdatedAt"], item["characters"] = "new", nil, []string{"原生数组😀"}
	item["userGuidance"] = false // false selects the existing author field, not a binding error.
	receipt, err := b.CommitRange(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := receipt["snapshot"].([]map[string]any)
	if snapshot[0]["notes"] != "new" || snapshot[0]["notesUpdatedAt"] != "" || snapshot[0]["userGuidance"] != "old-guide" || !reflect.DeepEqual(snapshot[0]["characters"], []any{"原生数组😀"}) {
		t.Fatalf("author group/native arrays %v", snapshot)
	}
	if _, err := b.CommitRange(ctx, request); err != nil {
		t.Fatalf("native array replay %v", err)
	}
}

func TestBlueprintConcurrentIdempotenceAndConflict(t *testing.T) {
	db, first, path := blueprintFixture(t)
	ctx := context.Background()
	dsn, err := ExistingDSN(path)
	if err != nil {
		t.Fatal(err)
	}
	other, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	second := NewBlueprints(other)
	request := blueprintRequest("concurrent", "replace-range", 2, 3)
	start := make(chan struct{})
	results := make(chan map[string]any, 2)
	failures := make(chan error, 2)
	var group sync.WaitGroup
	for _, b := range []*Blueprints{first, second} {
		group.Add(1)
		go func(b *Blueprints) {
			defer group.Done()
			<-start
			row, err := b.CommitRange(ctx, request)
			if err != nil {
				failures <- err
				return
			}
			results <- row
		}(b)
	}
	close(start)
	group.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	fresh, replay := 0, 0
	for row := range results {
		if row["idempotent"] == true {
			replay++
		} else {
			fresh++
		}
	}
	if fresh != 1 || replay != 1 || narrativeCount(t, db, "blueprint_commit_operations") != 1 || narrativeCount(t, db, "blueprint_character_sync_operations") != 1 {
		t.Fatalf("concurrent receipts fresh=%d replay=%d", fresh, replay)
	}
	// Distinct simultaneous payloads under one new ID produce exactly one winner,
	// one operation conflict, and no mixed per-chapter result.
	requests := []map[string]any{blueprintRequest("race", "replace-range", 4, 5), blueprintRequest("race", "replace-range", 4, 5)}
	for _, item := range requests[1]["blueprints"].([]map[string]any) {
		item["title"] = "alternate"
	}
	start = make(chan struct{})
	failures = make(chan error, 2)
	var success, conflict int
	for i, b := range []*Blueprints{first, second} {
		group.Add(1)
		go func(b *Blueprints, input map[string]any) {
			defer group.Done()
			<-start
			_, err := b.CommitRange(ctx, input)
			failures <- err
		}(b, requests[i])
	}
	close(start)
	group.Wait()
	close(failures)
	for err := range failures {
		if err == nil {
			success++
		} else if errors.Is(err, ErrBlueprintOperationConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("race success=%d conflicts=%d", success, conflict)
	}
	a, err := first.Get(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	b, err := first.Get(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if (a["title"] == "alternate") != (b["title"] == "alternate") {
		t.Fatal("mixed winning payload")
	}
}
