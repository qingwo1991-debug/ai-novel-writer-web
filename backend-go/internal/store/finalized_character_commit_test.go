package store

// These tests provision a real SQLite database from the pinned asset in
// internal/admission (WriteBaselineV7, baseline_v7_ddl.go:744) and drive
// CommitFinalizedCharacterStates, the port of
// electron/repositories/summary-repository.ts:423-475, against the real schema:
// drafts, contents, finalization_outbox, continuity_projection_meta,
// character_identity_meta, character_identity_proposals, characters,
// character_roster_meta, project_core and the candidate store in
// summary_snapshots.character_state_candidates.
//
// The staged candidate key is a golden value derived once from the reference's
// own definition (summary-repository.ts:272-274:
// "fcs-state:" + sha256(JSON.stringify([draftId, finalizationId, contentHash,
// characterId, field, sha256(value)]))), so the test fails if the derivation
// drifts by one byte: the row is staged, written and then found again by that
// exact key. The staged store is asserted byte for byte in the one canonical
// order this package writes (character_state_candidates.go:1121-1156). The
// reference emits whatever key order the staging literal had (:697 serializes the
// merged objects as they are), so its freshly staged row and the same row read
// back and re-saved are not byte-identical; this port writes every row through the
// canonical writer, which makes them identical and lets one order be asserted.
//
// Every refusal is proved twice over: by its exact reference code, and by the
// file's own bytes, which the refused commit must leave untouched. Each refusal
// runs on its own fresh database, so no earlier state can stand in for a later
// one. Every accepted effect is read a second time through a separate mode=ro
// connection, which proves the write is durable on disk and excludes the
// possibility that the writer only saw its own uncommitted view.
//
// The store cannot observe a lease: :426 refuses a missing database handle, and
// the live-lease proof belongs to the adapter that borrows the project database
// (cmd/novel-server/production_finalized_character_linux.go). The last test
// therefore proves that arm with an absent handle and with a closed pool, each
// byte-stable.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/characterstate"
)

const (
	fcmProjectID        = "commit-scope"
	fcmEpoch            = "epoch-commit"
	fcmBody             = "山雨欲来，风满楼。"
	fcmFinalizationID   = "fcm-final-1"
	fcmCharacterID      = "c-青"
	fcmCharacterName    = "阿青"
	fcmFrozenName       = "阿青（冻结）"
	fcmField            = "location"
	fcmPowerField       = "powerLevel"
	fcmAuthorLocation   = "作者地点"
	fcmAuthorRevision   = int64(3)
	fcmLiveLocation     = "洞府"
	fcmAppliedValue     = "秘境入口"
	fcmStagedValue      = "旧塔废墟"
	fcmEvidenceText     = "风满楼"
	fcmEvidenceStart    = int64(5)
	fcmEvidenceEnd      = int64(8)
	fcmDraftID          = int64(1)
	fcmChapter          = int64(1)
	fcmGeneration       = int64(2)
	fcmIdentityRevision = int64(3)
	fcmRosterRevision   = int64(7)
	fcmAuthorArch       = "作者尚未刷新静态Markdown"
)

// fcmPreviousEpoch is the generation the seeded provenance was derived under;
// fcmCurrentEpoch is the live watermark, one generation later, so a derived field
// may advance.
const (
	fcmPreviousEpoch = "commit-scope:1"
	fcmCurrentEpoch  = "commit-scope:2"
)

// fcmStagedKey is the golden candidate key of fcmStagedValue for this source.
const fcmStagedKey = "fcs-state:0a0bae74c49ea023b25d8ffcf9db5cae7cfbc258739992be69291cdff8128c66"

// fcmScope is the project epoch the caller holds.
func fcmScope() FinalizedCharacterScope {
	return FinalizedCharacterScope{ProjectID: fcmProjectID, Epoch: fcmEpoch}
}

