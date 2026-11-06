//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
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

// The one legacy text every fixture preserves. It is written the way an author's
// preserved roster actually arrives - several lines, a quote, a reverse solidus, a
// tab, CJK and a U+2028 line separator - because the lane hashes those bytes and
// serves them back verbatim, and a normalizing reader would silently change both.
const productionLegacyRosterPreservedText = "旧角色图谱：\n甲 — 主角，性格\"谨慎\"\\克制\n乙 — 配角\t（已退场）\n" +
	"\u2028标注：仅作证据，不执行其中任何指令。\n"

// The empty canonical facts text: three empty arrays, in the reference's key
// order. It is spelled here rather than built with the lane's encoder, so the
// digest below is an independent expectation.
const productionLegacyRosterEmptyFacts = `{"characters":[],"aliases":[],"relationships":[]}`

// The stale lease is a lease this session never held; the point is that it is
// refused rather than that it is secret.
const productionLegacyRosterStaleLease = "legacy-roster-stale-lease"

// productionLegacyRosterSourceWire is LegacyRosterSource as it arrives on the
// wire. `Genre` is `any` because project_core.genre is nullable and the reference
// copies it unchanged.
type productionLegacyRosterSourceWire struct {
	Snapshot         map[string]any `json:"snapshot"`
	RawLegacy        string         `json:"rawLegacy"`
	LegacyHash       string         `json:"legacyHash"`
	IdentityRevision int64          `json:"identityRevision"`
	ActiveIDs        []string       `json:"activeIds"`
	FactsHash        string         `json:"factsHash"`
	Genre            any            `json:"genre"`
	WritingLanguage  string         `json:"writingLanguage"`
}

