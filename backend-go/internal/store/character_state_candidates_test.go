package store

// These tests provision a real SQLite database from the pinned asset in
// internal/admission (WriteBaselineV7, baseline_v7_ddl.go:744) and drive the
// finalized-character state-candidate operations of
// character_state_candidates.go against the real schema: drafts, contents,
// finalization_outbox, continuity_projection_meta, character_identity_meta,
// character_identity_proposals, characters, character_roster_meta, project_core
// and the candidate store in summary_snapshots.character_state_candidates
// (baseline_v7_ddl.go:630-643).
//
// The candidate key is asserted against a golden value derived once from the
// reference's own definition (summary-repository.ts:272-274:
// "fcs-state:" + sha256(JSON.stringify([draftId, finalizationId, contentHash,
// characterId, field, sha256(value)]))), so the test fails if the derivation
// drifts by one byte - the stored candidate is looked up by that key later.
//
// Every refusal is proved twice over: by its exact reference code, and by the
// file's own bytes, which the refused call must leave untouched. Each refusal
// runs on its own fresh database, so no earlier state can stand in for a later
// one. Every accepted effect is proved a second time through a separate mode=ro
// connection, which both proves the write is durable on disk and excludes the
// possibility that the writer only saw its own uncommitted view.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
)

const (
	cscBody           = "山雨欲来，风满楼。"
	cscFinalizationID = "fcs-final-1"
	cscSecondFinalID  = "fcs-final-2"
	cscThirdFinalID   = "fcs-final-3"
	cscCharacterID    = "c-青"
	cscCharacterName  = "阿青"
	cscFrozenName     = "阿青（冻结）"
	cscField          = "location"
	cscAuthorLocation = "作者地点"
	cscAuthorRevision = int64(3)
	// The exact stored value: leading spaces and a newline are part of the prose
	// a source-bound candidate keeps verbatim (summary-repository.ts:157).
	cscCandidateValue = "  北塔\n"
	// Golden derivations, computed outside this package from the reference's two
	// digests. Both are byte-exact contracts: a stored candidate is found by the
	// first, and a replayed decision is matched by the second.
	cscCandidateKey   = "fcs-state:48675eb46c7719c4bd4c95c90296d45eee83416e855f6cfaeca187c441871f96"
	cscPayloadHash    = "8fd3f70c4daa649bf71f6288097f1856fcef9f92828a0173c0175b7e3a9949bf"
	cscOperationID    = "accept-location-1"
	cscDeclineID      = "decline-location-1"
	cscRosterRevision = int64(7)
	cscAuthorArch     = "作者尚未刷新静态Markdown"
)

// cscFixture provisions the pinned v7 schema in a fresh file, then opens it the
// way a project database is opened (ExistingDSN: mode=rw, foreign keys on).
func cscFixture(t *testing.T) (*sql.DB, *FinalizedCharacters, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "character state candidates #? 中文.db")
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
	// A character-managed project always has its identity singleton
	// (baseline_v7_ddl.go:123-125); the freeze receipt below records revision 3.
	mustExec(t, db, "INSERT INTO character_identity_meta(id,revision,legacy_count) VALUES('main',3,0)")
	// The source read INNER JOINs this row (finalized_character_context.go:307-313),
	// so it must exist; generation 2 is what every snapshot row below also stores.
	mustExec(t, db, "INSERT INTO continuity_projection_meta(id,generation) VALUES('main',2)")
	// The roster meta row makes the projection refresh observable; the author's
	// compatibility prose must survive an accepted state decision.
	mustExec(t, db, `INSERT INTO character_roster_meta(id,schema_version,revision,migration_state,legacy_markdown,projection_hash,fact_hash)
 VALUES('main',1,?, 'ready','','','')`, cscRosterRevision)
	mustExec(t, db, "INSERT INTO project_core(id,project_name,characters_arch) VALUES('main','合成项目',?)", cscAuthorArch)
	return db, NewFinalizedCharacters(db), path
}

