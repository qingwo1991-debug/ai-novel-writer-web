package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// Only synthetic records in a private copy of the actual TS baseline-v7.
func charactersFixture(t *testing.T) (*sql.DB, *Characters) {
	t.Helper()
	db, _, _ := summaryFixture(t)
	// A central-runner-generated empty fixture should contain no identities.
	// This assertion avoids ever cleaning or repurposing author data.
	if count := narrativeCount(t, db, "characters"); count != 0 { t.Fatalf("baseline contains %d unexpected characters", count) }
	return db, NewCharacters(db)
}
func charactersSeed(t *testing.T, db *sql.DB, id, name, role string, retired int) {
	t.Helper()
	mustExec(t, db, "INSERT INTO characters(character_id,name,role,retired) VALUES(?,?,?,?)", id, name, role, retired)
}
func charactersSetProvenance(t *testing.T, db *sql.DB, id string, provenance any) {
	t.Helper()
	text, err := blueprintJSON(provenance)
	if err != nil { t.Fatal(err) }
	mustExec(t, db, "UPDATE characters SET cs_provenance=? WHERE character_id=?", text, id)
}
func charactersDerived(source map[string]any, revision, version, generation any, project string) map[string]any {
	return map[string]any{"kind": "derived", "revision": revision, "source": source,
		"sourceOrder": map[string]any{"chapterNumber": source["chapterNumber"], "authoritativeFinalizationRevision": version, "continuityEpoch": project+":"+summaryJSString(generation)}}
}
func charactersFields(t *testing.T, c *Characters, project, id string, want []string) {
	t.Helper()
	got, err := c.CurrentDerivedFields(context.Background(), project)
	if err != nil || !reflect.DeepEqual(got[id], want) { t.Fatalf("derived %s = %#v %v, want %#v", id, got, err, want) }
}

