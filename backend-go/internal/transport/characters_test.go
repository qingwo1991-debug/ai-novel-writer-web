package transport

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// SQL fixtures reproduce baseline/M02 columns, not a fake repository or a
// schema-admission substitute. No TypeScript execution supplies these results.
const charactersTransportColumns = `
 role TEXT DEFAULT 'supporting', gender TEXT DEFAULT '', age TEXT DEFAULT '',
 appearance TEXT DEFAULT '', personality TEXT DEFAULT '', background TEXT DEFAULT '',
 abilities TEXT DEFAULT '', motivation TEXT DEFAULT '', relationships TEXT DEFAULT '',
 arc TEXT DEFAULT '', notes TEXT DEFAULT '', cs_location TEXT DEFAULT '',
 cs_power_level TEXT DEFAULT '', cs_physical_state TEXT DEFAULT '', cs_mental_state TEXT DEFAULT '',
 cs_key_items TEXT DEFAULT '', cs_recent_events TEXT DEFAULT '', cs_updated_at_chapter INTEGER DEFAULT NULL,
 cs_provenance TEXT NOT NULL DEFAULT '{}', created_at TEXT DEFAULT 'created', updated_at TEXT DEFAULT 'updated'
`
const charactersTransportIdentitySchema = `
CREATE TABLE character_identity_meta(id TEXT PRIMARY KEY,revision INTEGER NOT NULL,legacy_count INTEGER NOT NULL);
INSERT INTO character_identity_meta VALUES('main',0,0);
CREATE TABLE character_aliases(character_id TEXT,name TEXT,source_key TEXT,valid_from INTEGER,valid_through INTEGER,
 PRIMARY KEY(character_id,name,source_key,valid_from));
CREATE TABLE character_relationships(relationship_id TEXT PRIMARY KEY,source_character_id TEXT,target_character_id TEXT,
 relation TEXT,source_display_snapshot TEXT,target_display_snapshot TEXT,provenance_json TEXT,approval_id TEXT);
CREATE TABLE character_identity_proposals(proposal_id TEXT PRIMARY KEY,owner_character_id TEXT,source_key TEXT,
 source_hash TEXT,raw_value TEXT,candidate_ids_json TEXT,resolved_character_id TEXT,approval_id TEXT);
CREATE TABLE character_identity_approvals(operation_id TEXT PRIMARY KEY,payload_hash TEXT,receipt_json TEXT);
CREATE TABLE character_identity_origins(character_id TEXT PRIMARY KEY,source_key TEXT,original_row_json TEXT,original_hash TEXT);
CREATE TABLE character_roster_meta(id TEXT PRIMARY KEY,schema_version INTEGER,revision INTEGER,migration_state TEXT,
 legacy_markdown TEXT,projection_hash TEXT,fact_hash TEXT,updated_at TEXT DEFAULT 'unchanged');
INSERT INTO character_roster_meta(id,schema_version,revision,migration_state,legacy_markdown,projection_hash,fact_hash)
 VALUES('main',1,0,'empty','','','');
CREATE TABLE character_roster_operations(operation_id TEXT PRIMARY KEY,payload_hash TEXT,committed_revision INTEGER,projection_hash TEXT);
CREATE TABLE project_core(id TEXT PRIMARY KEY,writing_language TEXT,characters_arch TEXT);
INSERT INTO project_core VALUES('main','zh-CN','');
CREATE TABLE continuity_projection_meta(id TEXT PRIMARY KEY,generation INTEGER,stale_from_chapter INTEGER);
INSERT INTO continuity_projection_meta VALUES('main',0,NULL);
CREATE TABLE summary_snapshots(draft_id INTEGER,chapter_number INTEGER,source_finalization_id TEXT,
 source_content_hash TEXT,projection_generation INTEGER);
`

var charactersTransportOperations = []string{"db:character-get-all", "db:character-roster-read", "character-identity:read", "db:character-roster-commit"}

func charactersTransportDB(t *testing.T, identity bool) (*sql.DB, *store.Characters) {
	t.Helper()
	db, _ := transportDraftDB(t)
	prefix := "name TEXT PRIMARY KEY,"
	if identity {
		prefix = "character_id TEXT PRIMARY KEY, legacy_key TEXT, static_provenance TEXT NOT NULL DEFAULT '{}', identity_revision INTEGER NOT NULL DEFAULT 0, retired INTEGER NOT NULL DEFAULT 0, name TEXT,"
	}
	draftExec(t, db, "CREATE TABLE characters("+prefix+charactersTransportColumns+");")
	if identity {
		draftExec(t, db, charactersTransportIdentitySchema)
	}
	return db, store.NewCharacters(db)
}

