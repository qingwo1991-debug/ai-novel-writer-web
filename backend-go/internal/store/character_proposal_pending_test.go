package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// These tests pin the import arm of the character-proposal proof
// (generation-character-proposal-proof.ts:66-105) with synthetic rows. The
// command-level test proves the finalized-generation arm end to end; the import
// arm has no unit coverage of its own, and the accept/reject boundary is exactly
// the store semantics the pending-finalized reads serve.
//
// The receipt's proposalSource and the effect's payload are both hashed with the
// package's own JSON.stringify-compatible encoder, so the assertions below also
// pin the byte identity the desktop's textHash reproduces.

func pendingFinalizedImportDB(t *testing.T) *sql.DB {
	t.Helper()
	db, _, _ := narrativeFixture(t)
	return db
}

func pendingFinalizedImportSource(operationID string) map[string]any {
	return map[string]any{"kind": "import", "operationId": operationID}
}

// pendingFinalizedImportEntries is one receipt's characterEntries. Every entry has
// to carry a relationships key: the proof destructures it out and refuses an entry
// without one.
func pendingFinalizedImportEntries() []any {
	return []any{plotObject{
		{"name", "阿青"},
		{"role", "protagonist"},
		{"relationships", []any{plotObject{
			{"target", "白露"},
			{"relation", "旧识"},
		}}},
	}}
}

func pendingFinalizedImportJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := plotJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func pendingFinalizedImportPayloadHash(t *testing.T, proposalSource any) string {
	t.Helper()
	return summaryHash(pendingFinalizedImportJSON(t, proposalSource))
}

func pendingFinalizedImportReceipt(t *testing.T, operationID, payloadHash string, proposalSource any) string {
	t.Helper()
	return pendingFinalizedImportJSON(t, plotObject{
		{"operationId", operationID},
		{"payloadHash", payloadHash},
		{"proposalSource", proposalSource},
	})
}

func pendingFinalizedImportSeed(t *testing.T, db *sql.DB, operationID, payloadHash, receiptJSON string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO import_global_fact_operations(operation_id,payload_hash,receipt_json) VALUES(?,?,?)`,
		operationID, payloadHash, receiptJSON)
}

func pendingFinalizedImportProve(t *testing.T, db *sql.DB, projectID string, source map[string]any) (map[string]any, error) {
	t.Helper()
	return NewPendingFinalized(db).Prove(context.Background(), projectID, source)
}

// TestPendingFinalizedImportProofAcceptsWithoutEffect is the provenance-null arm
// (:82-86): a receipt that hashes to its row and carries one entry, and no effect
// row naming the operation, proves to items, a source hash and no provenance.
func TestPendingFinalizedImportProofAcceptsWithoutEffect(t *testing.T) {
	db := pendingFinalizedImportDB(t)
	projectID := "ci-project-import"
	operationID := "novel-import-global-ci-accept"
	entries := pendingFinalizedImportEntries()
	proposalSource := plotObject{{"characterEntries", entries}}
	payloadHash := pendingFinalizedImportPayloadHash(t, proposalSource)
	pendingFinalizedImportSeed(t, db, operationID, payloadHash,
		pendingFinalizedImportReceipt(t, operationID, payloadHash, proposalSource))

	proof, err := pendingFinalizedImportProve(t, db, projectID, pendingFinalizedImportSource(operationID))
	if err != nil {
		t.Fatalf("prove accepted source: %v", err)
	}
	items, ok := proof["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("proof items = %#v, want one item", proof["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("proof item is %T, want the decoded object", items[0])
	}
	if got := item["selectionKey"]; got != "import:0" {
		t.Fatalf("selectionKey = %v, want import:0", got)
	}
	if got := item["sourceId"]; got != operationID+":0" {
		t.Fatalf("sourceId = %v, want %v", got, operationID+":0")
	}
	fields, _ := item["fields"].(map[string]any)
	if fields["name"] != "阿青" || fields["role"] != "protagonist" {
		t.Fatalf("fields = %#v, want the entry's two static fields", fields)
	}
	if _, present := fields["relationships"]; present {
		t.Fatalf("relationships leaked into fields: %#v", fields)
	}
	if got := proof["sourceHash"]; got != summaryHash(pendingFinalizedImportJSON(t, []any{payloadHash, entries})) {
		t.Fatalf("sourceHash = %v, want the hash of [payload_hash, characterEntries]", got)
	}
	if provenance, present := proof["provenance"]; !present || provenance != nil {
		t.Fatalf("provenance = %#v, want null", proof["provenance"])
	}
}

// TestPendingFinalizedImportProofAcceptsEffectWithoutHandle is the second
// provenance-null arm (:90): a committed effect row whose payload names the same
// operation and entries but carries no generationRunHandle still proves, and the
// provenance stays null rather than guessed.
func TestPendingFinalizedImportProofAcceptsEffectWithoutHandle(t *testing.T) {
	db := pendingFinalizedImportDB(t)
	projectID := "ci-project-import"
	operationID := "novel-import-global-ci-effect"
	entries := pendingFinalizedImportEntries()
	proposalSource := plotObject{{"characterEntries", entries}}
	payloadHash := pendingFinalizedImportPayloadHash(t, proposalSource)
	pendingFinalizedImportSeed(t, db, operationID, payloadHash,
		pendingFinalizedImportReceipt(t, operationID, payloadHash, proposalSource))

	payload := map[string]any{"operationId": operationID, "characterEntries": entries}
	payloadText := pendingFinalizedImportJSON(t, payload)
	mustExec(t, db, `INSERT INTO import_runs(id,purpose,root_run_id,effect_namespace,source_fingerprint,
 manifest_fingerprint,locale,total_chapters,manifest_chapter_count) VALUES(?,?,?,?,?,?,?,?,?)`,
		"ci-import-run", "reference", "ci-import-root", "ci-ns", "src-fp", "man-fp", "zh-CN", 1, 1)
	mustExec(t, db, `INSERT INTO import_run_receipts(run_id,schema_version,effect_namespace,effect_key,
 stage,batch_id,kind,payload_json,payload_hash,state) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		"ci-import-run", 1, "ci-ns", "ci-key", "global", "ci-batch", "project-global-facts",
		payloadText, summaryHash(payloadText), "committed")

	proof, err := pendingFinalizedImportProve(t, db, projectID, pendingFinalizedImportSource(operationID))
	if err != nil {
		t.Fatalf("prove accepted source: %v", err)
	}
	if proof["sourceHash"] != summaryHash(pendingFinalizedImportJSON(t, []any{payloadHash, entries})) {
		t.Fatalf("sourceHash = %v, want the receipt's own hash", proof["sourceHash"])
	}
	if provenance, present := proof["provenance"]; !present || provenance != nil {
		t.Fatalf("provenance = %#v, want null for an effect without a run handle", proof["provenance"])
	}
}

