//go:build linux

package main

// This file proves the two `character-proposal` pending-finalized names over the
// real HTTP handler and the frozen session:
//
//	character-proposal:list-pending-finalized  the summary projection
//	character-proposal:read-pending-finalized  one proven batch, and its refusals
//
// The fixture stages two `finalized-generation` proposals directly. This lane
// cannot stage one - the only proposal writer it serves is the import commit
// (internal/store/dbmaintenance.go:671), and no begin in it freezes the author
// input proveFinalizedCharacterGeneration reads - so the rows are seeded, and
// the proof they carry is computed by the store's own proven function. The point
// of the test is not that the seed matches some hand-written digest: it is that
// the served read re-proves the row from the generation ledger and the
// finalization receipt and answers the stored batch, and that the list pages the
// two rows it proves.
//
// Every cited reference line is re-read through productionSeamContains, which
// ignores white space because CI formats the sources before running the tests.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

const (
	// The open nonce and the run epoch. The run epoch is deliberately not the
	// session lease: a run outlives the lease it was admitted under, and the
	// proof compares the handle with the run's binding, never with the lease.
	productionCharacterProposalPendingOpenNonce = "character-proposal-pending-open-"
	productionCharacterProposalPendingEpoch     = "lease-0000000001-character-proposal-pending"
	productionCharacterProposalPendingRunNonce  = "ci-character-proposal-pending-run"
	productionCharacterProposalPendingSnapshot  = "ci-character-proposal-pending-snapshot"

	// The finalization the proposals are built on, and its frozen prose.
	productionCharacterProposalPendingDraftID      = 900
	productionCharacterProposalPendingChapter      = 2
	productionCharacterProposalPendingRevision     = 1
	productionCharacterProposalPendingTitle        = "第二章 门"
	productionCharacterProposalPendingContent      = "第二章：他推开门。门外有人敲门。"
	productionCharacterProposalPendingFinalization = "ci-character-proposal-pending-finalization-1"

	// The model receipt the provenance names, and the two artifact bodies. Both
	// are complete state answers with two occurrences, so each proposal carries
	// two items and a summary that could not be fabricated from the request.
	productionCharacterProposalPendingModelRevision = "c1c2c3c4c1c2c3c4c1c2c3c4c1c2c3c4c1c2c3c4c1c2c3c4c1c2c3c4c1c2c3c4"
	productionCharacterProposalPendingAnswerOne     = `{"updates":[{"name":"沉舟","currentState":{"location":"门内"},"evidence":{"text":"他推开门"}},{"name":"白露","currentState":{"location":"钥匙铺"},"evidence":{"text":"门外有人敲门"}}]}`
	productionCharacterProposalPendingAnswerTwo     = `{"updates":[{"name":"程守","currentState":{"location":"门外"},"evidence":{"text":"他推开门"}},{"name":"守夜人","currentState":{"mentalState":"警惕"},"evidence":{"text":"门外有人敲门"}}]}`

	// The two batch ids. The reference derives `cpb:` plus a hash of the project
	// and the source; the read never re-derives it, it matches the row's own id,
	// so a readable fixture id is honest here and keeps the list order explicit.
	productionCharacterProposalPendingBatchOne = "cpb:pending-finalized-one"
	productionCharacterProposalPendingBatchTwo = "cpb:pending-finalized-two"

	productionCharacterProposalPendingStaleLease = "stale-character-proposal-pending-lease"
	productionCharacterProposalPendingForeignDir = "character-proposal-pending-foreign"
)

// productionCharacterProposalPendingLane is one admitted project, two seeded
// proposals and the two-name binder installed on the same server type the
// production lane mounts.
type productionCharacterProposalPendingLane struct {
	projectPath string
	database    string
	author      *productionDurableClient
	frozen      map[string]any
	durable     *productionDurableOpen
}

// productionCharacterProposalPendingFixture opens the disposable root, seeds the
// finalization chain and the two proposals before admission, then mounts
// project:open and this binder.
func productionCharacterProposalPendingFixture(t *testing.T) *productionCharacterProposalPendingLane {
	t.Helper()
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	productionCharacterProposalPendingSeed(t, projectPath)
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatal(err)
	}
	open, err := openProduction(context.Background(), layout, productionConfig{
		Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = open.Close() })
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-character-proposal-pending")
	if err := server.BindProjects(open.durable); err != nil {
		t.Fatal(err)
	}
	if err := newProductionCharacterProposalPending(open.durable).Bind(server); err != nil {
		t.Fatal(err)
	}
	author, frozen := productionExecuteSession(t, server, projectPath, productionCharacterProposalPendingOpenNonce)
	return &productionCharacterProposalPendingLane{
		projectPath: projectPath,
		database:    filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName),
		author:      author,
		frozen:      frozen,
		durable:     open.durable,
	}
}