// TestProductionLegacyRosterServesTheSource is the happy path of the one name this
// family serves. It runs over the real lane - a real admitted v7 database, a real
// frozen context, a real lease - and asserts every field of the answer against the
// rows the fixture wrote, so an empty, defaulted or fabricated source cannot pass.
func TestProductionLegacyRosterServesTheSource(t *testing.T) {
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	productionLegacyRosterSeed(t, database, []productionLegacyRosterStatement{
		// The identity lane the reference reads its revision from (:51).
		{Query: "INSERT OR REPLACE INTO character_identity_meta (id, revision, legacy_count) VALUES ('main', 3, 0)"},
		// The preserved roster, in the state the adoption exists for: cards the
		// author already has, and the old text they came from (:92).
		{Query: `INSERT OR REPLACE INTO character_roster_meta
 (id, schema_version, revision, migration_state, legacy_markdown, projection_hash, fact_hash)
 VALUES ('main', 1, 9, 'legacy_cards_preserved', ?, '', '')`,
			Args: []any{productionLegacyRosterPreservedText}},
		// A retired identity: not listed, never adopted, but still hashed (:60).
		{Query: `INSERT INTO characters
 (character_id, name, role, retired, created_at, updated_at)
 VALUES ('ci-character-0-retired', '乙', 'supporting', 1, '2024-05-06 07:08:09', '2024-05-06 07:08:09')`},
		// An author alias and an author relationship, so the two row sets that are
		// not `characters` are covered by the digest too.
		{Query: `INSERT INTO character_aliases (character_id, name, source_key, valid_from, valid_through)
 VALUES ('ci-character-1', '甲', 'author-alias-1', 0, NULL)`},
		{Query: `INSERT INTO character_aliases (character_id, name, source_key, valid_from, valid_through)
 VALUES ('ci-character-0-retired', '乙', 'author-alias-2', 0, NULL)`},
		{Query: `INSERT INTO character_relationships
 (relationship_id, source_character_id, target_character_id, relation,
  source_display_snapshot, target_display_snapshot, provenance_json, approval_id)
 VALUES ('ci-relationship-1', 'ci-character-1', 'ci-character-0-retired', 'mentor',
         '甲', '乙', '{"origin":"author"}', '')`},
		// A writing language that is not the default, so resolveWritingLanguage is
		// exercised by a value instead of by its fallback.
		{Query: "UPDATE project_core SET writing_language='en-US' WHERE id='main'"},
	})
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionDurableTab)
	frozen := productionLegacyRosterOpen(t, author, projectPath)

	t.Run("every field is the project's own", func(t *testing.T) {
		source := productionLegacyRosterRead(t, author, frozen)
		if source.RawLegacy != productionLegacyRosterPreservedText {
			t.Fatalf("rawLegacy is not the preserved text: %d bytes", len(source.RawLegacy))
		}
		if expected := sha256Hex(productionLegacyRosterPreservedText); source.LegacyHash != expected {
			t.Fatalf("legacyHash is %s, want the digest of the preserved bytes", source.LegacyHash)
		}
		if source.IdentityRevision != 3 {
			t.Fatalf("identityRevision is %d, want the row's 3", source.IdentityRevision)
		}
		// Retired identities are neither listed nor adoptable: the reference filters
		// `row.retired === 0` and only the id ordering of the statement is left
		// (:60).
		if joined := strings.Join(source.ActiveIDs, ","); joined != "ci-character-1" {
			t.Fatalf("activeIds is %s, want only the active card", joined)
		}
		if source.Genre != "fantasy" {
			t.Fatalf("genre is %v, want the project_core value", source.Genre)
		}
		if source.WritingLanguage != productionLegacyRosterLanguageEnUS {
			t.Fatalf("writingLanguage is %s, want the project's en-US", source.WritingLanguage)
		}
		if len(source.FactsHash) != 64 || source.FactsHash != strings.ToLower(source.FactsHash) {
			t.Fatalf("factsHash is not a lowercase sha256: %s", source.FactsHash)
		}
		if source.FactsHash == source.LegacyHash {
			t.Fatal("factsHash repeats the legacy digest, so the rows were not hashed")
		}
		// The snapshot is the store's own roster read, not a second rendering: the
		// project is preserved-and-unprojected, so it must say exactly that rather
		// than presenting the cards as a finished roster.
		if state := syntheticString(source.Snapshot["migrationState"]); state != "legacy_cards_preserved" {
			t.Fatalf("snapshot migrationState is %s, want legacy_cards_preserved", state)
		}
		if status := syntheticString(source.Snapshot["status"]); status != "inconsistent" {
			t.Fatalf("snapshot status is %s, want inconsistent", status)
		}
		if text := syntheticString(source.Snapshot["legacyMarkdown"]); text != productionLegacyRosterPreservedText {
			t.Fatalf("snapshot legacyMarkdown is not the preserved text: %d bytes", len(text))
		}
		if revision, ok := syntheticFloat64(source.Snapshot["revision"]); !ok || revision != 9 {
			t.Fatalf("snapshot revision is %v, want the roster row's 9", source.Snapshot["revision"])
		}
		entries := syntheticMapList(source.Snapshot["entries"])
		if len(entries) != 1 || syntheticString(entries[0]["characterId"]) != "ci-character-1" {
			t.Fatalf("snapshot entries is %d, want the one active card", len(entries))
		}
	})

	t.Run("replay answers the same bytes", func(t *testing.T) {
		before := productionLegacyRosterResult(t, author, frozen)
		after := productionLegacyRosterResult(t, author, frozen)
		if before != after {
			t.Fatalf("a replayed read answered different bytes:\n%s\n%s", before, after)
		}
		// Order of the answer's keys is part of the contract the renderer decodes.
		want := `{"snapshot":`
		if !strings.HasPrefix(before, want) {
			t.Fatalf("the answer starts with %.40s, want the snapshot first", before)
		}
	})

	t.Run("a read writes nothing", func(t *testing.T) {
		before := productionGraphReadsBytes(t, database)
		productionLegacyRosterResult(t, author, frozen)
		productionGraphReadsUnchanged(t, database, before, productionLegacyRosterReadSourceName)
	})

	t.Run("a retired row is hashed, not listed", func(t *testing.T) {
		// The case a naive implementation passes: hash only what is adoptable and a
		// project whose author removed a card keeps the same factsHash, so a stale
		// adoption request would still match and the removed card could be
		// resurrected. The reference hashes the rows themselves (:60).
		//
		// This is the last subtest because it edits the file the live permit already
		// hashed; every assertion below runs through this test's own connection.
		db := productionLegacyRosterWriter(t, database)
		defer func() { _ = db.Close() }()
		before, err := productionLegacyRosterReadSource(context.Background(), db)
		if err != nil {
			t.Fatalf("read the seeded source: %v", err)
		}
		if _, err := db.Exec("DELETE FROM characters WHERE character_id='ci-character-0-retired'"); err != nil {
			t.Fatalf("remove the retired identity: %v", err)
		}
		after, err := productionLegacyRosterReadSource(context.Background(), db)
		if err != nil {
			t.Fatalf("read the source again: %v", err)
		}
		if strings.Join(after.ActiveIDs, ",") != strings.Join(before.ActiveIDs, ",") {
			t.Fatalf("removing a retired identity changed activeIds: %v", after.ActiveIDs)
		}
		if after.RawLegacy != before.RawLegacy || after.LegacyHash != before.LegacyHash {
			t.Fatal("removing a retired identity changed the legacy text")
		}
		if after.FactsHash == before.FactsHash {
			t.Fatal("factsHash ignored a retired identity, so a stale adoption would still match")
		}
	})

	t.Run("a source changed under the run is refused", func(t *testing.T) {
		// The rows above were rewritten after the permit hashed the file, so this is
		// the case the adoption's CAS tokens exist for: the answer can no longer be
		// proved to be the source the run started from, and the lane must refuse
		// rather than describe a file it does not own.
		productionEditorRefused(t, author, "changed source", productionLegacyRosterReadSourceName, []any{frozen})
	})
}

