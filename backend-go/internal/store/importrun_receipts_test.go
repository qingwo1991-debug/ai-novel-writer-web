package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
)

// These tests provision a real SQLite database from the pinned asset in
// internal/admission (WriteBaselineV7, baseline_v7_ddl.go:744) instead of
// borrowing a TypeScript oracle, so every receipt statement runs against the
// real schema: the receipt table (:328), its composite primary key, the ledger's
// CHECK and UNIQUE constraints (:262) and the project_core row it writes.
//
// Each write is proved twice over: once by a separate mode=ro connection, and
// once by the file's own bytes, which a refusal must leave untouched.

const (
	receiptRunID     = "run-receipts-1"
	receiptOwner     = "owner-receipts"
	receiptEpoch     = int64(7)
	receiptStage     = "style"
	receiptBatchID   = "done"
	receiptEffectKey = "writing-style"
	receiptKind      = "project-writing-style"
	receiptStyle     = "克制而具体"
	// receiptPayloadJSON is the canonical encoding of a one-key payload, which is
	// what canonicalPayload is supposed to produce.
	receiptPayloadJSON = `{"writingStyle":"克制而具体"}`
)

var receiptExecution = ImportRunExecution{Owner: receiptOwner, Epoch: receiptEpoch}

func receiptFixture(t *testing.T) (*sql.DB, *ImportRuns, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "import receipts #? 中文.db")
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	uri.RawQuery = "mode=rwc&_busy_timeout=5000&_foreign_keys=on"
	seed, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.WriteBaselineV7(path, seed); err != nil {
		_ = seed.Close()
		t.Fatalf("provision baseline v7: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	dsn, err := ExistingDSN(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	return db, NewImportRuns(db), path
}

// receiptSeedRun creates one running import run at the receipt's stage, plus the
// project_core row a real project always has. The lease is live, so a non-replay
// commit is allowed.
func receiptSeedRun(t *testing.T, db *sql.DB, purpose string) {
	t.Helper()
	receiptExec(t, db, `INSERT INTO import_runs (
 id, purpose, root_run_id, effect_namespace, source_fingerprint, manifest_fingerprint,
 locale, stage, status, execution_owner, execution_epoch, lease_expires_at,
 total_chapters, manifest_chapter_count
) VALUES (?, ?, ?, ?, ?, ?, 'zh-CN', ?, 'running', ?, ?, ?, 1, 1)`,
		receiptRunID, purpose, receiptRunID, "import:"+purpose+":"+receiptRunID,
		strings.Repeat("a", 64), strings.Repeat("b", 64), receiptStage,
		receiptOwner, receiptEpoch, time.Now().UnixMilli()+15*60*1000)
	receiptExec(t, db, `INSERT INTO project_core (id) VALUES ('main')`)
}

func receiptExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// receiptReadOnly opens the same file a second time, read-only, so a write is
// proved by a connection that could not have performed it.
func receiptReadOnly(t *testing.T, path string) *sql.DB {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	uri.RawQuery = "mode=ro&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("read-only connection: %v", err)
	}
	return db
}