// fcmFixture provisions the pinned v7 schema in a fresh file and seeds every
// singleton a commit reads or rewrites: the identity revision the freeze receipt
// records, the projection watermark the source read joins, the roster meta row the
// projection refresh bumps, and the author's compatibility prose, which no state
// write may touch.
func fcmFixture(t *testing.T) (*sql.DB, *FinalizedCharacters, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "finalized character commit #? 中文.db")
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
	// (baseline_v7_ddl.go:123-125); the freeze receipt below records this revision.
	mustExec(t, db, "INSERT INTO character_identity_meta(id,revision,legacy_count) VALUES('main',?,0)", fcmIdentityRevision)
	// The baseline creates continuity_projection_meta empty, while the source read
	// INNER JOINs it (finalized_character_context.go:307-313). The watermark is
	// seeded explicitly and read back, because every snapshot row below and every
	// source order the commit derives depends on this one number.
	mustExec(t, db, `INSERT INTO continuity_projection_meta(id,generation) VALUES('main',?)
 ON CONFLICT(id) DO UPDATE SET generation=excluded.generation`, fcmGeneration)
	if got := fcmInteger(t, db, "SELECT generation FROM continuity_projection_meta WHERE id='main'"); got != fcmGeneration {
		t.Fatalf("projection generation %d, want %d", got, fcmGeneration)
	}
	mustExec(t, db, `INSERT INTO character_roster_meta(id,schema_version,revision,migration_state,legacy_markdown,projection_hash,fact_hash)
 VALUES('main',1,?,'ready','','','')`, fcmRosterRevision)
	mustExec(t, db, "INSERT INTO project_core(id,project_name,characters_arch) VALUES('main','合成项目',?)", fcmAuthorArch)
	return db, NewFinalizedCharacters(db), path
}

// fcmReadOnly opens the same file a second time, read-only, so a committed write
// is proved by a connection that could not have performed it.
func fcmReadOnly(t *testing.T, path string) *sql.DB {
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

// fcmDigest is the whole file's bytes: a call that wrote anything at all fails
// even if it committed no logical row.
func fcmDigest(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// fcmRefusal runs one refused commit and proves both the exact reference code and
// that the database is byte-identical afterwards.
func fcmRefusal(t *testing.T, path string, want error, call func() error) {
	t.Helper()
	before := fcmDigest(t, path)
	err := call()
	if err == nil {
		t.Fatalf("expected refusal %v, got nil", want)
	}
	if !errors.Is(err, want) || err.Error() != want.Error() {
		t.Fatalf("expected refusal %v, got %v", want, err)
	}
	if after := fcmDigest(t, path); after != before {
		t.Fatalf("%v changed the database", want)
	}
}

// fcmText reads one text column of one row.
func fcmText(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var value string
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("read text: %v", err)
	}
	return value
}

// fcmInteger reads one integer column of one row.
func fcmInteger(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var value int64
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("read integer: %v", err)
	}
	return value
}

// fcmJSON encodes a fixture value exactly the way the reference's JSON.stringify
// would, so a seeded provenance or an expected write-back is compared byte for
// byte.
func fcmJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := plotJSON(value)
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	return raw
}

// fcmSourceOrder is the reference's FinalizedSourceIdentity key order
// (src/shared/finalized-continuity.ts:12-17).
func fcmSourceOrder(source FinalizedCharacterSourceIdentity) plotObject {
	return plotObject{{"draftId", source.DraftID}, {"finalizationId", source.FinalizationID},
		{"chapterNumber", source.ChapterNumber}, {"contentHash", source.ContentHash}}
}

// fcmOrder is the derived source order of a verified context
// (src/shared/character-identity.ts:33-35).
func fcmOrder(epoch string, chapter, revision int64) plotObject {
	return plotObject{{"continuityEpoch", epoch}, {"chapterNumber", chapter},
		{"authoritativeFinalizationRevision", revision}}
}

// fcmPreviousOrder is the order the seeded provenance was derived under: the
// generation before the live watermark, at the same chapter and the same
// authoritative finalization revision the live order carries.
func fcmPreviousOrder() plotObject {
	return fcmOrder(fcmPreviousEpoch, fcmChapter, 1)
}

// fcmLiveOrder is the order the context read derives from the live watermark
// (finalized_character_commit.go:891-895).
func fcmLiveOrder() plotObject {
	return fcmOrder(fcmCurrentEpoch, fcmChapter, 1)
}