// stale is the three-key context a server that never issued this lease would
// have: the same project, a lease it cannot prove.
func (l *productionCharacterProposalPendingLane) stale() map[string]any {
	return map[string]any{"projectId": l.frozen["projectId"], "projectPath": l.projectPath,
		"leaseId": productionCharacterProposalPendingStaleLease}
}

// foreign is a well-formed context naming a project this lane never opened.
func (l *productionCharacterProposalPendingLane) foreign() map[string]any {
	return map[string]any{"projectId": l.frozen["projectId"], "projectPath": l.foreignPath(),
		"leaseId": l.frozen["leaseId"]}
}

func (l *productionCharacterProposalPendingLane) foreignPath() string {
	return filepath.Join(filepath.Dir(l.projectPath), productionCharacterProposalPendingForeignDir)
}

// bytes reads the admitted database file, the baseline a refusal must leave
// alone.
func (l *productionCharacterProposalPendingLane) bytes(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(l.database)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// seed writes the identity singletons the identity schema needs, the finalization
// chain the current-source assertion reads, one finalized-character-state run with
// two settled artifacts, and the two proposals whose proofs the store computes.
func productionCharacterProposalPendingSeed(t *testing.T, projectPath string) {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = db.Close()
		}
	}()
	db.SetMaxOpenConns(1)
	contentHash := sha256Hex(productionCharacterProposalPendingContent)
	statements := []string{
		"DELETE FROM character_identity_proposals",
		"DELETE FROM character_relationships",
		"DELETE FROM character_identity_approvals",
		"DELETE FROM character_identity_origins",
		"DELETE FROM character_aliases",
		"DELETE FROM characters",
		"DELETE FROM character_roster_operations",
		"UPDATE project_core SET characters_arch='', character_states='{}' WHERE id='main'",
		`INSERT OR REPLACE INTO character_roster_meta
 (id, schema_version, revision, migration_state, legacy_markdown, projection_hash, fact_hash)
 VALUES ('main', 1, 0, 'empty', '', '', '')`,
		`INSERT OR REPLACE INTO character_identity_meta (id, revision, legacy_count) VALUES ('main', 0, 0)`,
		// The finalization chain proveFinalizedCharacterGeneration's context read
		// joins: one finalized draft whose prose still hashes to the receipt.
		fmt.Sprintf(`INSERT OR REPLACE INTO contents (id, body) VALUES (%d, '%s')`,
			productionCharacterProposalPendingDraftID, productionCharacterProposalPendingContent),
		fmt.Sprintf(`INSERT OR REPLACE INTO drafts (id, chapter_number, version, status, source,
 content_id, word_count, source_dependencies, created_at, updated_at)
 VALUES (%d, %d, %d, 'finalized', 'write', %d, %d, '[]',
 '2024-05-06 09:10:11', '2024-05-06 09:10:12')`,
			productionCharacterProposalPendingDraftID, productionCharacterProposalPendingChapter,
			productionCharacterProposalPendingRevision, productionCharacterProposalPendingDraftID,
			len(productionCharacterProposalPendingContent)),
		fmt.Sprintf(`INSERT OR REPLACE INTO finalization_outbox (finalization_id, draft_id,
 chapter_number, chapter_title, content_hash, content_revision, content_snapshot,
 target_file_name, knowledge_document_id, publication_status, last_error)
 VALUES ('%s', %d, %d, '%s', '%s', %d, '%s', '%s', '', 'published', '')`,
			productionCharacterProposalPendingFinalization, productionCharacterProposalPendingDraftID,
			productionCharacterProposalPendingChapter, productionCharacterProposalPendingTitle,
			contentHash, productionCharacterProposalPendingRevision,
			productionCharacterProposalPendingContent, productionCharacterProposalPendingTitle+".txt"),
		`INSERT OR REPLACE INTO continuity_projection_meta (id, generation, stale_from_chapter)
 VALUES ('main', 0, NULL)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("seed pending-finalized rows: %v", err)
		}
	}
	// The frozen context is one author input string, so its hash is over exactly
	// the bytes the run froze.
	serialized, err := json.Marshal(map[string]any{
		"projectId": productionEditorProjectID,
		"epoch":     productionCharacterProposalPendingEpoch,
		"source": map[string]any{
			"draftId":        productionCharacterProposalPendingDraftID,
			"finalizationId": productionCharacterProposalPendingFinalization,
			"chapterNumber":  productionCharacterProposalPendingChapter,
			"contentHash":    contentHash,
		},
		"content":              productionCharacterProposalPendingContent,
		"projectionGeneration": 0,
		"identityRevision":     0,
		"identityStatus":       "legacy",
		"sourceOrder": map[string]any{
			"continuityEpoch":                   productionEditorProjectID + ":0",
			"chapterNumber":                     productionCharacterProposalPendingChapter,
			"authoritativeFinalizationRevision": productionCharacterProposalPendingRevision,
		},
		"characters": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ordinal := 0
	ledger := store.NewGenerationLedger(db, func() int64 { return 1700000000000 }, func() string {
		ordinal++
		return fmt.Sprintf("production-character-proposal-pending-%02d", ordinal)
	})
	request, err := json.Marshal(map[string]any{
		"projectId":         productionEditorProjectID,
		"epoch":             productionCharacterProposalPendingEpoch,
		"operation":         "finalized-character-state",
		"uiActionNonce":     productionCharacterProposalPendingRunNonce,
		"frozenInputHash":   strings.Repeat("a", 64),
		"fingerprint":       productionCharacterProposalPendingFingerprint(),
		"contextSnapshotId": productionCharacterProposalPendingSnapshot,
		"sourceManifest": map[string]any{
			"operation": "finalized-character-state",
			"authorInputs": []any{map[string]any{
				"id":   "finalized-character-context",
				"text": string(serialized),
			}},
			"finalizedCharacterContextHash": sha256Hex(string(serialized)),
			"selectedFinalizedDraftIds":     []any{productionCharacterProposalPendingDraftID},
			"modelReceipt": map[string]any{
				"modelRevision": productionCharacterProposalPendingModelRevision,
			},
		},
		"sourceRefs": []any{map[string]any{
			"projectId":   productionEditorProjectID,
			"epoch":       productionCharacterProposalPendingEpoch,
			"sourceId":    "finalized:" + fmt.Sprintf("%d", productionCharacterProposalPendingDraftID) + ":" + productionCharacterProposalPendingFinalization,
			"contentHash": contentHash,
			"revision":    0,
		}},
		"budget": map[string]any{
			"maxPhysicalRequests": 4, "maxTokenLiability": 8000,
			"maxOutputPerRequest": 1000, "maxActiveElapsedMs": 600000,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	value, err := ledger.CallJSON(context.Background(), "open", json.RawMessage("["+string(request)+"]"))
	if err != nil {
		t.Fatalf("seed run open: %v", err)
	}
	run := syntheticMap(value)
	runID := syntheticString(run["runId"])
	rootID := syntheticString(run["rootActionId"])
	if runID == "" || rootID == "" {
		t.Fatalf("seed run open returned no identity: %v", run)
	}
	artifactOne := productionCharacterProposalPendingArtifact(t, ledger, runID,
		productionCharacterProposalPendingRunNonce+"-1", productionCharacterProposalPendingAnswerOne)
	artifactTwo := productionCharacterProposalPendingArtifact(t, ledger, runID,
		productionCharacterProposalPendingRunNonce+"-2", productionCharacterProposalPendingAnswerTwo)
	handle := map[string]any{
		"projectId":    productionEditorProjectID,
		"epoch":        productionCharacterProposalPendingEpoch,
		"rootActionId": rootID,
		"runId":        runID,
	}
	sources := []map[string]any{
		{
			"kind":   "finalized-generation",
			"handle": handle,
			"artifact": map[string]any{
				"artifactId": syntheticString(artifactOne["artifactId"]),
				"revision":   syntheticNumber(artifactOne["revision"]),
				"textHash":   syntheticString(artifactOne["textHash"]),
			},
		},
		{
			"kind":   "finalized-generation",
			"handle": handle,
			"artifact": map[string]any{
				"artifactId": syntheticString(artifactTwo["artifactId"]),
				"revision":   syntheticNumber(artifactTwo["revision"]),
				"textHash":   syntheticString(artifactTwo["textHash"]),
			},
		},
	}
	batchIDs := []string{productionCharacterProposalPendingBatchOne, productionCharacterProposalPendingBatchTwo}
	for index, source := range sources {
		proof, err := store.NewPendingFinalized(db).Prove(context.Background(), productionEditorProjectID, source)
		if err != nil {
			t.Fatalf("prove seeded source %v: %v", index, err)
		}
		items, ok := proof["items"].([]any)
		if !ok || len(items) != 2 {
			t.Fatalf("the seeded proof carries %v items, want the two named occurrences", len(items))
		}
		resolved := make([]any, 0, len(items))
		for _, value := range items {
			item, ok := value.(map[string]any)
			if !ok {
				t.Fatalf("the proof item is %T", value)
			}
			copied := make(map[string]any, len(item)+1)
			for key, field := range item {
				copied[key] = field
			}
			copied["resolution"] = map[string]any{"status": "unresolved", "candidateIds": []any{}}
			resolved = append(resolved, copied)
		}
		envelope := map[string]any{
			"version":   1,
			"projectId": productionEditorProjectID,
			"proof":     proof,
			"batch": map[string]any{
				"proposalBatchId": batchIDs[index],
				"revision":        0,
				"status":          "pending-approval",
				"source":          source,
				"items":           resolved,
			},
		}
		encoded, err := productionCharacterProposalEncode(envelope)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO character_identity_proposals
 (proposal_id, owner_character_id, source_key, source_hash, raw_value, candidate_ids_json)
 VALUES (?, NULL, ?, ?, ?, '[]')`, batchIDs[index],
			productionCharacterProposalSourceKeyPrefix+productionEditorProjectID,
			productionCharacterProposalTextHash(encoded), encoded); err != nil {
			t.Fatalf("seed pending-finalized proposal: %v", err)
		}
	}
	// The rows must be in the admitted file, not in a sidecar the admission
	// digest does not cover.
	closed = true
	productionEditorCheckpoint(t, db, database)
}

