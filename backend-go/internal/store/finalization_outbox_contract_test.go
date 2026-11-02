package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Ignore wall-clock values only. Published null/non-null remains contractual;
// immutable source text/hash, filenames, errors and character receipt bytes do not
// get normalized away.
func foContractValue(value any) any {
	switch value := value.(type) {
	case []byte:
		data := make([]int, len(value)); for i, b := range value { data[i] = int(b) }
		return map[string]any{"type": "Buffer", "data": data}
	case map[string]any:
		if value == nil { return nil }
		out := map[string]any{}
		for key, child := range value {
			if key == "created_at" || key == "updated_at" || key == "createdAt" || key == "updatedAt" { continue }
			if key == "published_at" || key == "publishedAt" { if child == nil { out[key] = nil } else { out[key] = "<published>" }; continue }
			out[key] = foContractValue(child)
		}
		return out
	case []map[string]any:
		out := make([]any, len(value)); for i, child := range value { out[i] = foContractValue(child) }; return out
	case []any:
		out := make([]any, len(value)); for i, child := range value { out[i] = foContractValue(child) }; return out
	default: return value
	}
}

func TestOriginalFinalizationOutboxContracts(t *testing.T) {
	contract := os.Getenv("GO_FINALIZATION_OUTBOX_CONTRACT")
	if contract == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" { t.Fatal("GO_FINALIZATION_OUTBOX_CONTRACT actual original repository oracle mandatory") }
		t.Skip("GitHub-only recursive original finalization repository oracle")
	}
	data, err := os.ReadFile(contract)
	if err != nil { t.Fatal(err) }
	var oracle struct {
		FormatVersion int `json:"formatVersion"`
		Cases []struct {
			Name string `json:"name"`
			Database string `json:"database"`
			Method string `json:"method"`
			Input FinalizationCommitInput `json:"input"`
			Result any `json:"result"`
			Error string `json:"error"`
			After map[string]any `json:"after"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &oracle); err != nil { t.Fatal(err) }
	if oracle.FormatVersion != 1 || len(oracle.Cases) < 35 { t.Fatal("incomplete actual original finalization oracle", len(oracle.Cases)) }
	normalize := func(value any) any {
		encoded, err := json.Marshal(value)
		if err != nil { t.Fatal(err) }
		var parsed any
		if err := json.Unmarshal(encoded, &parsed); err != nil { t.Fatal(err) }
		return foContractValue(parsed)
	}
	seen := map[string]bool{}
	for _, fixture := range oracle.Cases {
		if seen[fixture.Name] { t.Fatal("duplicate oracle case", fixture.Name) }; seen[fixture.Name] = true
		t.Run(fixture.Name, func(t *testing.T) {
			original, err := os.ReadFile(fixture.Database)
			if err != nil { t.Fatal(err) }
			private := filepath.Join(t.TempDir(), "private.db")
			if err := os.WriteFile(private, original, 0600); err != nil { t.Fatal(err) }
			dsn, err := ExistingDSN(private)
			if err != nil { t.Fatal(err) }
			db, err := sql.Open("sqlite3", dsn)
			if err != nil { t.Fatal(err) }
			defer db.Close()
			// Ownership is an explicitly TEST-ONLY injected capability. This is not
			// an oracle for a missing handler owner, publisher or generation proof.
			proof := new(int)
			owner := foTestVerifier(func(_ context.Context, _ *sql.Conn, scope FinalizationScope, _ FinalizationMutation, actual any) error {
				if scope != foTestScope || actual != proof { return ErrFinalizationOwnerRequired }
				return nil
			})
			r := NewFinalizationOutbox(db, foTestScope, owner)
			ctx := context.Background()
			var result *FinalizationRecord
			var opErr error
			switch fixture.Method {
			case "Commit": result, opErr = r.CommitWithOwnerProof(ctx, fixture.Input, proof)
			case "Get": result, opErr = r.Get(ctx, fixture.Input.FinalizationID)
			case "GetByDraftID": result, opErr = r.GetByDraftID(ctx, fixture.Input.DraftID)
			case "MarkPending": result, opErr = r.MarkPublicationPendingWithOwnerProof(ctx, fixture.Input.FinalizationID, "Synthetic publication failure 中文", proof)
			case "MarkPublished": result, opErr = r.MarkPublishedWithOwnerProof(ctx, fixture.Input.FinalizationID, proof)
			default: t.Fatal("unknown actual oracle method", fixture.Method)
			}
			code := ""; if opErr != nil { code = opErr.Error() }
			if code != fixture.Error { t.Fatal("error Go/TS", code, fixture.Error) }
			if !reflect.DeepEqual(normalize(result), normalize(fixture.Result)) { t.Fatalf("result Go=%#v TS=%#v", normalize(result), normalize(fixture.Result)) }
			after := map[string]any{}
			allowed := map[string]bool{"contents": true, "drafts": true, "finalization_outbox": true, "continuity_projection_meta": true, "character_identity_meta": true, "character_identity_proposals": true, "characters": true, "character_aliases": true, "reviews": true, "revisions": true}
			if len(fixture.After) != len(allowed) { t.Fatal("incomplete original side-effect snapshot") }
			for table := range fixture.After {
				if !allowed[table] { t.Fatal("unexpected oracle table", table) }
				rows, err := rrMaps(ctx, db, "SELECT * FROM "+table+" ORDER BY rowid")
				if err != nil { t.Fatal(err) }
				after[table] = rows
			}
			// Convert SQLite BLOBs before JSON serialization to avoid []byte base64.
			if !reflect.DeepEqual(normalize(foContractValue(after)), normalize(fixture.After)) { t.Fatalf("database effects differ\nGo=%#v\nTS=%#v", normalize(foContractValue(after)), normalize(fixture.After)) }
			still, err := os.ReadFile(fixture.Database)
			if err != nil || !bytes.Equal(original, still) { t.Fatal("original oracle clone modified", err) }
		})
	}
}