// fcmDraft seeds one finalized draft with its frozen prose, the immutable
// finalization receipt beside it and the identity freeze receipt that names
// fcmFrozenName, which is the name a staged candidate must carry.
func fcmDraft(t *testing.T, db *sql.DB) FinalizedCharacterSourceIdentity {
	t.Helper()
	source := FinalizedCharacterSourceIdentity{DraftID: fcmDraftID, ChapterNumber: fcmChapter,
		FinalizationID: fcmFinalizationID, ContentHash: summaryHash(fcmBody)}
	mustExec(t, db, "INSERT INTO contents(id,body) VALUES(?,?)", fcmDraftID, fcmBody)
	mustExec(t, db, `INSERT INTO drafts(id,chapter_number,version,status,content_id,word_count)
 VALUES(?,?,1,'finalized',?,?)`, fcmDraftID, fcmChapter, fcmDraftID, len(fcmBody))
	mustExec(t, db, `INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,chapter_title,
 content_hash,content_revision,content_snapshot,target_file_name) VALUES(?,?,?,?,?,1,?,'fcm.txt')`,
		fcmFinalizationID, fcmDraftID, fcmChapter, "合成章节", source.ContentHash, fcmBody)
	raw := fcmFreezeRaw(t, source, []any{})
	mustExec(t, db, "INSERT OR REPLACE INTO character_identity_proposals(proposal_id,source_key,source_hash,raw_value,candidate_ids_json)"+
		" VALUES(?,'"+summarySnapshotKind+"',?,?,'[]')", "fcs:"+fcmFinalizationID, summaryHash(raw), raw)
	return source
}

// fcmFreezeRaw is the freeze receipt body of this fixture's one character.
func fcmFreezeRaw(t *testing.T, source FinalizedCharacterSourceIdentity, aliases []any) string {
	t.Helper()
	return fcmJSON(t, plotObject{{"kind", summarySnapshotKind}, {"source", fcmSourceOrder(source)},
		{"identityRevision", fcmIdentityRevision},
		{"characters", []any{plotObject{{"characterId", fcmCharacterID},
			{"displayNameSnapshot", fcmFrozenName}, {"aliases", aliases}}}}})
}

// fcmProvenance seeds a derived location at revision revision under order, plus an
// unrelated derived field whose position and bytes every write must preserve.
func fcmProvenance(t *testing.T, source FinalizedCharacterSourceIdentity, revision int64, order plotObject) string {
	t.Helper()
	return fcmJSON(t, plotObject{
		{"location", plotObject{{"kind", "derived"}, {"source", fcmSourceOrder(source)},
			{"revision", revision}, {"sourceOrder", order}}},
		{fcmPowerField, plotObject{{"kind", "derived"}, {"revision", int64(1)}}},
	})
}

// fcmAuthorProvenance seeds the author's own location at revision, which no
// commit may overwrite: a model answer for an author field is a proposal.
func fcmAuthorProvenance(t *testing.T, revision int64) string {
	t.Helper()
	return fcmJSON(t, plotObject{
		{"location", plotObject{{"kind", "author"}, {"chapterNumber", fcmChapter}, {"revision", revision}}},
		{fcmPowerField, plotObject{{"kind", "derived"}, {"revision", int64(1)}}},
	})
}

// fcmCharacter seeds the live character row the commit reads and, for an applied
// field, writes. Its static name is deliberately not the frozen display name.
func fcmCharacter(t *testing.T, db *sql.DB, location, provenance string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO characters(character_id,name,retired,cs_updated_at_chapter,cs_location,cs_power_level,cs_provenance)
 VALUES(?,?,0,1,?,?,?)`, fcmCharacterID, fcmCharacterName, location, "练气", provenance)
}

// fcmSnapshot seeds the summary_snapshots row a draft's candidates live in
// (baseline_v7_ddl.go:630-643) at the fixture's watermark.
func fcmSnapshot(t *testing.T, db *sql.DB, source FinalizedCharacterSourceIdentity, store string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO summary_snapshots(draft_id,chapter_number,chapter_notes,character_state_candidates,
 source_finalization_id,source_content_hash,projection_generation) VALUES(?,?,'',?,?,?,?)`,
		source.DraftID, source.ChapterNumber, store, source.FinalizationID, source.ContentHash, fcmGeneration)
}

// fcmStore is the version-2 candidate store of one snapshot row (:177).
func fcmStore(t *testing.T, pending, receipts []any) string {
	t.Helper()
	if pending == nil {
		pending = []any{}
	}
	if receipts == nil {
		receipts = []any{}
	}
	return fcmJSON(t, plotObject{{"version", int64(2)}, {"pending", pending}, {"receipts", receipts}})
}

