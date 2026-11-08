//go:build linux

package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// TestProductionLegacyRosterAdoptsExistingCards is the whole point of the one
// write this family serves: an author-owned, preserved-and-inconsistent roster is
// adopted into the identity tables with its approval receipt, in one transaction.
// It also pins the two refusals that must leave the database byte-identical, and
// the reference's idempotent-or-conflict replay decision.
func TestProductionLegacyRosterAdoptsExistingCards(t *testing.T) {
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	// The editor-read fixture already seeds one active card (ci-character-1,
	// protagonist). The adoption only needs the preserved-and-inconsistent roster
	// state and an identity revision, so the source re-proof has something to CAS.
	productionLegacyRosterSeed(t, database, []productionLegacyRosterStatement{
		{Query: "INSERT OR REPLACE INTO character_identity_meta (id, revision, legacy_count) VALUES ('main', 3, 0)"},
		{Query: `INSERT OR REPLACE INTO character_roster_meta
 (id, schema_version, revision, migration_state, legacy_markdown, projection_hash, fact_hash)
 VALUES ('main', 1, 9, 'legacy_cards_preserved', ?, '', '')`,
			Args: []any{productionLegacyRosterPreservedText}},
	})
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionDurableTab)
	frozen := productionLegacyRosterOpen(t, author, projectPath)
	source := productionLegacyRosterRead(t, author, frozen)
	revision, ok := syntheticFloat64(source.Snapshot["revision"])
	if !ok {
		t.Fatalf("snapshot revision is not numeric: %v", source.Snapshot["revision"])
	}
	request := map[string]any{
		"operationId":              "legacy-roster-adoption-1",
		"expectedRevision":         revision,
		"expectedLegacyHash":       source.LegacyHash,
		"expectedIdentityRevision": source.IdentityRevision,
		"expectedFactsHash":        source.FactsHash,
	}

	before := productionGraphReadsBytes(t, database)

	t.Run("a foreign project and a stale lease are refused without writing", func(t *testing.T) {
		foreign := map[string]any{"projectId": "foreign-project-id",
			"projectPath": filepath.Join(root, "not-admitted"), "leaseId": frozen["leaseId"]}
		productionEditorRefused(t, author, "foreign project", productionLegacyRosterAdoptExistingName,
			[]any{request, foreign})
		productionGraphReadsUnchanged(t, database, before, productionLegacyRosterAdoptExistingName)

		stale := map[string]any{"projectId": frozen["projectId"], "projectPath": projectPath,
			"leaseId": productionLegacyRosterStaleLease}
		productionEditorRefused(t, author, "stale lease", productionLegacyRosterAdoptExistingName,
			[]any{request, stale})
		productionGraphReadsUnchanged(t, database, before, productionLegacyRosterAdoptExistingName)
	})

	t.Run("the author-owned card is adopted with the approval receipt", func(t *testing.T) {
		receipt := productionLegacyRosterAdoptReceipt(t, author, []any{request, frozen})
		if !receipt.Success || receipt.OperationID != "legacy-roster-adoption-1" {
			t.Fatalf("adoption receipt is wrong: %+v", receipt)
		}
		if len(receipt.ActiveIDs) != 1 || receipt.ActiveIDs[0] != "ci-character-1" {
			t.Fatalf("adoption activeIds is %v, want the one active card", receipt.ActiveIDs)
		}
		if state := syntheticString(receipt.Snapshot["status"]); state != "ready" {
			t.Fatalf("adoption snapshot status is %s, want ready", state)
		}
		if state := syntheticString(receipt.Snapshot["migrationState"]); state != "ready" {
			t.Fatalf("adoption snapshot migrationState is %s, want ready", state)
		}
		// Read back over a separate mode=ro connection, never the lane's handle:
		// the approval row, the projection and the roster meta must all be the
		// rows this transaction wrote.
		receiptJSON, arch, migration := productionLegacyRosterAdoptReadback(t, database, "legacy-cards-adoption:legacy-roster-adoption-1")
		var envelope map[string]any
		if err := json.Unmarshal([]byte(receiptJSON), &envelope); err != nil {
			t.Fatalf("the stored approval receipt is not JSON: %v", err)
		}
		if envelope["kind"] != "legacy-cards-adoption" || envelope["version"] != float64(1) {
			t.Fatalf("the stored approval receipt is not the adoption envelope: %v", envelope)
		}
		if arch == "" {
			t.Fatal("the projection was not written to project_core.characters_arch")
		}
		if migration != "ready" {
			t.Fatalf("character_roster_meta.migration_state is %s, want ready", migration)
		}
	})

	t.Run("a replay answers the same receipt and a different request conflicts", func(t *testing.T) {
		first := productionLegacyRosterAdoptResult(t, author, []any{request, frozen})
		second := productionLegacyRosterAdoptResult(t, author, []any{request, frozen})
		if first != second {
			t.Fatalf("the idempotent replay answered different bytes:\n%s\n%s", first, second)
		}
		// The reference's decision line (legacy-roster-source.ts:88):
		//   if (requestHash !== saved.requestHash || !isDeepStrictEqual(original, saved.request))
		// A different payload under the same operation id is a nonce conflict, not
		// a second adoption.
		conflict := map[string]any{
			"operationId":              "legacy-roster-adoption-1",
			"expectedRevision":         revision + 1,
			"expectedLegacyHash":       source.LegacyHash,
			"expectedIdentityRevision": source.IdentityRevision,
			"expectedFactsHash":        source.FactsHash,
		}
		productionLegacyRosterAdoptRefused(t, author, []any{conflict, frozen}, "LEGACY_ROSTER_ADOPTION_NONCE_CONFLICT")
	})
}

