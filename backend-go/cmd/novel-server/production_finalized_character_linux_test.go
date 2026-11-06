//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// This file proves the one name production_finalized_character_linux.go binds,
// `finalized-character:read-context`, one subtest per aspect.
//
// # The two fixtures, and why there are two
//
// The served subtest drives the real HTTP handler over a disposable admitted
// project, because that is where the binding, the frozen-context admission, the
// live lease and the wire answer live. Its rows are seeded through the
// neighbouring generation-write fixture's own hook
// (productionGenerationWriteOpen, production_generation_write_linux_test.go:130),
// which writes into the project database before the lane admits it - and the
// finalized draft plus its outbox receipt are that fixture's own rows
// (productionGenerationWriteSeedOutbox:243), not a second copy of them. The
// freeze receipt and the character row are added on top, because no neighbour
// seeds a frozen character.
//
// The store refusals are driven over internal/admission's pinned v7 baseline
// (WriteBaselineV7) instead: each of them needs its own database, and each of
// them refuses inside internal/store/finalized_character_context.go, where a
// project root, a manifest and a lease would add nothing and would hide which
// row made the read refuse. One helper writes that fixture and the same helpers
// write the rows both fixtures share.
//
// # What every subtest proves
//
// The served subtest compares the wire answer with a second read over a separate
// mode=ro connection, field for field, so nothing is asserted against a constant
// this file also wrote. Every refusal subtest compares the database file byte for
// byte across the refused call: a read that wrote anything at all - a repair, a
// backfill, an INSERT OR IGNORE - fails even when it committed no logical row.
// No subtest mints an operation id, a run or a context id the server never
// produced, and nothing here weakens a refusal.

const (
	// productionFinalizedCharacterProjectID is the fixture's frozen project. The
	// store fixture has no lease fence, so its scope carries this literal epoch;
	// the served subtest uses the live lease instead and never this constant.
	productionFinalizedCharacterProjectID = "pfc-project-1"
	productionFinalizedCharacterEpoch     = "pfc-epoch-1"
	// productionFinalizedCharacterGeneration is the projection generation both
	// fixtures write and the served answer must echo in sourceOrder.continuityEpoch.
	productionFinalizedCharacterGeneration = 2
	// The frozen character: the receipt's snapshot name differs from the live row
	// on purpose, so the answer proves it projects the snapshot rather than
	// re-reading characters.name.
	productionFinalizedCharacterCharacterID = "pfc-character-1"
	productionFinalizedCharacterLiveName    = "阿青"
	productionFinalizedCharacterFrozenName  = "阿青（冻结）"
	productionFinalizedCharacterAliasOne    = "青妹"
	productionFinalizedCharacterAliasTwo    = "阿青"
	productionFinalizedCharacterLocation    = "山门"
	productionFinalizedCharacterPowerLevel  = "筑基"
	// productionFinalizedCharacterSnapshotKind is store's summarySnapshotKind
	// (internal/store/summary.go:512), the source_key the freeze receipt is filed
	// under and the kind its snapshot must carry.
	productionFinalizedCharacterSnapshotKind = "finalized-character-snapshot:v1"
)

// TestProductionFinalizedCharacterReadContextServesTheFinalizationRead is the
// whole of this lane's claim about the channel: it is served, its answer is the
// store's own FinalizedCharacterContext, and every refusal it can produce is the
// reference's symbolic code with the admitted bytes untouched.
func TestProductionFinalizedCharacterReadContextServesTheFinalizationRead(t *testing.T) {
	t.Run("finalized-character:read-context", testProductionFinalizedCharacterServed)
	t.Run("refuses without a live project lease", testProductionFinalizedCharacterLease)
	t.Run("store: scope required", testProductionFinalizedCharacterScopeRefusal)
	t.Run("store: source changed", testProductionFinalizedCharacterSourceRefusal)
	t.Run("store: id retired", testProductionFinalizedCharacterRetiredRefusal)
	t.Run("store: field revision invalid", testProductionFinalizedCharacterRevisionRefusal)
	t.Run("store: snapshot invalid", testProductionFinalizedCharacterSnapshotRefusal)
	t.Run("store: read transaction required", testProductionFinalizedCharacterTransactionRefusal)
}

