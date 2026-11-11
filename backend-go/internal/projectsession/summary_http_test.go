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

func TestSummaryRealPrivateSessionHTTPAndImmutableSource(t *testing.T) {
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
		seed.Close()
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(prose))
	hash := hex.EncodeToString(sum[:])
	if _, err := seed.Exec("UPDATE drafts SET status='finalized' WHERE id=1"); err != nil {
		seed.Close()
		t.Fatal(err)
	}
	if _, err := seed.Exec(`INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,chapter_title,content_hash,content_revision,content_snapshot,target_file_name) VALUES('synthetic-summary-final',1,1,'合成不可变定稿',?,1,?,'synthetic.txt')`, hash, prose); err != nil {
		seed.Close()
		t.Fatal(err)
	}
	if _, err := seed.Exec(`INSERT INTO summary_snapshots(draft_id,chapter_number,chapter_notes,source_finalization_id,source_content_hash,projection_generation) VALUES(1,1,'合成已定稿章记','synthetic-summary-final',?,0)`, hash); err != nil {
		seed.Close()
		t.Fatal(err)
	}
	// Synthetic, immutable identity snapshot with no named characters. This is test
	// setup only, not a newly admitted generation/finalization write API.
	raw, err := json.Marshal(map[string]any{"kind": "finalized-character-snapshot:v1", "source": map[string]any{"draftId": 1, "finalizationId": "synthetic-summary-final", "chapterNumber": 1, "contentHash": hash}, "identityRevision": 0, "characters": []any{}})
	if err != nil {
		seed.Close()
		t.Fatal(err)
	}
	frozenHash := sha256.Sum256(raw)
	if _, err := seed.Exec(`INSERT INTO character_identity_proposals(proposal_id,source_key,source_hash,raw_value,candidate_ids_json) VALUES('fcs:synthetic-summary-final','finalized-character-snapshot:v1',?,?,'[]')`, hex.EncodeToString(frozenHash[:]), string(raw)); err != nil {
		seed.Close()
		t.Fatal(err)
	}
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
	provider := transport.SummaryProviderFunc(func(ctx context.Context, c transport.Caller, raw any) (*store.Summary, func(), error) {
		expected, err := ParseContext(raw)
		if err != nil {
			return nil, nil, err
		}
		return manager.BeginSummary(ctx, Owner{SessionID: c.SessionID, ClientID: c.ClientID}, expected)
	})
	if err := server.BindSummary(provider); err != nil {
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
	cookie, csrf := bootstrap()
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
	const tab = "synthetic-summary-tab-0001"
	if status, _ := invoke(cookie, csrf, tab, "config:get", []any{}); status != 200 {
		t.Fatal(status)
	}
	owner := Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}
	expected, err := manager.OpenScratchCopy(context.Background(), owner, project, contract, t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	call := func(op string, args ...any) any {
		args = append(args, project.Path(), expected)
		status, wire := invoke(cookie, csrf, tab, op, args)
		if status != 200 {
			t.Fatal(op, status, wire)
		}
		return wire["result"]
	}
	if latest := call("db:get-latest-summary"); latest != nil {
		t.Fatal("formal projection leaked to legacy latest", latest)
	}
	const states = "  合成作者快照🙂\n<&>\u2028\u2029  "
	saved := call("db:save-summary-snapshot", 1, states).(map[string]any)
	if saved["success"] != true {
		t.Fatal(saved)
	}
	latest := call("db:get-latest-summary").(map[string]any)
	if latest["characterStates"] != states || latest["chapterNumber"] != float64(1) {
		t.Fatal(latest)
	}
	refreshed, err := manager.OpenScratchCopy(context.Background(), owner, project, contract, t.TempDir(), 32<<20)
	if err != nil || refreshed != expected {
		t.Fatal("refresh lost private edits", refreshed, err)
	}
	valid := call("db:continuity-read-source", 1).(map[string]any)
	if valid["status"] != "valid" {
		t.Fatal(valid)
	}
	snapshot := valid["snapshot"].(map[string]any)
	if snapshot["content"] != prose || snapshot["chapterTitle"] != "合成不可变定稿" {
		t.Fatal(snapshot)
	}
	rows := call("db:continuity-list-before", 2).([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["sourceStatus"] != "current" {
		t.Fatal(rows)
	}
	for _, op := range []string{"db:continuity-save-finalized", "db:continuity-save-character-state-candidates"} {
		denied := call(op, map[string]any{"source": "untrusted-renderer"}).(map[string]any)
		if denied["success"] != false || denied["error"] != "Error: GENERATION_FINALIZATION_ADMISSION_REQUIRED" {
			t.Fatal(denied)
		}
	}
	// Explicit mutation of the captured TEST database simulates invalidation, not
	// renderer authority. Original author source is never opened for these calls.
	repo, done, err := manager.BeginSummary(context.Background(), owner, expected)
	if err != nil || repo == nil {
		t.Fatal(err)
	}
	if _, err := manager.mounts[expected.ProjectPath].db.Exec("UPDATE continuity_projection_meta SET generation=1,stale_from_chapter=1 WHERE id='main'"); err != nil {
		done()
		t.Fatal(err)
	}
	done()
	rows = call("db:continuity-list-before", 2).([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["sourceStatus"] != "stale" {
		t.Fatal(rows)
	}
	if call("db:continuity-read-source", 1).(map[string]any)["status"] != "valid" {
		t.Fatal("projection invalidation erased source")
	}
	repo, done, err = manager.BeginSummary(context.Background(), owner, expected)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.mounts[expected.ProjectPath].db.Exec("UPDATE finalization_outbox SET content_hash=? WHERE draft_id=1", "bad"); err != nil {
		done()
		t.Fatal(err)
	}
	done()
	if call("db:continuity-read-source", 1).(map[string]any)["status"] != "invalid" {
		t.Fatal("corrupt receipt accepted")
	}
	if call("db:get-latest-summary").(map[string]any)["characterStates"] != states {
		t.Fatal("legacy author snapshot overwritten")
	}
	otherCookie, otherCSRF := bootstrap()
	for _, other := range []struct {
		cookie    *http.Cookie
		csrf, tab string
	}{{cookie, csrf, "synthetic-summary-tab-0002"}, {otherCookie, otherCSRF, tab}} {
		status, _ := invoke(other.cookie, other.csrf, other.tab, "db:continuity-read-source", []any{1, project.Path(), expected})
		if status == 200 {
			t.Fatal("cross-owner source leak")
		}
	}
	if status, _ := invoke(cookie, "bad-csrf", tab, "db:save-summary-snapshot", []any{1, "must not save", project.Path(), expected}); status != 403 {
		t.Fatal(status)
	}
	_, done, err = manager.BeginSummary(context.Background(), owner, expected)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(manager.Release(owner, expected), novelruntime.ErrBusy) {
		t.Fatal("released active continuity reader")
	}
	if !errors.Is(manager.Close(), novelruntime.ErrBusy) {
		t.Fatal("closed active continuity reader")
	}
	done()
	done()
	if err := manager.Release(owner, expected); err != nil {
		t.Fatal(err)
	}
	if status, _ := invoke(cookie, csrf, tab, "db:get-latest-summary", []any{project.Path(), expected}); status == 200 {
		t.Fatal("revoked source lease reused")
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("source fixture changed", err)
	}
}