// fcmAnswer is a model answer that proposes one state field and locates it by its
// unique verbatim text (finalized-continuity.ts:212 computes the offsets).
func fcmAnswer(t *testing.T, field, value string) string {
	t.Helper()
	return fcmJSON(t, plotObject{{"updates", []any{plotObject{
		{"characterId", fcmCharacterID},
		{"currentState", plotObject{{field, value}}},
		{"evidence", plotObject{{"text", fcmEvidenceText}}},
	}}}})
}

// fcmResponse parses an answer against the caller's context, the way the adapter
// hands a response to the commit.
func fcmResponse(t *testing.T, frozen FinalizedCharacterContext, answer string) characterstate.Response {
	t.Helper()
	parsed, err := characterstate.ParseResponse(answer, finalizedCharacterParseContext(frozen))
	if err != nil {
		t.Fatalf("parse model answer: %v", err)
	}
	return parsed
}

// fcmFrozen reads the context the caller would hold, through the published read.
func fcmFrozen(t *testing.T, ctx context.Context, characters *FinalizedCharacters) *FinalizedCharacterContext {
	t.Helper()
	frozen, err := characters.ReadContext(ctx, fcmDraftID, fcmScope())
	if err != nil {
		t.Fatalf("read context: %v", err)
	}
	return frozen
}

// fcmStagedStore is the exact candidate store the commit must leave behind for one
// staged proposal of fcmStagedValue against an author-protected location.
func fcmStagedStore(t *testing.T, source FinalizedCharacterSourceIdentity) string {
	t.Helper()
	evidence := plotObject{{"start", fcmEvidenceStart}, {"end", fcmEvidenceEnd}, {"text", fcmEvidenceText}}
	return fcmStore(t, []any{plotObject{
		{"characterId", fcmCharacterID},
		{"characterName", fcmFrozenName},
		{"field", fcmField},
		{"value", fcmStagedValue},
		{"evidence", evidence},
		{"selectionKey", "state:" + fcmCharacterID + ":" + fcmField},
		{"displayName", fcmFrozenName},
		{"rawValue", plotObject{
			{"characterId", fcmCharacterID},
			{"currentState", plotObject{{fcmField, fcmStagedValue}}},
			{"evidence", evidence},
		}},
		{"reason", "author-protected"},
		{"candidateKey", fcmStagedKey},
		{"source", fcmSourceOrder(source)},
		{"expectedFieldRevision", fcmAuthorRevision},
		{"expectedFieldValueHash", summaryHash(fcmAuthorLocation)},
	}}, nil)
}

