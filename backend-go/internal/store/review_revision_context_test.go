package store

// These tests provision the pinned v7 schema from the asset in internal/admission
// (WriteBaselineV7, baseline_v7_ddl.go:744) and drive the review-revision context
// freeze producer of review_revision_context.go against the real schema: drafts,
// contents, finalization_outbox, blueprints, project_core, summary_snapshots,
// continuity_projection_meta, characters, character_identity_proposals,
// character_identity_meta and consistency_exemptions.
//
// What is proved here, and why each proof is the shape it is:
//
//   - The frozen text is compared BYTE FOR BYTE against an expected object built
//     in the test in the reference's own literal key order. The verifier hashes
//     the serialized value (reviewcycle_effect.go:189-190), so any key-order or
//     escaping drift is a hard failure of the whole review-revision family, not a
//     cosmetic difference.
//   - sha256 and FNV-1a are recomputed here from the reference's own definitions
//     instead of calling the package helpers, so a change inside summaryHash or
//     rrctxStableFactKey cannot silently agree with itself.
//   - Every capture is bracketed by a digest of the whole database file: a
//     read-only builder that wrote anything (even a rolled-back page) fails even
//     if no logical row changed.
//   - Each refusal is proved by its exact reference code AND by the unchanged
//     file bytes, on its own fresh database, so no earlier state can stand in for
//     a later one.
//   - Projections are read back through a second, independent connection, which
//     both proves the capture saw committed rows and excludes the possibility that
//     the reader only observed its own view.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
)

const (
	rrctxTestProjectID = "proj-review-context"
	rrctxChapterOne    = "第一章：雨落长街，灯影摇晃。"
	rrctxChapterTwo    = "第二章：旧梦重来，灯下人散。"
	rrctxChapterThree  = "第三章：风起，帐落。"
	rrctxSourceThree   = "第三章：风未起。"
	rrctxCandidateBody = "候选稿：灯下无人。"
	rrctxFinalOne      = "rrctx-final-1"
	rrctxFinalTwo      = "rrctx-final-2"
	rrctxHeroID        = "c-hero"
	rrctxHeroName      = "张三"
	rrctxOtherName     = "李四"
	rrctxStaleName     = "旧人"
	rrctxFocusText     = "聚焦雨夜里的对话"
	// rrctxFactKeyGolden is continuityStableFactKey (consistency-preflight.ts:58-76)
	// evaluated by hand outside this package for the fact seeded by
	// the dead-subject fact: category "character-state", sourceChapter 1, entities
	// ["张三"] sorted, statement "张三已经死亡" normalized. The derivation is
	// "fact:" + FNV-1a 64 of "character-state:1:张三:张三已经死亡".
	rrctxFactKeyGolden = "fact:6c9ed09cbd23ee2c"
	rrctxChapterTitle  = "合成章节"
)

// rrctxFixture provisions the pinned v7 schema in a fresh file, then opens it the
// way a project database is opened (ExistingDSN: mode=rw, foreign keys on).
func rrctxFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "review revision context #? 中文.db")
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
	// The continuity list INNER JOINs this singleton (summary.go:506), so a
	// project without it reports no finalized continuity at all. It is seeded
	// through the upsert the projection refresh itself uses, then read back, so a
	// fixture cannot silently depend on a different generation.
	mustExec(t, db, `INSERT INTO continuity_projection_meta(id,generation) VALUES('main',2)
 ON CONFLICT(id) DO UPDATE SET generation=excluded.generation, stale_from_chapter=NULL`)
	var generation int64
	if err := db.QueryRow("SELECT generation FROM continuity_projection_meta WHERE id='main'").Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != 2 {
		t.Fatalf("continuity projection generation: got %d, want 2", generation)
	}
	return db, path
}

// rrctxReopen opens the same file a second time, so a frozen value read through
// one connection is proved against what another connection can see.
func rrctxReopen(t *testing.T, path string) *sql.DB {
	t.Helper()
	dsn, err := ExistingDSN(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("reopened connection: %v", err)
	}
	return db
}

// rrctxDigest is the whole file's bytes: a capture that wrote anything at all
// fails even if it committed no logical row.
func rrctxDigest(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// rrctxSHA is sha256 hex from the reference's definition (textHash,
// generation-run-repository.ts:92), implemented here so the source digest and the
// frozen digest are not proved by the package's own helper.
func rrctxSHA(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// rrctxJSON encodes a fixture value the way JSON.stringify would, so a frozen
// text is compared byte for byte.
func rrctxJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := plotJSON(value)
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return raw
}

// rrctxCore seeds the single project_core row every capture reads.
func rrctxCore(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO project_core(id,project_name,genre,sub_genre,target_audience,total_chapters,
 words_per_chapter,writing_language,creative_strategy,narrative_thread_dormant_threshold,plot_structure,
 narrative_pov,writing_style,reference_works,global_guidance,golden_finger,core_outline,world_setting,
 protagonist_profile,worldbuilding)
 VALUES('main','合成项目','玄幻','东方玄幻','成年读者',120,3000,'zh-CN','auto',3,'three_act',
 'third_limited','白描','无','保持节奏','无','开局入城','旧城雨巷','主角沉默寡言','架空旧城')`)
}

// rrctxBlueprintRow seeds one blueprint row. notes is written by the
// post-processing extraction, so it is always present and empty by default.
func rrctxBlueprintRow(t *testing.T, db *sql.DB, chapter int64, keyEvents, characters string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO blueprints(chapter_number,title,role,purpose,key_events,characters,
 suspense_hook,user_guidance,notes) VALUES(?,'雨夜','推进','推进主线',?,?,'门外是谁','','')`,
		chapter, keyEvents, characters)
}

// rrctxDraft seeds one working draft with its prose and the recorded predecessor
// list the author's selection froze.
func rrctxDraft(t *testing.T, db *sql.DB, id, chapter, version int64, status, body, dependencies string) int64 {
	t.Helper()
	mustExec(t, db, "INSERT INTO contents(id,body) VALUES(?,?)", id, body)
	mustExec(t, db, `INSERT INTO drafts(id,chapter_number,version,status,content_id,word_count,source_dependencies)
 VALUES(?,?,?,?,?,?,?)`, id, chapter, version, status, id, len(body), dependencies)
	return id
}