// testProductionFinalizedCharacterServed is the happy path over a real admitted
// project with a real baseline-v7 database and a finalized draft.
func testProductionFinalizedCharacterServed(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionFinalizedCharacterSeed)
	projectID := syntheticString(lane.frozen["projectId"])
	leaseID := syntheticString(lane.frozen["leaseId"])
	if projectID == "" || leaseID == "" {
		t.Fatalf("the admitted project handed back no project/lease: %v", lane.frozen)
	}
	before := lane.bytes(t)
	request := []any{map[string]any{"draftId": 1}, lane.frozen}
	var served struct {
		ContextID string         `json:"contextId"`
		Context   map[string]any `json:"context"`
		Origin    *string        `json:"originProjectId"`
	}
	lane.author.decode(t, productionFinalizedCharacterContextName, request, &served)
	// The id is the reference's per-call opaque value (:23): a version 4 UUID, not
	// a name, a draft id or a digest of the context.
	if len(served.ContextID) != 36 || served.ContextID[8] != '-' || served.ContextID[14] != '4' {
		t.Fatalf("contextId %q is not a version 4 UUID", served.ContextID)
	}
	// originProjectId is the transfer origin the main process owns
	// (finalized-character-generation.ts:27). This lane proves no transfer, so the
	// key is absent rather than filled with the browser's own project.
	if served.Origin != nil {
		t.Fatalf("originProjectId = %q, want the absent key: no transfer authority exists here", *served.Origin)
	}
	productionFinalizedCharacterAssertContext(t, served.Context, projectID, leaseID)

	// The answer is the store value, and the store value survives on disk: the
	// same read over a connection that could not have written anything it sees.
	replay, err := store.NewFinalizedCharacters(productionStoryStateWriteReadback(t, lane.projectPath)).
		ReadContext(context.Background(), 1, store.FinalizedCharacterScope{ProjectID: projectID, Epoch: leaseID})
	if err != nil {
		t.Fatalf("mode=ro read-back: %v", err)
	}
	encoded, err := json.Marshal(replay)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, served.Context) {
		t.Fatalf("the served context is not the store's context:\nserved=%v\nstored=%v", served.Context, decoded)
	}

	// A second call mints a second id and reads the same journal, because the id
	// is per call and the context is the database's, not a cache's.
	var again struct {
		ContextID string         `json:"contextId"`
		Context   map[string]any `json:"context"`
	}
	lane.author.decode(t, productionFinalizedCharacterContextName, request, &again)
	if again.ContextID == served.ContextID {
		t.Fatalf("two calls answered the same contextId %q; the reference mints one per call", again.ContextID)
	}
	if !reflect.DeepEqual(again.Context, served.Context) {
		t.Fatalf("a second read changed the context:\nfirst=%v\nsecond=%v", served.Context, again.Context)
	}

	// The reference's request guard (finalized-character-generation.ts:22):
	// !Number.isSafeInteger(draftId) || draftId < 1.
	for _, invalid := range []struct {
		label string
		args  []any
	}{
		{"no draft id", []any{map[string]any{}, lane.frozen}},
		{"zero", []any{map[string]any{"draftId": 0}, lane.frozen}},
		{"negative", []any{map[string]any{"draftId": -1}, lane.frozen}},
		{"fraction", []any{map[string]any{"draftId": 1.5}, lane.frozen}},
		{"text", []any{map[string]any{"draftId": "1"}, lane.frozen}},
		{"not an object", []any{"draft", lane.frozen}},
	} {
		productionFinalizedCharacterRefusal(t, invalid.label,
			lane.author.call(t, productionFinalizedCharacterContextName, invalid.args),
			productionFinalizedCharacterDraftInvalid)
	}
	// The request arity is one, and the frozen context is the second wire argument.
	productionFinalizedCharacterRefusal(t, "no frozen context",
		lane.author.call(t, productionFinalizedCharacterContextName, []any{map[string]any{"draftId": 1}}),
		"INVALID_ARGUMENTS")
	productionFinalizedCharacterRefusal(t, "a context that is not the three frozen keys",
		lane.author.call(t, productionFinalizedCharacterContextName,
			[]any{map[string]any{"draftId": 1}, map[string]any{"projectId": projectID}}),
		"PROJECT_SESSION_REQUIRED")

	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("the served reads changed the admitted database")
	}
}