func TestCommitFinalizedCharacterStatesAppliesOneField(t *testing.T) {
	db, characters, path := fcmFixture(t)
	source := fcmDraft(t, db)
	fcmCharacter(t, db, fcmLiveLocation, fcmProvenance(t, source, 0, fcmPreviousOrder()))
	fcmSnapshot(t, db, source, fcmStore(t, nil, nil))
	ctx := context.Background()
	frozen := fcmFrozen(t, ctx, characters)

	receipt, err := characters.CommitFinalizedCharacterStates(ctx, *frozen,
		fcmResponse(t, *frozen, fcmAnswer(t, fcmField, fcmAppliedValue)), "")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	// :466 and :437: one field applied, nothing held back, no proposal.
	if receipt.Applied != 1 || receipt.Unchanged != 0 || len(receipt.Candidates) != 0 {
		t.Fatalf("receipt applied=%d unchanged=%d candidates=%d, want applied 1 with no proposal",
			receipt.Applied, receipt.Unchanged, len(receipt.Candidates))
	}
	// The effect, proved by a connection that could not have written it.
	readOnly := fcmReadOnly(t, path)
	if got := fcmText(t, readOnly, "SELECT cs_location FROM characters WHERE character_id=?", fcmCharacterID); got != fcmAppliedValue {
		t.Fatalf("cs_location %s, want %s", got, fcmAppliedValue)
	}
	// :465 the field's provenance is rewritten to revision+1 under the *live*
	// order, and the untouched field keeps its own bytes and position.
	want := fcmJSON(t, plotObject{
		{"location", plotObject{{"kind", "derived"}, {"source", fcmSourceOrder(source)},
			{"revision", int64(1)}, {"sourceOrder", fcmLiveOrder()}}},
		{fcmPowerField, plotObject{{"kind", "derived"}, {"revision", int64(1)}}},
	})
	if got := fcmText(t, readOnly, "SELECT cs_provenance FROM characters WHERE character_id=?", fcmCharacterID); got != want {
		t.Fatalf("cs_provenance %s, want %s", got, want)
	}
	// :469 MAX(COALESCE(cs_updated_at_chapter,0), chapterNumber)
	if got := fcmInteger(t, readOnly, "SELECT cs_updated_at_chapter FROM characters WHERE character_id=?", fcmCharacterID); got != fcmChapter {
		t.Fatalf("cs_updated_at_chapter %d, want %d", got, fcmChapter)
	}
	// :473 the projection refresh: one revision forward and a new valid fact hash.
	if got := fcmInteger(t, readOnly, "SELECT revision FROM character_roster_meta WHERE id='main'"); got != fcmRosterRevision+1 {
		t.Fatalf("roster revision %d, want %d", got, fcmRosterRevision+1)
	}
	if got := fcmText(t, readOnly, "SELECT fact_hash FROM character_roster_meta WHERE id='main'"); !summaryHashValid(got) {
		t.Fatal("roster fact hash is not a digest")
	}
	// The refresh must never rewrite the author's compatibility prose.
	if got := fcmText(t, readOnly, "SELECT characters_arch FROM project_core WHERE id='main'"); got != fcmAuthorArch {
		t.Fatalf("characters_arch %s, want %s", got, fcmAuthorArch)
	}
	// No candidate was staged, so the store is untouched.
	if got := fcmText(t, readOnly, "SELECT character_state_candidates FROM summary_snapshots WHERE draft_id=?", fcmDraftID); got != fcmStore(t, nil, nil) {
		t.Fatalf("candidate store %s, want an empty store", got)
	}
}

func TestCommitFinalizedCharacterStatesStagesAuthorProtectedCandidate(t *testing.T) {
	db, characters, path := fcmFixture(t)
	source := fcmDraft(t, db)
	fcmCharacter(t, db, fcmAuthorLocation, fcmAuthorProvenance(t, fcmAuthorRevision))
	fcmSnapshot(t, db, source, fcmStore(t, nil, nil))
	ctx := context.Background()
	frozen := fcmFrozen(t, ctx, characters)

	receipt, err := characters.CommitFinalizedCharacterStates(ctx, *frozen,
		fcmResponse(t, *frozen, fcmAnswer(t, fcmField, fcmStagedValue)), "")
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	// :457-461 the author's field is never overwritten: the answer becomes one
	// candidate and nothing is applied.
	if receipt.Applied != 0 || receipt.Unchanged != 0 || len(receipt.Candidates) != 1 {
		t.Fatalf("receipt applied=%d unchanged=%d candidates=%d, want unchanged 0 with one proposal",
			receipt.Applied, receipt.Unchanged, len(receipt.Candidates))
	}
	staged := receipt.Candidates[0]
	if staged.CandidateKey != fcmStagedKey {
		t.Fatalf("candidate key %s, want %s", staged.CandidateKey, fcmStagedKey)
	}
	if staged.CharacterID != fcmCharacterID || staged.CharacterName != fcmFrozenName ||
		staged.Field != fcmField || staged.Value != fcmStagedValue {
		t.Fatal("candidate identity is not the frozen character's field")
	}
	if staged.ExpectedFieldRevision != fcmAuthorRevision ||
		staged.ExpectedFieldValueHash != summaryHash(fcmAuthorLocation) {
		t.Fatal("candidate expectation is not the live author field")
	}
	if staged.Reason == nil || *staged.Reason != "author-protected" {
		t.Fatal("candidate reason is not author-protected")
	}
	// The staged row, byte for byte, and the character it belongs to untouched.
	readOnly := fcmReadOnly(t, path)
	if got := fcmText(t, readOnly, "SELECT character_state_candidates FROM summary_snapshots WHERE draft_id=?", fcmDraftID); got != fcmStagedStore(t, source) {
		t.Fatalf("candidate store %s, want %s", got, fcmStagedStore(t, source))
	}
	if got := fcmText(t, readOnly, "SELECT cs_location FROM characters WHERE character_id=?", fcmCharacterID); got != fcmAuthorLocation {
		t.Fatalf("cs_location %s, want the author value %s", got, fcmAuthorLocation)
	}
	if got := fcmText(t, readOnly, "SELECT cs_provenance FROM characters WHERE character_id=?", fcmCharacterID); got != fcmAuthorProvenance(t, fcmAuthorRevision) {
		t.Fatalf("cs_provenance %s, want the seeded author provenance", got)
	}
	if got := fcmInteger(t, readOnly, "SELECT cs_updated_at_chapter FROM characters WHERE character_id=?", fcmCharacterID); got != fcmChapter {
		t.Fatalf("cs_updated_at_chapter %d, want the untouched %d", got, fcmChapter)
	}
	// :473 runs only for an applied field, so the roster is not invalidated.
	if got := fcmInteger(t, readOnly, "SELECT revision FROM character_roster_meta WHERE id='main'"); got != fcmRosterRevision {
		t.Fatalf("roster revision %d, want the untouched %d", got, fcmRosterRevision)
	}
	// The row a later decision addresses is found by the key this commit derived.
	candidate, err := characters.ReadPendingStateCandidate(ctx, fcmDraftID, fcmStagedKey)
	if err != nil {
		t.Fatalf("read staged candidate: %v", err)
	}
	if candidate.CandidateKey != fcmStagedKey || candidate.Field != fcmField || candidate.Value != fcmStagedValue {
		t.Fatal("staged candidate is not addressable by its key")
	}
	if candidate.ExpectedFieldRevision != fcmAuthorRevision ||
		candidate.ExpectedFieldValueHash != summaryHash(fcmAuthorLocation) {
		t.Fatal("staged candidate lost the field it expects")
	}
}