// TestProductionLegacyRosterServesAPendingRoster is the case an author importing a
// half-written book hits first: the preserved text is there and the cards are not.
// The answer must say what the project actually holds - the text, no cards, and a
// roster that is waiting for a repair - instead of presenting an empty roster as if
// it had been parsed.
func TestProductionLegacyRosterServesAPendingRoster(t *testing.T) {
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	productionLegacyRosterSeed(t, database, []productionLegacyRosterStatement{
		{Query: "INSERT OR REPLACE INTO character_identity_meta (id, revision, legacy_count) VALUES ('main', 0, 0)"},
		{Query: `INSERT OR REPLACE INTO character_roster_meta
 (id, schema_version, revision, migration_state, legacy_markdown, projection_hash, fact_hash)
 VALUES ('main', 1, 0, 'legacy_markdown_pending', ?, '', '')`,
			Args: []any{productionLegacyRosterPreservedText}},
		{Query: "DELETE FROM characters"},
	})
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionDurableTab)
	frozen := productionLegacyRosterOpen(t, author, projectPath)

	result := productionLegacyRosterResult(t, author, frozen)
	source := productionLegacyRosterRead(t, author, frozen)
	if source.RawLegacy != productionLegacyRosterPreservedText {
		t.Fatalf("the preserved text was not served: %d bytes", len(source.RawLegacy))
	}
	if source.LegacyHash != sha256Hex(productionLegacyRosterPreservedText) {
		t.Fatal("legacyHash is not the digest of the preserved bytes")
	}
	if len(source.ActiveIDs) != 0 {
		t.Fatalf("a project with no cards answered %d active ids", len(source.ActiveIDs))
	}
	// An empty list, not JSON null: the renderer maps over this array.
	if !strings.Contains(result, `"activeIds":[]`) {
		t.Fatalf("activeIds is not an empty array: %s", result)
	}
	// The digest of three empty arrays is knowable without knowing the column set of
	// the shipped baseline, so it pins the encoder's framing exactly.
	if expected := sha256Hex(productionLegacyRosterEmptyFacts); source.FactsHash != expected {
		t.Fatalf("factsHash is %s, want the digest of the three empty row sets", source.FactsHash)
	}
	if state := syntheticString(source.Snapshot["migrationState"]); state != "legacy_markdown_pending" {
		t.Fatalf("snapshot migrationState is %s, want legacy_markdown_pending", state)
	}
	if status := syntheticString(source.Snapshot["status"]); status != "legacy_repair_required" {
		t.Fatalf("snapshot status is %s, want legacy_repair_required", status)
	}
	if entries := syntheticMapList(source.Snapshot["entries"]); len(entries) != 0 {
		t.Fatalf("snapshot entries is %d, want none", len(entries))
	}
}