// productionFinalizedCharacterAssertContext is the answer's contract: the same
// field names, in the same place, as FinalizedCharacterContext
// (src/shared/finalized-continuity.ts:113-124).
func productionFinalizedCharacterAssertContext(t *testing.T, context map[string]any, projectID, leaseID string) {
	t.Helper()
	for key, want := range map[string]any{
		"projectId":            projectID,
		"epoch":                leaseID,
		"content":              productionEditorSeed,
		"projectionGeneration": float64(productionFinalizedCharacterGeneration),
		"identityRevision":     float64(1),
		"identityStatus":       "bound",
	} {
		if got := context[key]; got != want {
			t.Fatalf("context.%s = %v, want %v", key, got, want)
		}
	}
	source := productionFinalizedCharacterObject(t, "context.source", context["source"])
	for key, want := range map[string]any{
		"draftId":        float64(1),
		"finalizationId": productionGenerationWriteFinalizationID,
		"chapterNumber":  float64(1),
		"contentHash":    sha256Hex(productionEditorSeed),
	} {
		if got := source[key]; got != want {
			t.Fatalf("context.source.%s = %v, want %v", key, got, want)
		}
	}
	order := productionFinalizedCharacterObject(t, "context.sourceOrder", context["sourceOrder"])
	for key, want := range map[string]any{
		"continuityEpoch":                   projectID + ":2",
		"chapterNumber":                     float64(1),
		"authoritativeFinalizationRevision": float64(1),
	} {
		if got := order[key]; got != want {
			t.Fatalf("context.sourceOrder.%s = %v, want %v", key, got, want)
		}
	}
	characters := productionFinalizedCharacterList(t, "context.characters", context["characters"])
	if len(characters) != 1 {
		t.Fatalf("context.characters carries %d entries, want the one frozen character", len(characters))
	}
	character := productionFinalizedCharacterObject(t, "context.characters[0]", characters[0])
	if character["characterId"] != productionFinalizedCharacterCharacterID {
		t.Fatalf("characterId = %v, want %q", character["characterId"], productionFinalizedCharacterCharacterID)
	}
	if character["displayNameSnapshot"] != productionFinalizedCharacterFrozenName {
		t.Fatalf("displayNameSnapshot = %v, want the frozen %q",
			character["displayNameSnapshot"], productionFinalizedCharacterFrozenName)
	}
	aliases := productionFinalizedCharacterList(t, "aliases", character["aliases"])
	if !reflect.DeepEqual(aliases, []any{productionFinalizedCharacterAliasOne, productionFinalizedCharacterAliasTwo}) {
		t.Fatalf("aliases = %v", aliases)
	}
	// The six state fields in characterStateFields order
	// (internal/store/characters.go:31): the same set and order the reference's
	// CHARACTER_STATE_TEXT_FIELDS carries.
	fields := productionFinalizedCharacterList(t, "fields", character["fields"])
	wantFields := []string{"location", "powerLevel", "physicalState", "mentalState", "keyItems", "recentEvents"}
	if len(fields) != len(wantFields) {
		t.Fatalf("fields carries %d entries, want %d", len(fields), len(wantFields))
	}
	for index, want := range wantFields {
		field := productionFinalizedCharacterObject(t, fmt.Sprintf("fields[%d]", index), fields[index])
		if field["field"] != want {
			t.Fatalf("fields[%d].field = %v, want %q", index, field["field"], want)
		}
		// Every snapshot is scoped by the admitted project epoch, never by a value
		// the browser sent.
		if field["projectId"] != projectID || field["epoch"] != leaseID {
			t.Fatalf("fields[%d] is scoped %v/%v, want %q/%q", index, field["projectId"], field["epoch"], projectID, leaseID)
		}
		if field["characterId"] != productionFinalizedCharacterCharacterID {
			t.Fatalf("fields[%d].characterId = %v", index, field["characterId"])
		}
		if field["valueHash"] != sha256Hex(productionFinalizedCharacterText(field["value"])) {
			t.Fatalf("fields[%d].valueHash does not hash its own value", index)
		}
	}
	location := productionFinalizedCharacterObject(t, "fields[0]", fields[0])
	if location["value"] != productionFinalizedCharacterLocation || location["revision"] != float64(0) {
		t.Fatalf("location = %v at revision %v", location["value"], location["revision"])
	}
	provenance := productionFinalizedCharacterObject(t, "location.provenance", location["provenance"])
	if provenance["kind"] != "author" || provenance["chapterNumber"] != float64(1) {
		t.Fatalf("location.provenance = %v, want the author arm of chapter 1", provenance)
	}
	power := productionFinalizedCharacterObject(t, "fields[1]", fields[1])
	if power["value"] != productionFinalizedCharacterPowerLevel || power["revision"] != float64(2) {
		t.Fatalf("powerLevel = %v at revision %v", power["value"], power["revision"])
	}
	derived := productionFinalizedCharacterObject(t, "powerLevel.sourceOrder", power["sourceOrder"])
	if derived["continuityEpoch"] != projectID+":2" || derived["authoritativeFinalizationRevision"] != float64(1) {
		t.Fatalf("powerLevel.sourceOrder = %v", derived)
	}
}