// productionCharacterProposalPendingFingerprint is the nine-hash binding
// fingerprint every admitted run carries.
func productionCharacterProposalPendingFingerprint() map[string]any {
	hash := strings.Repeat("a", 64)
	return map[string]any{
		"chapterBriefHash": hash, "authorGuidanceHash": hash, "dependencyHash": hash,
		"contextSnapshotHash": hash, "templateHash": hash, "skillSnapshotHash": hash,
		"modelLeaseRevision": hash, "policyHash": hash, "outputContractHash": hash,
	}
}

// productionCharacterProposalPendingArtifact settles one answer through the real
// ledger and returns the artifact the ledger wrote.
func productionCharacterProposalPendingArtifact(t *testing.T, ledger *store.GenerationLedger,
	runID, nonce, text string) map[string]any {
	t.Helper()
	ctx := context.Background()
	value, err := ledger.Call(ctx, "reserve", []any{runID, nonce, strings.Repeat("b", 64), 200, 100})
	if err != nil {
		t.Fatalf("seed artifact reserve: %v", err)
	}
	attemptID := syntheticString(syntheticMap(syntheticMap(value)["attempt"])["attemptId"])
	if attemptID == "" {
		t.Fatalf("seed artifact reserve returned no attempt: %v", value)
	}
	for _, step := range []struct {
		method string
		args   []any
	}{
		{"markDispatched", []any{attemptID}},
		{"snapshot", []any{attemptID, 0, text}},
		{"settle", []any{attemptID, map[string]any{"trusted": true, "actualTokens": 120}, "stop"}},
	} {
		if _, err := ledger.Call(ctx, step.method, step.args); err != nil {
			t.Fatalf("seed artifact %v: %v", step.method, err)
		}
	}
	value, err = ledger.Call(ctx, "receipt", []any{attemptID})
	if err != nil {
		t.Fatalf("seed artifact receipt: %v", err)
	}
	artifact := syntheticMap(syntheticMap(value)["artifact"])
	if syntheticString(artifact["artifactId"]) == "" || syntheticString(artifact["textHash"]) == "" {
		t.Fatalf("seed artifact receipt carries no identity: %v", value)
	}
	return artifact
}