// TestProductionLegacyRosterRefusesASourceItCannotName pins the three source
// refusals and the order they are decided in. Each one is a project the lane must
// not describe: it has no identity lane, it has no preserved roster, or the value
// it does have is not text.
func TestProductionLegacyRosterRefusesASourceItCannotName(t *testing.T) {
	t.Run("no identity lane", func(t *testing.T) {
		// The durable fixture is an admitted project whose schema has no identity
		// tables at all, which is the reference's first refusal
		// (legacy-roster-source.ts:49).
		root, projectPath := productionDurableDataRoot(t)
		handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
			productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		author := newProductionDurableClient(t, handler, productionDurableTab)
		frozen := productionLegacyRosterOpen(t, author, projectPath)
		database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
		before := productionGraphReadsBytes(t, database)
		productionLegacyRosterRefused(t, author, []any{frozen}, "LEGACY_ROSTER_ID_SCHEMA_REQUIRED")
		productionGraphReadsUnchanged(t, database, before, "legacy-roster:read-source")
	})

	t.Run("no preserved roster", func(t *testing.T) {
		// The identity lane is there and the roster row is not: the reference's one
		// source guard (:50-53) answers, not a store error about a missing table.
		root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
		database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
		productionLegacyRosterSeed(t, database, []productionLegacyRosterStatement{
			// The row is deleted rather than assumed absent, so this case is the same
			// case whatever the shipped baseline seeds.
			{Query: "DELETE FROM character_roster_meta"},
			{Query: "INSERT OR REPLACE INTO character_identity_meta (id, revision, legacy_count) VALUES ('main', 0, 0)"},
		})
		handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
			productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		author := newProductionDurableClient(t, handler, productionDurableTab)
		frozen := productionLegacyRosterOpen(t, author, projectPath)
		before := productionGraphReadsBytes(t, database)
		productionLegacyRosterRefused(t, author, []any{frozen}, "LEGACY_ROSTER_SOURCE_INVALID")
		productionGraphReadsUnchanged(t, database, before, "legacy-roster:read-source")
	})

	t.Run("the preserved roster is not text", func(t *testing.T) {
		// A BLOB in legacy_markdown is a value the store refuses as unsupported
		// input. The source guard is decided first, exactly as in the reference,
		// where the typeof check is part of :53 and the roster read is :59.
		root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
		database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
		productionLegacyRosterSeed(t, database, []productionLegacyRosterStatement{
			{Query: "INSERT OR REPLACE INTO character_identity_meta (id, revision, legacy_count) VALUES ('main', 0, 0)"},
			{Query: `INSERT OR REPLACE INTO character_roster_meta
 (id, schema_version, revision, migration_state, legacy_markdown, projection_hash, fact_hash)
 VALUES ('main', 1, 0, 'legacy_markdown_pending', ?, '', '')`,
				Args: []any{[]byte{0x00, 0x01}}},
		})
		handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
			productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		author := newProductionDurableClient(t, handler, productionDurableTab)
		frozen := productionLegacyRosterOpen(t, author, projectPath)
		productionLegacyRosterRefused(t, author, []any{frozen}, "LEGACY_ROSTER_SOURCE_INVALID")
	})

	t.Run("a retired card with no id", func(t *testing.T) {
		// The one refusal that cannot be driven through the lane: admission's own
		// identity invariant already refuses a blank character_id when the project is
		// opened (internal/admission/identity.go:51-58), so this guard is
		// defence-in-depth over the rows the run reads. It is still the reference's
		// own code (:54-55), and it still applies to retired rows, which is the part
		// a filter on active cards would get wrong.
		_, projectPath, _, _, _ := productionEditorReadsRoot(t)
		database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
		productionLegacyRosterSeed(t, database, []productionLegacyRosterStatement{
			{Query: "INSERT OR REPLACE INTO character_identity_meta (id, revision, legacy_count) VALUES ('main', 0, 0)"},
			{Query: `INSERT OR REPLACE INTO character_roster_meta
 (id, schema_version, revision, migration_state, legacy_markdown, projection_hash, fact_hash)
 VALUES ('main', 1, 0, 'legacy_cards_preserved', ?, '', '')`,
				Args: []any{productionLegacyRosterPreservedText}},
			{Query: `INSERT INTO characters (character_id, name, role, retired, created_at, updated_at)
 VALUES (' ', '幽灵', 'minor', 1, '2024-05-06 07:08:09', '2024-05-06 07:08:09')`},
		})
		db := productionLegacyRosterWriter(t, database)
		defer func() { _ = db.Close() }()
		if _, err := productionLegacyRosterReadSource(context.Background(), db); err == nil {
			t.Fatal("a blank character id was served as a source")
		} else if err.Error() != "LEGACY_ROSTER_ID_INVALID" {
			t.Fatalf("a blank character id answered %s, want LEGACY_ROSTER_ID_INVALID", err)
		}
	})

	t.Run("both broken answers the earlier guard", func(t *testing.T) {
		// Ordering is part of the contract: a project that is broken twice must not
		// answer the deeper code when the reference would never have reached it.
		_, projectPath, _, _, _ := productionEditorReadsRoot(t)
		database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
		productionLegacyRosterSeed(t, database, []productionLegacyRosterStatement{
			{Query: "DELETE FROM character_roster_meta"},
			{Query: "INSERT OR REPLACE INTO character_identity_meta (id, revision, legacy_count) VALUES ('main', 0, 0)"},
			{Query: `INSERT INTO characters (character_id, name, role, retired, created_at, updated_at)
 VALUES (' ', '幽灵', 'minor', 1, '2024-05-06 07:08:09', '2024-05-06 07:08:09')`},
		})
		db := productionLegacyRosterWriter(t, database)
		defer func() { _ = db.Close() }()
		if _, err := productionLegacyRosterReadSource(context.Background(), db); err == nil {
			t.Fatal("a project with no preserved roster was served")
		} else if err.Error() != "LEGACY_ROSTER_SOURCE_INVALID" {
			t.Fatalf("a project broken twice answered %s, want the source guard", err)
		}
	})
}