// testProductionFinalizedCharacterLease proves the operation is refused without a
// live project lease, and that the fixture itself is not what refused.
func testProductionFinalizedCharacterLease(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionFinalizedCharacterSeed)
	before := lane.bytes(t)
	request := map[string]any{"draftId": 1}
	// A lease this session never held, and a context with no lease at all: both
	// are refused by the admission (production_durable_linux.go:313-360) before a
	// row is read, and neither may answer 200 or hand back a context.
	productionEditorRefused(t, lane.author, "stale lease", productionFinalizedCharacterContextName,
		[]any{request, lane.stale()})
	productionEditorRefused(t, lane.author, "no lease", productionFinalizedCharacterContextName,
		[]any{request, map[string]any{"projectId": lane.frozen["projectId"], "projectPath": lane.projectPath}})
	// The same fixture under its live lease still answers, so the two refusals
	// above are the lease's and not a fixture that never had a readable draft.
	var served struct {
		Context map[string]any `json:"context"`
	}
	lane.author.decode(t, productionFinalizedCharacterContextName, []any{request, lane.frozen}, &served)
	if len(served.Context) == 0 {
		t.Fatal("the live lease answered no context")
	}
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("a refused read changed the admitted database")
	}
}

// testProductionFinalizedCharacterScopeRefusal is summary-repository.ts:402 over
// all three arms: a blank project, a blank epoch and no database at all.
func testProductionFinalizedCharacterScopeRefusal(t *testing.T) {
	fixture := productionFinalizedCharacterProvision(t)
	fixture.bound(t)
	for _, scope := range []store.FinalizedCharacterScope{
		{},
		{ProjectID: productionFinalizedCharacterProjectID},
		{ProjectID: "   ", Epoch: productionFinalizedCharacterEpoch},
		{ProjectID: productionFinalizedCharacterProjectID, Epoch: "\t"},
	} {
		fixture.refused(t, fmt.Sprintf("scope %q/%q", scope.ProjectID, scope.Epoch),
			store.ErrFinalizedCharacterScopeRequired, func() (map[string]any, error) {
				return productionFinalizedCharacterReadContext(context.Background(), fixture.db, 1, scope)
			})
	}
	// The reference's first arm is `!database` (:402), and the store answers it
	// with the same refusal instead of an empty context.
	fixture.refused(t, "no database", store.ErrFinalizedCharacterScopeRequired,
		func() (map[string]any, error) {
			return productionFinalizedCharacterReadContext(context.Background(), nil, 1,
				productionFinalizedCharacterScope())
		})
}

// testProductionFinalizedCharacterSourceRefusal is summary-repository.ts:406 and
// :409: the draft is not finalized, and the draft is finalized but a newer
// finalized draft owns the chapter.
func testProductionFinalizedCharacterSourceRefusal(t *testing.T) {
	open := productionFinalizedCharacterProvision(t)
	productionFinalizedCharacterInsertDraft(t, open.db, 1, 1, 1, "draft", productionGenerationWriteFinalizationID)
	open.refused(t, "the draft is not finalized", store.ErrFinalizedCharacterSourceChanged, open.read)

	superseded := productionFinalizedCharacterProvision(t)
	source := productionFinalizedCharacterInsertDraft(t, superseded.db, 1, 1, 1, "finalized",
		productionGenerationWriteFinalizationID)
	productionFinalizedCharacterInsertCharacter(t, superseded.db, productionFinalizedCharacterLiveName, 0,
		productionFinalizedCharacterProvenanceJSON(source, 0))
	productionFinalizedCharacterInsertFreeze(t, superseded.db, source, 1)
	productionFinalizedCharacterInsertDraft(t, superseded.db, 2, 1, 2, "finalized", "")
	superseded.refused(t, "a newer finalized draft owns the chapter",
		store.ErrFinalizedCharacterSourceChanged, superseded.read)
}