// TestProductionCharacterProposalPendingServesBothNames is the served claim over
// the real wire: the list pages the two proven proposals in proposal_id order,
// and the read answers each stored batch with its source, status and items.
func TestProductionCharacterProposalPendingServesBothNames(t *testing.T) {
	lane := productionCharacterProposalPendingFixture(t)

	var summaries []map[string]any
	lane.author.decode(t, productionCharacterProposalListPendingName, []any{lane.frozen}, &summaries)
	if len(summaries) != 2 {
		t.Fatalf("the list answered %v summaries, want the two seeded proposals: %v", len(summaries), summaries)
	}
	for position, want := range []string{productionCharacterProposalPendingBatchOne, productionCharacterProposalPendingBatchTwo} {
		if got := syntheticString(summaries[position]["proposalBatchId"]); got != want {
			t.Fatalf("summary %v is %v, want %v in proposal_id order", position, got, want)
		}
		if got := syntheticString(summaries[position]["finalizationId"]); got != productionCharacterProposalPendingFinalization {
			t.Fatalf("summary %v names finalization %v, want %v", position, got, productionCharacterProposalPendingFinalization)
		}
		if _, present := summaries[position]["revision"]; !present {
			t.Fatalf("summary %v carries no revision: %v", position, summaries[position])
		}
	}

	for position, batchID := range []string{productionCharacterProposalPendingBatchOne, productionCharacterProposalPendingBatchTwo} {
		var batch map[string]any
		lane.author.decode(t, productionCharacterProposalReadPendingName,
			[]any{map[string]any{"proposalBatchId": batchID}, lane.frozen}, &batch)
		if got := syntheticString(batch["proposalBatchId"]); got != batchID {
			t.Fatalf("read %v answered batch %v", position, got)
		}
		if got := syntheticString(batch["status"]); got != "pending-approval" {
			t.Fatalf("read %v answered status %v, want the stored pending status", position, got)
		}
		source := syntheticMap(batch["source"])
		if got := syntheticString(source["kind"]); got != "finalized-generation" {
			t.Fatalf("read %v answered source kind %v", position, got)
		}
		items := syntheticSlice(batch["items"])
		if len(items) != 2 {
			t.Fatalf("read %v answered %v items, want the two named occurrences", position, len(items))
		}
		for item, value := range items {
			entry := syntheticMap(value)
			if syntheticString(entry["selectionKey"]) == "" {
				t.Fatalf("read %v item %v carries no selection key: %v", position, item, entry)
			}
			resolution := syntheticMap(entry["resolution"])
			if syntheticString(resolution["status"]) != "unresolved" {
				t.Fatalf("read %v item %v resolves against an empty roster as %v", position, item, resolution)
			}
		}
	}
}