func TestCommitFinalizedCharacterStatesTreatsRepeatAsUnchanged(t *testing.T) {
	t.Run("AppliedValue", func(t *testing.T) {
		db, characters, path := fcmFixture(t)
		source := fcmDraft(t, db)
		// A derived field that already holds exactly what the model proposes, from
		// this very source, at the revision the caller read.
		fcmCharacter(t, db, fcmAppliedValue, fcmProvenance(t, source, 1, fcmPreviousOrder()))
		fcmSnapshot(t, db, source, fcmStore(t, nil, nil))
		ctx := context.Background()
		frozen := fcmFrozen(t, ctx, characters)
		answer := fcmResponse(t, *frozen, fcmAnswer(t, fcmField, fcmAppliedValue))

		before := fcmDigest(t, path)
		receipt, err := characters.CommitFinalizedCharacterStates(ctx, *frozen, answer, "")
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
		// :454 the repeated answer is not a write.
		if receipt.Applied != 0 || receipt.Unchanged != 1 || len(receipt.Candidates) != 0 {
			t.Fatalf("receipt applied=%d unchanged=%d candidates=%d, want unchanged 1 and no write",
				receipt.Applied, receipt.Unchanged, len(receipt.Candidates))
		}
		if after := fcmDigest(t, path); after != before {
			t.Fatal("a repeated value wrote to the database")
		}
		if got := fcmInteger(t, fcmReadOnly(t, path), "SELECT revision FROM character_roster_meta WHERE id='main'"); got != fcmRosterRevision {
			t.Fatalf("roster revision %d, want the untouched %d", got, fcmRosterRevision)
		}
	})

	t.Run("AuthorEcho", func(t *testing.T) {
		db, characters, path := fcmFixture(t)
		source := fcmDraft(t, db)
		fcmCharacter(t, db, fcmAuthorLocation, fcmAuthorProvenance(t, fcmAuthorRevision))
		fcmSnapshot(t, db, source, fcmStore(t, nil, nil))
		ctx := context.Background()
		frozen := fcmFrozen(t, ctx, characters)
		// The model says what the author already wrote, so there is no proposal.
		answer := fcmResponse(t, *frozen, fcmAnswer(t, fcmField, fcmAuthorLocation))

		before := fcmDigest(t, path)
		receipt, err := characters.CommitFinalizedCharacterStates(ctx, *frozen, answer, "")
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
		// :456 `if (value === actual.value) { receipt.unchanged++; continue }`
		if receipt.Applied != 0 || receipt.Unchanged != 1 || len(receipt.Candidates) != 0 {
			t.Fatalf("receipt applied=%d unchanged=%d candidates=%d, want unchanged 1 and no proposal",
				receipt.Applied, receipt.Unchanged, len(receipt.Candidates))
		}
		if after := fcmDigest(t, path); after != before {
			t.Fatal("an echoed author value wrote to the database")
		}
	})
}