// testProductionFinalizedCharacterRetiredRefusal is summary-repository.ts:413: a
// frozen character whose live row is gone is a retired id, not an empty card.
func testProductionFinalizedCharacterRetiredRefusal(t *testing.T) {
	fixture := productionFinalizedCharacterProvision(t)
	source := productionFinalizedCharacterInsertDraft(t, fixture.db, 1, 1, 1, "finalized",
		productionGenerationWriteFinalizationID)
	productionFinalizedCharacterInsertCharacter(t, fixture.db, productionFinalizedCharacterLiveName, 1,
		productionFinalizedCharacterProvenanceJSON(source, 0))
	productionFinalizedCharacterInsertFreeze(t, fixture.db, source, 1)
	fixture.refused(t, "the frozen character is retired", store.ErrFinalizedCharacterIDRetired, fixture.read)
}

// testProductionFinalizedCharacterRevisionRefusal is summary-repository.ts:84: a
// stored field revision that is not a safe integer refuses the whole read instead
// of defaulting to zero.
func testProductionFinalizedCharacterRevisionRefusal(t *testing.T) {
	fixture := productionFinalizedCharacterProvision(t)
	source := productionFinalizedCharacterInsertDraft(t, fixture.db, 1, 1, 1, "finalized",
		productionGenerationWriteFinalizationID)
	productionFinalizedCharacterInsertCharacter(t, fixture.db, productionFinalizedCharacterLiveName, 0,
		productionFinalizedCharacterProvenanceJSON(source, -1))
	productionFinalizedCharacterInsertFreeze(t, fixture.db, source, 1)
	fixture.refused(t, "the stored field revision is negative",
		store.ErrFinalizedCharacterFieldRevisionInvalid, fixture.read)
}

// testProductionFinalizedCharacterSnapshotRefusal is summary-repository.ts:75: a
// freeze receipt that does not hash to its own bytes is refused, never repaired.
func testProductionFinalizedCharacterSnapshotRefusal(t *testing.T) {
	fixture := productionFinalizedCharacterProvision(t)
	source := fixture.bound(t)
	snapshot := productionFinalizedCharacterSnapshotJSON(source, 1)
	productionFinalizedCharacterInsertFreezeRaw(t, fixture.db, source.FinalizationID, snapshot,
		sha256Hex(snapshot+" "))
	fixture.refused(t, "the receipt does not hash to its bytes",
		store.ErrSummaryCharacterSnapshotInvalid, fixture.read)
}

// testProductionFinalizedCharacterTransactionRefusal covers the one store refusal
// this adapter never produces: ReadFinalizedCharacterContextTx with no transaction
// (finalized_character_context.go:210-213). ReadContext opens its own read-only
// transaction, so the adapter cannot reach it - the mapping is still proved, so a
// future variant that nests transactions cannot leak an unmapped error.
func testProductionFinalizedCharacterTransactionRefusal(t *testing.T) {
	fixture := productionFinalizedCharacterProvision(t)
	fixture.bound(t)
	fixture.refused(t, "no transaction", store.ErrFinalizedCharacterReadTransactionRequired,
		func() (map[string]any, error) {
			_, err := store.ReadFinalizedCharacterContextTx(context.Background(), nil, 1,
				productionFinalizedCharacterScope())
			return nil, err
		})
}

// productionFinalizedCharacterScope is the store fixture's admitted scope. There
// is no lease fence over a pinned baseline, so the epoch is the fixture's literal;
// the served subtest never uses this and takes the live lease instead.
func productionFinalizedCharacterScope() store.FinalizedCharacterScope {
	return store.FinalizedCharacterScope{ProjectID: productionFinalizedCharacterProjectID,
		Epoch: productionFinalizedCharacterEpoch}
}