// rrctxFinalizedDraft seeds one finalized draft, its immutable finalization
// receipt and its continuity snapshot row, which is what the projection list
// reads. A finalized chapter carries no recorded predecessor: it is not a
// candidate the author selected.
func rrctxFinalizedDraft(t *testing.T, db *sql.DB, id, chapter, version int64, finalizationID, body,
	notes, facts, candidates string) int64 {
	t.Helper()
	hash := rrctxSHA(body)
	rrctxDraft(t, db, id, chapter, version, "finalized", body, "[]")
	mustExec(t, db, `INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,chapter_title,
 content_hash,content_revision,content_snapshot,target_file_name) VALUES(?,?,?,?,?,1,?,'rrctx.txt')`,
		finalizationID, id, chapter, rrctxChapterTitle, hash, body)
	mustExec(t, db, `INSERT INTO summary_snapshots(draft_id,chapter_number,chapter_notes,continuity_facts,
 character_state_candidates,source_finalization_id,source_content_hash,projection_generation)
 VALUES(?,?,?,?,?,?,?,2)`, id, chapter, notes, facts, candidates, finalizationID, hash)
	return id
}

// rrctxSnapshotOnly seeds a snapshot row whose chapter notes are non-empty but
// whose draft carries no receipt; the list keeps one row per chapter (the newest
// snapshot), so this row must never win over a later one.
func rrctxSnapshotOnly(t *testing.T, db *sql.DB, id, chapter, version int64, notes, facts string) int64 {
	t.Helper()
	rrctxDraft(t, db, id, chapter, version, "superseded", "旧版正文。", "[]")
	mustExec(t, db, `INSERT INTO summary_snapshots(draft_id,chapter_number,chapter_notes,continuity_facts,
 character_state_candidates) VALUES(?,?,?,?,'[]')`, id, chapter, notes, facts)
	return id
}

// rrctxFrozenRoster is the identity freeze receipt a finalization leaves behind
// (character_identity_proposals "fcs:<finalizationId>"), which the continuity list
// compares a projection against (summary.go:518-585).
func rrctxFrozenRoster(t *testing.T, db *sql.DB, finalizationID string, draftID, chapter int64,
	body, characters string) {
	t.Helper()
	raw := `{"kind":"` + summarySnapshotKind + `","source":{"draftId":` + phNumberString(draftID) +
		`,"finalizationId":"` + finalizationID + `","chapterNumber":` + phNumberString(chapter) +
		`,"contentHash":"` + rrctxSHA(body) + `"},"identityRevision":3,"characters":` + characters + `}`
	mustExec(t, db, `INSERT INTO character_identity_proposals(proposal_id,source_key,source_hash,raw_value,candidate_ids_json)
 VALUES(?,?,?,?,'[]')`, "fcs:"+finalizationID, summarySnapshotKind, rrctxSHA(raw), raw)
}