func TestCommitFinalizedCharacterStatesRefusesChangedContext(t *testing.T) {
	t.Run("FreezeReceipt", func(t *testing.T) {
		db, characters, path := fcmFixture(t)
		source := fcmDraft(t, db)
		fcmCharacter(t, db, fcmAuthorLocation, fcmAuthorProvenance(t, fcmAuthorRevision))
		fcmSnapshot(t, db, source, fcmStore(t, nil, nil))
		ctx := context.Background()
		frozen := fcmFrozen(t, ctx, characters)
		answer := fcmResponse(t, *frozen, fcmAnswer(t, fcmField, fcmStagedValue))
		// The freeze receipt now names an alias the caller's context never saw.
		raw := fcmFreezeRaw(t, source, []any{"别名甲"})
		mustExec(t, db, "UPDATE character_identity_proposals SET raw_value=?,source_hash=? WHERE proposal_id=?",
			raw, summaryHash(raw), "fcs:"+fcmFinalizationID)
		// :430-434 the identity projection differs, so nothing may be written.
		fcmRefusal(t, path, ErrFinalizedCharacterContextChanged, func() error {
			_, err := characters.CommitFinalizedCharacterStates(ctx, *frozen, answer, "")
			return err
		})
	})

	t.Run("FieldScope", func(t *testing.T) {
		db, characters, path := fcmFixture(t)
		source := fcmDraft(t, db)
		fcmCharacter(t, db, fcmAuthorLocation, fcmAuthorProvenance(t, fcmAuthorRevision))
		fcmSnapshot(t, db, source, fcmStore(t, nil, nil))
		ctx := context.Background()
		frozen := fcmFrozen(t, ctx, characters)
		answer := fcmResponse(t, *frozen, fcmAnswer(t, fcmField, fcmStagedValue))
		// :433 the caller's own snapshot carries a foreign project, which the
		// commit refuses before it touches a row.
		frozen.Characters[0].Fields[0].ProjectID = "另一个项目"
		fcmRefusal(t, path, ErrFinalizedCharacterContextChanged, func() error {
			_, err := characters.CommitFinalizedCharacterStates(ctx, *frozen, answer, "")
			return err
		})
	})
}

func TestCommitFinalizedCharacterStatesRefusesChangedFrozenSource(t *testing.T) {
	db, characters, path := fcmFixture(t)
	source := fcmDraft(t, db)
	fcmCharacter(t, db, fcmAuthorLocation, fcmAuthorProvenance(t, fcmAuthorRevision))
	fcmSnapshot(t, db, source, fcmStore(t, nil, nil))
	ctx := context.Background()
	frozen := fcmFrozen(t, ctx, characters)
	answer := fcmResponse(t, *frozen, fcmAnswer(t, fcmField, fcmStagedValue))
	// The finalization receipt is no longer the digest of the frozen prose, so the
	// source read answers null.
	mustExec(t, db, "UPDATE finalization_outbox SET content_hash=? WHERE draft_id=?",
		summaryHash("另一份定稿"), fcmDraftID)
	// :406 the :304-309 source guards, shared by every reader of this source.
	fcmRefusal(t, path, ErrFinalizedCharacterSourceChanged, func() error {
		_, err := characters.CommitFinalizedCharacterStates(ctx, *frozen, answer, "")
		return err
	})
}

func TestCommitFinalizedCharacterStatesRefusesChangedSnapshotHash(t *testing.T) {
	db, characters, path := fcmFixture(t)
	source := fcmDraft(t, db)
	fcmCharacter(t, db, fcmAuthorLocation, fcmAuthorProvenance(t, fcmAuthorRevision))
	fcmSnapshot(t, db, source, fcmStore(t, nil, nil))
	ctx := context.Background()
	frozen := fcmFrozen(t, ctx, characters)
	answer := fcmResponse(t, *frozen, fcmAnswer(t, fcmField, fcmStagedValue))
	// The freeze receipt no longer hashes to the digest that addresses it.
	mustExec(t, db, "UPDATE character_identity_proposals SET source_hash=? WHERE proposal_id=?",
		summaryHash("不是这份快照"), "fcs:"+fcmFinalizationID)
	// :70-75 through the frozen read at :410.
	fcmRefusal(t, path, ErrSummaryCharacterSnapshotInvalid, func() error {
		_, err := characters.CommitFinalizedCharacterStates(ctx, *frozen, answer, "")
		return err
	})
}

