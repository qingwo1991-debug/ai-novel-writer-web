//go:build linux

package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// The story-state fixture: two finalized drafts (chapter 2 carries a matching
// finalization_outbox receipt, chapter 3 deliberately does not, so the export
// snapshot has to answer a null finalizationId), two narrative thread plans (one
// with a confirmed event, one without) and two consistency exemptions (one
// active, one revoked). Chapter 1 stays the working draft the editor fixture
// seeded, which is what proves the authority sequence counts finalized rows only.
const (
	productionStoryStateBackedContentID   = 2
	productionStoryStateUnbackedContentID = 3
	productionStoryStateBackedDraftID     = 2
	productionStoryStateUnbackedDraftID   = 3
	productionStoryStateBackedChapter     = 2
	productionStoryStateUnbackedChapter   = 3
	productionStoryStateBackedBody        = "第二章正文：他推开门，门后有一封信。"
	productionStoryStateUnbackedBody      = "第三章正文：信上只有一行字。"
	productionStoryStateBackedWords       = 18
	productionStoryStateUnbackedWords     = 13
	productionStoryStateFinalizationID    = "ci-storystate-finalization-2"
	productionStoryStateChapterTitle      = "第二章 门"
	productionStoryStateTargetFile        = "chapter-0002.md"

	productionStoryStatePlanID          = 1
	productionStoryStatePlainPlanID     = 2
	productionStoryStatePlanTitle       = "门的秘密"
	productionStoryStatePlanType        = "mystery"
	productionStoryStatePlanIntent      = "让读者怀疑门后有人"
	productionStoryStatePlanStart       = 1
	productionStoryStatePlanEnd         = 3
	productionStoryStatePlainPlanTitle  = "旧案"
	productionStoryStatePlainPlanType   = "subplot"
	productionStoryStatePlainPlanIntent = "终局伏笔"
	productionStoryStatePlainPlanStart  = 5
	productionStoryStatePlainPlanEnd    = 9
	productionStoryStateEventID         = 1
	productionStoryStateEventType       = "planted"
	productionStoryStateEventEvidence   = "门后有一封信"
	productionStoryStateEventReason     = "把悬念落在一封信上"
	productionStoryStateFilterCharacter = "门后有人"

	productionStoryStateActiveExemptionKey     = "ci-storystate-key-active"
	productionStoryStateActiveExemptionReason  = "作者已核对"
	productionStoryStateRevokedExemptionKey    = "ci-storystate-key-revoked"
	productionStoryStateRevokedExemptionReason = "事实已改写"
)

// productionStoryStateSeed is the identity of the rows the fixture committed, so
// every assertion below compares against what the database really holds.
type productionStoryStateSeed struct {
	BackedDraftID       int64
	UnbackedDraftID     int64
	BackedBodyHash      string
	UnbackedBodyHash    string
	FinalizationID      string
	PlanID              int64
	PlainPlanID         int64
	EventID             int64
	ActiveExemptionKey  string
	RevokedExemptionKey string
}

// productionStoryStateBodyHash is store's own draftBodyHash: the sha256 of the
// UTF-8 body, lowercase hex. The export snapshot and the authority fingerprint
// both carry it, so the test recomputes it instead of copying a constant.
func productionStoryStateBodyHash(body string) string {
	digest := sha256.Sum256([]byte(body))
	return hex.EncodeToString(digest[:])
}

// productionStoryStateAuthority is one row of the authority fingerprint: the
// finalized draft identity plus the body the fingerprint hashes.
type productionStoryStateAuthority struct {
	draftID       int64
	chapterNumber int64
	version       int64
	body          string
}