// cscReadOnly opens the same file a second time, read-only, so a write is proved
// by a connection that could not have performed it.
func cscReadOnly(t *testing.T, path string) *sql.DB {
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

// cscDigest is the whole file's bytes: a call that wrote anything at all fails
// even if it committed no logical row.
func cscDigest(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// cscRefusal runs one refused call and proves both the exact reference code and
// that the database is byte-identical afterwards.
func cscRefusal(t *testing.T, path string, want error, call func() error) {
	t.Helper()
	before := cscDigest(t, path)
	err := call()
	if err == nil {
		t.Fatalf("expected refusal %v, got nil", want)
	}
	if !errors.Is(err, want) || err.Error() != want.Error() {
		t.Fatalf("expected refusal %v, got %v", want, err)
	}
	if after := cscDigest(t, path); after != before {
		t.Fatalf("%v changed the database", want)
	}
}

// cscJSON encodes a fixture value exactly the way the reference's JSON.stringify
// would, so a seeded store or an expected write-back is compared byte for byte.
func cscJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := plotJSON(value)
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return raw
}

// cscSourceOrder is the reference's FinalizedSourceIdentity key order
// (src/shared/finalized-continuity.ts:12-17).
func cscSourceOrder(source FinalizedCharacterSourceIdentity) plotObject {
	return plotObject{{"draftId", source.DraftID}, {"finalizationId", source.FinalizationID},
		{"chapterNumber", source.ChapterNumber}, {"contentHash", source.ContentHash}}
}

// cscDraft seeds one finalized draft with its frozen prose, the immutable
// finalization receipt beside it and the identity freeze receipt that names
// cscFrozenName, which is the name a candidate must carry to be pending.
func cscDraft(t *testing.T, db *sql.DB, id, chapter int64, finalizationID, body string) FinalizedCharacterSourceIdentity {
	t.Helper()
	source := FinalizedCharacterSourceIdentity{DraftID: id, ChapterNumber: chapter,
		FinalizationID: finalizationID, ContentHash: summaryHash(body)}
	mustExec(t, db, "INSERT INTO contents(id,body) VALUES(?,?)", id, body)
	mustExec(t, db, `INSERT INTO drafts(id,chapter_number,version,status,content_id,word_count)
 VALUES(?,?,1,'finalized',?,?)`, id, chapter, id, len(body))
	mustExec(t, db, `INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,chapter_title,
 content_hash,content_revision,content_snapshot,target_file_name) VALUES(?,?,?,?,?,1,?,'csc.txt')`,
		finalizationID, id, chapter, "合成章节", source.ContentHash, body)
	raw := cscJSON(t, plotObject{{"kind", summarySnapshotKind}, {"source", cscSourceOrder(source)},
		{"identityRevision", int64(3)},
		{"characters", []any{plotObject{{"characterId", cscCharacterID},
			{"displayNameSnapshot", cscFrozenName}, {"aliases", []any{}}}}}})
	mustExec(t, db, `INSERT OR REPLACE INTO character_identity_proposals(proposal_id,source_key,source_hash,raw_value,candidate_ids_json)
 VALUES(?,'`+summarySnapshotKind+`',?,?,'[]')`, "fcs:"+finalizationID, summaryHash(raw), raw)
	return source
}

// cscProvenance is the stored field-level provenance of an author-protected
// field: location is the author's, at revision cscAuthorRevision, and powerLevel is
// an unrelated derived field whose position and bytes a decision must preserve.
func cscProvenance(t *testing.T, revision int64) string {
	t.Helper()
	return cscJSON(t, plotObject{
		{"location", plotObject{{"kind", "author"}, {"chapterNumber", int64(1)}, {"revision", revision}}},
		{"powerLevel", plotObject{{"kind", "derived"}, {"revision", int64(1)}}},
	})
}

// cscCharacter seeds the live character row an accepted decision mutates. Its
// static name is deliberately not the frozen display name.
func cscCharacter(t *testing.T, db *sql.DB, provenance string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO characters(character_id,name,retired,cs_updated_at_chapter,cs_location,cs_power_level,cs_provenance)
 VALUES(?,?,0,1,?,?,?)`, cscCharacterID, cscCharacterName, cscAuthorLocation, "练气", provenance)
}

// cscCandidate is a source-bound candidate in the reference's own key order
// (summary-repository.ts:153-168). reason is empty for a derived proposal.
func cscCandidate(t *testing.T, source FinalizedCharacterSourceIdentity, value, key, reason string) plotObject {
	t.Helper()
	object := plotObject{{"characterId", cscCharacterID}, {"characterName", cscFrozenName},
		{"field", cscField}, {"value", value}}
	if reason != "" {
		object = append(object, plotField{"reason", reason})
	}
	object = append(object, plotField{"candidateKey", key}, plotField{"source", cscSourceOrder(source)},
		plotField{"expectedFieldRevision", cscAuthorRevision},
		plotField{"expectedFieldValueHash", summaryHash(cscAuthorLocation)})
	return object
}

// cscLegacyCandidate is a preserved locator: it carries no bound metadata at all
// (summary-repository.ts:57-58), so only a read may upgrade it.
func cscLegacyCandidate(value string) plotObject {
	return plotObject{{"characterId", cscCharacterID}, {"characterName", cscFrozenName},
		{"field", cscField}, {"value", value}}
}

// cscStore is the version-2 candidate store of one snapshot row (:177).
func cscStore(t *testing.T, pending, receipts []any) string {
	t.Helper()
	return cscJSON(t, plotObject{{"version", int64(2)}, {"pending", pending}, {"receipts", receipts}})
}

// cscSnapshot seeds the summary_snapshots row a draft's candidates live in
// (baseline_v7_ddl.go:630-643). projection_generation 2 is the watermark the
// fixture's continuity_projection_meta row carries, so the source guard passes.
func cscSnapshot(t *testing.T, db *sql.DB, source FinalizedCharacterSourceIdentity, store string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO summary_snapshots(draft_id,chapter_number,chapter_notes,character_state_candidates,
 source_finalization_id,source_content_hash,projection_generation) VALUES(?,?,'',?,?,?,2)`,
		source.DraftID, source.ChapterNumber, store, source.FinalizationID, source.ContentHash)
}

