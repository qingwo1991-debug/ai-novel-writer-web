package store

// These tests provision a real SQLite database from the pinned asset in
// internal/admission (WriteBaselineV7, baseline_v7_ddl.go:744) and drive the
// finalized-character read path of finalized_character_context.go against the
// real schema: drafts, contents, finalization_outbox,
// continuity_projection_meta, character_identity_proposals,
// character_identity_meta and characters.
//
// Every refusal is proved twice over: by its exact reference code, and by the
// file's own bytes, which a read - refused or not - must leave untouched. Each
// refusal runs on its own fresh database, so no earlier state can stand in for a
// later one. The happy path is read a second time through a separate mode=ro
// connection, which both proves the result is durable on disk and excludes the
// possibility that the read wrote something the first pass then saw.

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
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
)

const (
	fccProjectID      = "project-finalized-characters"
	fccEpoch          = "epoch-lease-1"
	fccFinalizationID = "fcc-final-1"
	fccNewerFinalID   = "fcc-final-2"
	fccBody           = "山雨欲来，风满楼。"
	fccCharacterID    = "c-青"
	fccCharacterName  = "阿青"
	fccFrozenName     = "阿青（冻结）"
	fccLocation       = "山门"
	fccPowerLevel     = "筑基"
)

func fccScope() FinalizedCharacterScope {
	return FinalizedCharacterScope{ProjectID: fccProjectID, Epoch: fccEpoch}
}

// fccFixture provisions the pinned v7 schema in a fresh file, then opens it the
// way a project database is opened (ExistingDSN: mode=rw, foreign keys on).
func fccFixture(t *testing.T) (*sql.DB, *FinalizedCharacters, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "finalized character #? 中文.db")
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
	// The baseline creates continuity_projection_meta empty
	// (baseline_v7_ddl.go:200-204) while the source read INNER JOINs it
	// (summary-repository.ts:288), so this row must be inserted: an UPDATE matches
	// nothing on a fresh database, the join then yields no row, and every read
	// reports FINALIZED_CHARACTER_SOURCE_CHANGED from the :304 !row arm.
	mustExec(t, db, "INSERT INTO continuity_projection_meta(id,generation) VALUES('main',2)")
	return db, NewFinalizedCharacters(db), path
}

