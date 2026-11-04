package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// Synthetic values only, seeded into the private byte-copy of the original
// baseline v7 that narrativeFixture provides. Execution is GitHub-only.
const identityTestHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func identityFixture(t *testing.T) (*sql.DB, *CharacterIdentities) {
	t.Helper()
	db, _ := rosterFixture(t)
	return db, NewCharacterIdentities(db)
}
func identityAuthorize() func(CharacterIdentityCommitRequest) bool {
	return func(CharacterIdentityCommitRequest) bool { return true }
}
func identityRequest(operationID string) CharacterIdentityCommitRequest {
	return CharacterIdentityCommitRequest{
		Approval: CharacterIdentityApproval{
			OperationID: operationID, ExpectedRevision: 17, Action: "author-edit",
			Source: &CharacterIdentityProvenance{Kind: "author", Source: CharacterIdentitySourceRef{
				ProjectID: "identity-test-project", Epoch: "identity-test-epoch",
				SourceID: "identity-test-source", Revision: 17, ContentHash: identityTestHash,
			}},
		},
		Creations: []CharacterIdentityCreation{{SelectionKey: "identity-test-row",
			Fields: []CharacterIdentityField{{Key: "name", Value: "甲"}, {Key: "role", Value: "protagonist"}}}},
	}
}
func identitySeed(t *testing.T, db *sql.DB, id, name string) {
	t.Helper()
	mustExec(t, db, "INSERT INTO characters(character_id,name,role,retired,static_provenance) VALUES(?,?,?,0,?)",
		id, name, "supporting", `{"kind":"legacy","sourceKey":"legacy:characters:0"}`)
	mustExec(t, db, "INSERT INTO character_aliases VALUES(?,?,?,?,NULL)", id, name, "legacy:characters:0", 0)
}
func identityRows(t *testing.T, db *sql.DB, query string, args ...any) []map[string]any {
	t.Helper()
	rows, err := rrMaps(context.Background(), db, query, args...)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
func identityCount(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	return len(identityRows(t, db, "SELECT 1 FROM "+table))
}
func identityRevisionNow(t *testing.T, db *sql.DB) any {
	t.Helper()
	rows := identityRows(t, db, "SELECT revision FROM character_identity_meta WHERE id='main'")
	if len(rows) != 1 {
		t.Fatalf("identity meta rows %v", rows)
	}
	return rows[0]["revision"]
}
func identityFail(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

// One creation writes exactly one identity row, one open alias, one approval row
// and one identity revision — and a replay writes nothing at all.
func TestCharacterIdentityCommitCreatesIdentityAliasAndReceipt(t *testing.T) {
	db, identities := identityFixture(t)
	request := identityRequest("identity-test-create")
	receipt, err := identities.CommitIdentities(context.Background(), request, identityAuthorize())
	if err != nil {
		t.Fatal(err)
	}
	if receipt["idempotent"] != false || receipt["operationId"] != "identity-test-create" || receipt["revision"] != float64(18) {
		t.Fatalf("receipt %v", receipt)
	}
	created := receipt["created"].([]any)
	if len(created) != 1 {
		t.Fatalf("created %v", created)
	}
	first := created[0].(map[string]any)
	id, _ := first["characterId"].(string)
	if first["selectionKey"] != "identity-test-row" || len(id) != 36 {
		t.Fatalf("created %v", created)
	}
	characters := identityRows(t, db, "SELECT character_id,name,role,retired,identity_revision,static_provenance FROM characters")
	if len(characters) != 1 {
		t.Fatalf("characters %v", characters)
	}
	row := characters[0]
	if row["character_id"] != id || row["name"] != "甲" || row["role"] != "protagonist" || row["retired"] != int64(0) || row["identity_revision"] != int64(18) {
		t.Fatalf("row %v", row)
	}
	provenance := row["static_provenance"].(string)
	if want := `{"kind":"author","source":{"projectId":"identity-test-project","epoch":"identity-test-epoch","sourceId":"identity-test-source","revision":17,"contentHash":"` + identityTestHash + `"},"approvalId":"identity-test-create"}`; provenance != want {
		t.Fatalf("provenance %s\nwant %s", provenance, want)
	}
	aliases := identityRows(t, db, "SELECT character_id,name,source_key,valid_from,valid_through FROM character_aliases")
	if len(aliases) != 1 || aliases[0]["character_id"] != id || aliases[0]["name"] != "甲" ||
		aliases[0]["source_key"] != "identity-test-create" || aliases[0]["valid_from"] != int64(18) || aliases[0]["valid_through"] != nil {
		t.Fatalf("aliases %v", aliases)
	}
	if identityRevisionNow(t, db) != int64(18) {
		t.Fatalf("meta %v", identityRevisionNow(t, db))
	}
	receipts := identityRows(t, db, "SELECT operation_id,payload_hash,receipt_json FROM character_identity_approvals")
	if len(receipts) != 1 || receipts[0]["operation_id"] != "identity-test-create" || len(receipts[0]["payload_hash"].(string)) != 64 {
		t.Fatalf("receipts %v", receipts)
	}
	// The stored receipt is the returned receipt, so a replay is byte-identical.
	replay, err := identities.CommitIdentities(context.Background(), request, identityAuthorize())
	if err != nil {
		t.Fatal(err)
	}
	if replay["idempotent"] != true || replay["revision"] != float64(18) || replay["operationId"] != "identity-test-create" {
		t.Fatalf("replay %v", replay)
	}
	if len(replay["created"].([]any)) != 1 || replay["created"].([]any)[0].(map[string]any)["characterId"] != id {
		t.Fatalf("replay created %v", replay)
	}
	if identityCount(t, db, "characters") != 1 || identityCount(t, db, "character_aliases") != 1 ||
		identityCount(t, db, "character_identity_approvals") != 1 || identityRevisionNow(t, db) != int64(18) {
		t.Fatal("replay wrote again")
	}
	// Same operation ID with a different payload is a nonce conflict, never a
	// silent second write and never an idempotent echo.
	changed := identityRequest("identity-test-create")
	changed.Creations[0].Fields[0].Value = "乙"
	_, err = identities.CommitIdentities(context.Background(), changed, identityAuthorize())
	identityFail(t, err, ErrCharacterApprovalNonceConflict)
	if identityCount(t, db, "characters") != 1 {
		t.Fatal("nonce conflict wrote")
	}
}

// A change merges field provenance and rotates aliases; a retirement closes both
// the identity and every open alias at the previous revision.
func TestCharacterIdentityCommitChangesAliasesAndRetirement(t *testing.T) {
	db, identities := identityFixture(t)
	identitySeed(t, db, "identity-test-a", "旧名")
	identitySeed(t, db, "identity-test-b", "乙")
	request := identityRequest("identity-test-change")
	request.Creations = nil
	request.Changes = []CharacterIdentityChange{{CharacterID: "identity-test-a",
		Fields: []CharacterIdentityField{{Key: "name", Value: "新名"}, {Key: "role", Value: "支持角色"}}}}
	request.RetireIDs = []string{"identity-test-b"}
	if _, err := identities.CommitIdentities(context.Background(), request, identityAuthorize()); err != nil {
		t.Fatal(err)
	}
	rows := identityRows(t, db, "SELECT character_id,name,role,retired,identity_revision,static_provenance FROM characters WHERE character_id='identity-test-a'")
	if len(rows) != 1 || rows[0]["name"] != "新名" || rows[0]["role"] != "支持角色" || rows[0]["retired"] != int64(0) || rows[0]["identity_revision"] != int64(18) {
		t.Fatalf("changed %v", rows)
	}
	want := `{"kind":"legacy","sourceKey":"legacy:characters:0","fields":{"name":{"kind":"author","source":{"projectId":"identity-test-project","epoch":"identity-test-epoch","sourceId":"identity-test-source","revision":17,"contentHash":"` + identityTestHash + `"},"approvalId":"identity-test-change"},"role":{"kind":"author","source":{"projectId":"identity-test-project","epoch":"identity-test-epoch","sourceId":"identity-test-source","revision":17,"contentHash":"` + identityTestHash + `"},"approvalId":"identity-test-change"}}}`
	if rows[0]["static_provenance"] != want {
		t.Fatalf("merged %v\nwant %s", rows[0]["static_provenance"], want)
	}
	aliases := identityRows(t, db, "SELECT character_id,name,source_key,valid_from,valid_through FROM character_aliases ORDER BY character_id,valid_from")
	if len(aliases) != 3 {
		t.Fatalf("aliases %v", aliases)
	}
	if aliases[0]["name"] != "旧名" || aliases[0]["valid_from"] != int64(0) || aliases[0]["valid_through"] != int64(17) {
		t.Fatalf("closed alias %v", aliases[0])
	}
	if aliases[1]["name"] != "新名" || aliases[1]["source_key"] != "identity-test-change" ||
		aliases[1]["valid_from"] != int64(18) || aliases[1]["valid_through"] != nil {
		t.Fatalf("opened alias %v", aliases[1])
	}
	if aliases[2]["character_id"] != "identity-test-b" || aliases[2]["valid_through"] != int64(17) {
		t.Fatalf("retired alias %v", aliases[2])
	}
	retired := identityRows(t, db, "SELECT retired,identity_revision FROM characters WHERE character_id='identity-test-b'")
	if retired[0]["retired"] != int64(1) || retired[0]["identity_revision"] != int64(18) {
		t.Fatalf("retired %v", retired)
	}
	// No change may touch an already retired identity, and the refusal rolls back
	// any creation that preceded it in the same commit.
	second := identityRequest("identity-test-retired")
	second.Approval.ExpectedRevision = 18
	second.Creations = []CharacterIdentityCreation{{SelectionKey: "identity-test-c",
		Fields: []CharacterIdentityField{{Key: "name", Value: "丙"}}}}
	second.Changes = []CharacterIdentityChange{{CharacterID: "identity-test-b", Fields: []CharacterIdentityField{{Key: "notes", Value: "x"}}}}
	_, err := identities.CommitIdentities(context.Background(), second, identityAuthorize())
	identityFail(t, err, ErrCharacterIDUnknown)
	if identityCount(t, db, "characters") != 2 || identityCount(t, db, "character_identity_approvals") != 1 || identityRevisionNow(t, db) != int64(18) {
		t.Fatal("refusal wrote")
	}
	// An empty change list is not a write: no revision, no alias, no provenance.
	empty := identityRequest("identity-test-empty")
	empty.Approval.ExpectedRevision = 18
	empty.Creations = nil
	empty.Changes = []CharacterIdentityChange{{CharacterID: "identity-test-a"}}
	if _, err := identities.CommitIdentities(context.Background(), empty, identityAuthorize()); err != nil {
		t.Fatal(err)
	}
	untouched := identityRows(t, db, "SELECT identity_revision,static_provenance FROM characters WHERE character_id='identity-test-a'")
	if untouched[0]["identity_revision"] != int64(18) || untouched[0]["static_provenance"] != want {
		t.Fatalf("empty change wrote %v", untouched)
	}
}

// Every refusal below is the original's own code, and the position of each check
// decides which code wins when two conditions hold at once.
func TestCharacterIdentityCommitRefusals(t *testing.T) {
	for _, test := range []struct {
		name      string
		authorize func(CharacterIdentityCommitRequest) bool
		mutate    func(*CharacterIdentityCommitRequest)
		want      error
	}{
		{name: "no authorizer", want: ErrCharacterApprovalRequired},
		{name: "denied", authorize: func(CharacterIdentityCommitRequest) bool { return false }, want: ErrCharacterApprovalRequired},
		{name: "mutated field", authorize: func(request CharacterIdentityCommitRequest) bool {
			request.Creations[0].Fields[0].Value = "乙"
			return true
		}, want: ErrCharacterApprovalMutated},
		{name: "mutated provenance", authorize: func(request CharacterIdentityCommitRequest) bool {
			request.Approval.Source.Kind = "generated"
			return true
		}, want: ErrCharacterApprovalMutated},
		{name: "stale revision", mutate: func(request *CharacterIdentityCommitRequest) { request.Approval.ExpectedRevision = 16 }, want: ErrCharacterIDRevisionConflict},
		{name: "unknown action", mutate: func(request *CharacterIdentityCommitRequest) { request.Approval.Action = "bogus" }, want: ErrCharacterApprovalInvalid},
		{name: "missing source", mutate: func(request *CharacterIdentityCommitRequest) { request.Approval.Source = nil }, want: ErrCharacterApprovalInvalid},
		{name: "legacy provenance", mutate: func(request *CharacterIdentityCommitRequest) { request.Approval.Source.Kind = "legacy" }, want: ErrCharacterProvenanceInvalid},
		{name: "kind not allowed by action", mutate: func(request *CharacterIdentityCommitRequest) {
			request.Approval.Action = "adopt-generated"
		}, want: ErrCharacterProvenanceInvalid},
		{name: "blank source id", mutate: func(request *CharacterIdentityCommitRequest) { request.Approval.Source.Source.SourceID = "  " }, want: ErrCharacterProvenanceInvalid},
		{name: "negative source revision", mutate: func(request *CharacterIdentityCommitRequest) { request.Approval.Source.Source.Revision = -1 }, want: ErrCharacterProvenanceInvalid},
		{name: "short content hash", mutate: func(request *CharacterIdentityCommitRequest) { request.Approval.Source.Source.ContentHash = "abc" }, want: ErrCharacterProvenanceInvalid},
		{name: "generated without model revision", mutate: func(request *CharacterIdentityCommitRequest) {
			request.Approval.Action, request.Approval.Source.Kind = "adopt-generated", "generated"
		}, want: ErrCharacterProvenanceInvalid},
		{name: "duplicate change ids", mutate: func(request *CharacterIdentityCommitRequest) {
			request.Creations = nil
			request.Changes = []CharacterIdentityChange{{CharacterID: "x"}, {CharacterID: "x"}}
		}, want: ErrCharacterIDDuplicate},
		{name: "empty change id", mutate: func(request *CharacterIdentityCommitRequest) {
			request.Creations = nil
			request.Changes = []CharacterIdentityChange{{CharacterID: ""}}
		}, want: ErrCharacterIDDuplicate},
		{name: "duplicate selection keys", mutate: func(request *CharacterIdentityCommitRequest) {
			request.Creations = append(request.Creations, request.Creations[0])
		}, want: ErrCharacterIDDuplicate},
		{name: "duplicate retire ids", mutate: func(request *CharacterIdentityCommitRequest) {
			request.RetireIDs = []string{"x", "x"}
		}, want: ErrCharacterIDDuplicate},
		{name: "retire and change", mutate: func(request *CharacterIdentityCommitRequest) {
			request.Creations = nil
			request.Changes = []CharacterIdentityChange{{CharacterID: "x"}}
			request.RetireIDs = []string{"x"}
		}, want: ErrCharacterIDConflict},
		{name: "creation without name", mutate: func(request *CharacterIdentityCommitRequest) {
			request.Creations = []CharacterIdentityCreation{{SelectionKey: "row", Fields: []CharacterIdentityField{{Key: "role", Value: "minor"}}}}
		}, want: ErrCharacterFieldsInvalid},
		{name: "blank name", mutate: func(request *CharacterIdentityCommitRequest) {
			request.Creations[0].Fields[0].Value = " \ufeff\t"
		}, want: ErrCharacterFieldsInvalid},
		{name: "unaddressable field", mutate: func(request *CharacterIdentityCommitRequest) {
			request.Creations[0].Fields[1].Key = "cs_location"
		}, want: ErrCharacterFieldsInvalid},
		{name: "unknown character beats illegal field", mutate: func(request *CharacterIdentityCommitRequest) {
			request.Creations = nil
			request.Changes = []CharacterIdentityChange{{CharacterID: "identity-test-missing",
				Fields: []CharacterIdentityField{{Key: "cs_location", Value: "x"}}}}
		}, want: ErrCharacterIDUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, identities := identityFixture(t)
			request := identityRequest("identity-test-refusal")
			if test.mutate != nil {
				test.mutate(&request)
			}
			authorize := test.authorize
			if authorize == nil && test.want != ErrCharacterApprovalRequired {
				authorize = identityAuthorize()
			}
			if _, err := identities.CommitIdentities(context.Background(), request, authorize); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if identityCount(t, db, "characters") != 0 || identityCount(t, db, "character_aliases") != 0 ||
				identityCount(t, db, "character_identity_approvals") != 0 || identityRevisionNow(t, db) != int64(17) {
				t.Fatal("refusal wrote")
			}
		})
	}
}

// The schema gate stays a gate: the port never creates or repairs identity tables.
func TestCharacterIdentityCommitRequiresIdentitySchema(t *testing.T) {
	db, identities := identityFixture(t)
	mustExec(t, db, "DROP TABLE character_identity_meta")
	before := identityRows(t, db, "SELECT type,name,sql FROM sqlite_master ORDER BY type,name")
	_, err := identities.CommitIdentities(context.Background(), identityRequest("identity-test-schema"), identityAuthorize())
	identityFail(t, err, ErrCharacterIDSchemaRequired)
	after := identityRows(t, db, "SELECT type,name,sql FROM sqlite_master ORDER BY type,name")
	if len(before) != len(after) {
		t.Fatalf("schema changed %v / %v", before, after)
	}
	if identityCount(t, db, "character_identity_approvals") != 0 || identityCount(t, db, "characters") != 0 {
		t.Fatal("schema refusal wrote")
	}
	if _, err := NewCharacterIdentities(nil).CommitIdentities(context.Background(), identityRequest("identity-test-nil"), identityAuthorize()); !errors.Is(err, ErrProjectNotOpen) {
		t.Fatalf("nil project %v", err)
	}
}

// A commit is atomic: a refusal in a later stage leaves no earlier stage behind.
func TestCharacterIdentityCommitRollsBackEveryStage(t *testing.T) {
	db, identities := identityFixture(t)
	identitySeed(t, db, "identity-test-a", "甲")
	request := identityRequest("identity-test-rollback")
	request.Creations = append(request.Creations, CharacterIdentityCreation{SelectionKey: "identity-test-b",
		Fields: []CharacterIdentityField{{Key: "name", Value: "乙"}}})
	// The retire stage runs last, so the two creations and the change before it
	// must all be rolled back by this single refusal.
	request.Changes = []CharacterIdentityChange{{CharacterID: "identity-test-a", Fields: []CharacterIdentityField{{Key: "notes", Value: "x"}}}}
	request.RetireIDs = []string{"identity-test-missing"}
	_, err := identities.CommitIdentities(context.Background(), request, identityAuthorize())
	identityFail(t, err, ErrCharacterIDUnknown)
	if identityCount(t, db, "characters") != 1 || identityCount(t, db, "character_aliases") != 1 ||
		identityCount(t, db, "character_identity_approvals") != 0 || identityRevisionNow(t, db) != int64(17) {
		t.Fatal("rollback left rows")
	}
	kept := identityRows(t, db, "SELECT name,notes,identity_revision,static_provenance FROM characters WHERE character_id='identity-test-a'")
	if kept[0]["name"] != "甲" || kept[0]["notes"] != "" || kept[0]["identity_revision"] != int64(0) {
		t.Fatalf("rolled back change %v", kept)
	}
}