// cscRequest is the decision request a served caller sends back from a read.
func cscRequest(decision, operationID string) FinalizedCharacterStateDecisionRequest {
	return FinalizedCharacterStateDecisionRequest{DraftID: 1, CandidateKey: cscCandidateKey,
		CharacterID: cscCharacterID, Field: cscField, ExpectedFieldRevision: cscAuthorRevision,
		ExpectedFieldValueHash: summaryHash(cscAuthorLocation), OperationID: operationID,
		Decision: decision}
}

// cscStoredCandidate is one seeded draft with a live character and one pending
// source-bound candidate, the shape every decision test starts from.
func cscStoredCandidate(t *testing.T) (*sql.DB, *FinalizedCharacters, string) {
	t.Helper()
	db, characters, path := cscFixture(t)
	source := cscDraft(t, db, 1, 1, cscFinalizationID, cscBody)
	cscCharacter(t, db, cscProvenance(t, cscAuthorRevision))
	cscSnapshot(t, db, source, cscStore(t, []any{cscCandidate(t, source, cscCandidateValue, cscCandidateKey, "")}, []any{}))
	return db, characters, path
}

func TestFinalizedCharacterStateCandidatesListReturnsOnlyPendingOfDraft(t *testing.T) {
	db, characters, path := cscFixture(t)
	source := cscDraft(t, db, 1, 1, cscFinalizationID, cscBody)
	cscCharacter(t, db, cscProvenance(t, cscAuthorRevision))
	cscSnapshot(t, db, source, cscStore(t, []any{cscCandidate(t, source, cscCandidateValue, cscCandidateKey, "")}, []any{}))

	// A second draft whose store holds a decision but no pending candidate: the
	// outer query still selects it (:481) and it contributes nothing.
	second := cscDraft(t, db, 2, 2, cscSecondFinalID, "第二段正文。")
	cscSnapshot(t, db, second, cscStore(t, []any{}, []any{plotObject{
		{"candidateKey", cscCandidateKey}, {"operationId", "already-decided"},
		{"payloadHash", cscPayloadHash}, {"decision", FinalizedCharacterStateDecisionAccept},
		{"source", cscSourceOrder(second)}}}))

	// A third draft whose candidate carries a format-valid key that no longer
	// derives from it, and a character name the freeze receipt does not carry:
	// summary-repository.ts:359-369 keeps neither.
	third := cscDraft(t, db, 3, 3, cscThirdFinalID, "第三段正文。")
	stale := cscCandidate(t, third, cscCandidateValue, summaryHash("no longer derived"), "")
	stale[1] = plotField{"characterName", "另一个名字"}
	cscSnapshot(t, db, third, cscStore(t, []any{stale}, []any{}))

	before := cscDigest(t, path)
	list, err := characters.ListPendingStateCandidates(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if after := cscDigest(t, path); after != before {
		t.Fatal("the list wrote to the database")
	}
	want := []FinalizedCharacterStateCandidateSummary{{
		DraftID:        1,
		CandidateKey:   cscCandidateKey,
		FinalizationID: cscFinalizationID,
		CharacterID:    cscCharacterID,
		CharacterName:  cscFrozenName,
		Field:          cscField,
	}}
	if !reflect.DeepEqual(list, want) {
		t.Fatalf("list %+v, want %+v", list, want)
	}
	// The same projection over a connection that could not have written it.
	readOnly := cscReadOnly(t, path)
	rows, err := readOnly.Query(`SELECT character_state_candidates FROM summary_snapshots WHERE draft_id=1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no snapshot row on the read-only connection")
	}
	var stored string
	if err := rows.Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != cscStore(t, []any{cscCandidate(t, source, cscCandidateValue, cscCandidateKey, "")}, []any{}) {
		t.Fatalf("stored store %v", stored)
	}
}

func TestFinalizedCharacterStateCandidatesReadRefusesUnknownKey(t *testing.T) {
	db, characters, path := cscFixture(t)
	source := cscDraft(t, db, 1, 1, cscFinalizationID, cscBody)
	cscCharacter(t, db, cscProvenance(t, cscAuthorRevision))
	cscSnapshot(t, db, source, cscStore(t, []any{cscCandidate(t, source, cscCandidateValue, cscCandidateKey, "")}, []any{}))
	ctx := context.Background()

	before := cscDigest(t, path)
	candidate, err := characters.ReadPendingStateCandidate(ctx, 1, cscCandidateKey)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if after := cscDigest(t, path); after != before {
		t.Fatal("the read wrote to the database")
	}
	want := &FinalizedCharacterStateCandidate{
		CharacterID:            cscCharacterID,
		CharacterName:          cscFrozenName,
		Field:                  cscField,
		Value:                  cscCandidateValue,
		CandidateKey:           cscCandidateKey,
		Source:                 source,
		ExpectedFieldRevision:  cscAuthorRevision,
		ExpectedFieldValueHash: summaryHash(cscAuthorLocation),
	}
	if !reflect.DeepEqual(candidate, want) {
		t.Fatalf("candidate %+v, want %+v", candidate, want)
	}
	// :500 the argument guard and :505 the unknown key, each proved byte-stable.
	cscRefusal(t, path, ErrFinalizedCharacterStateCandidateInvalid, func() error {
		_, err := characters.ReadPendingStateCandidate(ctx, 1, "   ")
		return err
	})
	cscRefusal(t, path, ErrFinalizedCharacterStateCandidateInvalid, func() error {
		_, err := characters.ReadPendingStateCandidate(ctx, 0, cscCandidateKey)
		return err
	})
	cscRefusal(t, path, ErrFinalizedCharacterStateCandidateNotFound, func() error {
		_, err := characters.ReadPendingStateCandidate(ctx, 1, summaryHash("unknown candidate"))
		return err
	})
	// A draft with no snapshot row is the :349 source refusal, not an empty read.
	cscRefusal(t, path, ErrFinalizedCharacterStateSourceChanged, func() error {
		_, err := characters.ReadPendingStateCandidate(ctx, 2, cscCandidateKey)
		return err
	})
}

func TestFinalizedCharacterStateCandidatesReadUpgradesLegacyWithoutWriting(t *testing.T) {
	db, characters, path := cscFixture(t)
	source := cscDraft(t, db, 1, 1, cscFinalizationID, cscBody)
	cscCharacter(t, db, cscProvenance(t, cscAuthorRevision))
	legacy := cscStore(t, []any{cscLegacyCandidate(cscCandidateValue)}, []any{})
	cscSnapshot(t, db, source, legacy)
	ctx := context.Background()

	// The key of a legacy candidate is derived from its trimmed value
	// (summary-repository.ts:157), so the list is the only way to learn it.
	list, err := characters.ListPendingStateCandidates(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list %+v, %v", list, err)
	}
	before := cscDigest(t, path)
	candidate, err := characters.ReadPendingStateCandidate(ctx, 1, list[0].CandidateKey)
	if err != nil {
		t.Fatalf("read legacy: %v", err)
	}
	// Reads do not write: the reference persists the derived key here (:507-511),
	// and this port deliberately does not, so the file must be unchanged.
	if after := cscDigest(t, path); after != before {
		t.Fatal("the read persisted the upgrade")
	}
	if candidate.CandidateKey != list[0].CandidateKey || candidate.Value != summaryTrim(cscCandidateValue) ||
		candidate.ExpectedFieldRevision != cscAuthorRevision ||
		candidate.ExpectedFieldValueHash != summaryHash(cscAuthorLocation) ||
		candidate.Source != source || candidate.CharacterID != cscCharacterID {
		t.Fatalf("upgraded %+v", candidate)
	}
	var stored string
	if err := db.QueryRow("SELECT character_state_candidates FROM summary_snapshots WHERE draft_id=1").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != legacy {
		t.Fatalf("legacy store was rewritten: %v", stored)
	}
	// The derived key is still what a decide addresses, even though it is not
	// stored: the decision consumes the candidate and stages its receipt. The key
	// differs from cscCandidateKey because it was derived from the trimmed value.
	request := cscRequest(FinalizedCharacterStateDecisionDecline, cscDeclineID)
	request.CandidateKey = list[0].CandidateKey
	if request.CandidateKey == cscCandidateKey {
		t.Fatal("the legacy upgrade derived the stored key")
	}
	receipt, err := characters.DecideStateCandidate(ctx, request)
	if err != nil {
		t.Fatalf("decide upgraded candidate: %v", err)
	}
	if receipt.CandidateKey != list[0].CandidateKey || receipt.Decision != FinalizedCharacterStateDecisionDecline {
		t.Fatalf("receipt %+v", receipt)
	}
	remaining, err := characters.ListPendingStateCandidates(ctx)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("after decline %+v, %v", remaining, err)
	}
}

func TestFinalizedCharacterStateCandidatesDecideApplies(t *testing.T) {
	_, characters, path := cscStoredCandidate(t)
	source := FinalizedCharacterSourceIdentity{DraftID: 1, ChapterNumber: 1,
		FinalizationID: cscFinalizationID, ContentHash: summaryHash(cscBody)}

	receipt, err := characters.DecideStateCandidate(context.Background(), cscRequest(FinalizedCharacterStateDecisionAccept, cscOperationID))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	want := &FinalizedCharacterStateDecisionReceipt{CandidateKey: cscCandidateKey,
		OperationID: cscOperationID, PayloadHash: cscPayloadHash,
		Decision: FinalizedCharacterStateDecisionAccept, Source: source, Idempotent: false}
	if !reflect.DeepEqual(receipt, want) {
		t.Fatalf("receipt %+v, want %+v", receipt, want)
	}

	// Every effect is read back through a connection that could not have written
	// it: the field became the candidate's verbatim prose, its provenance became an
	// author entry at the next revision and kept its stored key order, the candidate
	// left the store and the decision became a receipt, and the roster projection
	// was invalidated without rewriting the author's compatibility prose.
	readOnly := cscReadOnly(t, path)
	var location, provenance, arch string
	var updatedAtChapter int64
	if err := readOnly.QueryRow(`SELECT cs_location,cs_provenance,cs_updated_at_chapter FROM characters WHERE character_id=?`,
		cscCharacterID).Scan(&location, &provenance, &updatedAtChapter); err != nil {
		t.Fatal(err)
	}
	if location != cscCandidateValue {
		t.Fatalf("location %v, want %v", location, cscCandidateValue)
	}
	if updatedAtChapter != 1 {
		t.Fatalf("cs_updated_at_chapter %v", updatedAtChapter)
	}
	wantProvenance := cscJSON(t, plotObject{
		{"location", plotObject{{"kind", "author"}, {"chapterNumber", int64(1)}, {"revision", cscAuthorRevision + 1}}},
		{"powerLevel", plotObject{{"kind", "derived"}, {"revision", int64(1)}}},
	})
	if provenance != wantProvenance {
		t.Fatalf("provenance %v, want %v", provenance, wantProvenance)
	}
	wantStore := cscStore(t, []any{}, []any{plotObject{
		{"candidateKey", cscCandidateKey}, {"operationId", cscOperationID},
		{"payloadHash", cscPayloadHash}, {"decision", FinalizedCharacterStateDecisionAccept},
		{"source", cscSourceOrder(source)}}})
	var stored string
	if err := readOnly.QueryRow("SELECT character_state_candidates FROM summary_snapshots WHERE draft_id=1").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != wantStore {
		t.Fatalf("store %v, want %v", stored, wantStore)
	}
	var revision int64
	var factHash string
	if err := readOnly.QueryRow("SELECT revision,fact_hash FROM character_roster_meta WHERE id='main'").Scan(&revision, &factHash); err != nil {
		t.Fatal(err)
	}
	if revision != cscRosterRevision+1 || !summaryHashValid(factHash) {
		t.Fatalf("roster revision %v, fact hash %v", revision, factHash)
	}
	if err := readOnly.QueryRow("SELECT characters_arch FROM project_core WHERE id='main'").Scan(&arch); err != nil {
		t.Fatal(err)
	}
	if arch != cscAuthorArch {
		t.Fatalf("author compatibility prose was rewritten: %v", arch)
	}
	// The decided candidate is no longer pending on the writer's own connection.
	remaining, err := characters.ListPendingStateCandidates(context.Background())
	if err != nil || len(remaining) != 0 {
		t.Fatalf("after accept %+v, %v", remaining, err)
	}
}

func TestFinalizedCharacterStateCandidatesDecideRefusesStaleRevision(t *testing.T) {
	db, characters, path := cscStoredCandidate(t)
	// The author edited the field's provenance after the candidate was staged: the
	// value is unchanged but the field has moved to revision 4.
	mustExec(t, db, "UPDATE characters SET cs_provenance=? WHERE character_id=?",
		cscProvenance(t, cscAuthorRevision+1), cscCharacterID)

	cscRefusal(t, path, ErrFinalizedCharacterStateFieldConflict, func() error {
		_, err := characters.DecideStateCandidate(context.Background(), cscRequest(FinalizedCharacterStateDecisionAccept, cscOperationID))
		return err
	})
	// A refused decision leaves the candidate pending, so the author can still
	// decline it after the conflict.
	list, err := characters.ListPendingStateCandidates(context.Background())
	if err != nil || len(list) != 1 || list[0].CandidateKey != cscCandidateKey {
		t.Fatalf("candidate after conflict %+v, %v", list, err)
	}
	// The same expectation is still refused when only the value moved.
	mustExec(t, db, "UPDATE characters SET cs_location=? WHERE character_id=?", "作者并发修改", cscCharacterID)
	cscRefusal(t, path, ErrFinalizedCharacterStateFieldConflict, func() error {
		_, err := characters.DecideStateCandidate(context.Background(), cscRequest(FinalizedCharacterStateDecisionAccept, cscOperationID))
		return err
	})
	// A request whose expectation no longer matches the stored candidate is not
	// found at all (:540-544), and stages nothing.
	cscRefusal(t, path, ErrFinalizedCharacterStateCandidateNotFound, func() error {
		moved := cscRequest(FinalizedCharacterStateDecisionDecline, cscDeclineID)
		moved.ExpectedFieldValueHash = summaryHash("other field value")
		_, err := characters.DecideStateCandidate(context.Background(), moved)
		return err
	})
}

func TestFinalizedCharacterStateCandidatesDecideStagesAuthorProtectedProposal(t *testing.T) {
	db, characters, path := cscFixture(t)
	source := cscDraft(t, db, 1, 1, cscFinalizationID, cscBody)
	provenance := cscProvenance(t, cscAuthorRevision)
	cscCharacter(t, db, provenance)
	// An author-protected field stages a proposal (summary-repository.ts:455-462);
	// declining it is the arm of a decide that must decide "nothing to do".
	cscSnapshot(t, db, source, cscStore(t, []any{
		cscCandidate(t, source, cscCandidateValue, cscCandidateKey, "author-protected")}, []any{}))

	receipt, err := characters.DecideStateCandidate(context.Background(),
		cscRequest(FinalizedCharacterStateDecisionDecline, cscDeclineID))
	if err != nil {
		t.Fatalf("decline: %v", err)
	}
	payload := cscJSON(t, []any{int64(1), cscCandidateKey, cscCharacterID, cscField, cscAuthorRevision,
		summaryHash(cscAuthorLocation), cscDeclineID, FinalizedCharacterStateDecisionDecline})
	if receipt.PayloadHash != summaryHash(payload) || receipt.Decision != FinalizedCharacterStateDecisionDecline ||
		receipt.CandidateKey != cscCandidateKey || receipt.Source != source || receipt.Idempotent {
		t.Fatalf("receipt %+v", receipt)
	}

	// A decline applies nothing: the author's value, provenance and compatibility
	// prose are untouched, and the roster projection is not invalidated.
	readOnly := cscReadOnly(t, path)
	var location, storedProvenance, arch string
	if err := readOnly.QueryRow(`SELECT c.cs_location,c.cs_provenance,p.characters_arch FROM characters c, project_core p
 WHERE c.character_id=? AND p.id='main'`, cscCharacterID).Scan(&location, &storedProvenance, &arch); err != nil {
		t.Fatal(err)
	}
	if location != cscAuthorLocation || storedProvenance != provenance || arch != cscAuthorArch {
		t.Fatalf("decline changed the author field: %v / %v / %v", location, storedProvenance, arch)
	}
	var revision int64
	if err := readOnly.QueryRow("SELECT revision FROM character_roster_meta WHERE id='main'").Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != cscRosterRevision {
		t.Fatalf("decline invalidated the roster projection: %v", revision)
	}
	wantStore := cscStore(t, []any{}, []any{plotObject{
		{"candidateKey", cscCandidateKey}, {"operationId", cscDeclineID},
		{"payloadHash", summaryHash(payload)}, {"decision", FinalizedCharacterStateDecisionDecline},
		{"source", cscSourceOrder(source)}}})
	var stored string
	if err := readOnly.QueryRow("SELECT character_state_candidates FROM summary_snapshots WHERE draft_id=1").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != wantStore {
		t.Fatalf("store %v, want %v", stored, wantStore)
	}
	if remaining, err := characters.ListPendingStateCandidates(context.Background()); err != nil || len(remaining) != 0 {
		t.Fatalf("after decline %+v, %v", remaining, err)
	}
}

func TestFinalizedCharacterStateCandidatesDecideReplayIsIdempotent(t *testing.T) {
	_, characters, path := cscStoredCandidate(t)
	ctx := context.Background()
	request := cscRequest(FinalizedCharacterStateDecisionAccept, cscOperationID)
	if _, err := characters.DecideStateCandidate(ctx, request); err != nil {
		t.Fatalf("decide: %v", err)
	}

	// :534 the same operation and payload replays its receipt and writes nothing.
	before := cscDigest(t, path)
	replay, err := characters.DecideStateCandidate(ctx, request)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if after := cscDigest(t, path); after != before {
		t.Fatal("the replay wrote to the database")
	}
	if !replay.Idempotent || replay.PayloadHash != cscPayloadHash ||
		replay.Decision != FinalizedCharacterStateDecisionAccept || replay.OperationID != cscOperationID {
		t.Fatalf("replay %+v", replay)
	}
	// :533 the same operation id naming a different decision is a conflict.
	cscRefusal(t, path, ErrFinalizedCharacterStateOperationConflict, func() error {
		_, err := characters.DecideStateCandidate(ctx, cscRequest(FinalizedCharacterStateDecisionDecline, cscOperationID))
		return err
	})
	// :523 the request guard, proved byte-stable on the same database.
	cscRefusal(t, path, ErrFinalizedCharacterStateDecisionInvalid, func() error {
		invalid := request
		invalid.Decision = "maybe"
		_, err := characters.DecideStateCandidate(ctx, invalid)
		return err
	})
	cscRefusal(t, path, ErrFinalizedCharacterStateDecisionInvalid, func() error {
		invalid := request
		invalid.ExpectedFieldValueHash = "not a digest"
		_, err := characters.DecideStateCandidate(ctx, invalid)
		return err
	})
	cscRefusal(t, path, ErrFinalizedCharacterStateDecisionInvalid, func() error {
		invalid := request
		invalid.Field = "history"
		_, err := characters.DecideStateCandidate(ctx, invalid)
		return err
	})
}

func TestFinalizedCharacterStateCandidatesRefuseChangedSource(t *testing.T) {
	db, characters, path := cscStoredCandidate(t)
	// The immutable prose the candidate was staged against moved on.
	mustExec(t, db, "UPDATE finalization_outbox SET content_hash=? WHERE draft_id=1", summaryHash("另一份定稿"))
	ctx := context.Background()

	// The list swallows the per-draft refusal (:490-491) and returns no candidate.
	before := cscDigest(t, path)
	list, err := characters.ListPendingStateCandidates(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if after := cscDigest(t, path); after != before {
		t.Fatal("the list wrote to the database")
	}
	if len(list) != 0 {
		t.Fatalf("stale list %+v", list)
	}
	cscRefusal(t, path, ErrFinalizedCharacterStateSourceChanged, func() error {
		_, err := characters.ReadPendingStateCandidate(ctx, 1, cscCandidateKey)
		return err
	})
	cscRefusal(t, path, ErrFinalizedCharacterStateSourceChanged, func() error {
		_, err := characters.DecideStateCandidate(ctx, cscRequest(FinalizedCharacterStateDecisionAccept, cscOperationID))
		return err
	})
}