// TestProductionLaneRefusesLegacyRosterNames pins the six names this lane leaves
// unbound where the plan puts them: answering the fail-closed 501
// GO_OPERATION_NOT_MIGRATED, with nothing written while they do.
//
// Every subtest re-reads the seam the header names, so a port that moves or
// disappears fails here instead of leaving a note that points at nothing.
func TestProductionLaneRefusesLegacyRosterNames(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	author := newProductionDurableClient(t, handler, productionDurableTab)
	frozen := productionLegacyRosterOpen(t, author, projectPath)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	stale := map[string]any{"projectId": productionDurableProjectID, "projectPath": projectPath,
		"leaseId": productionLegacyRosterStaleLease}
	handle := map[string]any{"projectId": productionDurableProjectID, "epoch": productionLegacyRosterStaleLease,
		"rootActionId": "legacy-roster-missing-root", "runId": "legacy-roster-missing-run"}

	for _, operation := range []string{
		productionLegacyRosterAdoptExistingName,
		productionLegacyRosterBeginName,
		productionLegacyRosterReadName,
		productionLegacyRosterExecuteName,
		productionLegacyRosterStageName,
		productionLegacyRosterCancelName,
	} {
		operation := operation
		t.Run(operation, func(t *testing.T) {
			productionLegacyRosterSeamProof(t, operation)
			before := productionGraphReadsBytes(t, database)
			// The reference request, the same request under a stale lease, a request
			// with nothing in it and a request that is not an object: all four get the
			// same answer, because the name is not installed and the transport never
			// reaches the envelope, the lease or the request shape.
			adoption := map[string]any{"operationId": "legacy-roster-adoption-1", "expectedRevision": 9,
				"expectedLegacyHash": strings.Repeat("a", 64), "expectedIdentityRevision": 3,
				"expectedFactsHash": strings.Repeat("b", 64)}
			for _, args := range [][]any{
				{adoption, frozen},
				{map[string]any{"handle": handle}, frozen},
				{map[string]any{"handle": handle}, stale},
				{frozen},
				{"not-an-object", frozen},
			} {
				productionLegacyRosterUnbound(t, author, operation, args)
			}
			productionGraphReadsUnchanged(t, database, before, operation)
		})
	}
}