// TestPendingFinalizedImportProofMissingOperation is :69: a source naming an
// operation the global-facts ledger never recorded.
func TestPendingFinalizedImportProofMissingOperation(t *testing.T) {
	db := pendingFinalizedImportDB(t)
	_, err := pendingFinalizedImportProve(t, db, "ci-project-import",
		pendingFinalizedImportSource("novel-import-global-ci-missing"))
	if !errors.Is(err, ErrCharacterProposalPendingSourceMissing) {
		t.Fatalf("prove = %v, want %v", err, ErrCharacterProposalPendingSourceMissing)
	}
}

// TestPendingFinalizedImportProofReceiptPayloadHashDisagree is :72: the receipt's
// own payloadHash field disagrees with the row's payload_hash column.
func TestPendingFinalizedImportProofReceiptPayloadHashDisagree(t *testing.T) {
	db := pendingFinalizedImportDB(t)
	operationID := "novel-import-global-ci-payload"
	entries := pendingFinalizedImportEntries()
	proposalSource := plotObject{{"characterEntries", entries}}
	payloadHash := pendingFinalizedImportPayloadHash(t, proposalSource)
	pendingFinalizedImportSeed(t, db, operationID, strings.Repeat("a", 64),
		pendingFinalizedImportReceipt(t, operationID, payloadHash, proposalSource))

	_, err := pendingFinalizedImportProve(t, db, "ci-project-import", pendingFinalizedImportSource(operationID))
	if !errors.Is(err, ErrCharacterProposalPendingSourceInvalid) {
		t.Fatalf("prove = %v, want %v", err, ErrCharacterProposalPendingSourceInvalid)
	}
}

// TestPendingFinalizedImportProofReceiptOperationDisagree is :72: the receipt
// names an operation other than the one the source asked for.
func TestPendingFinalizedImportProofReceiptOperationDisagree(t *testing.T) {
	db := pendingFinalizedImportDB(t)
	operationID := "novel-import-global-ci-operation"
	entries := pendingFinalizedImportEntries()
	proposalSource := plotObject{{"characterEntries", entries}}
	payloadHash := pendingFinalizedImportPayloadHash(t, proposalSource)
	pendingFinalizedImportSeed(t, db, operationID, payloadHash,
		pendingFinalizedImportReceipt(t, "other-operation", payloadHash, proposalSource))

	_, err := pendingFinalizedImportProve(t, db, "ci-project-import", pendingFinalizedImportSource(operationID))
	if !errors.Is(err, ErrCharacterProposalPendingSourceInvalid) {
		t.Fatalf("prove = %v, want %v", err, ErrCharacterProposalPendingSourceInvalid)
	}
}

// TestPendingFinalizedImportProofReceiptSourceHashDisagree is :73: the row's
// payload_hash is no longer the hash of the receipt's proposalSource.
func TestPendingFinalizedImportProofReceiptSourceHashDisagree(t *testing.T) {
	db := pendingFinalizedImportDB(t)
	operationID := "novel-import-global-ci-source-hash"
	entries := pendingFinalizedImportEntries()
	proposalSource := plotObject{{"characterEntries", entries}}
	bad := strings.Repeat("b", 64)
	pendingFinalizedImportSeed(t, db, operationID, bad,
		pendingFinalizedImportReceipt(t, operationID, bad, proposalSource))

	_, err := pendingFinalizedImportProve(t, db, "ci-project-import", pendingFinalizedImportSource(operationID))
	if !errors.Is(err, ErrCharacterProposalPendingSourceInvalid) {
		t.Fatalf("prove = %v, want %v", err, ErrCharacterProposalPendingSourceInvalid)
	}
}

// TestPendingFinalizedImportProofSourceShapeRejects is :67 and :136-139: the
// source object has to be exactly the two keys the import arm reads.
func TestPendingFinalizedImportProofSourceShapeRejects(t *testing.T) {
	db := pendingFinalizedImportDB(t)
	operationID := "novel-import-global-ci-shape"
	for _, source := range []map[string]any{
		{"kind": "import", "operationId": operationID, "extra": true},
		{"kind": "import"},
	} {
		_, err := pendingFinalizedImportProve(t, db, "ci-project-import", source)
		if !errors.Is(err, ErrCharacterProposalPendingSourceInvalid) {
			t.Fatalf("prove %#v = %v, want %v", source, err, ErrCharacterProposalPendingSourceInvalid)
		}
	}
}