// fccReadOnly opens the same file a second time, read-only, so the happy path is
// read by a connection that could not have written what it sees.
func fccReadOnly(t *testing.T, path string) *sql.DB {
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

// fccDigest is the whole file's bytes: a read that wrote anything at all fails
// even if it committed no logical row.
func fccDigest(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// fccRefusal runs one refused read and proves both the exact reference code and
// that the database is byte-identical afterwards.
func fccRefusal(t *testing.T, path string, want error, read func() (*FinalizedCharacterContext, error)) {
	t.Helper()
	before := fccDigest(t, path)
	result, err := read()
	if err == nil {
		t.Fatalf("expected refusal %v, got %+v", want, result)
	}
	if !errors.Is(err, want) || err.Error() != want.Error() {
		t.Fatalf("expected refusal %v, got %v", want, err)
	}
	if after := fccDigest(t, path); after != before {
		t.Fatalf("%v changed the database", want)
	}
}

// fccDraft seeds one draft and, when finalizationID is not empty, the immutable
// receipt a finalization commit would have written beside it.
func fccDraft(t *testing.T, db *sql.DB, id, chapter, version int64, status, body, finalizationID string) FinalizedCharacterSourceIdentity {
	t.Helper()
	mustExec(t, db, "INSERT INTO contents(id,body) VALUES(?,?)", id, body)
	mustExec(t, db, `INSERT INTO drafts(id,chapter_number,version,status,content_id,word_count)
 VALUES(?,?,?,?,?,?)`, id, chapter, version, status, id, len(body))
	source := FinalizedCharacterSourceIdentity{
		DraftID:        id,
		ChapterNumber:  chapter,
		FinalizationID: finalizationID,
		ContentHash:    summaryHash(body),
	}
	if finalizationID == "" {
		return source
	}
	fccReceipt(t, db, id, chapter, finalizationID, source.ContentHash, body)
	return source
}

// fccReceipt writes the finalization_outbox row beside a draft with every column
// a source guard reads left to the caller, so a case can break exactly one.
func fccReceipt(t *testing.T, db *sql.DB, draftID, chapter int64, finalizationID, contentHash, contentSnapshot string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,chapter_title,
 content_hash,content_revision,content_snapshot,target_file_name) VALUES(?,?,?,?,?,1,?,'fcc.txt')`,
		finalizationID, draftID, chapter, fccFrozenName, contentHash, contentSnapshot)
}

// fccFreeze writes the freeze receipt in the exact key order and shape the sole
// writer uses (finalization_outbox.go:333-341).
func fccFreeze(t *testing.T, db *sql.DB, source FinalizedCharacterSourceIdentity, revision int64, characters []any) {
	t.Helper()
	orderedSource := plotObject{{"draftId", source.DraftID}, {"finalizationId", source.FinalizationID},
		{"chapterNumber", source.ChapterNumber}, {"contentHash", source.ContentHash}}
	raw, err := plotJSON(plotObject{{"kind", summarySnapshotKind}, {"source", orderedSource},
		{"identityRevision", revision}, {"characters", characters}})
	if err != nil {
		t.Fatal(err)
	}
	fccFreezeRaw(t, db, source.FinalizationID, raw, summaryHash(raw))
}

func fccFreezeRaw(t *testing.T, db *sql.DB, finalizationID, raw, sourceHash string) {
	t.Helper()
	mustExec(t, db, `INSERT OR REPLACE INTO character_identity_proposals(proposal_id,source_key,source_hash,raw_value,candidate_ids_json)
 VALUES(?,'`+summarySnapshotKind+`',?,?,'[]')`, "fcs:"+finalizationID, sourceHash, raw)
}

// fccFrozenCharacter is one frozen FinalizedCharacterReference plus its aliases.
func fccFrozenCharacter(id, name string, aliases ...any) map[string]any {
	return map[string]any{"characterId": id, "displayNameSnapshot": name, "aliases": aliases}
}

// fccCharacter seeds one character row. The static name is deliberately not the
// frozen display name: the context must report the snapshot, not the live row.
func fccCharacter(t *testing.T, db *sql.DB, id, name string, retired int64, location, powerLevel, provenance string) {
	t.Helper()
	mustExec(t, db, `INSERT INTO characters(character_id,name,retired,cs_updated_at_chapter,cs_location,
 cs_power_level,cs_provenance) VALUES(?,?,?,1,?,?,?)`, id, name, retired, location, powerLevel, provenance)
}

// fccProvenance marks location as author-supplied and powerLevel as derived with
// its revision, source and derived order, in the order roster writers use.
func fccProvenance(t *testing.T, source FinalizedCharacterSourceIdentity) string {
	t.Helper()
	order := plotObject{{"continuityEpoch", fccProjectID + ":2"}, {"chapterNumber", source.ChapterNumber},
		{"authoritativeFinalizationRevision", int64(1)}}
	storedSource := plotObject{{"draftId", source.DraftID}, {"finalizationId", source.FinalizationID},
		{"chapterNumber", source.ChapterNumber}, {"contentHash", source.ContentHash}}
	raw, err := plotJSON(plotObject{
		{"location", plotObject{{"kind", "author"}, {"chapterNumber", int64(1)}}},
		{"powerLevel", plotObject{{"kind", "derived"}, {"revision", int64(2)}, {"source", storedSource}, {"sourceOrder", order}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func fccNumber(t *testing.T, value any, label string) float64 {
	t.Helper()
	switch number := value.(type) {
	case int64:
		return float64(number)
	case float64:
		return number
	}
	t.Fatalf("%s is %T (%+v), want a number", label, value, value)
	return 0
}

func fccField(t *testing.T, snapshot FinalizedCharacterContext, field string) FinalizedCharacterFieldSnapshot {
	t.Helper()
	if len(snapshot.Characters) != 1 {
		t.Fatalf("%d characters, want 1", len(snapshot.Characters))
	}
	for _, candidate := range snapshot.Characters[0].Fields {
		if candidate.Field == field {
			return candidate
		}
	}
	t.Fatalf("field %v missing", field)
	return FinalizedCharacterFieldSnapshot{}
}

// TestFinalizedCharacterContextHappyPath is the bound arm: one frozen character
// of two marked fields, read back over a second, read-only connection.
func TestFinalizedCharacterContextHappyPath(t *testing.T) {
	db, characters, path := fccFixture(t)
	source := fccDraft(t, db, 1, 1, 1, "finalized", fccBody, fccFinalizationID)
	fccFreeze(t, db, source, 3, []any{fccFrozenCharacter(fccCharacterID, fccFrozenName, "青妹", "阿青")})
	fccCharacter(t, db, fccCharacterID, fccCharacterName, 0, fccLocation, fccPowerLevel, fccProvenance(t, source))
	before := fccDigest(t, path)

	snapshot, err := characters.ReadContext(context.Background(), 1, fccScope())
	if err != nil {
		t.Fatalf("read context: %v", err)
	}
	if snapshot.ProjectID != fccProjectID || snapshot.Epoch != fccEpoch {
		t.Fatalf("scope %v/%v", snapshot.ProjectID, snapshot.Epoch)
	}
	if !reflect.DeepEqual(snapshot.Source, source) {
		t.Fatalf("source %+v, want %+v", snapshot.Source, source)
	}
	if snapshot.Content != fccBody {
		t.Fatalf("content %v, want the frozen snapshot body", snapshot.Content)
	}
	if snapshot.ProjectionGeneration != 2 {
		t.Fatalf("projectionGeneration %d, want 2", snapshot.ProjectionGeneration)
	}
	if snapshot.IdentityRevision != 3 || snapshot.IdentityStatus != FinalizedCharacterIdentityBound {
		t.Fatalf("identity %d/%v, want 3/bound", snapshot.IdentityRevision, snapshot.IdentityStatus)
	}
	wantOrder := FinalizedCharacterSourceOrder{ContinuityEpoch: fccProjectID + ":2",
		ChapterNumber: 1, AuthoritativeFinalizationRevision: 1}
	if !reflect.DeepEqual(snapshot.SourceOrder, wantOrder) {
		t.Fatalf("sourceOrder %+v, want %+v", snapshot.SourceOrder, wantOrder)
	}
	if len(snapshot.Characters) != 1 {
		t.Fatalf("%d characters, want 1", len(snapshot.Characters))
	}
	character := snapshot.Characters[0]
	if character.CharacterID != fccCharacterID || character.DisplayNameSnapshot != fccFrozenName {
		t.Fatalf("character %+v, want the frozen reference", character)
	}
	if !reflect.DeepEqual(character.Aliases, []string{"青妹", "阿青"}) {
		t.Fatalf("aliases %+v", character.Aliases)
	}
	if got := len(character.Fields); got != len(characterStateFields) {
		t.Fatalf("%d fields, want %d", got, len(characterStateFields))
	}
	for index, field := range character.Fields {
		if field.Field != characterStateFields[index] || field.CharacterID != fccCharacterID ||
			field.ProjectID != fccProjectID || field.Epoch != fccEpoch {
			t.Fatalf("field %d is %+v", index, field)
		}
	}

	location := fccField(t, *snapshot, "location")
	if location.Value != fccLocation || location.ValueHash != summaryHash(fccLocation) {
		t.Fatalf("location %v/%v", location.Value, location.ValueHash)
	}
	if location.Revision != 0 || location.Provenance["kind"] != "author" {
		t.Fatalf("location revision/provenance %d/%+v", location.Revision, location.Provenance)
	}
	if got := fccNumber(t, location.Provenance["chapterNumber"], "author chapterNumber"); got != 1 {
		t.Fatalf("author chapterNumber %v, want 1", got)
	}
	if location.SourceOrder != nil {
		t.Fatalf("author location carries sourceOrder %+v", location.SourceOrder)
	}

	power := fccField(t, *snapshot, "powerLevel")
	if power.Value != fccPowerLevel || power.ValueHash != summaryHash(fccPowerLevel) || power.Revision != 2 {
		t.Fatalf("powerLevel %+v", power)
	}
	if power.Provenance["kind"] != "derived" {
		t.Fatalf("powerLevel provenance %+v", power.Provenance)
	}
	storedSource, ok := power.Provenance["source"].(map[string]any)
	if !ok || storedSource["finalizationId"] != fccFinalizationID || storedSource["contentHash"] != source.ContentHash {
		t.Fatalf("derived source %+v", power.Provenance["source"])
	}
	storedOrder, ok := power.SourceOrder.(map[string]any)
	if !ok || storedOrder["continuityEpoch"] != fccProjectID+":2" {
		t.Fatalf("derived sourceOrder %+v", power.SourceOrder)
	}
	if got := fccNumber(t, storedOrder["authoritativeFinalizationRevision"], "order revision"); got != 1 {
		t.Fatalf("order revision %v, want 1", got)
	}

	// The three unmarked fields keep the legacy arm and no derived order.
	untouched := fccField(t, *snapshot, "mentalState")
	if untouched.Value != "" || untouched.Revision != 0 || untouched.Provenance["kind"] != "legacy" || untouched.SourceOrder != nil {
		t.Fatalf("unmarked field %+v", untouched)
	}

	if after := fccDigest(t, path); after != before {
		t.Fatal("the read changed the database")
	}
	// A connection that could not have written this must see the same context.
	readBack, err := NewFinalizedCharacters(fccReadOnly(t, path)).ReadContext(context.Background(), 1, fccScope())
	if err != nil {
		t.Fatalf("read-only context: %v", err)
	}
	if !reflect.DeepEqual(readBack, snapshot) {
		t.Fatalf("read-only context %+v, want %+v", readBack, snapshot)
	}
}

// TestFinalizedCharacterContextRefusesRetiredCharacter is
// FINALIZED_CHARACTER_ID_RETIRED (summary-repository.ts:413).
func TestFinalizedCharacterContextRefusesRetiredCharacter(t *testing.T) {
	db, characters, path := fccFixture(t)
	source := fccDraft(t, db, 1, 1, 1, "finalized", fccBody, fccFinalizationID)
	fccFreeze(t, db, source, 3, []any{fccFrozenCharacter(fccCharacterID, fccFrozenName, "青妹")})
	fccCharacter(t, db, fccCharacterID, fccCharacterName, 1, fccLocation, fccPowerLevel, "{}")
	fccRefusal(t, path, ErrFinalizedCharacterIDRetired, func() (*FinalizedCharacterContext, error) {
		return characters.ReadContext(context.Background(), 1, fccScope())
	})
}

// TestFinalizedCharacterContextRefusesBrokenSource walks the :303-310 null clause
// one arm per subtest, so a red run names the single guard its fixture broke.
// Every arm reaches the caller as FINALIZED_CHARACTER_SOURCE_CHANGED (:406).
func TestFinalizedCharacterContextRefusesBrokenSource(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, db *sql.DB) FinalizedCharacterSourceIdentity
	}{
		{
			// :304 !row
			name: "no finalization receipt",
			seed: func(t *testing.T, db *sql.DB) FinalizedCharacterSourceIdentity {
				return fccDraft(t, db, 1, 1, 1, "finalized", fccBody, "")
			},
		},
		{
			// :305 row.status !== 'finalized'
			name: "status is not finalized",
			seed: func(t *testing.T, db *sql.DB) FinalizedCharacterSourceIdentity {
				return fccDraft(t, db, 1, 1, 1, "draft", fccBody, fccFinalizationID)
			},
		},
		{
			// :306 !row.finalizationId.trim()
			name: "finalization id is blank",
			seed: func(t *testing.T, db *sql.DB) FinalizedCharacterSourceIdentity {
				source := fccDraft(t, db, 1, 1, 1, "finalized", fccBody, "")
				fccReceipt(t, db, 1, 1, " ", source.ContentHash, fccBody)
				return source
			},
		},
		{
			// :307 !/^[a-f0-9]{64}$/u.test(row.contentHash)
			name: "content hash is not a sha256",
			seed: func(t *testing.T, db *sql.DB) FinalizedCharacterSourceIdentity {
				source := fccDraft(t, db, 1, 1, 1, "finalized", fccBody, "")
				fccReceipt(t, db, 1, 1, fccFinalizationID, "not-a-sha256", fccBody)
				return source
			},
		},
		{
			// :308 row.content !== row.contentSnapshot
			name: "draft body differs from the frozen snapshot",
			seed: func(t *testing.T, db *sql.DB) FinalizedCharacterSourceIdentity {
				source := fccDraft(t, db, 1, 1, 1, "finalized", fccBody, "")
				fccReceipt(t, db, 1, 1, fccFinalizationID, summaryHash(fccBody), "另一版正文")
				return source
			},
		},
		{
			// :309 sha256(row.contentSnapshot) !== row.contentHash
			name: "frozen snapshot does not hash to the receipt",
			seed: func(t *testing.T, db *sql.DB) FinalizedCharacterSourceIdentity {
				source := fccDraft(t, db, 1, 1, 1, "finalized", fccBody, "")
				fccReceipt(t, db, 1, 1, fccFinalizationID, summaryHash("别的正文"), fccBody)
				return source
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			db, characters, path := fccFixture(t)
			source := testCase.seed(t, db)
			fccRefusal(t, path, ErrFinalizedCharacterSourceChanged, func() (*FinalizedCharacterContext, error) {
				return characters.ReadContext(context.Background(), source.DraftID, fccScope())
			})
		})
	}
}

// TestFinalizedCharacterContextRefusesSupersededDraft is the
// "a newer finalized draft exists for this chapter" arm
// (summary-repository.ts:407-409).
func TestFinalizedCharacterContextRefusesSupersededDraft(t *testing.T) {
	db, characters, path := fccFixture(t)
	older := fccDraft(t, db, 1, 1, 1, "finalized", fccBody, fccFinalizationID)
	fccDraft(t, db, 2, 1, 2, "finalized", "新的定稿。", fccNewerFinalID)
	fccFreeze(t, db, older, 3, []any{fccFrozenCharacter(fccCharacterID, fccFrozenName, "青妹")})
	fccCharacter(t, db, fccCharacterID, fccCharacterName, 0, fccLocation, fccPowerLevel, fccProvenance(t, older))
	fccRefusal(t, path, ErrFinalizedCharacterSourceChanged, func() (*FinalizedCharacterContext, error) {
		return characters.ReadContext(context.Background(), 1, fccScope())
	})
}

// TestFinalizedCharacterContextRefusesMissingScope is
// FINALIZED_CHARACTER_SCOPE_REQUIRED (summary-repository.ts:402), answered before
// any read: this database holds no draft at all. Each blank-scope spelling is its
// own subtest so a red run names the character it could not trim.
func TestFinalizedCharacterContextRefusesMissingScope(t *testing.T) {
	db, characters, path := fccFixture(t)
	scopes := []struct {
		name  string
		scope FinalizedCharacterScope
	}{
		{name: "blank project id", scope: FinalizedCharacterScope{Epoch: fccEpoch}},
		{name: "project id is only whitespace", scope: FinalizedCharacterScope{ProjectID: " \t\ufeff ", Epoch: fccEpoch}},
		{name: "blank epoch", scope: FinalizedCharacterScope{ProjectID: fccProjectID}},
		{name: "epoch is only a no-break space", scope: FinalizedCharacterScope{ProjectID: fccProjectID, Epoch: "\u00a0"}},
		{name: "epoch is only spaces", scope: FinalizedCharacterScope{ProjectID: fccProjectID, Epoch: "  "}},
	}
	for _, testCase := range scopes {
		t.Run(testCase.name, func(t *testing.T) {
			fccRefusal(t, path, ErrFinalizedCharacterScopeRequired, func() (*FinalizedCharacterContext, error) {
				return characters.ReadContext(context.Background(), 1, testCase.scope)
			})
		})
	}
	t.Run("no database", func(t *testing.T) {
		// The reference's first arm is `!database`.
		fccRefusal(t, path, ErrFinalizedCharacterScopeRequired, func() (*FinalizedCharacterContext, error) {
			return NewFinalizedCharacters(nil).ReadContext(context.Background(), 1, fccScope())
		})
	})
	if got := narrativeCount(t, db, "drafts"); got != 0 {
		t.Fatalf("%d drafts, want none", got)
	}
}

// TestFinalizedCharacterContextLegacyWithoutFrozenSnapshot is the absent
// freeze-receipt arm: identityStatus 'legacy', revision 0, no characters
// (summary-repository.ts:417).
func TestFinalizedCharacterContextLegacyWithoutFrozenSnapshot(t *testing.T) {
	db, characters, path := fccFixture(t)
	source := fccDraft(t, db, 1, 1, 1, "finalized", fccBody, fccFinalizationID)
	before := fccDigest(t, path)

	snapshot, err := characters.ReadContext(context.Background(), 1, fccScope())
	if err != nil {
		t.Fatalf("read context: %v", err)
	}
	if snapshot.IdentityStatus != FinalizedCharacterIdentityLegacy || snapshot.IdentityRevision != 0 {
		t.Fatalf("identity %d/%v, want 0/legacy", snapshot.IdentityRevision, snapshot.IdentityStatus)
	}
	if snapshot.Characters == nil || len(snapshot.Characters) != 0 {
		t.Fatalf("characters %+v, want an empty list", snapshot.Characters)
	}
	if !reflect.DeepEqual(snapshot.Source, source) || snapshot.Content != fccBody {
		t.Fatalf("legacy source %+v", snapshot.Source)
	}
	if snapshot.SourceOrder.ContinuityEpoch != fccProjectID+":2" {
		t.Fatalf("continuityEpoch %v", snapshot.SourceOrder.ContinuityEpoch)
	}
	if after := fccDigest(t, path); after != before {
		t.Fatal("the read changed the database")
	}
}

// TestFinalizedCharacterContextRefusesCorruptSnapshot is
// FINALIZED_CHARACTER_SNAPSHOT_INVALID (summary-repository.ts:70-75): the receipt
// no longer hashes to its own bytes.
func TestFinalizedCharacterContextRefusesCorruptSnapshot(t *testing.T) {
	db, characters, path := fccFixture(t)
	source := fccDraft(t, db, 1, 1, 1, "finalized", fccBody, fccFinalizationID)
	orderedSource := plotObject{{"draftId", source.DraftID}, {"finalizationId", source.FinalizationID},
		{"chapterNumber", source.ChapterNumber}, {"contentHash", source.ContentHash}}
	raw, err := plotJSON(plotObject{{"kind", summarySnapshotKind}, {"source", orderedSource},
		{"identityRevision", int64(3)}, {"characters", []any{}}})
	if err != nil {
		t.Fatal(err)
	}
	fccFreezeRaw(t, db, fccFinalizationID, raw, strings.Repeat("0", 64))
	fccCharacter(t, db, fccCharacterID, fccCharacterName, 0, fccLocation, fccPowerLevel, "{}")
	fccRefusal(t, path, ErrSummaryCharacterSnapshotInvalid, func() (*FinalizedCharacterContext, error) {
		return characters.ReadContext(context.Background(), 1, fccScope())
	})
}

// TestFinalizedCharacterContextRefusesFieldRevision is
// FINALIZED_CHARACTER_FIELD_REVISION_INVALID (summary-repository.ts:84): a stored
// revision outside the safe non-negative integer range.
func TestFinalizedCharacterContextRefusesFieldRevision(t *testing.T) {
	db, characters, path := fccFixture(t)
	source := fccDraft(t, db, 1, 1, 1, "finalized", fccBody, fccFinalizationID)
	fccFreeze(t, db, source, 3, []any{fccFrozenCharacter(fccCharacterID, fccFrozenName, "青妹")})
	fccCharacter(t, db, fccCharacterID, fccCharacterName, 0, fccLocation, fccPowerLevel,
		`{"location":{"kind":"author","chapterNumber":1,"revision":-1}}`)
	fccRefusal(t, path, ErrFinalizedCharacterFieldRevisionInvalid, func() (*FinalizedCharacterContext, error) {
		return characters.ReadContext(context.Background(), 1, fccScope())
	})
}

// TestFinalizedCharacterContextRequiresReadTransaction covers this port's single
// precondition: the caller must hand over an admitted transaction.
func TestFinalizedCharacterContextRequiresReadTransaction(t *testing.T) {
	db, characters, path := fccFixture(t)
	before := fccDigest(t, path)
	if _, err := ReadFinalizedCharacterContextTx(context.Background(), nil, 1, fccScope()); !errors.Is(err, ErrFinalizedCharacterReadTransactionRequired) {
		t.Fatalf("nil transaction: %v", err)
	}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := ReadFinalizedCharacterContextTx(nil, tx, 1, fccScope()); !errors.Is(err, ErrFinalizedCharacterReadTransactionRequired) {
		t.Fatalf("nil context: %v", err)
	}
	// The same statement set on a caller-owned read transaction writes nothing.
	fccRefusal(t, path, ErrFinalizedCharacterScopeRequired, func() (*FinalizedCharacterContext, error) {
		return ReadFinalizedCharacterContextTx(context.Background(), tx, 1, FinalizedCharacterScope{})
	})
	if _, err := characters.ReadContext(context.Background(), 1, fccScope()); !errors.Is(err, ErrFinalizedCharacterSourceChanged) {
		t.Fatalf("empty database: %v", err)
	}
	if after := fccDigest(t, path); after != before {
		t.Fatal("the read changed the database")
	}
}