func receiptQueryRows(t *testing.T, db *sql.DB, query string, args ...any) []map[string]any {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	result := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		entry := make(map[string]any, len(columns))
		for index, column := range columns {
			if raw, ok := values[index].([]byte); ok {
				entry[column] = string(raw)
				continue
			}
			entry[column] = values[index]
		}
		result = append(result, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

// receiptDigest is the whole file's bytes, so a refusal that wrote anything at
// all fails even if it rolled the logical rows back.
func receiptDigest(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// receiptRefusal runs one refused call and proves both the code and that the
// database is byte-identical afterwards.
func receiptRefusal(t *testing.T, path, code string, call func() error) {
	t.Helper()
	before := receiptDigest(t, path)
	err := call()
	if err == nil {
		t.Fatalf("%s: expected a refusal, got none", code)
	}
	if err.Error() != code {
		t.Fatalf("%s: got %q", code, err.Error())
	}
	if after := receiptDigest(t, path); after != before {
		t.Fatalf("%s changed the database", code)
	}
}

func receiptMap(t *testing.T, value any) map[string]any {
	t.Helper()
	record, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected an object, got %T", value)
	}
	return record
}

func receiptText(t *testing.T, row map[string]any, column string) string {
	t.Helper()
	text, ok := row[column].(string)
	if !ok {
		t.Fatalf("%s is %T, want text", column, row[column])
	}
	return text
}

func receiptInt(t *testing.T, row map[string]any, column string) int64 {
	t.Helper()
	number, ok := row[column].(int64)
	if !ok {
		t.Fatalf("%s is %T, want an integer", column, row[column])
	}
	return number
}

func receiptRequest() ImportRunPrepareEffectReceiptRequest {
	return ImportRunPrepareEffectReceiptRequest{
		RunID:     receiptRunID,
		Stage:     receiptStage,
		BatchID:   receiptBatchID,
		EffectKey: receiptEffectKey,
		Kind:      receiptKind,
		Payload:   map[string]any{"writingStyle": receiptStyle},
	}
}

// TestImportRunEffectReceiptRoundTrip is the feature as it is used: prepare, then
// commit, then read it back.
func TestImportRunEffectReceiptRoundTrip(t *testing.T) {
	db, repository, path := receiptFixture(t)
	receiptSeedRun(t, db, "reference")
	ctx := context.Background()

	t.Run("prepare commit get", func(t *testing.T) {
		prepared, err := repository.PrepareEffectReceipt(ctx, receiptExecution, receiptRequest(), nil)
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		receipt := receiptMap(t, prepared)
		if receipt["state"] != "prepared" {
			t.Fatalf("prepared state is %v", receipt["state"])
		}
		if receipt["effectKey"] != receiptEffectKey {
			t.Fatalf("effectKey is %v", receipt["effectKey"])
		}
		if receipt["effectNamespace"] != "import:reference:"+receiptRunID {
			t.Fatalf("effectNamespace is %v", receipt["effectNamespace"])
		}
		if _, present := receipt["effectReceipt"]; present {
			t.Fatal("a prepared receipt must not carry an effect")
		}
		sum := sha256.Sum256([]byte(receiptPayloadJSON))
		if receipt["payloadHash"] != hex.EncodeToString(sum[:]) {
			t.Fatalf("payloadHash is %v", receipt["payloadHash"])
		}

		readOnly := receiptReadOnly(t, path)
		rows := receiptQueryRows(t, readOnly, `SELECT effect_namespace, effect_key, stage, batch_id, kind,
 payload_json, payload_hash, state, effect_receipt_json
 FROM import_run_receipts WHERE run_id = ?`, receiptRunID)
		if len(rows) != 1 {
			t.Fatalf("prepared receipt rows: %d", len(rows))
		}
		if receiptText(t, rows[0], "effect_namespace") != "import:reference:"+receiptRunID ||
			receiptText(t, rows[0], "effect_key") != receiptEffectKey ||
			receiptText(t, rows[0], "stage") != receiptStage ||
			receiptText(t, rows[0], "batch_id") != receiptBatchID ||
			receiptText(t, rows[0], "kind") != receiptKind ||
			receiptText(t, rows[0], "state") != "prepared" {
			t.Fatalf("prepared receipt row: %v", rows[0])
		}
		if receiptText(t, rows[0], "payload_json") != receiptPayloadJSON {
			t.Fatalf("payload_json is %q", rows[0]["payload_json"])
		}
		if rows[0]["effect_receipt_json"] != nil {
			t.Fatalf("a prepared receipt stored an effect: %v", rows[0]["effect_receipt_json"])
		}
		ledger := receiptQueryRows(t, readOnly, `SELECT state, evidence_kind, diagnostic_code
 FROM import_effect_ledger WHERE run_id = ? AND stage = ? AND batch_id = ?`,
			receiptRunID, receiptStage, receiptBatchID)
		if len(ledger) != 1 || receiptText(t, ledger[0], "state") != "partial" ||
			receiptText(t, ledger[0], "evidence_kind") != "receipt" ||
			receiptText(t, ledger[0], "diagnostic_code") != "EFFECT_PREPARED_NOT_COMMITTED" {
			t.Fatalf("prepared ledger row: %v", ledger)
		}

		committed, err := repository.CommitEffectReceipt(ctx, receiptRunID, receiptStage, receiptBatchID,
			receiptExecution, nil)
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
		if committed["cancelApplied"] != false {
			t.Fatalf("cancelApplied is %v", committed["cancelApplied"])
		}
		effect := receiptMap(t, committed["receipt"])
		if effect["state"] != "committed" {
			t.Fatalf("committed state is %v", effect["state"])
		}
		stored := receiptMap(t, effect["effectReceipt"])
		if stored["writingStyle"] != receiptStyle {
			t.Fatalf("effect writingStyle is %v", stored["writingStyle"])
		}
		run := receiptMap(t, committed["run"])
		if run["stage"] != receiptStage || run["status"] != "running" {
			t.Fatalf("run after commit: %v", run)
		}
		batches, ok := run["completedBatches"].(map[string][]string)
		if !ok || len(batches[receiptStage]) != 1 || batches[receiptStage][0] != receiptBatchID {
			t.Fatalf("completedBatches after commit: %v", run["completedBatches"])
		}

		rows = receiptQueryRows(t, readOnly, `SELECT state, effect_receipt_json
 FROM import_run_receipts WHERE run_id = ?`, receiptRunID)
		if len(rows) != 1 || receiptText(t, rows[0], "state") != "committed" {
			t.Fatalf("committed receipt row: %v", rows)
		}
		if receiptText(t, rows[0], "effect_receipt_json") != receiptPayloadJSON {
			t.Fatalf("effect_receipt_json is %q", rows[0]["effect_receipt_json"])
		}
		core := receiptQueryRows(t, readOnly, `SELECT writing_style FROM project_core WHERE id = 'main'`)
		if len(core) != 1 || receiptText(t, core[0], "writing_style") != receiptStyle {
			t.Fatalf("project_core after commit: %v", core)
		}
		ledger = receiptQueryRows(t, readOnly, `SELECT state, diagnostic_code
 FROM import_effect_ledger WHERE run_id = ? AND stage = ? AND batch_id = ?`,
			receiptRunID, receiptStage, receiptBatchID)
		if len(ledger) != 1 || receiptText(t, ledger[0], "state") != "complete" ||
			receiptText(t, ledger[0], "diagnostic_code") != "" {
			t.Fatalf("committed ledger row: %v", ledger)
		}
		runRow := receiptQueryRows(t, readOnly, `SELECT status, resumable FROM import_runs WHERE id = ?`, receiptRunID)
		if len(runRow) != 1 || receiptText(t, runRow[0], "status") != "running" ||
			receiptInt(t, runRow[0], "resumable") != 1 {
			t.Fatalf("import_runs after commit: %v", runRow)
		}

		served, err := repository.GetEffectReceipt(ctx, receiptRunID, receiptStage, receiptBatchID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		servedReceipt := receiptMap(t, served)
		if servedReceipt["state"] != "committed" || servedReceipt["payloadHash"] != effect["payloadHash"] {
			t.Fatalf("served receipt: %v", servedReceipt)
		}
		servedEffect := receiptMap(t, servedReceipt["effectReceipt"])
		if servedEffect["writingStyle"] != receiptStyle {
			t.Fatalf("served effect: %v", servedEffect)
		}

		// A replay returns before the lease, the checkpoint and the effect, so a
		// stale lease still replays and the author's later edit must survive.
		receiptExec(t, db, `UPDATE project_core SET writing_style = ? WHERE id = 'main'`, "作者自己改的文风")
		replayed, err := repository.CommitEffectReceipt(ctx, receiptRunID, receiptStage, receiptBatchID,
			ImportRunExecution{Owner: "stale-owner", Epoch: 999}, nil)
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if receiptMap(t, replayed["receipt"])["state"] != "committed" {
			t.Fatalf("replay receipt: %v", replayed["receipt"])
		}
		if receiptMap(t, replayed["run"])["id"] != receiptRunID {
			t.Fatalf("replay run: %v", replayed["run"])
		}
		after := receiptQueryRows(t, readOnly, `SELECT writing_style FROM project_core WHERE id = 'main'`)
		if len(after) != 1 || receiptText(t, after[0], "writing_style") != "作者自己改的文风" {
			t.Fatalf("a replay re-applied the effect: %v", after)
		}
	})
}

// TestImportRunEffectReceiptAuthorFillEmpty proves the author-manuscript arm:
// a run that only fills blanks keeps the author's stated style and offers the
// inference as a suggestion instead.
func TestImportRunEffectReceiptAuthorFillEmpty(t *testing.T) {
	db, repository, path := receiptFixture(t)
	receiptSeedRun(t, db, "author-manuscript")
	ctx := context.Background()
	receiptExec(t, db, `UPDATE project_core SET writing_style = ? WHERE id = 'main'`, "作者自定的文风")

	if _, err := repository.CommitEffectReceipt(ctx, receiptRunID, receiptStage, receiptBatchID,
		receiptExecution, nil); err == nil {
		t.Fatal("committing an unprepared receipt must fail")
	}
	if _, err := repository.PrepareEffectReceipt(ctx, receiptExecution, receiptRequest(), nil); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	committed, err := repository.CommitEffectReceipt(ctx, receiptRunID, receiptStage, receiptBatchID,
		receiptExecution, nil)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	effect := receiptMap(t, receiptMap(t, committed["receipt"])["effectReceipt"])
	if effect["mode"] != "fill-empty" {
		t.Fatalf("author mode is %v", effect["mode"])
	}
	if effect["writingStyle"] != "作者自定的文风" {
		t.Fatalf("author style was overwritten: %v", effect["writingStyle"])
	}
	suggestions, ok := effect["suggestions"].([]any)
	if !ok || len(suggestions) != 1 {
		t.Fatalf("suggestions: %v", effect["suggestions"])
	}
	suggestion := receiptMap(t, suggestions[0])
	if suggestion["field"] != "writingStyle" || suggestion["current"] != "作者自定的文风" ||
		suggestion["inferred"] != receiptStyle {
		t.Fatalf("suggestion: %v", suggestion)
	}
	readOnly := receiptReadOnly(t, path)
	core := receiptQueryRows(t, readOnly, `SELECT writing_style FROM project_core WHERE id = 'main'`)
	if len(core) != 1 || receiptText(t, core[0], "writing_style") != "作者自定的文风" {
		t.Fatalf("project_core after a fill-empty commit: %v", core)
	}
}

// TestImportRunEffectReceiptKeyInvalid proves IMPORT_RUN_RECEIPT_KEY_INVALID
// fires where the reference fires it: on the trimmed key and batch id only, and
// before the transaction starts.
func TestImportRunEffectReceiptKeyInvalid(t *testing.T) {
	db, repository, path := receiptFixture(t)
	receiptSeedRun(t, db, "reference")
	ctx := context.Background()

	blank := receiptRequest()
	blank.EffectKey = "   "
	receiptRefusal(t, path, "IMPORT_RUN_RECEIPT_KEY_INVALID", func() error {
		_, err := repository.PrepareEffectReceipt(ctx, receiptExecution, blank, nil)
		return err
	})

	// The key is trimmed and checked before anything reads the run, so a run that
	// does not exist still answers KEY_INVALID.
	missing := receiptRequest()
	missing.EffectKey = ""
	missing.RunID = "no-such-run"
	receiptRefusal(t, path, "IMPORT_RUN_RECEIPT_KEY_INVALID", func() error {
		_, err := repository.PrepareEffectReceipt(ctx, receiptExecution, missing, nil)
		return err
	})

	longKey := receiptRequest()
	longKey.EffectKey = strings.Repeat("k", 201)
	receiptRefusal(t, path, "IMPORT_RUN_RECEIPT_KEY_INVALID", func() error {
		_, err := repository.PrepareEffectReceipt(ctx, receiptExecution, longKey, nil)
		return err
	})

	longBatch := receiptRequest()
	longBatch.BatchID = strings.Repeat("b", 161)
	receiptRefusal(t, path, "IMPORT_RUN_RECEIPT_KEY_INVALID", func() error {
		_, err := repository.PrepareEffectReceipt(ctx, receiptExecution, longBatch, nil)
		return err
	})

	// 200 units is legal, so the very same call must get past the key gate and
	// fail later, as a key that is not the expected one.
	atLimit := receiptRequest()
	atLimit.EffectKey = strings.Repeat("k", 200)
	receiptRefusal(t, path, "IMPORT_RUN_RECEIPT_CORRUPT", func() error {
		_, err := repository.PrepareEffectReceipt(ctx, receiptExecution, atLimit, nil)
		return err
	})
}

// TestImportRunEffectReceiptConflict proves IMPORT_RUN_RECEIPT_CONFLICT: the row
// exists and disagrees, and no identical prepare is a conflict.
func TestImportRunEffectReceiptConflict(t *testing.T) {
	db, repository, path := receiptFixture(t)
	receiptSeedRun(t, db, "reference")
	ctx := context.Background()

	if _, err := repository.PrepareEffectReceipt(ctx, receiptExecution, receiptRequest(), nil); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	// An identical prepare replays the row and writes nothing, so it is not a
	// conflict and not a second insert.
	before := receiptDigest(t, path)
	replayed, err := repository.PrepareEffectReceipt(ctx, receiptExecution, receiptRequest(), nil)
	if err != nil {
		t.Fatalf("identical prepare: %v", err)
	}
	if receiptMap(t, replayed)["state"] != "prepared" {
		t.Fatalf("replayed prepare: %v", replayed)
	}
	if after := receiptDigest(t, path); after != before {
		t.Fatal("an identical prepare wrote")
	}

	differentPayload := receiptRequest()
	differentPayload.Payload = map[string]any{"writingStyle": "另一种文风"}
	receiptRefusal(t, path, "IMPORT_RUN_RECEIPT_CONFLICT", func() error {
		_, err := repository.PrepareEffectReceipt(ctx, receiptExecution, differentPayload, nil)
		return err
	})

	differentKey := receiptRequest()
	differentKey.EffectKey = "global-facts"
	receiptRefusal(t, path, "IMPORT_RUN_RECEIPT_CONFLICT", func() error {
		_, err := repository.PrepareEffectReceipt(ctx, receiptExecution, differentKey, nil)
		return err
	})

	differentKind := receiptRequest()
	differentKind.Kind = "author-finalized-batch"
	receiptRefusal(t, path, "IMPORT_RUN_RECEIPT_STAGE_MISMATCH", func() error {
		_, err := repository.PrepareEffectReceipt(ctx, receiptExecution, differentKind, nil)
		return err
	})
}

// TestImportRunEffectReceiptNotFound proves IMPORT_RUN_RECEIPT_NOT_FOUND and its
// distinction from the conflict above: the run read comes first, the receipt read
// second, and only a row that is absent is "not found".
func TestImportRunEffectReceiptNotFound(t *testing.T) {
	db, repository, path := receiptFixture(t)
	receiptSeedRun(t, db, "reference")
	ctx := context.Background()

	receiptRefusal(t, path, "IMPORT_RUN_RECEIPT_NOT_FOUND", func() error {
		_, err := repository.CommitEffectReceipt(ctx, receiptRunID, receiptStage, receiptBatchID, receiptExecution, nil)
		return err
	})

	receiptRefusal(t, path, "IMPORT_RUN_NOT_FOUND", func() error {
		_, err := repository.CommitEffectReceipt(ctx, "no-such-run", receiptStage, receiptBatchID, receiptExecution, nil)
		return err
	})

	// A receipt prepared for one stage is not found for another, because the
	// receipt read is scoped by the same three keys the key is derived from.
	if _, err := repository.PrepareEffectReceipt(ctx, receiptExecution, receiptRequest(), nil); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	receiptRefusal(t, path, "IMPORT_RUN_RECEIPT_NOT_FOUND", func() error {
		_, err := repository.CommitEffectReceipt(ctx, receiptRunID, "global", receiptBatchID, receiptExecution, nil)
		return err
	})
}

// TestImportRunEffectReceiptGetAnswers proves what get answers when there is
// nothing to answer with: null, never a refusal, and never a write.
func TestImportRunEffectReceiptGetAnswers(t *testing.T) {
	db, repository, path := receiptFixture(t)
	receiptSeedRun(t, db, "reference")
	ctx := context.Background()

	before := receiptDigest(t, path)
	served, err := repository.GetEffectReceipt(ctx, receiptRunID, receiptStage, receiptBatchID)
	if err != nil {
		t.Fatalf("a missing receipt is null, not a refusal: %v", err)
	}
	if served != nil {
		t.Fatalf("a missing receipt answered %v", served)
	}
	served, err = repository.GetEffectReceipt(ctx, "no-such-run", receiptStage, receiptBatchID)
	if err != nil || served != nil {
		t.Fatalf("a missing receipt is null even without a run: %v %v", served, err)
	}
	if after := receiptDigest(t, path); after != before {
		t.Fatal("a receipt read wrote")
	}

	// A row that exists but does not re-derive is corrupt, not null: the payload
	// hash no longer matches the payload.
	receiptExec(t, db, `INSERT INTO import_run_receipts (
 run_id, schema_version, effect_namespace, effect_key, stage, batch_id, kind, payload_json, payload_hash
) VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?)`,
		receiptRunID, "import:reference:"+receiptRunID, receiptEffectKey,
		receiptStage, receiptBatchID, receiptKind, receiptPayloadJSON, strings.Repeat("0", 64))
	receiptRefusal(t, path, "IMPORT_RUN_RECEIPT_CORRUPT", func() error {
		_, err := repository.GetEffectReceipt(ctx, receiptRunID, receiptStage, receiptBatchID)
		return err
	})
}