func TestCommitFinalizedCharacterStatesRefusesStaleSourceOrder(t *testing.T) {
	db, characters, path := fcmFixture(t)
	source := fcmDraft(t, db)
	// A derived field whose stored order is *ahead* of this commit's order: the
	// same generation, but an authoritative finalization revision the live order
	// does not carry.
	fcmCharacter(t, db, fcmLiveLocation, fcmProvenance(t, source, 1, fcmOrder(fcmCurrentEpoch, fcmChapter, 5)))
	fcmSnapshot(t, db, source, fcmStore(t, nil, nil))
	ctx := context.Background()
	frozen := fcmFrozen(t, ctx, characters)
	answer := fcmResponse(t, *frozen, fcmAnswer(t, fcmField, fcmAppliedValue))
	// character-identity.ts:84-87, the same-generation clause.
	fcmRefusal(t, path, ErrFinalizedCharacterSourceConflict, func() error {
		_, err := characters.CommitFinalizedCharacterStates(ctx, *frozen, answer, "")
		return err
	})
}

func TestCommitFinalizedCharacterStatesRefusesMovedField(t *testing.T) {
	db, characters, path := fcmFixture(t)
	source := fcmDraft(t, db)
	fcmCharacter(t, db, fcmAuthorLocation, fcmAuthorProvenance(t, fcmAuthorRevision))
	fcmSnapshot(t, db, source, fcmStore(t, nil, nil))
	ctx := context.Background()
	frozen := fcmFrozen(t, ctx, characters)
	answer := fcmResponse(t, *frozen, fcmAnswer(t, fcmField, fcmStagedValue))
	// The author's field moves on after the caller read it, so the answer was
	// decided against a revision that is no longer live.
	mustExec(t, db, "UPDATE characters SET cs_provenance=? WHERE character_id=?",
		fcmAuthorProvenance(t, fcmAuthorRevision+1), fcmCharacterID)
	// character-identity.ts:90-91, the base-revision clause.
	fcmRefusal(t, path, ErrFinalizedCharacterFieldConflict, func() error {
		_, err := characters.CommitFinalizedCharacterStates(ctx, *frozen, answer, "")
		return err
	})
}

func TestCommitFinalizedCharacterStatesRefusesWithoutLiveProject(t *testing.T) {
	db, characters, path := fcmFixture(t)
	source := fcmDraft(t, db)
	fcmCharacter(t, db, fcmLiveLocation, fcmProvenance(t, source, 0, fcmPreviousOrder()))
	fcmSnapshot(t, db, source, fcmStore(t, nil, nil))
	ctx := context.Background()
	frozen := fcmFrozen(t, ctx, characters)
	answer := fcmResponse(t, *frozen, fcmAnswer(t, fcmField, fcmAppliedValue))

	// :426 `if (!database) throw new Error('PROJECT_NOT_OPEN')`, with the handle the
	// adapter would have borrowed absent.
	before := fcmDigest(t, path)
	_, err := NewFinalizedCharacters(nil).CommitFinalizedCharacterStates(ctx, *frozen, answer, "")
	if err == nil || !errors.Is(err, ErrProjectNotOpen) || err.Error() != ErrProjectNotOpen.Error() {
		t.Fatalf("expected refusal %v, got %v", ErrProjectNotOpen, err)
	}
	if after := fcmDigest(t, path); after != before {
		t.Fatalf("%v changed the database", ErrProjectNotOpen)
	}

	// A handle whose pool is closed proves the same arm: no lease, no write.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before = fcmDigest(t, path)
	if _, err := characters.CommitFinalizedCharacterStates(ctx, *frozen, answer, ""); err == nil {
		t.Fatal("expected a refusal without a live project handle")
	}
	if after := fcmDigest(t, path); after != before {
		t.Fatal("a closed project handle changed the database")
	}
}