// TestProductionLegacyRosterBinderIsTheOneName pins the binder's own contract: it
// installs exactly the served name and never the refused six, it publishes nothing
// without a bridge, and every name of the family stays inventoried so an absent
// handler is 501 and never the 404 of a name the registry does not know.
func TestProductionLegacyRosterBinderIsTheOneName(t *testing.T) {
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
	if err := newProductionLegacyRoster(nil).Bind(server); err != nil {
		t.Fatalf("a bridge-less binder failed: %v", err)
	}
	if names := server.BoundOperations(); len(names) != 0 {
		t.Fatalf("a bridge-less binder installed %v, want nothing", names)
	}
	var empty *productionLegacyRoster
	if err := empty.Bind(server); err != nil {
		t.Fatalf("a nil lane failed: %v", err)
	}
	if names := server.BoundOperations(); len(names) != 0 {
		t.Fatalf("a nil lane installed %v, want nothing", names)
	}
	for _, operation := range productionLegacyRosterOperations() {
		fresh := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
		if err := fresh.Bind(operation, func(context.Context, transport.Caller, []any) (any, error) {
			return nil, nil
		}); err != nil {
			t.Fatalf("%s is not inventoried, so its unbound answer would not be 501: %v", operation, err)
		}
	}
	// The served name must never be documented as refused, and the six must be.
	if _, found := productionLegacyRosterSeam(productionLegacyRosterReadSourceName); found {
		t.Fatalf("%s is served here and must not appear in the seam table", productionLegacyRosterReadSourceName)
	}
	documented := 0
	for _, gap := range productionLegacyRosterSeams() {
		if len(gap.Missing) == 0 {
			t.Fatalf("%s documents no missing piece", gap.Operation)
		}
		documented++
	}
	if documented != len(productionLegacyRosterOperations())-1 {
		t.Fatalf("%d names are documented as refused, want every name but the served one", documented)
	}
	// A closed or unknown refusal must never leak a SQLite message or a path: the
	// one mapping layer answers the controller's own unmapped-failure code.
	if code := productionLegacyRosterFailure(errors.New("no such table: characters")); code.Error() != productionLegacyRosterRequestFailed {
		t.Fatalf("an unmapped failure answered %s, want %s", code, productionLegacyRosterRequestFailed)
	}
	if code := productionLegacyRosterFailure(store.ErrCharacterRosterSchemaUnsupported); code.Error() != "CHARACTER_ROSTER_SCHEMA_UNSUPPORTED" {
		t.Fatalf("a store refusal answered %s, want its own code", code)
	}
}

// productionLegacyRosterStatement is one seeded write, with its arguments kept
// separate so the preserved text needs no SQL quoting of its own.
type productionLegacyRosterStatement struct {
	Query string
	Args  []any
}

// productionLegacyRosterSeed writes the fixture rows through the same admitted
// shape the runtime writes, then folds the write-ahead log into the file the lane
// is about to admit: a sidecar carrying rows the admitted digest does not cover
// would hide them from the read.
func productionLegacyRosterSeed(t *testing.T, database string, statements []productionLegacyRosterStatement) {
	t.Helper()
	db := productionLegacyRosterWriter(t, database)
	for _, statement := range statements {
		if _, err := db.Exec(statement.Query, statement.Args...); err != nil {
			_ = db.Close()
			t.Fatalf("seed the fixture: %v; the statement was: %s", err, statement.Query)
		}
	}
	productionEditorCheckpoint(t, db, database)
}