// productionStoryStateFingerprint is store's authorityFingerprint
// (internal/store/draft_authority.go:84-104) rebuilt from the rows the test
// seeded: the sha256 of the compact JSON array of
// {draftId, chapterNumber, version, bodyHash} in chapter order. The payload holds
// only integers and lowercase hex, so plain concatenation is byte-identical to
// the store's SetEscapeHTML(false) encoder.
func productionStoryStateFingerprint(rows []productionStoryStateAuthority) string {
	parts := make([]string, 0, len(rows))
	for _, row := range rows {
		parts = append(parts, fmt.Sprintf(`{"draftId":%d,"chapterNumber":%d,"version":%d,"bodyHash":"%s"}`,
			row.draftID, row.chapterNumber, row.version, productionStoryStateBodyHash(row.body)))
	}
	return productionStoryStateBodyHash("[" + strings.Join(parts, ",") + "]")
}

// seedProductionStoryStateRows writes the two finalized chapters, the one
// finalization receipt, the two thread plans, the one confirmed event and the
// two exemptions the six bound reads project. It runs after the editor fixture,
// so the working draft, the character card, the blueprint and the project core
// row that fixture seeded are untouched; the foreign-key edges (draft content,
// receipt draft, confirmation plan and draft) are all satisfied, which is what
// the admission verifier requires before the lane opens.
func seedProductionStoryStateRows(t *testing.T, projectPath string) productionStoryStateSeed {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	backedHash := productionStoryStateBodyHash(productionStoryStateBackedBody)
	statements := []string{
		"DELETE FROM narrative_thread_confirmations",
		"DELETE FROM narrative_thread_plans",
		"DELETE FROM consistency_exemptions",
		"DELETE FROM finalization_outbox",
		fmt.Sprintf(`INSERT INTO contents (id, body) VALUES (%d, '%s')`,
			productionStoryStateBackedContentID, productionStoryStateBackedBody),
		fmt.Sprintf(`INSERT INTO contents (id, body) VALUES (%d, '%s')`,
			productionStoryStateUnbackedContentID, productionStoryStateUnbackedBody),
		fmt.Sprintf(`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies, created_at, updated_at)
 VALUES (%d, %d, 1, 'finalized', 'write', %d, %d, '[]', '2024-05-06 08:09:10', '2024-05-06 08:09:10')`,
			productionStoryStateBackedDraftID, productionStoryStateBackedChapter,
			productionStoryStateBackedContentID, productionStoryStateBackedWords),
		fmt.Sprintf(`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies, created_at, updated_at)
 VALUES (%d, %d, 1, 'finalized', 'write', %d, %d, '[]', '2024-05-06 08:09:11', '2024-05-06 08:09:11')`,
			productionStoryStateUnbackedDraftID, productionStoryStateUnbackedChapter,
			productionStoryStateUnbackedContentID, productionStoryStateUnbackedWords),
		fmt.Sprintf(`INSERT INTO finalization_outbox (finalization_id, draft_id, chapter_number,
 chapter_title, content_hash, content_revision, content_snapshot, target_file_name,
 publication_status, created_at, updated_at)
 VALUES ('%s', %d, %d, '%s', '%s', 1, '%s', '%s', 'published',
 '2024-05-06 08:09:10', '2024-05-06 08:09:10')`,
			productionStoryStateFinalizationID, productionStoryStateBackedDraftID,
			productionStoryStateBackedChapter, productionStoryStateChapterTitle, backedHash,
			productionStoryStateBackedBody, productionStoryStateTargetFile),
		fmt.Sprintf(`INSERT INTO narrative_thread_plans (id, title, type, target_start_chapter,
 target_end_chapter, author_intent, created_at, updated_at)
 VALUES (%d, '%s', '%s', %d, %d, '%s', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`,
			productionStoryStatePlanID, productionStoryStatePlanTitle, productionStoryStatePlanType,
			productionStoryStatePlanStart, productionStoryStatePlanEnd, productionStoryStatePlanIntent),
		fmt.Sprintf(`INSERT INTO narrative_thread_plans (id, title, type, target_start_chapter,
 target_end_chapter, author_intent, created_at, updated_at)
 VALUES (%d, '%s', '%s', %d, %d, '%s', '2024-05-06 07:08:10', '2024-05-06 07:08:10')`,
			productionStoryStatePlainPlanID, productionStoryStatePlainPlanTitle,
			productionStoryStatePlainPlanType, productionStoryStatePlainPlanStart,
			productionStoryStatePlainPlanEnd, productionStoryStatePlainPlanIntent),
		fmt.Sprintf(`INSERT INTO narrative_thread_confirmations (id, plan_id, draft_id, event_type,
 evidence, reason, created_at)
 VALUES (%d, %d, %d, '%s', '%s', '%s', '2024-05-06 08:09:10')`,
			productionStoryStateEventID, productionStoryStatePlanID,
			productionStoryStateBackedDraftID, productionStoryStateEventType,
			productionStoryStateEventEvidence, productionStoryStateEventReason),
		fmt.Sprintf(`INSERT INTO consistency_exemptions (stable_fact_key, reason, revoked)
 VALUES ('%s', '%s', 0)`,
			productionStoryStateActiveExemptionKey, productionStoryStateActiveExemptionReason),
		fmt.Sprintf(`INSERT INTO consistency_exemptions (stable_fact_key, reason, revoked)
 VALUES ('%s', '%s', 1)`,
			productionStoryStateRevokedExemptionKey, productionStoryStateRevokedExemptionReason),
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	// The lane admits the file itself, so the seeded rows must be checkpointed
	// into it: no sidecar may carry rows the admitted digest does not cover.
	productionEditorCheckpoint(t, db, database)
	return productionStoryStateSeed{
		BackedDraftID:       productionStoryStateBackedDraftID,
		UnbackedDraftID:     productionStoryStateUnbackedDraftID,
		BackedBodyHash:      backedHash,
		UnbackedBodyHash:    productionStoryStateBodyHash(productionStoryStateUnbackedBody),
		FinalizationID:      productionStoryStateFinalizationID,
		PlanID:              productionStoryStatePlanID,
		PlainPlanID:         productionStoryStatePlainPlanID,
		EventID:             productionStoryStateEventID,
		ActiveExemptionKey:  productionStoryStateActiveExemptionKey,
		RevokedExemptionKey: productionStoryStateRevokedExemptionKey,
	}
}

// TestProductionStoryStateReadsServeAdmittedAuthorDatabase is the authoritative
// export and narrative-thread half of this lane. Each of the six operations is
// asserted by its own subtest - never in a loop over the names - so a failure
// names the operation that broke. Every read is compared with the rows the
// admitted author database really holds, every read refuses a lease this server
// never issued under the same fence as every other author read, and none of them
// may touch the admitted bytes.
func TestProductionStoryStateReadsServeAdmittedAuthorDatabase(t *testing.T) {
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	seed := seedProductionStoryStateRows(t, projectPath)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	expected, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	// unchanged re-reads the admitted database: a served read and a refused lease
	// must both leave the author's bytes exactly as they were seeded.
	unchanged := func(t *testing.T, label string) {
		t.Helper()
		after, err := os.ReadFile(database)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, expected) {
			t.Fatalf("%s changed the admitted author database", label)
		}
	}

	author := newProductionDurableClient(t, handler, productionEditorTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "storystate-open-1", projectPath}, &opened)
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
	stale := map[string]any{"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": "stale-lease"}
	// The expected authority verdict: chapters 2 and 3 are finalized, chapter 1 is
	// still the working draft, so the sequence is invalid at chapter 1. A reader
	// that forgets the status filter would answer "continuous" with lastChapter 1
	// and a different fingerprint.
	fingerprint := productionStoryStateFingerprint([]productionStoryStateAuthority{
		{draftID: seed.BackedDraftID, chapterNumber: productionStoryStateBackedChapter, version: 1,
			body: productionStoryStateBackedBody},
		{draftID: seed.UnbackedDraftID, chapterNumber: productionStoryStateUnbackedChapter, version: 1,
			body: productionStoryStateUnbackedBody},
	})

	// db:draft-authority-sequence: the finalized rows only. The seeded draft of
	// chapter 1 is not finalized, so the run has a gap at 1 and no
	// nextChapterNumber key at all - exactly the original's invalid verdict.
	t.Run("db:draft-authority-sequence", func(t *testing.T) {
		var sequence map[string]any
		productionEditorDecode(t, author, "db:draft-authority-sequence", []any{projectPath, frozen}, &sequence)
		if sequence["status"] != "invalid" {
			t.Fatalf("db:draft-authority-sequence counted a non-finalized draft: %v", sequence)
		}
		if sequence["lastChapterNumber"] != float64(productionStoryStateUnbackedChapter) {
			t.Fatalf("db:draft-authority-sequence read the wrong last chapter: %v", sequence)
		}
		if sequence["firstGapChapterNumber"] != float64(1) {
			t.Fatalf("db:draft-authority-sequence read the wrong first gap: %v", sequence)
		}
		if _, present := sequence["nextChapterNumber"]; present {
			t.Fatalf("db:draft-authority-sequence invented a next chapter for an invalid run: %v", sequence)
		}
		duplicates, ok := sequence["duplicateChapterNumbers"].([]any)
		if !ok || len(duplicates) != 0 {
			t.Fatalf("db:draft-authority-sequence invented duplicate chapters: %v", sequence)
		}
		if sequence["authorityFingerprint"] != fingerprint {
			t.Fatalf("db:draft-authority-sequence fingerprinted %v; want %s over the finalized rows only",
				sequence["authorityFingerprint"], fingerprint)
		}
		productionEditorRefused(t, author, "stale lease", "db:draft-authority-sequence", []any{projectPath, stale})
		unchanged(t, "db:draft-authority-sequence")
	})

	// db:draft-export-snapshot: one row per chapter, highest version first, with
	// the receipt-backed chapter carrying its frozen identity and the
	// receipt-less chapter carrying a null finalizationId.
	t.Run("db:draft-export-snapshot", func(t *testing.T) {
		var snapshot []map[string]any
		productionEditorDecode(t, author, "db:draft-export-snapshot", []any{projectPath, frozen}, &snapshot)
		if len(snapshot) != 2 {
			t.Fatalf("db:draft-export-snapshot returned %d chapters; want the two finalized ones: %v",
				len(snapshot), snapshot)
		}
		backed := snapshot[0]
		if backed["draftId"] != float64(seed.BackedDraftID) ||
			backed["chapterNumber"] != float64(productionStoryStateBackedChapter) ||
			backed["version"] != float64(1) ||
			backed["finalizationId"] != seed.FinalizationID ||
			backed["contentHash"] != seed.BackedBodyHash ||
			backed["title"] != productionStoryStateChapterTitle ||
			backed["content"] != productionStoryStateBackedBody {
			t.Fatalf("db:draft-export-snapshot did not read the receipt-backed chapter: %v", backed)
		}
		unbacked := snapshot[1]
		if unbacked["draftId"] != float64(seed.UnbackedDraftID) ||
			unbacked["chapterNumber"] != float64(productionStoryStateUnbackedChapter) ||
			unbacked["finalizationId"] != nil ||
			unbacked["contentHash"] != seed.UnbackedBodyHash ||
			unbacked["title"] != "" ||
			unbacked["content"] != productionStoryStateUnbackedBody {
			t.Fatalf("db:draft-export-snapshot did not read the receipt-less chapter: %v", unbacked)
		}
		productionEditorRefused(t, author, "stale lease", "db:draft-export-snapshot", []any{projectPath, stale})
		unchanged(t, "db:draft-export-snapshot")
	})

	// db:draft-export-authority-current: the receipt the snapshot just produced is
	// current, and the same receipt with one altered body hash is not.
	t.Run("db:draft-export-authority-current", func(t *testing.T) {
		// Read the snapshot again so this subtest stands on its own, then hand the
		// five receipt fields back: the export snapshot's own answer is the receipt
		// the browser would send.
		var snapshot []map[string]any
		productionEditorDecode(t, author, "db:draft-export-snapshot", []any{projectPath, frozen}, &snapshot)
		if len(snapshot) != 2 {
			t.Fatalf("db:draft-export-authority-current needs the exported chapters; got %v", snapshot)
		}
		receipt := make([]any, 0, len(snapshot))
		for _, row := range snapshot {
			receipt = append(receipt, map[string]any{
				"draftId":        row["draftId"],
				"chapterNumber":  row["chapterNumber"],
				"version":        row["version"],
				"finalizationId": row["finalizationId"],
				"contentHash":    row["contentHash"],
			})
		}
		var current bool
		productionEditorDecode(t, author, "db:draft-export-authority-current",
			[]any{receipt, projectPath, frozen}, &current)
		if !current {
			t.Fatalf("db:draft-export-authority-current rejected the receipt it just produced: %v", receipt)
		}
		tampered := make([]any, 0, len(receipt))
		for index, item := range receipt {
			entry := item.(map[string]any)
			copied := make(map[string]any, len(entry))
			for key, value := range entry {
				copied[key] = value
			}
			if index == 0 {
				copied["contentHash"] = strings.Repeat("f", 64)
			}
			tampered = append(tampered, copied)
		}
		current = true
		productionEditorDecode(t, author, "db:draft-export-authority-current",
			[]any{tampered, projectPath, frozen}, &current)
		if current {
			t.Fatalf("db:draft-export-authority-current accepted a receipt with a foreign body hash: %v", tampered)
		}
		productionEditorRefused(t, author, "stale lease", "db:draft-export-authority-current",
			[]any{receipt, projectPath, stale})
		unchanged(t, "db:draft-export-authority-current")
	})

	// db:narrative-thread-list: both plans in id order. The plan with the confirmed
	// event is 'planted', dormant for the chapter the event did not reach; the
	// plan without events stays 'planned' and is not dormant.
	t.Run("db:narrative-thread-list", func(t *testing.T) {
		var threads []map[string]any
		productionEditorDecode(t, author, "db:narrative-thread-list", []any{projectPath, frozen}, &threads)
		if len(threads) != 2 {
			t.Fatalf("db:narrative-thread-list returned %d plans; want the two seeded ones: %v", len(threads), threads)
		}
		planted := threads[0]
		if planted["id"] != float64(seed.PlanID) ||
			planted["title"] != productionStoryStatePlanTitle ||
			planted["type"] != productionStoryStatePlanType ||
			planted["targetStartChapter"] != float64(productionStoryStatePlanStart) ||
			planted["targetEndChapter"] != float64(productionStoryStatePlanEnd) ||
			planted["authorIntent"] != productionStoryStatePlanIntent ||
			planted["status"] != productionStoryStateEventType ||
			planted["dormantChapters"] != float64(productionStoryStateUnbackedChapter-productionStoryStateBackedChapter) ||
			planted["overdue"] != false {
			t.Fatalf("db:narrative-thread-list did not read the planted thread: %v", planted)
		}
		events, ok := planted["events"].([]any)
		if !ok || len(events) != 1 {
			t.Fatalf("db:narrative-thread-list returned %v events; want the one seeded confirmation: %v", events, planted)
		}
		event, ok := events[0].(map[string]any)
		if !ok {
			t.Fatalf("db:narrative-thread-list returned %T as an event: %v", events[0], planted)
		}
		if event["id"] != float64(seed.EventID) ||
			event["planId"] != float64(seed.PlanID) ||
			event["draftId"] != float64(seed.BackedDraftID) ||
			event["type"] != productionStoryStateEventType ||
			event["evidence"] != productionStoryStateEventEvidence ||
			event["reason"] != productionStoryStateEventReason ||
			event["chapterNumber"] != float64(productionStoryStateBackedChapter) ||
			event["chapterTitle"] != productionStoryStateChapterTitle {
			t.Fatalf("db:narrative-thread-list did not read the frozen confirmation source: %v", event)
		}
		planned := threads[1]
		if planned["id"] != float64(seed.PlainPlanID) ||
			planned["status"] != "planned" ||
			planned["dormantChapters"] != float64(0) ||
			planned["overdue"] != false {
			t.Fatalf("db:narrative-thread-list did not read the event-less thread: %v", planned)
		}
		if events, ok := planned["events"].([]any); !ok || len(events) != 0 {
			t.Fatalf("db:narrative-thread-list invented events for the event-less thread: %v", planned)
		}
		productionEditorRefused(t, author, "stale lease", "db:narrative-thread-list", []any{projectPath, stale})
		unchanged(t, "db:narrative-thread-list")
	})

	// db:narrative-thread-list-relevant: the filter is applied, in both
	// directions (chapter inside a range and chapter outside it), and a character
	// named in the thread's own prose pulls a thread in even outside its range.
	t.Run("db:narrative-thread-list-relevant", func(t *testing.T) {
		var inRange []map[string]any
		productionEditorDecode(t, author, "db:narrative-thread-list-relevant",
			[]any{map[string]any{"chapterNumber": float64(productionStoryStateBackedChapter)}, projectPath, frozen},
			&inRange)
		if len(inRange) != 1 || inRange[0]["id"] != float64(seed.PlanID) {
			t.Fatalf("db:narrative-thread-list-relevant did not filter to the covering range: %v", inRange)
		}
		var outOfRange []map[string]any
		productionEditorDecode(t, author, "db:narrative-thread-list-relevant",
			[]any{map[string]any{"chapterNumber": float64(productionStoryStatePlainPlanStart + 1)}, projectPath, frozen},
			&outOfRange)
		if len(outOfRange) != 1 || outOfRange[0]["id"] != float64(seed.PlainPlanID) {
			t.Fatalf("db:narrative-thread-list-relevant did not filter to the later range: %v", outOfRange)
		}
		var byCharacter []map[string]any
		productionEditorDecode(t, author, "db:narrative-thread-list-relevant",
			[]any{map[string]any{
				"chapterNumber": float64(productionStoryStatePlainPlanStart + 1),
				"characters":    []any{productionStoryStateFilterCharacter},
			}, projectPath, frozen},
			&byCharacter)
		if len(byCharacter) != 2 ||
			byCharacter[0]["id"] != float64(seed.PlanID) ||
			byCharacter[1]["id"] != float64(seed.PlainPlanID) {
			t.Fatalf("db:narrative-thread-list-relevant ignored the character filter: %v", byCharacter)
		}
		productionEditorRefused(t, author, "stale lease", "db:narrative-thread-list-relevant",
			[]any{map[string]any{"chapterNumber": float64(productionStoryStateBackedChapter)}, projectPath, stale})
		unchanged(t, "db:narrative-thread-list-relevant")
	})

	// db:consistency-exemption-list: both exemptions in stable fact key order, with
	// the revoked integer projected as the original's boolean.
	t.Run("db:consistency-exemption-list", func(t *testing.T) {
		var exemptions []map[string]any
		productionEditorDecode(t, author, "db:consistency-exemption-list", []any{projectPath, frozen}, &exemptions)
		if len(exemptions) != 2 {
			t.Fatalf("db:consistency-exemption-list returned %d exemptions; want the two seeded ones: %v",
				len(exemptions), exemptions)
		}
		active := exemptions[0]
		if active["stableFactKey"] != seed.ActiveExemptionKey ||
			active["reason"] != productionStoryStateActiveExemptionReason ||
			active["revoked"] != false {
			t.Fatalf("db:consistency-exemption-list did not read the active exemption first: %v", active)
		}
		revoked := exemptions[1]
		if revoked["stableFactKey"] != seed.RevokedExemptionKey ||
			revoked["reason"] != productionStoryStateRevokedExemptionReason ||
			revoked["revoked"] != true {
			t.Fatalf("db:consistency-exemption-list lost the revoked flag: %v", revoked)
		}
		productionEditorRefused(t, author, "stale lease", "db:consistency-exemption-list", []any{projectPath, stale})
		unchanged(t, "db:consistency-exemption-list")
	})

	// The whole six-name surface, read and refused under one admitted lease, leaves
	// the author's file exactly as it was seeded.
	unchanged(t, "the story-state read surface")
}