func charactersTransportArgs(name string, expected map[string]any) []any {
	if name == "character-identity:read" {
		return []any{expected}
	}
	if name == "db:character-roster-commit" {
		return []any{map[string]any{"intent": "manual_edit"}, expected["projectPath"], expected}
	}
	return []any{expected["projectPath"], expected}
}

func charactersTransportDecode(t *testing.T, result any) any {
	t.Helper()
	wire, ok := result.(json.RawMessage)
	if !ok {
		t.Fatalf("result was not serialized under hold: %T", result)
	}
	var decoded any
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func TestCharactersHTTPOriginalShapesAndHeldSnapshot(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	db, repository := charactersTransportDB(t, true)
	expected := draftContext("project", "lease", "/synthetic/characters")
	owner := draftOwner(cookie, "synthetic-tab-0001")
	begins, releases := 0, 0
	if err := server.BindCharacters(CharactersProviderFunc(func(_ context.Context, caller Caller, raw any) (*store.Characters, func(), error) {
		begins++
		if caller != owner || !reflect.DeepEqual(raw, expected) {
			return nil, nil, novelruntime.ErrNotOwned
		}
		return repository, func() { releases++ }, nil
	})); err != nil {
		t.Fatal(err)
	}
	call := func(name string) any {
		return draftResult(t, draftRPC(t, server, cookie, csrf, owner.ClientID, name, charactersTransportArgs(name, expected)...))
	}
	if got := call("db:character-get-all"); !reflect.DeepEqual(got, []any{}) {
		t.Fatal("empty reads must be arrays", got)
	}
	if got := call("character-identity:read"); !reflect.DeepEqual(got, map[string]any{"revision": float64(0), "characters": []any{}, "aliases": []any{}, "relationships": []any{}}) {
		t.Fatal("identity is the direct proposal-service DTO", got)
	}
	roster := call("db:character-roster-read").(map[string]any)
	if roster["status"] != "empty" || roster["schemaVersion"] != float64(1) || roster["identityRevision"] != float64(0) || !reflect.DeepEqual(roster["entries"], []any{}) || !reflect.DeepEqual(roster["aliases"], []any{}) || !reflect.DeepEqual(roster["currentDerivedFields"], map[string]any{}) {
		t.Fatal(roster)
	}
	draftExec(t, db, `INSERT INTO characters(character_id,name,role,static_provenance,identity_revision,cs_updated_at_chapter,cs_location) VALUES('b','配角','supporting','{"kind":"legacy","sourceKey":"legacy:b"}',2,0,'原始位置😀')`)
	draftExec(t, db, `INSERT INTO characters(character_id,name,role,static_provenance,identity_revision) VALUES('a','主角','protagonist','{"kind":"author"}',3)`)
	draftExec(t, db, `INSERT INTO characters(character_id,name,retired) VALUES('z','退场',1)`)
	draftExec(t, db, `UPDATE character_identity_meta SET revision=3`)
	draftExec(t, db, `INSERT INTO character_aliases VALUES('b','旧称','source:b',1,NULL),('z','退场旧称','source:z',0,2)`)
	draftExec(t, db, `INSERT INTO character_relationships VALUES('r','a','b','同伴','主角','配角','{}','approval')`)
	draftExec(t, db, `INSERT INTO character_identity_proposals VALUES('private',NULL,'source','hash','private raw','[]',NULL,NULL)`)
	all := call("db:character-get-all").([]any)
	if len(all) != 2 || all[0].(map[string]any)["characterId"] != "a" || all[1].(map[string]any)["currentState"].(map[string]any)["updatedAtChapter"] != float64(0) {
		t.Fatal("All must filter retired and retain chapter zero", all)
	}
	identity := call("character-identity:read").(map[string]any)
	if len(identity) != 4 || identity["revision"] != float64(3) || len(identity["characters"].([]any)) != 3 || len(identity["aliases"].([]any)) != 2 {
		t.Fatal("identity must include retired rows but no private proposal registry", identity)
	}
	person := identity["characters"].([]any)[1].(map[string]any)
	if len(person) != 5 || person["characterId"] != "b" || person["retired"] != false || person["revision"] != float64(2) || len(person["fields"].(map[string]any)) != 11 || person["fields"].(map[string]any)["name"] != "配角" {
		t.Fatal("wrong public character shape", person)
	}
	if !reflect.DeepEqual(identity["relationships"], []any{map[string]any{"relationshipId": "r", "sourceCharacterId": "a", "targetCharacterId": "b", "relation": "同伴"}}) {
		t.Fatal(identity)
	}
	roster = call("db:character-roster-read").(map[string]any)
	if roster["status"] != "inconsistent" || len(roster["entries"].([]any)) != 2 || len(roster["aliases"].([]any)) != 1 {
		t.Fatal("raw history stays readable without authorizing readiness", roster)
	}
	for _, row := range []struct{ intent, code string }{{"manual_edit", "CHARACTER_AUTHOR_SCOPE_REQUIRED"}, {"architecture_generation", "CHARACTER_ID_WRITE_REQUIRED"}, {"chapter_progress", "CHARACTER_ID_WRITE_REQUIRED"}} {
		requireDraftFailure(t, draftRPC(t, server, cookie, csrf, owner.ClientID, "db:character-roster-commit", map[string]any{"intent": row.intent, "generationRunHandle": map[string]any{"projectId": "forged"}}, expected["projectPath"], expected), row.code)
	}
	var revision, operations int
	if err := db.QueryRow("SELECT revision FROM character_identity_meta").Scan(&revision); err != nil || revision != 3 {
		t.Fatal(revision, err)
	}
	if err := db.QueryRow("SELECT count(*) FROM character_roster_operations").Scan(&operations); err != nil || operations != 0 {
		t.Fatal("denials wrote receipts", operations, err)
	}
	if begins != 9 || releases != begins {
		t.Fatal("borrow/release mismatch", begins, releases)
	}
}

func TestCharactersDerivedFieldsUseCapturedAdmittedIdentity(t *testing.T) {
	server := fixture(t)
	db, repository := charactersTransportDB(t, true)
	body := "定稿😀"
	sum := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(sum[:])
	draftSeed(t, db, 1, 1, 1, "finalized", body)
	draftExec(t, db, `INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,content_hash,content_snapshot) VALUES('f',1,1,?,?)`, hash, body)
	draftExec(t, db, `INSERT INTO summary_snapshots VALUES(1,1,'f',?,0)`, hash)
	provenance, err := json.Marshal(map[string]any{"location": map[string]any{"kind": "derived", "revision": 1, "source": map[string]any{"draftId": 1, "chapterNumber": 1, "finalizationId": "f", "contentHash": hash}, "sourceOrder": map[string]any{"chapterNumber": 1, "authoritativeFinalizationRevision": 1, "continuityEpoch": "admitted:0"}}})
	if err != nil { t.Fatal(err) }
	draftExec(t, db, `INSERT INTO characters(character_id,name,cs_location,cs_updated_at_chapter,cs_provenance) VALUES('c','角色','可信位置',1,?)`, string(provenance))
	expected := draftContext("admitted", "lease", "/synthetic/characters")
	releases := 0
	if err := server.BindCharacters(CharactersProviderFunc(func(_ context.Context, _ Caller, raw any) (*store.Characters, func(), error) {
		if !reflect.DeepEqual(raw, expected) { t.Fatal(raw) }
		// Both identities are mutable Go maps, but neither mutation may change
		// the admitted ID already captured for the combined repository read.
		raw.(map[string]any)["projectId"] = "provider mutation"
		expected["projectId"] = "renderer mutation"
		return repository, func() {
			releases++
			draftExec(t, db, `UPDATE characters SET cs_location='after release'`)
		}, nil
	})); err != nil { t.Fatal(err) }
	result, err := server.handlers["db:character-roster-read"](context.Background(), Caller{}, []any{expected["projectPath"], expected})
	if err != nil { t.Fatal(err) }
	roster := charactersTransportDecode(t, result).(map[string]any)
	if releases != 1 || !reflect.DeepEqual(roster["currentDerivedFields"], map[string]any{"c": []any{"location"}}) || roster["entries"].([]any)[0].(map[string]any)["currentState"].(map[string]any)["location"] != "可信位置" {
		t.Fatal("result was not read and frozen under one admitted hold", roster, releases)
	}
}

func TestCharactersLegacyAllAndNoReadDDL(t *testing.T) {
	server := fixture(t)
	db, repository := charactersTransportDB(t, false)
	draftExec(t, db, `INSERT INTO characters(name,role,relationships) VALUES('旧卡','main','不解析的关系原文')`)
	if err := server.BindCharacters(CharactersProviderFunc(func(context.Context, Caller, any) (*store.Characters, func(), error) { return repository, func(){}, nil })); err != nil { t.Fatal(err) }
	cookie, csrf := bootstrap(t, server)
	expected := draftContext("p", "l", "/synthetic/legacy")
	all := draftResult(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:character-get-all", charactersTransportArgs("db:character-get-all", expected)...)).([]any)
	if len(all) != 1 || all[0].(map[string]any)["role"] != "protagonist" || all[0].(map[string]any)["relationships"] != "不解析的关系原文" { t.Fatal(all) }
	for _, name := range []string{"db:character-roster-read", "character-identity:read"} {
		response := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", name, charactersTransportArgs(name, expected)...)
		requireCharactersGenericError(t, response.Code, response.Body.String(), "CHARACTER_ID_SCHEMA_REQUIRED")
	}
	var created int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name IN ('character_identity_meta','character_roster_meta','character_roster_operations')").Scan(&created); err != nil || created != 0 { t.Fatal("read performed legacy DDL", created, err) }
}

func requireCharactersGenericError(t *testing.T, status int, body, code string) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil { t.Fatal(err) }
	want := map[string]any{"error": map[string]any{"code": code, "message": "服务暂时无法完成此操作，请重试或查看服务器日志。"}}
	if status != 500 || !reflect.DeepEqual(got, want) { t.Fatalf("HTTP %d %s, want generic 500 %s", status, body, code) }
}

func TestCharactersBindingAtomicAndUnavailable(t *testing.T) {
	provider := CharactersProviderFunc(func(context.Context, Caller, any) (*store.Characters, func(), error) { return nil, nil, nil })
	for _, name := range charactersTransportOperations {
		server := fixture(t)
		if err := server.Bind(name, func(context.Context, Caller, []any) (any, error) { return nil, nil }); err != nil { t.Fatal(err) }
		if err := server.BindCharacters(provider); err == nil || err.Error() != "OPERATION_ALREADY_BOUND" || len(server.handlers) != 1 { t.Fatal("partial bind", err) }
		server = fixture(t)
		delete(server.registry.Operations, name)
		if err := server.BindCharacters(provider); err == nil || err.Error() != "OPERATION_NOT_EXPOSED" || len(server.handlers) != 0 { t.Fatal("partial inventory bind", err) }
	}
	var nilFunc CharactersProviderFunc
	var nilPointer *charactersNilProvider
	for _, value := range []CharactersProvider{nil, nilFunc, nilPointer} {
		server := fixture(t)
		if err := server.BindCharacters(value); err == nil || len(server.handlers) != 0 { t.Fatal("nil provider accepted", err) }
	}
	if _, _, err := nilFunc.BeginCharacters(context.Background(), Caller{}, nil); err == nil { t.Fatal("nil function accepted") }
	_, repository := charactersTransportDB(t, true)
	expected := draftContext("p", "l", "/synthetic/c")
	for _, repo := range []*store.Characters{nil, repository} {
		server := fixture(t)
		if err := server.BindCharacters(CharactersProviderFunc(func(context.Context, Caller, any) (*store.Characters, func(), error) { return repo, nil, nil })); err != nil { t.Fatal(err) }
		_, err := server.handlers["character-identity:read"](context.Background(), Caller{}, []any{expected})
		if err == nil || err.Error() != "CHARACTER_PROVIDER_UNAVAILABLE" { t.Fatal(err) }
	}
	server := fixture(t)
	releases := 0
	if err := server.BindCharacters(CharactersProviderFunc(func(context.Context, Caller, any) (*store.Characters, func(), error) { return nil, func(){releases++}, nil })); err != nil { t.Fatal(err) }
	_, err := server.handlers["character-identity:read"](context.Background(), Caller{}, []any{expected})
	if err == nil || err.Error() != "CHARACTER_PROVIDER_UNAVAILABLE" || releases != 1 { t.Fatal("nil repository leaked hold",err,releases) }
	if value, err := charactersSerialized(make(chan int)); value != nil || err == nil || charactersCode(err) != "CHARACTER_OPERATION_FAILED" { t.Fatal("serialization failure escaped finite vocabulary",value,err) }
}

type charactersNilProvider struct{}
func (*charactersNilProvider) BeginCharacters(context.Context, Caller, any) (*store.Characters, func(), error) { panic("typed nil provider invoked") }

func TestCharactersMalformedContextRejectedBeforeBorrow(t *testing.T) {
	server := fixture(t)
	begins := 0
	if err := server.BindCharacters(CharactersProviderFunc(func(context.Context, Caller, any) (*store.Characters, func(), error) { begins++; return nil, nil, nil })); err != nil { t.Fatal(err) }
	expected := draftContext("p", "l", "/synthetic/c")
	check := func(name, code string, args []any) {
		t.Helper()
		result, err := server.handlers[name](context.Background(), Caller{}, args)
		if name == "db:character-roster-commit" {
			want := map[string]any{"success": false, "error": "Error: " + code}
			if err != nil || !reflect.DeepEqual(charactersTransportDecode(t, result), want) { t.Fatal(result, err) }
		} else if result != nil || err == nil || charactersCode(err) != code { t.Fatal(name, result, err) }
	}
	for _, name := range charactersTransportOperations {
		check(name, "INVALID_ARGUMENTS", nil)
		args := charactersTransportArgs(name, expected)
		check(name, "INVALID_ARGUMENTS", append(args, "extra"))
		for _, bad := range []any{nil, true, map[string]any{}, draftContext("", "l", "/synthetic/c"), draftContext("p", string([]byte{0xff}), "/synthetic/c"), map[string]any{"projectId":"p", "leaseId":"l", "projectPath":"/synthetic/c", "extra":true}} {
			args := charactersTransportArgs(name, expected)
			args[len(args)-1] = bad
			check(name, "PROJECT_SESSION_REQUIRED", args)
		}
		if name != "character-identity:read" {
			index := 0
			if name == "db:character-roster-commit" { index = 1 }
			for _, bad := range []any{nil, "", true, string([]byte{0xff})} {
				args := charactersTransportArgs(name, expected); args[index] = bad
				check(name, "PROJECT_CONTEXT_REQUIRED", args)
			}
			args := charactersTransportArgs(name, expected); args[index] = "/foreign"
			check(name, "PROJECT_CONTEXT_CHANGED", args)
		}
	}
	// Original register(...,0) has no expectedPath argument.
	check("character-identity:read", "INVALID_ARGUMENTS", []any{expected["projectPath"], expected})
	if begins != 0 { t.Fatal("malformed input borrowed", begins) }
}

func TestCharactersHTTPAuthOwnershipStaleAndUnbound(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	otherCookie, otherCSRF := bootstrap(t, server)
	_, repository := charactersTransportDB(t, true)
	expected := draftContext("p", "l", "/synthetic/c")
	owner := draftOwner(cookie, "synthetic-tab-0001")
	begins := 0
	if err := server.BindCharacters(CharactersProviderFunc(func(_ context.Context, caller Caller, raw any) (*store.Characters, func(), error) {
		begins++
		if caller != owner { return nil, nil, novelruntime.ErrNotOwned }
		if !reflect.DeepEqual(raw, expected) { return nil, nil, security.NewHTTPError(409, "PROJECT_SESSION_STALE") }
		return repository, func(){}, nil
	})); err != nil { t.Fatal(err) }
	body, err := json.Marshal(map[string]any{"args": []any{expected}})
	if err != nil { t.Fatal(err) }
	for _, row := range []struct{ headers map[string]string; status int; code string }{
		{map[string]string{"X-Novel-User":""},401,"AUTHENTICATION_REQUIRED"},
		{map[string]string{"X-CSRF-Token":"bad"},403,"CSRF_DENIED"},
		{map[string]string{"X-Client-Id":""},400,"CLIENT_ID_REQUIRED"},
		{map[string]string{"Origin":"https://foreign.invalid"},403,"ORIGIN_DENIED"},
		{map[string]string{"Sec-Fetch-Site":"cross-site"},403,"CROSS_SITE_DENIED"},
	} { requireDraftHTTPError(t, rpc(t, server, cookie, csrf, "character-identity:read", string(body), row.headers), row.status, row.code) }
	if begins != 0 { t.Fatal("HTTP rejection borrowed", begins) }
	for _, name := range charactersTransportOperations {
		for _, response := range []struct{ foreign bool; tab string }{{false,"synthetic-other-tab"},{true,owner.ClientID}} {
			c, token := cookie, csrf
			if response.foreign { c, token = otherCookie, otherCSRF }
			got := draftRPC(t, server, c, token, response.tab, name, charactersTransportArgs(name, expected)...)
			if name == "db:character-roster-commit" { requireDraftFailure(t, got, "PROJECT_SESSION_NOT_OWNED") } else { requireCharactersGenericError(t, got.Code, got.Body.String(), "PROJECT_SESSION_NOT_OWNED") }
		}
		stale := draftContext("p", "old", "/synthetic/c")
		got := draftRPC(t, server, cookie, csrf, owner.ClientID, name, charactersTransportArgs(name, stale)...)
		if name == "db:character-roster-commit" { requireDraftFailure(t, got, "PROJECT_SESSION_STALE") } else { requireDraftHTTPError(t, got,409,"PROJECT_SESSION_STALE") }
	}
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, owner.ClientID, "character-proposal:list-pending-finalized", expected),501,"GO_OPERATION_NOT_MIGRATED")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, owner.ClientID, "db:character-forged", expected),404,"OPERATION_NOT_EXPOSED")
}