// TestProductionCharacterProposalPendingRefusesForeignProjectAndStaleLease pins
// the admission both names share: a project this lane never opened and a lease
// this lane never issued are refused before any row is read, both names answer
// the identical refusal, and the admitted database is byte-identical.
func TestProductionCharacterProposalPendingRefusesForeignProjectAndStaleLease(t *testing.T) {
	lane := productionCharacterProposalPendingFixture(t)
	before := lane.bytes(t)

	calls := []struct {
		operation string
		args      func(ctx map[string]any) []any
	}{
		{productionCharacterProposalListPendingName, func(ctx map[string]any) []any { return []any{ctx} }},
		{productionCharacterProposalReadPendingName, func(ctx map[string]any) []any {
			return []any{map[string]any{"proposalBatchId": productionCharacterProposalPendingBatchOne}, ctx}
		}},
	}
	foreignBodies := make([]string, 0, len(calls))
	staleBodies := make([]string, 0, len(calls))
	for _, call := range calls {
		foreignBodies = append(foreignBodies,
			productionCharacterProposalPendingRefusal(t, call.operation, "a foreign project",
				lane.author.call(t, call.operation, call.args(lane.foreign())).Body.String()))
		staleBodies = append(staleBodies,
			productionCharacterProposalPendingRefusal(t, call.operation, "a stale lease",
				lane.author.call(t, call.operation, call.args(lane.stale())).Body.String()))
	}
	// Each refusal is the same answer whichever name asked: the fence is the
	// borrow, not the operation.
	for index := 1; index < len(calls); index++ {
		if foreignBodies[index] != foreignBodies[0] {
			t.Fatalf("%v and %v answered different foreign refusals: %v / %v",
				calls[0].operation, calls[index].operation,
				productionCharacterProposalPendingClip(foreignBodies[0]),
				productionCharacterProposalPendingClip(foreignBodies[index]))
		}
		if staleBodies[index] != staleBodies[0] {
			t.Fatalf("%v and %v answered different stale-lease refusals: %v / %v",
				calls[0].operation, calls[index].operation,
				productionCharacterProposalPendingClip(staleBodies[0]),
				productionCharacterProposalPendingClip(staleBodies[index]))
		}
	}
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("a refused call changed the admitted database")
	}
}