// productionLegacyRosterWriter opens the author database for the test's own writes
// and direct reads. The lane's handler never uses it: it is how the fixture is
// written before the permit hashes the file, and how the two ordering cases are
// driven without an admission that would refuse them first.
func productionLegacyRosterWriter(t *testing.T, database string) *sql.DB {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatalf("open the author database: %v", err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// productionLegacyRosterOpen opens the fixture's project and returns the frozen
// context every project-scoped call carries.
func productionLegacyRosterOpen(t *testing.T, author *productionDurableClient, projectPath string) map[string]any {
	t.Helper()
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "legacy-roster-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	projectID := syntheticString(opened.Project["id"])
	leaseID := syntheticString(opened.Project["sessionLease"])
	if projectID == "" || leaseID == "" {
		t.Fatalf("project:open returned %s/%s, want the seeded project with a live lease", projectID, leaseID)
	}
	return map[string]any{"projectId": projectID, "projectPath": projectPath, "leaseId": leaseID}
}

// productionLegacyRosterResult is the served answer as the bytes it arrived as, so
// a replay can be compared without a decode that would forgive a changed key order.
func productionLegacyRosterResult(t *testing.T, author *productionDurableClient, frozen map[string]any) string {
	t.Helper()
	response := author.call(t, productionLegacyRosterReadSourceName, []any{frozen})
	if response.Code != http.StatusOK {
		t.Fatalf("%s answered %d %s", productionLegacyRosterReadSourceName, response.Code, response.Body.String())
	}
	var wire struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || len(wire.Result) == 0 {
		t.Fatalf("%s: %v %s", productionLegacyRosterReadSourceName, err, response.Body.String())
	}
	return string(wire.Result)
}

// productionLegacyRosterRead decodes the served answer.
func productionLegacyRosterRead(t *testing.T, author *productionDurableClient, frozen map[string]any) productionLegacyRosterSourceWire {
	t.Helper()
	var source productionLegacyRosterSourceWire
	if err := json.Unmarshal([]byte(productionLegacyRosterResult(t, author, frozen)), &source); err != nil {
		t.Fatalf("%s: %v", productionLegacyRosterReadSourceName, err)
	}
	return source
}

// productionLegacyRosterRefused asserts one symbolic refusal of the served name.
// The status is asserted only as "not a success": the code, not the HTTP status, is
// what the reference's error gate publishes
// (electron/controllers/generation-controller.ts:88-101).
func productionLegacyRosterRefused(t *testing.T, author *productionDurableClient, args []any, code string) {
	t.Helper()
	response := author.call(t, productionLegacyRosterReadSourceName, args)
	if response.Code == http.StatusOK {
		t.Fatalf("%s answered 200 for a refused source: %s", productionLegacyRosterReadSourceName, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v %s", productionLegacyRosterReadSourceName, err, response.Body.String())
	}
	if body.Error.Code != code {
		t.Fatalf("%s refused with %s, want %s: %s", productionLegacyRosterReadSourceName,
			body.Error.Code, code, response.Body.String())
	}
}

// productionLegacyRosterUnbound asserts the fail-closed answer of a name this lane
// leaves uninstalled.
func productionLegacyRosterUnbound(t *testing.T, author *productionDurableClient, operation string, args []any) {
	t.Helper()
	response := author.call(t, operation, args)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("%s answered %d %s, want 501 GO_OPERATION_NOT_MIGRATED", operation, response.Code, response.Body.String())
	}
	if !bytes.Contains(response.Body.Bytes(), []byte("GO_OPERATION_NOT_MIGRATED")) {
		t.Fatalf("%s answered %d %s without GO_OPERATION_NOT_MIGRATED", operation, response.Code, response.Body.String())
	}
}

// productionLegacyRosterSeamProof re-reads every line the seam table names, so the
// documentation cannot rot: a moved declaration or a renamed refusal fails here
// instead of leaving a note that points at nothing. The marker is searched for
// across the whole file rather than only at the recorded line, because a seam
// shifted by an unrelated edit is not a regression while a deleted one is; the
// recorded line is still reported so a reader can find it quickly.
func productionLegacyRosterSeamProof(t *testing.T, operation string) {
	t.Helper()
	gap, found := productionLegacyRosterSeam(operation)
	if !found {
		t.Fatalf("%s has no documented seam", operation)
	}
	if len(gap.Missing) == 0 {
		t.Fatalf("%s documents no missing piece", operation)
	}
	for _, line := range gap.Missing {
		raw, err := os.ReadFile(line.Path)
		if err != nil {
			t.Fatalf("%s: %s: %v", operation, line.Path, err)
		}
		if !strings.Contains(string(raw), line.Marker) {
			t.Fatalf("%s: no longer documents %s anywhere in %s (it was recorded at line %d)",
				operation, line.Marker, line.Path, line.Line)
		}
	}
}