// rrctxCharacter seeds one active character with the given notes and an
// author-provenanced location at chapter 2.
func rrctxCharacter(t *testing.T, db *sql.DB, id, name, notes string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO characters(character_id,name,role,notes,retired,cs_location,cs_updated_at_chapter,cs_provenance)
 VALUES(?,?,'supporting',?,0,'旧城',2,'{"location":{"kind":"author","chapterNumber":2}}')`, id, name, notes)
}

// rrctxReviewRequest is the request shape the RPC layer hands the producer for
// review-chapter (review-revision-context.ts:41-53).
func rrctxReviewRequest(draftID, chapter, version int64, status, contentHash string) map[string]any {
	return map[string]any{
		"operation": "review-chapter",
		"draftId":   draftID,
		"expectedDraft": map[string]any{
			"chapterNumber": chapter,
			"version":       version,
			"status":        status,
			"contentHash":   contentHash,
		},
		"authorInputs": []any{map[string]any{"id": "review-focus", "text": rrctxFocusText}},
		"uiLocale":     "zh-CN",
	}
}

// rrctxCaptureOK runs one accepted capture and proves it wrote nothing.
func rrctxCaptureOK(t *testing.T, contexts *ReviewRevisionContexts, path string,
	request map[string]any) *FrozenReviewRevisionContext {
	t.Helper()
	before := rrctxDigest(t, path)
	frozen, err := contexts.Capture(context.Background(), rrctxTestProjectID, request)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if frozen == nil {
		t.Fatal("capture returned no frozen context")
	}
	if after := rrctxDigest(t, path); after != before {
		t.Fatal("capture changed the database")
	}
	return frozen
}

// rrctxRefusal proves one refused capture by its exact reference code and by the
// untouched file bytes.
func rrctxRefusal(t *testing.T, contexts *ReviewRevisionContexts, path string, want error,
	request map[string]any) {
	t.Helper()
	before := rrctxDigest(t, path)
	frozen, err := contexts.Capture(context.Background(), rrctxTestProjectID, request)
	if err == nil {
		t.Fatalf("expected refusal %v, got nil", want)
	}
	if frozen != nil {
		t.Fatalf("expected no frozen context on %v", want)
	}
	if !errors.Is(err, want) || err.Error() != want.Error() {
		t.Fatalf("expected refusal %v, got %v", want, err)
	}
	if after := rrctxDigest(t, path); after != before {
		t.Fatalf("%v changed the database", want)
	}
}

// rrctxConfigOrder is the hashed NovelConfig literal (review-revision-context.ts:
// 69-75), including the capital-OV narrativePOV key spelled from core.narrativePov.
func rrctxConfigOrder() plotObject {
	return plotObject{
		{"genre", "玄幻"},
		{"subGenre", "东方玄幻"},
		{"targetAudience", "成年读者"},
		{"totalChapters", int64(120)},
		{"wordsPerChapter", int64(3000)},
		{"writingLanguage", "zh-CN"},
		{"creativeStrategy", "auto"},
		{"narrativeThreadDormantChapterThreshold", int64(3)},
		{"plotStructure", "three_act"},
		{"narrativePOV", "third_limited"},
		{"writingStyle", "白描"},
		{"referenceWorks", "无"},
		{"globalGuidance", "保持节奏"},
		{"goldenFinger", "无"},
		{"coreOutline", "开局入城"},
		{"worldSetting", "旧城雨巷"},
		{"protagonistProfile", "主角沉默寡言"},
	}
}

// rrctxBlueprintOrder is the hashed blueprint literal (review-revision-context.ts:
// 93-94); notesUpdatedAt is dropped by the reference's destructuring.
func rrctxBlueprintOrder(chapter int64, keyEvents string, characters []any) plotObject {
	return plotObject{
		{"chapterNumber", chapter},
		{"title", "雨夜"},
		{"role", "推进"},
		{"purpose", "推进主线"},
		{"keyEvents", keyEvents},
		{"characters", characters},
		{"suspenseHook", "门外是谁"},
		{"userGuidance", ""},
		{"notes", ""},
	}
}

// rrctxKeys is the frozen object's key sequence, used to prove the two optional
// keys sit where the reference spreads them.
func rrctxKeys(object plotObject) []string {
	keys := make([]string, len(object))
	for i, field := range object {
		keys[i] = field.key
	}
	return keys
}

func rrctxKeyIndex(object plotObject, key string) int {
	for i, field := range object {
		if field.key == key {
			return i
		}
	}
	return -1
}

// rrctxHas reports the value and whether the key is present at all, which is the
// distinction this package's ordered objects preserve and a map would lose.
func rrctxHas(object plotObject, key string) (any, bool) {
	for _, field := range object {
		if field.key == key {
			return field.value, true
		}
	}
	return nil, false
}

func rrctxField(t *testing.T, object plotObject, key string) any {
	t.Helper()
	for _, field := range object {
		if field.key == key {
			return field.value
		}
	}
	t.Fatalf("missing key %v", key)
	return nil
}

// TestReviewRevisionContextFreezesFirstChapter is the no-history arm: one working
// draft, one blueprint, no finalized chapter. The whole frozen text is compared
// byte for byte, because the verifier recomputes its digest.
func TestReviewRevisionContextFreezesFirstChapter(t *testing.T) {
	db, path := rrctxFixture(t)
	rrctxCore(t, db)
	draft := rrctxDraft(t, db, 1, 1, 1, "revised", rrctxChapterOne, "[]")
	rrctxBlueprintRow(t, db, 1, "开门；见客\n点灯", `["张三"]`)
	contexts := NewReviewRevisionContexts(db)
	request := rrctxReviewRequest(draft, 1, 1, "revised", rrctxSHA(rrctxChapterOne))
	frozen := rrctxCaptureOK(t, contexts, path, request)

	expected := plotObject{
		{"version", int64(1)},
		{"operation", "review-chapter"},
		{"source", plotObject{
			{"id", draft},
			{"chapterNumber", int64(1)},
			{"version", int64(1)},
			{"status", "revised"},
			{"content", rrctxChapterOne},
		}},
		{"sourceHash", rrctxSHA(rrctxChapterOne)},
		{"config", rrctxConfigOrder()},
		{"writingLanguage", "zh-CN"},
		{"uiLocale", "zh-CN"},
		{"authorInputs", []any{plotObject{{"id", "review-focus"}, {"text", rrctxFocusText}}}},
		{"characterStates", "（暂无）"},
		{"worldbuilding", "架空旧城"},
		{"history", []any{}},
		{"blueprints", []any{rrctxBlueprintOrder(1, "开门；见客\n点灯", []any{"张三"})}},
		{"frozenGoals", plotObject{
			{"chapterNumber", int64(1)},
			{"coverage", "complete"},
			{"items", []any{
				plotObject{{"id", "ch1:keyEvents:1"}, {"text", "开门"}},
				plotObject{{"id", "ch1:keyEvents:2"}, {"text", "见客"}},
				plotObject{{"id", "ch1:keyEvents:3"}, {"text", "点灯"}},
			}},
		}},
		{"preflightFindings", []any{}},
	}
	if got, want := frozen.Text, rrctxJSON(t, expected); got != want {
		t.Fatalf("frozen text drifted from the reference literal\n got: %s\nwant: %s", got, want)
	}
	if frozen.ContextHash != rrctxSHA(frozen.Text) {
		t.Fatalf("context hash %v does not hash the frozen text", frozen.ContextHash)
	}
	if frozen.Operation != ReviewRevisionOperationReviewChapter || frozen.DraftID != draft ||
		frozen.ChapterNumber != 1 || frozen.Version != 1 || frozen.Status != "revised" {
		t.Fatalf("frozen identity: %v %v %v %v %v", frozen.Operation, frozen.DraftID,
			frozen.ChapterNumber, frozen.Version, frozen.Status)
	}
	if frozen.SourceHash != rrctxSHA(rrctxChapterOne) {
		t.Fatalf("source hash %v", frozen.SourceHash)
	}
	// The manifest the generation ledger persists must echo the same three values
	// the verifier reads back (reviewcycle_effect.go:166-207).
	manifest := frozen.SourceManifestFields()
	expectedManifest := plotObject{
		{"operation", "review-chapter"},
		{"authorInputs", []any{plotObject{
			{"id", "review-revision-context"},
			{"text", frozen.Text},
		}}},
		{"reviewRevisionContext", expected},
		{"reviewRevisionContextHash", rrctxSHA(frozen.Text)},
	}
	if got, want := rrctxJSON(t, manifest), rrctxJSON(t, expectedManifest); got != want {
		t.Fatalf("manifest drifted\n got: %s\nwant: %s", got, want)
	}
	// No history, no confirmation, no recheck: the verifier requires the last two
	// to be absent, not null (reviewcycle_effect.go:196).
	for _, key := range []string{"confirmation", "recheck", "predecessor"} {
		if rrctxKeyIndex(frozen.Context, key) >= 0 {
			t.Fatalf("frozen context carries %v", key)
		}
	}
}

// TestReviewRevisionContextIsPureAndSensitive proves the same inputs freeze the
// same bytes (twice, and through a second connection), and that changing a hashed
// input changes the digest.
func TestReviewRevisionContextIsPureAndSensitive(t *testing.T) {
	db, path := rrctxFixture(t)
	rrctxCore(t, db)
	draft := rrctxDraft(t, db, 1, 1, 1, "revised", rrctxChapterOne, "[]")
	rrctxBlueprintRow(t, db, 1, "开门", `["张三"]`)
	contexts := NewReviewRevisionContexts(db)
	request := rrctxReviewRequest(draft, 1, 1, "revised", rrctxSHA(rrctxChapterOne))
	first := rrctxCaptureOK(t, contexts, path, request)
	second := rrctxCaptureOK(t, contexts, path, request)
	if first.Text != second.Text || first.ContextHash != second.ContextHash {
		t.Fatal("two captures of unchanged inputs disagree")
	}
	if reopened := rrctxCaptureOK(t, NewReviewRevisionContexts(rrctxReopen(t, path)), path, request); reopened.ContextHash != first.ContextHash {
		t.Fatal("a reopened project freezes a different context")
	}
	// One hashed input moves: the blueprint's explicit list boundaries.
	mustExec(t, db, "UPDATE blueprints SET key_events='开门；关门' WHERE chapter_number=1")
	changed := rrctxCaptureOK(t, contexts, path, request)
	if changed.ContextHash == first.ContextHash {
		t.Fatal("a changed blueprint did not change the hash")
	}
	// Restoring the input restores the digest: no hidden state leaks in.
	mustExec(t, db, "UPDATE blueprints SET key_events='开门' WHERE chapter_number=1")
	if restored := rrctxCaptureOK(t, contexts, path, request); restored.ContextHash != first.ContextHash {
		t.Fatal("restoring the input did not restore the hash")
	}
	// The author's worldbuilding is hashed verbatim.
	mustExec(t, db, "UPDATE project_core SET worldbuilding='架空旧城（修订）' WHERE id='main'")
	if world := rrctxCaptureOK(t, contexts, path, request); world.ContextHash == first.ContextHash {
		t.Fatal("a changed worldbuilding did not change the hash")
	}
	mustExec(t, db, "UPDATE project_core SET worldbuilding='架空旧城' WHERE id='main'")
	// The UI locale is part of the frozen value even though it is not prose.
	other := rrctxReviewRequest(draft, 1, 1, "revised", rrctxSHA(rrctxChapterOne))
	other["uiLocale"] = "en-US"
	if locale := rrctxCaptureOK(t, contexts, path, other); locale.ContextHash == first.ContextHash {
		t.Fatal("a changed uiLocale did not change the hash")
	}
}

// TestReviewRevisionContextFreezesFinalizedHistory proves the continuity list the
// history carries: only finalized chapters before the source, one row per chapter,
// newest snapshot only, receipt/projection matching, and the two source statuses.
func TestReviewRevisionContextFreezesFinalizedHistory(t *testing.T) {
	db, path := rrctxFixture(t)
	rrctxCore(t, db)
	// The older draft of chapter one is seeded first so its snapshot row has the
	// lower id: the list must keep the newest snapshot, not the oldest.
	rrctxSnapshotOnly(t, db, 20, 1, 1, "旧版摘要",
		`[{"category":"plot","entities":["`+rrctxStaleName+`"],"statement":"旧事","sourceChapter":1,"evidence":"旧证"}]`)
	factOne := `[{"category":"character-state","entities":["阿青"],"statement":"阿青在雨里","sourceChapter":1,"evidence":"第一章原文"}]`
	rrctxFinalizedDraft(t, db, 1, 1, 2, rrctxFinalOne, rrctxChapterOne, "第一章摘要", factOne, "[]")
	rrctxFrozenRoster(t, db, rrctxFinalOne, 1, 1, rrctxChapterOne, "[]")
	// Chapter two is receipted but has no identity freeze receipt, so its
	// projection is preservable but not provable: sourceStatus stays "legacy".
	candidateTwo := `[{"characterName":"阿青","field":"location","value":"塔下","candidateKey":"k-2",` +
		`"source":{"contentHash":"` + rrctxSHA(rrctxChapterTwo) + `","chapterNumber":2,` +
		`"finalizationId":"` + rrctxFinalTwo + `","draftId":2},"expectedFieldRevision":2,` +
		`"expectedFieldValueHash":"` + rrctxSHA("塔下") + `","rawValue":{"b":1,"a":2}}]`
	rrctxFinalizedDraft(t, db, 2, 2, 1, rrctxFinalTwo, rrctxChapterTwo, "第二章摘要", "[]", candidateTwo)
	// Rows outside the bound: the source chapter itself and the chapter after it.
	rrctxFinalizedDraft(t, db, 3, 3, 1, "rrctx-final-3", rrctxChapterThree, "第三章摘要", "[]", "[]")
	rrctxFinalizedDraft(t, db, 4, 4, 1, "rrctx-final-4", "第四章：未到。", "第四章摘要", "[]", "[]")
	source := rrctxDraft(t, db, 10, 3, 2, "revised", rrctxSourceThree, "[]")
	contexts := NewReviewRevisionContexts(db)
	request := rrctxReviewRequest(source, 3, 2, "revised", rrctxSHA(rrctxSourceThree))
	frozen := rrctxCaptureOK(t, contexts, path, request)

	history, ok := rrctxField(t, frozen.Context, "history").([]any)
	if !ok || len(history) != 2 {
		t.Fatalf("history: %v", rrctxField(t, frozen.Context, "history"))
	}
	firstEntry, _ := history[0].(plotObject)
	secondEntry, _ := history[1].(plotObject)
	if rrctxField(t, firstEntry, "draftId") != int64(1) || rrctxField(t, firstEntry, "chapterNumber") != int64(1) {
		t.Fatalf("history order: %v", rrctxJSON(t, firstEntry))
	}
	if rrctxField(t, secondEntry, "draftId") != int64(2) || rrctxField(t, secondEntry, "chapterNumber") != int64(2) {
		t.Fatalf("history order: %v", rrctxJSON(t, secondEntry))
	}
	// The identity literal (review-revision-context.ts:108-110).
	identity, _ := rrctxField(t, firstEntry, "identity").(plotObject)
	if got, want := rrctxJSON(t, identity), rrctxJSON(t, plotObject{
		{"projectId", rrctxTestProjectID},
		{"sourceId", "finalized:1"},
		{"revision", int64(1)},
		{"contentHash", rrctxSHA(rrctxChapterOne)},
		{"provenance", "finalized"},
	}); got != want {
		t.Fatalf("history identity drifted\n got: %s\nwant: %s", got, want)
	}
	// The source literal beside the identity (review-revision-context.ts:103).
	sourceLiteral, _ := rrctxField(t, firstEntry, "source").(plotObject)
	if got, want := rrctxJSON(t, sourceLiteral), rrctxJSON(t, plotObject{
		{"draftId", int64(1)},
		{"finalizationId", rrctxFinalOne},
		{"chapterNumber", int64(1)},
		{"contentHash", rrctxSHA(rrctxChapterOne)},
	}); got != want {
		t.Fatalf("history source drifted\n got: %s\nwant: %s", got, want)
	}
	if rrctxField(t, firstEntry, "chapterTitle") != rrctxChapterTitle {
		t.Fatalf("chapter title: %v", rrctxField(t, firstEntry, "chapterTitle"))
	}
	// The projection of chapter one is provable, so it is "current" and carries
	// the reference's own normalized facts in the reference's key order.
	projectionOne, _ := rrctxField(t, firstEntry, "projection").(plotObject)
	if projectionOne == nil {
		t.Fatal("chapter one projection is missing")
	}
	if got, want := rrctxJSON(t, projectionOne), rrctxJSON(t, plotObject{
		{"draftId", int64(1)},
		{"chapterNumber", int64(1)},
		{"chapterTitle", rrctxChapterTitle},
		{"chapterNotes", "第一章摘要"},
		{"facts", []any{plotObject{
			{"category", "character-state"},
			{"entities", []any{"阿青"}},
			{"statement", "阿青在雨里"},
			{"sourceChapter", int64(1)},
			{"evidence", "第一章原文"},
		}}},
		{"source", plotObject{
			{"draftId", int64(1)},
			{"finalizationId", rrctxFinalOne},
			{"chapterNumber", int64(1)},
			{"contentHash", rrctxSHA(rrctxChapterOne)},
		}},
		{"sourceStatus", "current"},
	}); got != want {
		t.Fatalf("projection drifted\n got: %s\nwant: %s", got, want)
	}
	// Chapter two keeps a receipt but no identity freeze receipt, so its
	// projection is legacy. The reference attaches a projection to a history entry
	// only when it is current AND its frozen source equals the source being
	// revised (review-revision-context.ts:104):
	//
	//   const projection = projections.find(item => item.draftId === row.id
	//     && item.sourceStatus === 'current' && isDeepStrictEqual(item.source, sourceIdentity))
	//
	// So a legacy row contributes its identity and its material identity but no
	// projection, and preflightFindings (:152) never sees its facts. Asserting that
	// absence is the point of this block: an earlier version of this test expected a
	// legacy projection here and was wrong, not the capture.
	if value, present := rrctxHas(secondEntry, "projection"); present {
		t.Fatalf("chapter two carries a projection the reference would not attach: %v",
			rrctxJSON(t, value))
	}
	if rrctxField(t, secondEntry, "identity") == nil {
		t.Fatal("chapter two lost its identity")
	}
	// A second connection sees the same source rows, so the capture read
	// committed state rather than its own view.
	reopened := rrctxReopen(t, path)
	var notes string
	if err := reopened.QueryRow("SELECT chapter_notes FROM summary_snapshots WHERE draft_id=2").Scan(&notes); err != nil {
		t.Fatal(err)
	}
	if notes != "第二章摘要" {
		t.Fatalf("reopened notes: %v", notes)
	}
}

// TestReviewRevisionContextPreflightFindings proves the blueprint continuity risk
// at :152: a structured terminal character state from a provable projection is
// reported once, its stable key matches the reference derivation, an active
// exemption suppresses it, and a blueprint that no longer schedules the subject
// reports nothing.
func TestReviewRevisionContextPreflightFindings(t *testing.T) {
	db, path := rrctxFixture(t)
	rrctxCore(t, db)
	fact := `[{"category":"character-state","entities":["` + rrctxHeroName + `"],` +
		`"statement":"` + rrctxHeroName + `已经死亡","sourceChapter":1,"evidence":"原作第1章",` +
		`"characterRefs":[{"characterId":"` + rrctxHeroID + `","displayNameSnapshot":"` + rrctxHeroName + `"}]}]`
	rrctxFinalizedDraft(t, db, 1, 1, 1, rrctxFinalOne, rrctxChapterOne, "第一章摘要", fact, "[]")
	rrctxFrozenRoster(t, db, rrctxFinalOne, 1, 1, rrctxChapterOne,
		`[{"characterId":"`+rrctxHeroID+`","displayNameSnapshot":"`+rrctxHeroName+`","aliases":[]}]`)
	rrctxBlueprintRow(t, db, 2, "重逢", `["`+rrctxHeroName+`"]`)
	source := rrctxDraft(t, db, 2, 2, 1, "revised", rrctxChapterTwo, "[]")
	contexts := NewReviewRevisionContexts(db)
	request := rrctxReviewRequest(source, 2, 1, "revised", rrctxSHA(rrctxChapterTwo))
	frozen := rrctxCaptureOK(t, contexts, path, request)

	findings, ok := rrctxField(t, frozen.Context, "preflightFindings").([]any)
	if !ok || len(findings) != 1 {
		t.Fatalf("findings: %v", rrctxField(t, frozen.Context, "preflightFindings"))
	}
	finding, _ := findings[0].(plotObject)
	if got, want := rrctxField(t, finding, "stableFactKey"), rrctxFactKeyGolden; got != want {
		t.Fatalf("stable fact key: got %v, want %v", got, want)
	}
	if got := rrctxField(t, finding, "severity"); got != "warning" {
		t.Fatalf("severity: %v", got)
	}
	if got := rrctxField(t, finding, "sourceChapter"); got != int64(1) {
		t.Fatalf("source chapter: %v", got)
	}
	if got := rrctxField(t, finding, "evidence"); got != "原作第1章" {
		t.Fatalf("evidence: %v", got)
	}
	issue, _ := rrctxField(t, finding, "issue").(plotObject)
	if issue == nil || !strings.Contains(rrctxJSON(t, issue), rrctxHeroName) {
		t.Fatalf("issue: %v", rrctxJSON(t, finding))
	}
	suggestion, _ := rrctxField(t, finding, "suggestion").(plotObject)
	if suggestion == nil || !strings.Contains(rrctxJSON(t, suggestion), "回忆") {
		t.Fatalf("suggestion: %v", rrctxJSON(t, finding))
	}
	// The subject leaves the cast: no contradiction remains, so no finding.
	mustExec(t, db, "UPDATE blueprints SET characters=? WHERE chapter_number=2", `["`+rrctxOtherName+`"]`)
	if quiet := rrctxCaptureOK(t, contexts, path, request); len(rrctxField(t, quiet.Context, "preflightFindings").([]any)) != 0 {
		t.Fatal("a blueprint without the subject still reported a finding")
	}
	mustExec(t, db, "UPDATE blueprints SET characters=? WHERE chapter_number=2", `["`+rrctxHeroName+`"]`)
	// An active exemption suppresses the finding; a revoked one does not.
	mustExec(t, db, "INSERT INTO consistency_exemptions(stable_fact_key,reason,revoked) VALUES(?,?,0)",
		rrctxFactKeyGolden, "作者确认这是回忆")
	if exempt := rrctxCaptureOK(t, contexts, path, request); len(rrctxField(t, exempt.Context, "preflightFindings").([]any)) != 0 {
		t.Fatal("an active exemption did not suppress the finding")
	}
	mustExec(t, db, "UPDATE consistency_exemptions SET revoked=1 WHERE stable_fact_key=?", rrctxFactKeyGolden)
	if revoked := rrctxCaptureOK(t, contexts, path, request); len(rrctxField(t, revoked.Context, "preflightFindings").([]any)) != 1 {
		t.Fatal("a revoked exemption still suppressed the finding")
	}
}

// TestReviewRevisionContextFreezesChapterGoals proves the goal freeze: explicit
// chapter goals, author-source must-show markers limited to the current chapter,
// and the three coverage arms.
func TestReviewRevisionContextFreezesChapterGoals(t *testing.T) {
	db, path := rrctxFixture(t)
	rrctxCore(t, db)
	mustExec(t, db, "UPDATE project_core SET world_setting=? WHERE id='main'",
		"【第3章必现】雨停\n正文设定")
	rrctxCharacter(t, db, rrctxHeroID, rrctxHeroName,
		"   【第3章必现】 灯灭   \n【第4章必现】别章")
	// Only the current chapter and the five after it are in the window.
	rrctxBlueprintRow(t, db, 2, "不该出现的上一章", `[]`)
	rrctxBlueprintRow(t, db, 3, "", `[]`)
	rrctxBlueprintRow(t, db, 5, "窗口内", `[]`)
	rrctxBlueprintRow(t, db, 9, "超出窗口", `[]`)
	source := rrctxDraft(t, db, 3, 3, 1, "revised", rrctxChapterThree, "[]")
	contexts := NewReviewRevisionContexts(db)
	request := rrctxReviewRequest(source, 3, 1, "revised", rrctxSHA(rrctxChapterThree))
	frozen := rrctxCaptureOK(t, contexts, path, request)

	blueprints, _ := rrctxField(t, frozen.Context, "blueprints").([]any)
	if len(blueprints) != 2 {
		t.Fatalf("blueprint window: %v", rrctxJSON(t, rrctxField(t, frozen.Context, "blueprints")))
	}
	if got := rrctxField(t, blueprints[0].(plotObject), "chapterNumber"); got != int64(3) {
		t.Fatalf("first blueprint: %v", got)
	}
	if got := rrctxField(t, blueprints[1].(plotObject), "chapterNumber"); got != int64(5) {
		t.Fatalf("second blueprint: %v", got)
	}
	goals, _ := rrctxField(t, frozen.Context, "frozenGoals").(plotObject)
	if got, want := rrctxJSON(t, goals), rrctxJSON(t, plotObject{
		{"chapterNumber", int64(3)},
		{"coverage", "complete"},
		{"items", []any{
			plotObject{{"id", "ch3:mustShow:1"}, {"text", "雨停"}},
			plotObject{{"id", "ch3:mustShow:2"}, {"text", "灯灭"}},
		}},
	}); got != want {
		t.Fatalf("frozen goals drifted\n got: %s\nwant: %s", got, want)
	}
	// The character state line is bound to a chapter, not to a permanent rule.
	states, _ := rrctxField(t, frozen.Context, "characterStates").(string)
	if !strings.Contains(states, "旧城 @ch2") || !strings.Contains(states, "作者状态（按标注章节理解，非永久约束）") {
		t.Fatalf("character states: %v", states)
	}
	// No blueprint in the window: the checklist is unknown, never invented.
	quiet := rrctxDraft(t, db, 20, 20, 1, "revised", "第二十章：无纲。", "[]")
	quietRequest := rrctxReviewRequest(quiet, 20, 1, "revised", rrctxSHA("第二十章：无纲。"))
	unknown := rrctxCaptureOK(t, contexts, path, quietRequest)
	unknownGoals, _ := rrctxField(t, unknown.Context, "frozenGoals").(plotObject)
	if got := rrctxField(t, unknownGoals, "coverage"); got != "unknown" {
		t.Fatalf("coverage without a blueprint: %v", got)
	}
	// A blueprint with no explicit boundaries: configured, but empty.
	empty := rrctxDraft(t, db, 30, 30, 1, "revised", "第三十章：空纲。", "[]")
	rrctxBlueprintRow(t, db, 30, "", `[]`)
	emptyRequest := rrctxReviewRequest(empty, 30, 1, "revised", rrctxSHA("第三十章：空纲。"))
	unconfigured := rrctxCaptureOK(t, contexts, path, emptyRequest)
	unconfiguredGoals, _ := rrctxField(t, unconfigured.Context, "frozenGoals").(plotObject)
	if got := rrctxField(t, unconfiguredGoals, "coverage"); got != "not_configured" {
		t.Fatalf("coverage with empty key events: %v", got)
	}
}

// TestReviewRevisionContextFreezesPredecessor proves the author's selected
// predecessor is taken from the draft's own dependency ledger, that it is placed
// where the reference spreads it, and that a dependency whose prose moved is a
// history change rather than a silently different predecessor.
func TestReviewRevisionContextFreezesPredecessor(t *testing.T) {
	db, path := rrctxFixture(t)
	rrctxCore(t, db)
	rrctxDraft(t, db, 9, 1, 1, "revised", rrctxCandidateBody, "[]")
	dependencies := `[{"draftId":9,"contentHash":"` + rrctxSHA(rrctxCandidateBody) + `","kind":"candidate"}]`
	source := rrctxDraft(t, db, 10, 2, 1, "revised", rrctxChapterTwo, dependencies)
	contexts := NewReviewRevisionContexts(db)
	request := rrctxReviewRequest(source, 2, 1, "revised", rrctxSHA(rrctxChapterTwo))
	frozen := rrctxCaptureOK(t, contexts, path, request)

	keys := rrctxKeys(frozen.Context)
	blueprintsAt, predecessorAt, goalsAt := rrctxKeyIndex(frozen.Context, "blueprints"),
		rrctxKeyIndex(frozen.Context, "predecessor"), rrctxKeyIndex(frozen.Context, "frozenGoals")
	if predecessorAt < 0 || predecessorAt != blueprintsAt+1 || predecessorAt != goalsAt-1 {
		t.Fatalf("predecessor position: %v", keys)
	}
	if got, want := rrctxJSON(t, rrctxField(t, frozen.Context, "predecessor")), rrctxJSON(t, plotObject{
		{"draftId", int64(9)},
		{"chapterNumber", int64(1)},
		{"chapterTitle", ""},
		{"content", rrctxCandidateBody},
		{"identity", plotObject{
			{"projectId", rrctxTestProjectID},
			{"sourceId", "candidate:9"},
			{"revision", int64(1)},
			{"contentHash", rrctxSHA(rrctxCandidateBody)},
			{"provenance", "generated"},
		}},
	}); got != want {
		t.Fatalf("predecessor drifted\n got: %s\nwant: %s", got, want)
	}
	// The prose behind the dependency moved: the frozen predecessor would be a
	// different material, so the capture refuses.
	mustExec(t, db, "UPDATE contents SET body=? WHERE id=9", "候选稿：灯下有人。")
	rrctxRefusal(t, contexts, path, ErrReviewRevisionHistoryChanged, request)
}

// TestReviewRevisionContextRefusesInvalidRequests proves the request guard, each
// arm on its own fresh database and with the file proved untouched.
func TestReviewRevisionContextRefusesInvalidRequests(t *testing.T) {
	db, path := rrctxFixture(t)
	rrctxCore(t, db)
	draft := rrctxDraft(t, db, 1, 1, 1, "revised", rrctxChapterOne, "[]")
	contexts := NewReviewRevisionContexts(db)
	valid := func() map[string]any {
		return rrctxReviewRequest(draft, 1, 1, "revised", rrctxSHA(rrctxChapterOne))
	}
	mutations := map[string]func(map[string]any){
		"unknown key": func(request map[string]any) { request["reviewNotes"] = "x" },
		"unknown operation": func(request map[string]any) {
			request["operation"] = "summarize"
		},
		"draft id below one": func(request map[string]any) { request["draftId"] = int64(0) },
		"missing expected draft": func(request map[string]any) {
			delete(request, "expectedDraft")
		},
		"unknown expected field": func(request map[string]any) {
			request["expectedDraft"].(map[string]any)["wordCount"] = int64(1)
		},
		"missing author inputs": func(request map[string]any) { delete(request, "authorInputs") },
		"three author inputs": func(request map[string]any) {
			request["authorInputs"] = []any{
				map[string]any{"id": "review-focus", "text": "a"},
				map[string]any{"id": "review-focus", "text": "a"},
				map[string]any{"id": "review-focus", "text": "a"},
			}
		},
		"duplicate author input": func(request map[string]any) {
			request["authorInputs"] = []any{
				map[string]any{"id": "review-focus", "text": "a"},
				map[string]any{"id": "review-focus", "text": "b"},
			}
		},
		"input outside the operation": func(request map[string]any) {
			request["authorInputs"] = []any{map[string]any{"id": "user-prompt", "text": "a"}}
		},
		"input with unknown key": func(request map[string]any) {
			request["authorInputs"] = []any{map[string]any{"id": "review-focus", "text": "a", "kind": "b"}}
		},
		"non-string input text": func(request map[string]any) {
			request["authorInputs"] = []any{map[string]any{"id": "review-focus", "text": int64(1)}}
		},
		"confirmation on review-chapter": func(request map[string]any) {
			request["reviewSourceId"] = int64(4)
		},
		"empty review cycle": func(request map[string]any) {
			request["reviewCycleId"] = "  "
			request["expectedMergedHash"] = "h"
		},
		"recheck on refine-draft": func(request map[string]any) {
			request["operation"] = "refine-draft"
			request["authorInputs"] = []any{map[string]any{"id": "user-prompt", "text": "a"}}
			request["reviewCycleId"] = "cycle-1"
			request["expectedMergedHash"] = "h"
		},
		"unknown locale": func(request map[string]any) { request["uiLocale"] = "fr-FR" },
	}
	for name, mutate := range mutations {
		request := valid()
		mutate(request)
		t.Run(name, func(t *testing.T) {
			rrctxRefusal(t, contexts, path, ErrReviewRevisionContextInvalid, request)
		})
	}
	t.Run("empty project id", func(t *testing.T) {
		before := rrctxDigest(t, path)
		frozen, err := contexts.Capture(context.Background(), "  ", valid())
		if !errors.Is(err, ErrReviewRevisionContextProjectRequired) || frozen != nil {
			t.Fatalf("expected %v, got %v", ErrReviewRevisionContextProjectRequired, err)
		}
		if after := rrctxDigest(t, path); after != before {
			t.Fatal("refusal changed the database")
		}
	})
}

// TestReviewRevisionContextRefusesUnprovableMaterial proves the arms this lane
// cannot freeze and the state proofs a capture makes: an unreadable project row, a
// missing project row, a source that is no longer the author's selection, and a
// receipt whose prose moved.
func TestReviewRevisionContextRefusesUnprovableMaterial(t *testing.T) {
	t.Run("missing project row", func(t *testing.T) {
		db, path := rrctxFixture(t)
		draft := rrctxDraft(t, db, 1, 1, 1, "revised", rrctxChapterOne, "[]")
		contexts := NewReviewRevisionContexts(db)
		rrctxRefusal(t, contexts, path, ErrReviewRevisionContextInvalid,
			rrctxReviewRequest(draft, 1, 1, "revised", rrctxSHA(rrctxChapterOne)))
	})
	t.Run("expected draft disagrees", func(t *testing.T) {
		db, path := rrctxFixture(t)
		rrctxCore(t, db)
		draft := rrctxDraft(t, db, 1, 1, 1, "revised", rrctxChapterOne, "[]")
		contexts := NewReviewRevisionContexts(db)
		rrctxRefusal(t, contexts, path, ErrReviewRevisionSourceChanged,
			rrctxReviewRequest(draft, 1, 1, "reviewed", rrctxSHA(rrctxChapterOne)))
		rrctxRefusal(t, contexts, path, ErrReviewRevisionSourceChanged,
			rrctxReviewRequest(draft, 1, 1, "revised", rrctxSHA(rrctxChapterTwo)))
	})
	t.Run("draft is not the latest", func(t *testing.T) {
		db, path := rrctxFixture(t)
		rrctxCore(t, db)
		old := rrctxDraft(t, db, 1, 1, 1, "revised", rrctxChapterOne, "[]")
		rrctxDraft(t, db, 2, 1, 2, "revised", rrctxChapterTwo, "[]")
		contexts := NewReviewRevisionContexts(db)
		rrctxRefusal(t, contexts, path, ErrReviewRevisionSourceChanged,
			rrctxReviewRequest(old, 1, 1, "revised", rrctxSHA(rrctxChapterOne)))
	})
	t.Run("missing draft", func(t *testing.T) {
		db, path := rrctxFixture(t)
		rrctxCore(t, db)
		contexts := NewReviewRevisionContexts(db)
		rrctxRefusal(t, contexts, path, ErrReviewRevisionSourceChanged,
			rrctxReviewRequest(404, 1, 1, "revised", rrctxSHA(rrctxChapterOne)))
	})
	t.Run("receipt does not describe the prose", func(t *testing.T) {
		db, path := rrctxFixture(t)
		rrctxCore(t, db)
		rrctxFinalizedDraft(t, db, 1, 1, 1, rrctxFinalOne, rrctxChapterOne, "第一章摘要", "[]", "[]")
		mustExec(t, db, "UPDATE finalization_outbox SET content_hash=? WHERE draft_id=1", rrctxSHA("别的正文"))
		source := rrctxDraft(t, db, 2, 2, 1, "revised", rrctxChapterTwo, "[]")
		contexts := NewReviewRevisionContexts(db)
		rrctxRefusal(t, contexts, path, ErrReviewRevisionHistoryChanged,
			rrctxReviewRequest(source, 2, 1, "revised", rrctxSHA(rrctxChapterTwo)))
	})
	t.Run("refine from review needs a confirmation", func(t *testing.T) {
		db, path := rrctxFixture(t)
		rrctxCore(t, db)
		draft := rrctxDraft(t, db, 1, 1, 1, "revised", rrctxChapterOne, "[]")
		contexts := NewReviewRevisionContexts(db)
		request := rrctxReviewRequest(draft, 1, 1, "revised", rrctxSHA(rrctxChapterOne))
		request["operation"] = "refine-from-review"
		request["authorInputs"] = []any{}
		request["reviewSourceId"] = int64(7)
		request["confirmedReviewContent"] = "{}"
		rrctxRefusal(t, contexts, path, ErrReviewRevisionContextConfirmationUnavailable, request)
	})
	t.Run("recheck needs the planner", func(t *testing.T) {
		db, path := rrctxFixture(t)
		rrctxCore(t, db)
		draft := rrctxDraft(t, db, 1, 1, 1, "revised", rrctxChapterOne, "[]")
		contexts := NewReviewRevisionContexts(db)
		request := rrctxReviewRequest(draft, 1, 1, "revised", rrctxSHA(rrctxChapterOne))
		request["reviewCycleId"] = "cycle-1"
		request["expectedMergedHash"] = rrctxSHA(rrctxChapterOne)
		rrctxRefusal(t, contexts, path, ErrReviewRevisionContextRecheckUnavailable, request)
	})
	t.Run("worldbuilding must be text", func(t *testing.T) {
		db, path := rrctxFixture(t)
		rrctxCore(t, db)
		mustExec(t, db, "UPDATE project_core SET worldbuilding=NULL WHERE id='main'")
		draft := rrctxDraft(t, db, 1, 1, 1, "revised", rrctxChapterOne, "[]")
		contexts := NewReviewRevisionContexts(db)
		rrctxRefusal(t, contexts, path, ErrReviewRevisionContextWorldbuildingInvalid,
			rrctxReviewRequest(draft, 1, 1, "revised", rrctxSHA(rrctxChapterOne)))
	})
	t.Run("blueprint cast must be text", func(t *testing.T) {
		db, path := rrctxFixture(t)
		rrctxCore(t, db)
		rrctxBlueprintRow(t, db, 1, "开门", `[{"name":"张三"}]`)
		draft := rrctxDraft(t, db, 1, 1, 1, "revised", rrctxChapterOne, "[]")
		contexts := NewReviewRevisionContexts(db)
		rrctxRefusal(t, contexts, path, ErrReviewRevisionContextBlueprintInvalid,
			rrctxReviewRequest(draft, 1, 1, "revised", rrctxSHA(rrctxChapterOne)))
	})
}
