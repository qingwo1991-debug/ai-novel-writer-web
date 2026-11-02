package projectsession

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
	novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

func TestCharactersActualPrivateSessionSourceProofAndIdentityWire(t *testing.T) {
	project, contract, source := sessionFixture(t)
	dsn, err := store.ExistingDSN(source)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	var prose string
	if err := seed.QueryRow("SELECT body FROM contents WHERE id=1").Scan(&prose); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(prose))
	hash := hex.EncodeToString(digest[:])
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := seed.Exec(query, args...); err != nil {
			t.Fatal(query, err)
		}
	}
	exec("INSERT INTO characters(character_id,name,role,static_provenance,identity_revision,retired,cs_location,cs_updated_at_chapter) VALUES('synthetic-active','合成角色😀','protagonist','{\"kind\":\"legacy\"}',0,0,'冻结地点',1)")
	exec("INSERT INTO characters(character_id,name,role,static_provenance,identity_revision,retired) VALUES('synthetic-retired','合成旧角色','minor','{\"kind\":\"legacy\"}',0,1)")
	exec("INSERT INTO character_aliases VALUES('synthetic-active','合成别名','synthetic-source',0,NULL)")
	exec("UPDATE drafts SET status='finalized' WHERE id=1")
	exec("INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,chapter_title,content_hash,content_revision,content_snapshot,target_file_name) VALUES('synthetic-character-final',1,1,'合成冻结来源',?,1,?,'synthetic.txt')", hash, prose)
	exec("INSERT INTO summary_snapshots(draft_id,chapter_number,source_finalization_id,source_content_hash,projection_generation) VALUES(1,1,'synthetic-character-final',?,0)", hash)
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open("../../../web/operations.json")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := policy.Load(file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	server := transport.New(registry, security.NewOrigins("http://synthetic.test"), "synthetic")
	defer server.Close()
	manager := New()
	defer manager.Close()
	var captured *sql.DB
	if err := server.BindCharacters(transport.CharactersProviderFunc(func(ctx context.Context, c transport.Caller, raw any) (*store.Characters, func(), error) {
		expected, err := ParseContext(raw)
		if err != nil {
			return nil, nil, err
		}
		repo, done, err := manager.BeginCharacters(ctx, Owner{c.SessionID, c.ClientID}, expected)
		if err == nil {
			captured = manager.mounts[expected.ProjectPath].db
		}
		return repo, done, err
	})); err != nil {
		t.Fatal(err)
	}
	var caller transport.Caller
	if err := server.Bind("config:get", func(_ context.Context, c transport.Caller, _ []any) (any, error) { caller = c; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	bootstrap := func() (*http.Cookie, string) {
		r := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
		r.Header.Set("X-Novel-User", "synthetic-author")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		var result struct {
			Token string `json:"csrfToken"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return w.Result().Cookies()[0], result.Token
	}
	invoke := func(cookie *http.Cookie, csrf, tab, op string, args []any) (int, map[string]any) {
		body, err := json.Marshal(map[string]any{"args": args})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/operations/"+op, bytes.NewReader(body))
		r.AddCookie(cookie)
		r.Header.Set("X-Novel-User", "synthetic-author")
		r.Header.Set("X-Client-Id", tab)
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Origin", "http://synthetic.test")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return w.Code, result
	}
	cookie, csrf := bootstrap()
	const tab = "synthetic-characters-tab-0001"
	if status, _ := invoke(cookie, csrf, tab, "config:get", []any{}); status != 200 {
		t.Fatal(status)
	}
	owner := Owner{caller.SessionID, caller.ClientID}
	expected, err := manager.OpenScratchCopy(context.Background(), owner, project, contract, t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	call := func(op string, args ...any) any {
		if op != "character-identity:read" {
			args = append(args, project.Path())
		}
		args = append(args, expected)
		status, wire := invoke(cookie, csrf, tab, op, args)
		if status != 200 {
			t.Fatal(op, status, wire)
		}
		return wire["result"]
	}
	all := call("db:character-get-all").([]any)
	if len(all) != 1 || all[0].(map[string]any)["characterId"] != "synthetic-active" {
		t.Fatal(all)
	}
	identity := call("character-identity:read").(map[string]any)
	chars := identity["characters"].([]any)
	if len(chars) != 2 {
		t.Fatal(identity)
	}
	first := chars[0].(map[string]any)
	if first["fields"] == nil || first["retired"] != false || first["provenance"] == nil {
		t.Fatal("public identity DTO was replaced by raw SQL rows", identity)
	}
	if _, present := identity["proposals"]; present {
		t.Fatal("internal proposals leaked into public identity DTO", identity)
	}
	roster := call("db:character-roster-read").(map[string]any)
	if len(roster["entries"].([]any)) != 1 || len(roster["aliases"].([]any)) != 1 {
		t.Fatal(roster)
	}
	// Mutation below is synthetic test setup inside the captured scratch, not a new
	// author/generation write API. A valid epoch is supplied by the admitted lease.
	setProvenance := func(generation int) {
		raw, err := json.Marshal(map[string]any{"location": map[string]any{"kind": "derived", "revision": 1, "source": map[string]any{"draftId": 1, "finalizationId": "synthetic-character-final", "chapterNumber": 1, "contentHash": hash}, "sourceOrder": map[string]any{"chapterNumber": 1, "authoritativeFinalizationRevision": 1, "continuityEpoch": expected.ProjectID + ":" + string(rune('0'+generation))}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := captured.Exec("UPDATE characters SET cs_provenance=? WHERE character_id='synthetic-active'", string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	grants := func() []any {
		return call("db:character-roster-read").(map[string]any)["currentDerivedFields"].(map[string]any)["synthetic-active"].([]any)
	}
	setProvenance(0)
	if got := grants(); len(got) != 1 || got[0] != "location" {
		t.Fatal(got)
	}
	if _, err := captured.Exec("UPDATE continuity_projection_meta SET generation=1,stale_from_chapter=1 WHERE id='main'"); err != nil {
		t.Fatal(err)
	}
	if got := grants(); len(got) != 0 {
		t.Fatal("stale projection received a derived field grant", got)
	}
	if _, err := captured.Exec("UPDATE summary_snapshots SET projection_generation=1 WHERE draft_id=1"); err != nil {
		t.Fatal(err)
	}
	if got := grants(); len(got) != 0 {
		t.Fatal("old epoch received a derived field grant", got)
	}
	setProvenance(1)
	if got := grants(); len(got) != 1 {
		t.Fatal(got)
	}
	if _, err := captured.Exec("UPDATE contents SET body='synthetic-body-replaced' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if got := grants(); len(got) != 0 {
		t.Fatal("body mismatch received a derived field grant", got)
	}
	for _, intent := range []string{"manual_edit", "initialize"} {
		result := call("db:character-roster-commit", map[string]any{"intent": intent}).(map[string]any)
		if result["success"] != false {
			t.Fatal(result)
		}
	}
	other, otherCSRF := bootstrap()
	if status, _ := invoke(other, otherCSRF, tab, "character-identity:read", []any{expected}); status == 200 {
		t.Fatal("foreign cookie acquired identity")
	}
	if status, _ := invoke(cookie, csrf, tab+"-other", "db:character-roster-read", []any{project.Path(), expected}); status == 200 {
		t.Fatal("foreign tab acquired roster")
	}
	if status, _ := invoke(cookie, "bad", tab, "character-identity:read", []any{expected}); status != 403 {
		t.Fatal("CSRF", status)
	}
	_, done, err := manager.BeginCharacters(context.Background(), owner, expected)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(owner, expected); !errors.Is(err, novelruntime.ErrBusy) {
		t.Fatal(err)
	}
	if err := manager.Close(); !errors.Is(err, novelruntime.ErrBusy) {
		t.Fatal(err)
	}
	done()
	done()
	if err := manager.Release(owner, expected); err != nil {
		t.Fatal(err)
	}
	if status, _ := invoke(cookie, csrf, tab, "character-identity:read", []any{expected}); status == 200 {
		t.Fatal("revoked identity read admitted")
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("source fixture changed: %v", err)
	}
}