// productionCharacterProposalPendingRefusal requires the reference's refusal
// envelope with one of the lease or project codes the borrow can raise, and
// answers the body so two names can be compared.
func productionCharacterProposalPendingRefusal(t *testing.T, operation, label, body string) string {
	t.Helper()
	if body == "" {
		t.Fatalf("%v %v answered no body", operation, label)
	}
	code := productionCharacterProposalPendingRefusalCode(t, body)
	switch code {
	case "PROJECT_NOT_OPEN", "PROJECT_SESSION_NOT_OWNED", "PROJECT_SESSION_STALE",
		"PROJECT_SESSION_REQUIRED", "PROJECT_SESSION_CONTEXT_REQUIRED", "PROJECT_CONTEXT_CHANGED":
	default:
		t.Fatalf("%v %v was refused with %v, which is not a lease refusal", operation, label, code)
	}
	return body
}

// productionCharacterProposalPendingRefusalCode reads the wire refusal code.
func productionCharacterProposalPendingRefusalCode(t *testing.T, body string) string {
	t.Helper()
	var refusal struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &refusal); err != nil {
		t.Fatalf("a refusal is not the wire envelope: %v", err)
	}
	return refusal.Error.Code
}

// productionCharacterProposalPendingClip keeps a refusal body short in a failure
// message.
func productionCharacterProposalPendingClip(value string) string {
	if len(value) > 160 {
		return value[:160]
	}
	return value
}

// TestProductionCharacterProposalPendingBindsTwoNames pins the wiring: the
// binder installs exactly the two inventoried names, a nil bridge installs
// nothing, and a nil server is refused. It also re-reads the reference lines the
// port is built on, through productionSeamContains so CI's formatting cannot
// turn a white-space change into a failure.
func TestProductionCharacterProposalPendingBindsTwoNames(t *testing.T) {
	lane := productionCharacterProposalPendingFixture(t)
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
	if err := newProductionCharacterProposalPending(lane.durable).Bind(server); err != nil {
		t.Fatalf("the two-name binder failed: %v", err)
	}
	names := server.BoundOperations()
	if len(names) != 2 {
		t.Fatalf("the binder installed %v, want the two pending-finalized names", names)
	}
	for _, want := range []string{productionCharacterProposalListPendingName, productionCharacterProposalReadPendingName} {
		found := false
		for _, name := range names {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("%v is not bound: %v", want, names)
		}
	}
	if err := newProductionCharacterProposalPending(nil).Bind(nil); err != nil {
		t.Fatalf("a nil bridge was refused instead of installing nothing: %v", err)
	}
	if err := newProductionCharacterProposalPending(lane.durable).Bind(nil); err == nil {
		t.Fatal("a nil server was accepted")
	}

	for _, reference := range []struct {
		path   string
		marker string
	}{
		{"../../../electron/services/character-proposal-service.ts", "listPendingFinalized(): PendingFinalizedCharacterProposalSummary[] {"},
		{"../../../electron/services/character-proposal-service.ts", "readPendingFinalized(proposalBatchId: string): CharacterProposalBatch {"},
		{"../../../electron/services/character-proposal-service.ts", "if (envelope.batch.status !== 'pending-approval' || !source) return []"},
		{"../../../electron/services/character-proposal-service.ts", "if (!isDeepStrictEqual(envelope.proof, currentProof))"},
		{"../../../electron/services/character-proposal-service.ts", "if (batch.status !== 'pending-approval' || !['finalized-generation', 'import'].includes(batch.source.kind))"},
		{"../../../electron/services/finalized-character-generation-proof.ts", "export function proveFinalizedCharacterGeneration("},
		{"../../../electron/services/generation-character-proposal-proof.ts", "function importCharacterEntriesFromArtifacts("},
	} {
		raw, err := os.ReadFile(reference.path)
		if err != nil {
			t.Fatal(err)
		}
		if !productionSeamContains(string(raw), reference.marker) {
			t.Fatalf("%v no longer says %v, so this port is quoting nothing", reference.path, reference.marker)
		}
	}

	// The binder line the integrator adds to production_open_linux.go is stated
	// in this binder's own header and published by the residual record, so the
	// wiring report and the code cannot disagree.
	for _, path := range []string{
		"production_character_proposal_pending_linux.go",
		"production_character_proposal_residual_linux.go",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !productionSeamContains(string(raw), productionCharacterProposalResidualBinder) {
			t.Fatalf("%v no longer states the binder line %v", path, productionCharacterProposalResidualBinder)
		}
	}
}