// productionLegacyRosterAdoptReceiptWire is AdoptLegacyCardsReceipt
// (electron/services/legacy-roster-source.ts:27-32) as it arrives on the wire.
type productionLegacyRosterAdoptReceiptWire struct {
	Success     bool           `json:"success"`
	OperationID string         `json:"operationId"`
	Snapshot    map[string]any `json:"snapshot"`
	ActiveIDs   []string       `json:"activeIds"`
}

// productionLegacyRosterAdoptResult is the served answer as the bytes it arrived
// as, so an idempotent replay can be compared without a decode that would forgive
// a changed key order.
func productionLegacyRosterAdoptResult(t *testing.T, author *productionDurableClient, args []any) string {
	t.Helper()
	response := author.call(t, productionLegacyRosterAdoptExistingName, args)
	if response.Code != http.StatusOK {
		t.Fatalf("%s answered %d %s", productionLegacyRosterAdoptExistingName, response.Code, response.Body.String())
	}
	var wire struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || len(wire.Result) == 0 {
		t.Fatalf("%s: %v %s", productionLegacyRosterAdoptExistingName, err, response.Body.String())
	}
	return string(wire.Result)
}

func productionLegacyRosterAdoptReceipt(t *testing.T, author *productionDurableClient, args []any) productionLegacyRosterAdoptReceiptWire {
	t.Helper()
	var receipt productionLegacyRosterAdoptReceiptWire
	if err := json.Unmarshal([]byte(productionLegacyRosterAdoptResult(t, author, args)), &receipt); err != nil {
		t.Fatalf("%s: %v", productionLegacyRosterAdoptExistingName, err)
	}
	return receipt
}

// productionLegacyRosterAdoptRefused asserts one symbolic refusal of the served
// adoption name.
func productionLegacyRosterAdoptRefused(t *testing.T, author *productionDurableClient, args []any, code string) {
	t.Helper()
	response := author.call(t, productionLegacyRosterAdoptExistingName, args)
	if response.Code == http.StatusOK {
		t.Fatalf("%s answered 200 for a refused adoption: %s", productionLegacyRosterAdoptExistingName, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v %s", productionLegacyRosterAdoptExistingName, err, response.Body.String())
	}
	if body.Error.Code != code {
		t.Fatalf("%s refused with %s, want %s: %s", productionLegacyRosterAdoptExistingName,
			body.Error.Code, code, response.Body.String())
	}
}

// productionLegacyRosterAdoptReadback reads the persisted adoption over a
// separate mode=ro connection, so the receipt, the projection and the roster meta
// are asserted against the file the transaction wrote, not the lane's answer.
func productionLegacyRosterAdoptReadback(t *testing.T, database, operationID string) (string, string, string) {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatalf("open the author database read-only: %v", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	var receiptJSON, charactersArch, migrationState string
	if err := db.QueryRow("SELECT receipt_json FROM character_identity_approvals WHERE operation_id=?", operationID).Scan(&receiptJSON); err != nil {
		t.Fatalf("mode=ro approval read-back: %v", err)
	}
	if err := db.QueryRow("SELECT COALESCE(characters_arch,'') FROM project_core WHERE id='main'").Scan(&charactersArch); err != nil {
		t.Fatalf("mode=ro projection read-back: %v", err)
	}
	if err := db.QueryRow("SELECT migration_state FROM character_roster_meta WHERE id='main'").Scan(&migrationState); err != nil {
		t.Fatalf("mode=ro roster-meta read-back: %v", err)
	}
	return receiptJSON, charactersArch, migrationState
}