// productionFinalizedCharacterFixture is one disposable pinned-baseline database.
type productionFinalizedCharacterFixture struct {
	path string
	db   *sql.DB
}

// productionFinalizedCharacterProvision stamps the pinned v7 baseline into a
// fresh file (internal/admission/baseline_v7_ddl.go:744) and opens it the way a
// project database is opened. The projection and identity singletons are written
// explicitly: the pinned baseline creates the tables and no rows, and the read
// joins continuity_projection_meta and the freeze receipt.
func productionFinalizedCharacterProvision(t *testing.T) *productionFinalizedCharacterFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "finalized-character-context.db")
	seed := productionFinalizedCharacterOpen(t, path, "mode=rwc")
	if err := admission.WriteBaselineV7(path, seed); err != nil {
		_ = seed.Close()
		t.Fatalf("provision baseline v7: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	dsn, err := store.ExistingDSN(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = db.Close() })
	fixture := &productionFinalizedCharacterFixture{path: path, db: db}
	productionFinalizedCharacterExec(t, db, "INSERT OR IGNORE INTO continuity_projection_meta(id,generation) VALUES('main',?)",
		productionFinalizedCharacterGeneration)
	productionFinalizedCharacterExec(t, db, "UPDATE continuity_projection_meta SET generation=? WHERE id='main'",
		productionFinalizedCharacterGeneration)
	productionFinalizedCharacterExec(t, db, "INSERT INTO character_identity_meta(id,revision,legacy_count) VALUES('main',3,0)")
	return fixture
}

// bound seeds the happy-path arm and returns the frozen source identity.
func (f *productionFinalizedCharacterFixture) bound(t *testing.T) store.FinalizedCharacterSourceIdentity {
	t.Helper()
	source := productionFinalizedCharacterInsertDraft(t, f.db, 1, 1, 1, "finalized",
		productionGenerationWriteFinalizationID)
	productionFinalizedCharacterInsertCharacter(t, f.db, productionFinalizedCharacterLiveName, 0,
		productionFinalizedCharacterProvenanceJSON(source, 0))
	productionFinalizedCharacterInsertFreeze(t, f.db, source, 1)
	return source
}

// read is the adapter's own read: the same function the handler runs once the
// admission has handed it a database and the admitted scope.
func (f *productionFinalizedCharacterFixture) read() (map[string]any, error) {
	return productionFinalizedCharacterReadContext(context.Background(), f.db, 1,
		productionFinalizedCharacterScope())
}

// bytes is the whole file: a read - refused or not - must leave every byte alone.
func (f *productionFinalizedCharacterFixture) bytes(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// refused asserts one refusal three times over: the store's own value, the
// symbolic code this lane publishes for it, and the bytes it did not touch.
func (f *productionFinalizedCharacterFixture) refused(t *testing.T, label string, want error,
	read func() (map[string]any, error)) {
	t.Helper()
	before := f.bytes(t)
	answer, err := read()
	if err == nil {
		t.Fatalf("%s: the read was served: %v", label, answer)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s: refused with %v, want %v", label, err, want)
	}
	if answer != nil {
		t.Fatalf("%s: a refusal carried an answer: %v", label, answer)
	}
	// The code the browser receives is the store value's own string: the table in
	// the binder is the only place the mapping lives, and this is where a drifted
	// constant is caught.
	if code := productionFinalizedCharacterReadCode(err); code != want.Error() {
		t.Fatalf("%s: mapped to %q, want %q", label, code, want.Error())
	}
	if published := productionFinalizedCharacterReadFailure(err); published == nil || published.Error() != want.Error() {
		t.Fatalf("%s: published %v, want %q", label, published, want.Error())
	}
	if after := f.bytes(t); !bytes.Equal(before, after) {
		t.Fatalf("%s: the refused read changed the database", label)
	}
}

// productionFinalizedCharacterSeed writes the served fixture's context rows into
// an admitted project, before the lane admits it. The finalized draft and its
// outbox receipt are the neighbouring generation-write fixture's rows.
func productionFinalizedCharacterSeed(t *testing.T, projectPath string) {
	t.Helper()
	productionGenerationWriteSeedOutbox(t, projectPath)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	db := productionFinalizedCharacterOpen(t, database, "mode=rw")
	source := store.FinalizedCharacterSourceIdentity{DraftID: 1, ChapterNumber: 1,
		FinalizationID: productionGenerationWriteFinalizationID, ContentHash: sha256Hex(productionEditorSeed)}
	productionFinalizedCharacterExec(t, db, "INSERT OR IGNORE INTO continuity_projection_meta(id,generation) VALUES('main',?)",
		productionFinalizedCharacterGeneration)
	productionFinalizedCharacterExec(t, db, "UPDATE continuity_projection_meta SET generation=? WHERE id='main'",
		productionFinalizedCharacterGeneration)
	productionFinalizedCharacterInsertCharacter(t, db, productionFinalizedCharacterLiveName, 0,
		productionFinalizedCharacterProvenanceJSON(source, 0))
	productionFinalizedCharacterInsertFreeze(t, db, source, 1)
	// The lane admits the file itself, so the seeded rows must be checkpointed into
	// it: no sidecar may carry rows the admitted digest does not cover.
	productionEditorCheckpoint(t, db, database)
}

// productionFinalizedCharacterOpen opens one database file with the given mode
// prefix and the pragmas the shipped DSNs use.
func productionFinalizedCharacterOpen(t *testing.T, database, mode string) *sql.DB {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = mode + "&_busy_timeout=5000&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// productionFinalizedCharacterExec is one seeded statement.
func productionFinalizedCharacterExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("seed %q: %v", query, err)
	}
}

// productionFinalizedCharacterInsertDraft writes one draft, its content and -
// when finalizationID is not empty - the immutable receipt the read joins.
func productionFinalizedCharacterInsertDraft(t *testing.T, db *sql.DB, id, chapter, version int64,
	status, finalizationID string) store.FinalizedCharacterSourceIdentity {
	t.Helper()
	productionFinalizedCharacterExec(t, db, "INSERT INTO contents(id,body) VALUES(?,?)", id, productionEditorSeed)
	productionFinalizedCharacterExec(t, db, `INSERT INTO drafts(id,chapter_number,version,status,content_id,word_count)
 VALUES(?,?,?,?,?,?)`, id, chapter, version, status, id, len(productionEditorSeed))
	source := store.FinalizedCharacterSourceIdentity{DraftID: id, ChapterNumber: chapter,
		FinalizationID: finalizationID, ContentHash: sha256Hex(productionEditorSeed)}
	if finalizationID == "" {
		return source
	}
	productionFinalizedCharacterExec(t, db, `INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,
 chapter_title,content_hash,content_revision,content_snapshot,target_file_name)
 VALUES(?,?,?,?,?,1,?,?)`, finalizationID, id, chapter, productionGenerationWriteFinalTitle,
		source.ContentHash, productionEditorSeed, productionGenerationWriteSeedTargetFile)
	return source
}

// productionFinalizedCharacterInsertCharacter writes one live character row with
// its field-level provenance. The schema defaults (identity_revision 0, retired 0)
// are kept unless the caller retires it, so the admission's identity invariant
// still holds.
func productionFinalizedCharacterInsertCharacter(t *testing.T, db *sql.DB, name string, retired int64, provenance string) {
	t.Helper()
	productionFinalizedCharacterExec(t, db, `INSERT INTO characters(character_id,name,retired,
 cs_updated_at_chapter,cs_location,cs_power_level,cs_provenance)
 VALUES(?,?,?,1,?,?,?)`, productionFinalizedCharacterCharacterID, name, retired,
		productionFinalizedCharacterLocation, productionFinalizedCharacterPowerLevel, provenance)
}

// productionFinalizedCharacterInsertFreeze files one freeze receipt whose hash is
// its own bytes, the shape the sole freeze writer produces.
func productionFinalizedCharacterInsertFreeze(t *testing.T, db *sql.DB,
	source store.FinalizedCharacterSourceIdentity, revision int64) {
	t.Helper()
	snapshot := productionFinalizedCharacterSnapshotJSON(source, revision)
	productionFinalizedCharacterInsertFreezeRaw(t, db, source.FinalizationID, snapshot, sha256Hex(snapshot))
}

// productionFinalizedCharacterInsertFreezeRaw files one freeze receipt verbatim,
// so a subtest can store bytes that do not hash to the recorded digest.
func productionFinalizedCharacterInsertFreezeRaw(t *testing.T, db *sql.DB, finalizationID, snapshot, hash string) {
	t.Helper()
	productionFinalizedCharacterExec(t, db, `INSERT OR REPLACE INTO character_identity_proposals(
 proposal_id,source_key,source_hash,raw_value,candidate_ids_json) VALUES(?,?,?,?,'[]')`,
		"fcs:"+finalizationID, productionFinalizedCharacterSnapshotKind, hash, snapshot)
}

// productionFinalizedCharacterSnapshotJSON is the freeze receipt's snapshot
// (finalization_outbox.go:340): kind, source, identityRevision and the frozen
// characters, whose snapshot name is deliberately not the live row's name.
func productionFinalizedCharacterSnapshotJSON(source store.FinalizedCharacterSourceIdentity, revision int64) string {
	return fmt.Sprintf(`{"kind":%q,"source":{"draftId":%d,"finalizationId":%q,"chapterNumber":%d,"contentHash":%q},`+
		`"identityRevision":%d,"characters":[{"characterId":%q,"displayNameSnapshot":%q,"aliases":[%q,%q]}]}`,
		productionFinalizedCharacterSnapshotKind, source.DraftID, source.FinalizationID, source.ChapterNumber,
		source.ContentHash, revision, productionFinalizedCharacterCharacterID,
		productionFinalizedCharacterFrozenName, productionFinalizedCharacterAliasOne, productionFinalizedCharacterAliasTwo)
}

// productionFinalizedCharacterProvenanceJSON is the field-level provenance the
// character row carries: one author field and one derived field with the source
// order the commit compares. A negative locationRevision writes the reference's
// own invalid revision (:84) instead.
func productionFinalizedCharacterProvenanceJSON(source store.FinalizedCharacterSourceIdentity, locationRevision int64) string {
	if locationRevision < 0 {
		return fmt.Sprintf(`{"location":{"kind":"author","chapterNumber":%d,"revision":-1}}`, source.ChapterNumber)
	}
	return fmt.Sprintf(`{"location":{"kind":"author","chapterNumber":%d},`+
		`"powerLevel":{"kind":"derived","revision":2,`+
		`"source":{"draftId":%d,"finalizationId":%q,"chapterNumber":%d,"contentHash":%q},`+
		`"sourceOrder":{"continuityEpoch":%q,"chapterNumber":%d,"authoritativeFinalizationRevision":1}}}`,
		source.ChapterNumber, source.DraftID, source.FinalizationID, source.ChapterNumber, source.ContentHash,
		fmt.Sprintf("%s:%d", productionFinalizedCharacterProjectID, productionFinalizedCharacterGeneration),
		source.ChapterNumber)
}

// productionFinalizedCharacterRefusal asserts one served refusal over HTTP: never
// 200, never the fail-closed 501, and the expected symbolic code.
func productionFinalizedCharacterRefusal(t *testing.T, label string, response *httptest.ResponseRecorder, want string) {
	t.Helper()
	if response.Code == http.StatusOK {
		t.Fatalf("%s answered 200 instead of refusing: %s", label, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v %s", label, err, response.Body.String())
	}
	if body.Error.Code == "GO_OPERATION_NOT_MIGRATED" {
		t.Fatalf("%s is unbound in the production lane: %s", label, response.Body.String())
	}
	if want != "" && body.Error.Code != want {
		t.Fatalf("%s refused with %q, want %q: %s", label, body.Error.Code, want, response.Body.String())
	}
}

// productionFinalizedCharacterObject decodes one JSON object of the answer.
func productionFinalizedCharacterObject(t *testing.T, label string, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, not an object: %v", label, value, value)
	}
	return object
}

// productionFinalizedCharacterList decodes one JSON array of the answer.
func productionFinalizedCharacterList(t *testing.T, label string, value any) []any {
	t.Helper()
	list, ok := value.([]any)
	if !ok {
		t.Fatalf("%s is %T, not a list: %v", label, value, value)
	}
	return list
}

// productionFinalizedCharacterText reads one snapshot value, which is always a
// string in this answer.
func productionFinalizedCharacterText(value any) string {
	text, _ := value.(string)
	return text
}