func TestCharactersMissingProject(t *testing.T) {
	for _, c := range []*Characters{nil, NewCharacters(nil), {}} {
		if rows, err := c.All(context.Background()); err != nil || rows == nil || len(rows) != 0 { t.Fatalf("all %v %v", rows, err) }
		if row, err := c.IdentitySnapshot(context.Background()); row != nil || !errors.Is(err, ErrProjectNotOpen) { t.Fatalf("identity %v %v", row, err) }
		if row, err := c.IdentityView(context.Background()); row != nil || !errors.Is(err, ErrProjectNotOpen) { t.Fatalf("view %v %v", row, err) }
		if row, err := c.CurrentDerivedFields(context.Background(), "p"); row != nil || !errors.Is(err, ErrProjectNotOpen) { t.Fatalf("derived %v %v", row, err) }
	}
}
func TestCharactersRoleAndRawTruthyProjection(t *testing.T) {
	for _, tc := range []struct{ input any; want string }{
		{" \ufeffMAIN\u2029", "protagonist"}, {"主角", "protagonist"}, {"女主", "protagonist"}, {"核心主角", "protagonist"},
		{"VILLAIN", "antagonist"}, {"敌人", "antagonist"}, {"对手", "antagonist"}, {"核心配角", "supporting"},
		{"support", "supporting"}, {"重要配角", "supporting"}, {"次要角色", "minor"}, {"龙套", "minor"},
		{"MAİN", "supporting"}, {"MİNOR", "supporting"}, {"\u0085minor", "supporting"}, {"unknown", "supporting"}, {nil, "supporting"}, {int64(1), "supporting"},
	} {
		if got := charactersRole(tc.input); got != tc.want { t.Fatalf("role %#v = %s", tc.input, got) }
	}
	row := map[string]any{"character_id": "c", "name": nil, "role": " MAIN ", "gender": int64(0), "age": int64(-3), "notes": []byte{},
		"cs_updated_at_chapter": int64(0), "cs_location": []byte{0, 255}, "cs_provenance": `{"arbitrary":{"number":9007199254740993,"list":[null,true]}}`}
	got, err := charactersRowToData(row)
	if err != nil || got["name"] != nil || got["gender"] != "" || got["age"] != int64(-3) || got["role"] != "protagonist" { t.Fatalf("row %#v %v", got, err) }
	state := got["currentState"].(map[string]any)
	if state["updatedAtChapter"] != int64(0) || state["keyItems"] != "" { t.Fatal(state) }
	if !reflect.DeepEqual(state["location"], map[string]any{"type": "Buffer", "data": []int{0,255}}) { t.Fatal(state) }
	arbitrary := state["provenance"].(map[string]any)["arbitrary"].(map[string]any)
	if arbitrary["number"] != float64(9007199254740992) { t.Fatal(arbitrary) }
	if !reflect.DeepEqual(got["notes"], map[string]any{"type": "Buffer", "data": []int{}}) { t.Fatal(got) }
	for _, text := range []string{"", "  ", "null", "[]", "true", "1", "{broken"} {
		row["cs_provenance"] = text
		got, err := charactersRowToData(row)
		if err != nil { t.Fatal(err) }
		if _, exists := got["currentState"].(map[string]any)["provenance"]; exists { t.Fatal(got) }
	}
	for _, role := range []string{"__proto__", "__PROTO__", "constructor", "toString", "hasOwnProperty", "__lookupSetter__"} {
		row["role"] = role
		if _, err := charactersRowToData(row); !errors.Is(err, ErrCharacterInputUnsupported) { t.Fatalf("inherited role %s: %v", role, err) }
	}
}
func TestCharactersJSONNativeBoundary(t *testing.T) {
	for _, text := range []string{`{"x":"\ud800"}`, `{"x":"\udc00"}`, `{"x":1e400}`, strings.Repeat("[",65)+"0"+strings.Repeat("]",65), string([]byte{255}), strings.Repeat("x", (32<<20)+1)} {
		if _, err := charactersProvenance(text, false); !errors.Is(err, ErrCharacterInputUnsupported) { t.Fatalf("unsupported %q: %v", text[:min(len(text),80)], err) }
	}
	parsed, err := charactersProvenance(`{"x":"\ud83d\ude00","n":1e-400}`, false)
	if err != nil || parsed["x"] != "😀" || parsed["n"] != float64(0) { t.Fatalf("valid %#v %v", parsed, err) }
}
func TestCharactersAllOrderingRetirementAndSchemaPresence(t *testing.T) {
	db, c := charactersFixture(t)
	db.SetMaxOpenConns(1)
	charactersSeed(t, db, "z", "main alias", "main", 0)
	charactersSeed(t, db, "a", "protagonist", "protagonist", 0)
	charactersSeed(t, db, "c", "antagonist", "antagonist", 0)
	charactersSeed(t, db, "b", "supporting", "supporting", 0)
	charactersSeed(t, db, "d", "minor", "minor", 0)
	charactersSeed(t, db, "r", "retired", "protagonist", 1)
	mustExec(t, db, "UPDATE characters SET gender=NULL,age=0,cs_updated_at_chapter=0,cs_location='x',cs_provenance='{}' WHERE character_id='a'")
	rows, err := c.All(context.Background())
	if err != nil || len(rows) != 5 { t.Fatalf("all %v %v", rows, err) }
	for i, want := range []string{"a", "b", "c", "d", "z"} { if rows[i]["characterId"] != want { t.Fatal(rows) } }
	if rows[0]["gender"] != "" || rows[0]["age"] != "0" { t.Fatal(rows[0]) } // TEXT affinity, not input type.
	if rows[0]["currentState"].(map[string]any)["updatedAtChapter"] != int64(0) { t.Fatal(rows[0]) }
	mustExec(t, db, "ALTER TABLE character_identity_meta RENAME TO character_identity_meta_hidden")
	rows, err = c.All(context.Background())
	if err != nil || len(rows) != 6 { t.Fatalf("presence gate %v %v", rows, err) }
	if _, err := c.IdentitySnapshot(context.Background()); !errors.Is(err, ErrCharacterIDSchemaRequired) { t.Fatal(err) }
	// The read must not silently create/repair schema.
	var count int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='character_identity_meta'").Scan(&count); err != nil || count != 0 { t.Fatalf("repair %d %v", count, err) }
}
func TestCharactersRawIdentityAndDisplayView(t *testing.T) {
	db, c := charactersFixture(t)
	db.SetMaxOpenConns(1)
	charactersSeed(t, db, "b", "B", "main", 1)
	charactersSeed(t, db, "a", "A", "minor", 0)
	mustExec(t, db, "UPDATE characters SET age=NULL,gender=X'0041',static_provenance='[true,9007199254740993]',identity_revision=2 WHERE character_id='b'")
	mustExec(t, db, "UPDATE character_identity_meta SET revision=3 WHERE id='main'")
	mustExec(t, db, "INSERT INTO character_aliases VALUES('b','Z','synthetic:b',2,3),('a','B','synthetic:a',1,NULL),('a','A','synthetic:a',0,NULL)")
	mustExec(t, db, "INSERT INTO character_identity_approvals VALUES('synthetic-approval','hash','{}')")
	mustExec(t, db, "INSERT INTO character_relationships VALUES('r','a','b','friend','A','B','{}','synthetic-approval')")
	mustExec(t, db, "INSERT INTO character_identity_proposals VALUES('p',NULL,'synthetic','hash','not parsed','[]',NULL,NULL)")
	raw, err := c.IdentitySnapshot(context.Background())
	if err != nil || raw["revision"] != int64(3) || len(raw["proposals"].([]map[string]any)) != 1 { t.Fatalf("raw %v %v", raw, err) }
	characters := raw["characters"].([]map[string]any)
	if len(characters) != 2 || characters[0]["character_id"] != "a" || characters[1]["retired"] != int64(1) || characters[1]["age"] != nil { t.Fatal(characters) }
	aliases := raw["aliases"].([]map[string]any)
	if aliases[0]["name"] != "A" || aliases[1]["name"] != "B" || aliases[2]["name"] != "Z" || aliases[0]["valid_through"] != nil { t.Fatal(aliases) }
	view, err := c.IdentityView(context.Background())
	if err != nil || len(view) != 4 { t.Fatalf("view %v %v", view, err) }
	b := view["characters"].([]map[string]any)[1]
	fields := b["fields"].(map[string]any)
	if len(fields) != 11 || fields["role"] != "main" || fields["age"] != "" || fields["gender"] != "\x00A" || b["retired"] != true || b["revision"] != int64(2) { t.Fatal(b) }
	if !reflect.DeepEqual(b["provenance"], []any{true,float64(9007199254740992)}) { t.Fatal(b) }
	// INTEGER-affinity storage is read as JS binary64 before String(number).
	// Use the nullable static name field's BLOB alternative above for text,
	// and rowToData directly for the out-of-safe INTEGER wire domain.
	if got := narrativeJoinText(int64(9007199254740993)); got != "9007199254740992" { t.Fatalf("JS integer String %s", got) }
	if got := charactersWire(int64(9007199254740993)); got != float64(9007199254740992) { t.Fatalf("JS integer wire %v", got) }
	mustExec(t, db, "UPDATE characters SET gender=X'e282' WHERE character_id='b'")
	if _, err := c.IdentityView(context.Background()); !errors.Is(err, ErrCharacterInputUnsupported) { t.Fatalf("malformed Buffer display %v", err) }
	if _, err := c.IdentitySnapshot(context.Background()); err != nil { t.Fatalf("raw Buffer rejected %v", err) }
	mustExec(t, db, "UPDATE characters SET gender='valid',static_provenance='{broken' WHERE character_id='b'")
	if _, err := c.IdentityView(context.Background()); !errors.Is(err, ErrCharacterInputUnsupported) { t.Fatal(err) }
	if _, err := c.IdentitySnapshot(context.Background()); err != nil { t.Fatalf("raw evidence unexpectedly parsed: %v", err) }
	mustExec(t, db, "DELETE FROM character_identity_meta")
	if _, err := c.IdentitySnapshot(context.Background()); !errors.Is(err, ErrCharacterIDMetaMissing) { t.Fatal(err) }
}
func TestCharactersDerivedSourceProofAndRetiredEmptyArrays(t *testing.T) {
	db, c := charactersFixture(t)
	db.SetMaxOpenConns(1)
	charactersSeed(t, db, "a", "A", "protagonist", 0)
	charactersSeed(t, db, "r", "R", "supporting", 1)
	charactersSeed(t, db, "e", "E", "minor", 0)
	mustExec(t, db, "UPDATE characters SET cs_location='',cs_recent_events='x',cs_power_level=X'61'")
	source := summarySeed(t, db, 891001, 891001, 2, "finalized", "完整正文😀\x00", true)
	summaryProjection(t, db, source, true)
	entry := charactersDerived(source, int64(1), int64(2), int64(0), "project")
	charactersSetProvenance(t, db, "a", map[string]any{"recentEvents": entry, "location": entry, "powerLevel": entry})
	charactersSetProvenance(t, db, "r", map[string]any{"location": entry})
	charactersFields(t, c, "project", "a", []string{"location", "recentEvents"})
	charactersFields(t, c, "project", "r", []string{"location"})
	charactersFields(t, c, "project", "e", []string{})
	charactersFields(t, c, "other", "a", []string{})
	for _, mutation := range []struct{ sql string; args []any }{
		{"UPDATE contents SET body='edited' WHERE id=891001", nil},
		{"UPDATE finalization_outbox SET content_snapshot='edited' WHERE draft_id=891001", nil},
		{"UPDATE finalization_outbox SET content_hash=upper(content_hash) WHERE draft_id=891001", nil},
		{"UPDATE summary_snapshots SET source_finalization_id='other' WHERE draft_id=891001", nil},
		{"UPDATE summary_snapshots SET source_content_hash='other' WHERE draft_id=891001", nil},
		{"UPDATE summary_snapshots SET projection_generation=1 WHERE draft_id=891001", nil},
		{"UPDATE drafts SET version=0 WHERE id=891001", nil},
		{"UPDATE drafts SET status='superseded' WHERE id=891001", nil},
	} {
		mustExec(t, db, mutation.sql, mutation.args...)
		charactersFields(t, c, "project", "a", []string{})
		mustExec(t, db, "UPDATE contents SET body=? WHERE id=891001", "完整正文😀\x00")
		mustExec(t, db, "UPDATE finalization_outbox SET content_snapshot=?,content_hash=? WHERE draft_id=891001", "完整正文😀\x00", source["contentHash"])
		mustExec(t, db, "UPDATE summary_snapshots SET source_finalization_id=?,source_content_hash=?,projection_generation=0 WHERE draft_id=891001", source["finalizationId"], source["contentHash"])
		mustExec(t, db, "UPDATE drafts SET version=2,status='finalized' WHERE id=891001")
	}
	mustExec(t, db, "UPDATE continuity_projection_meta SET generation=1,stale_from_chapter=891002 WHERE id='main'")
	charactersFields(t, c, "project", "a", []string{"location", "recentEvents"})
	mustExec(t, db, "UPDATE continuity_projection_meta SET stale_from_chapter=891001 WHERE id='main'")
	charactersFields(t, c, "project", "a", []string{})
	mustExec(t, db, "UPDATE summary_snapshots SET projection_generation=1 WHERE draft_id=891001")
	fresh := charactersDerived(source, int64(1), int64(2), int64(1), "project")
	charactersSetProvenance(t, db, "a", map[string]any{"location": fresh})
	charactersFields(t, c, "project", "a", []string{"location"})
	// Same-version latest id wins, as does a strictly newer version.
	mustExec(t, db, "DROP INDEX idx_drafts_chapter_version")
	summarySeed(t, db, 891002, 891001, 2, "finalized", "新定稿", true)
	charactersFields(t, c, "project", "a", []string{})
	mustExec(t, db, "DELETE FROM continuity_projection_meta")
	if _, err := c.CurrentDerivedFields(context.Background(), "project"); !errors.Is(err, sql.ErrNoRows) { t.Fatal(err) }
}
func TestCharactersDerivedStrictEntryAndOrderDomain(t *testing.T) {
	source := map[string]any{"draftId": int64(1), "chapterNumber": int64(2), "finalizationId": "f", "contentHash": summaryHash("body")}
	proof := map[string]any{"draftVersion": int64(3), "projectionGeneration": int64(4)}
	proofs := map[string]map[string]any{charactersSourceKey(source): proof}
	watermark := map[string]any{"generation": int64(4), "staleFromChapter": nil}
	makeEntry := func() map[string]any { return charactersDerived(source, int64(1), int64(3), int64(4), "p") }
	if !charactersDerivedTrusted(makeEntry(), proofs, watermark, "p") { t.Fatal("valid source rejected") }
	for _, mutate := range []func(map[string]any){
		func(e map[string]any){ e["extra"] = true },
		func(e map[string]any){ e["revision"] = int64(0) },
		func(e map[string]any){ e["revision"] = 1.5 },
		func(e map[string]any){ e["revision"] = "1" },
		func(e map[string]any){ e["revision"] = float64(9007199254740992) },
		func(e map[string]any){ e["kind"] = "author" },
		func(e map[string]any){ delete(e,"sourceOrder") },
		func(e map[string]any){ e["sourceOrder"].(map[string]any)["extra"] = true },
		func(e map[string]any){ e["sourceOrder"].(map[string]any)["chapterNumber"] = "2" },
		func(e map[string]any){ e["sourceOrder"].(map[string]any)["authoritativeFinalizationRevision"] = int64(2) },
		func(e map[string]any){ e["sourceOrder"].(map[string]any)["continuityEpoch"] = "origin:4" },
	} {
		entry := makeEntry(); mutate(entry)
		if charactersDerivedTrusted(entry, proofs, watermark, "p") { t.Fatalf("invalid entry admitted %#v", entry) }
	}
	// Actual validator does not constrain extra keys inside source itself.
	source["extra"] = true
	if !charactersDerivedTrusted(makeEntry(), proofs, watermark, "p") { t.Fatal("source extra keys unexpectedly forbidden") }
}
func TestCharactersCancellationAndUnsupportedRawText(t *testing.T) {
	db, c := charactersFixture(t)
	db.SetMaxOpenConns(1)
	charactersSeed(t, db, "a", "A", "minor", 0)
	ctx, cancel := context.WithCancel(context.Background()); cancel()
	if _, err := c.All(ctx); !errors.Is(err, context.Canceled) { t.Fatal(err) }
	if _, err := c.IdentitySnapshot(ctx); !errors.Is(err, context.Canceled) { t.Fatal(err) }
	if _, err := c.IdentityView(ctx); !errors.Is(err, context.Canceled) { t.Fatal(err) }
	if _, err := c.CurrentDerivedFields(ctx,"p"); !errors.Is(err, context.Canceled) { t.Fatal(err) }
	if _, err := c.All(context.Background()); err != nil { t.Fatalf("connection not reusable %v", err) }
	mustExec(t, db, "UPDATE characters SET notes=CAST(X'ff' AS TEXT) WHERE character_id='a'")
	if _, err := c.All(context.Background()); !errors.Is(err, ErrCharacterInputUnsupported) { t.Fatal(err) }
	if _, err := c.IdentitySnapshot(context.Background()); !errors.Is(err, ErrCharacterInputUnsupported) { t.Fatal(err) }
}