func TestCharactersProviderFailureFiniteRedactionAndRelease(t *testing.T) {
	var nilPublic *security.HTTPError
	expected := draftContext("p", "l", "/synthetic/c")
	for _, row := range []struct{ err error; code string; public bool }{
		{nilPublic,"CHARACTER_OPERATION_FAILED",false},
		{errors.New("private /author/book.sqlite SQL token"),"CHARACTER_OPERATION_FAILED",false},
		{errors.New("CHARACTER_PRIVATE_SECRET"),"CHARACTER_OPERATION_FAILED",false},
		{&security.HTTPError{Status:409,Code:"CHARACTER_PRIVATE_SECRET",Message:"private"},"CHARACTER_OPERATION_FAILED",false},
		{&security.HTTPError{Status:409,Code:"PROJECT_SESSION_NOT_OWNED",Message:"private"},"PROJECT_SESSION_NOT_OWNED",true},
		{context.Canceled,"REQUEST_CANCELED",false},
		{context.DeadlineExceeded,"REQUEST_DEADLINE_EXCEEDED",false},
		{errors.New("CHARACTER_ID_META_MISSING"),"CHARACTER_ID_META_MISSING",false},
		{errors.New("CHARACTER_INPUT_UNSUPPORTED"),"CHARACTER_INPUT_UNSUPPORTED",false},
	} {
		server := fixture(t)
		releases := 0
		if err := server.BindCharacters(CharactersProviderFunc(func(context.Context, Caller, any) (*store.Characters, func(), error) { return nil, func(){releases++}, row.err })); err != nil { t.Fatal(err) }
		cookie, csrf := bootstrap(t, server)
		for _, name := range charactersTransportOperations {
			got := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", name, charactersTransportArgs(name, expected)...)
			if strings.Contains(got.Body.String(),"private") || strings.Contains(got.Body.String(),"PRIVATE_SECRET") || strings.Contains(got.Body.String(),"book.sqlite") { t.Fatal("diagnostic leak",got.Body.String()) }
			if name == "db:character-roster-commit" { requireDraftFailure(t,got,row.code) } else if row.public { requireDraftHTTPError(t,got,409,row.code) } else { requireCharactersGenericError(t,got.Code,got.Body.String(),row.code) }
		}
		if releases != 4 { t.Fatal("provider error leaked hold",releases) }
	}
}

func TestCharactersClosedDatabaseFailureReleased(t *testing.T) {
	server := fixture(t)
	db, repository := charactersTransportDB(t,true)
	if err := db.Close(); err != nil { t.Fatal(err) }
	releases := 0
	if err := server.BindCharacters(CharactersProviderFunc(func(context.Context, Caller, any) (*store.Characters,func(),error){return repository,func(){releases++},nil})); err != nil { t.Fatal(err) }
	cookie,csrf := bootstrap(t,server)
	expected := draftContext("p","l","/synthetic/c")
	for _,name := range charactersTransportOperations[:3] {
		got := draftRPC(t,server,cookie,csrf,"synthetic-tab-0001",name,charactersTransportArgs(name,expected)...)
		requireCharactersGenericError(t,got.Code,got.Body.String(),"CHARACTER_OPERATION_FAILED")
	}
	if releases != 3 { t.Fatal(releases) }
}
