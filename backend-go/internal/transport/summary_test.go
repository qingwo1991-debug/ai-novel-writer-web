package transport

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// Isolated transport fixture using the actual baseline summary columns. This
// fixture is not project admission or a replacement for manager integration.
const summaryTransportSchema = `
CREATE TABLE summary_snapshots (
 id INTEGER PRIMARY KEY AUTOINCREMENT, draft_id INTEGER DEFAULT NULL,
 chapter_number INTEGER NOT NULL, character_states TEXT DEFAULT '',
 chapter_notes TEXT NOT NULL DEFAULT '', continuity_facts TEXT NOT NULL DEFAULT '[]',
 character_state_candidates TEXT NOT NULL DEFAULT '[]', source_finalization_id TEXT NOT NULL DEFAULT '',
 source_content_hash TEXT NOT NULL DEFAULT '', projection_generation INTEGER NOT NULL DEFAULT 0,
 created_at TEXT DEFAULT (datetime('now')),
 FOREIGN KEY (draft_id) REFERENCES drafts(id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX idx_summary_snapshots_draft ON summary_snapshots(draft_id) WHERE draft_id IS NOT NULL;
CREATE TABLE continuity_projection_meta (
 id TEXT PRIMARY KEY CHECK(id='main'), generation INTEGER NOT NULL DEFAULT 0 CHECK(generation>=0),
 stale_from_chapter INTEGER DEFAULT NULL CHECK(stale_from_chapter IS NULL OR stale_from_chapter>0)
);
INSERT INTO continuity_projection_meta(id) VALUES('main');
CREATE TABLE blueprints(chapter_number INTEGER PRIMARY KEY,title TEXT NOT NULL);
`

var summaryTransportOperations = []string{
	"db:save-summary-snapshot", "db:get-latest-summary", "db:continuity-list-before",
	"db:continuity-read-source", "db:continuity-save-finalized", "db:continuity-save-character-state-candidates",
}

func summaryTransportDB(t *testing.T) (*sql.DB, *store.Summary) {
	t.Helper()
	db, _ := transportDraftDB(t)
	draftExec(t, db, summaryTransportSchema)
	return db, store.NewSummary(db)
}

func summaryTransportSeed(t *testing.T, db *sql.DB) string {
	t.Helper()
	body := "定稿原文😀"
	digest := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(digest[:])
	draftSeed(t, db, 1, 1, 1, "finalized", body)
	draftSeed(t, db, 2, 2, 1, "finalized", "旧定稿")
	draftSeed(t, db, 3, 3, 1, "draft", "未定稿")
	draftExec(t, db, "INSERT INTO blueprints(chapter_number,title) VALUES(2,'旧标题')")
	draftExec(t, db, `INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,chapter_title,content_hash,content_snapshot) VALUES('receipt-1',1,1,'第一章',?,?)`, hash, body)
	draftExec(t, db, `INSERT INTO summary_snapshots(draft_id,chapter_number,chapter_notes,source_finalization_id,source_content_hash) VALUES(1,1,'连续性笔记','receipt-1',?)`, hash)
	return hash
}

func TestSummaryHTTPOriginalShapesAndHeldBorrow(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	db, repository := summaryTransportDB(t)
	expected := draftContext("project", "lease", "/synthetic/summary")
	owner := draftOwner(cookie, "synthetic-tab-0001")
	var begins, releases atomic.Int64
	if err := server.BindSummary(SummaryProviderFunc(func(_ context.Context, caller Caller, raw any) (*store.Summary, func(), error) {
		begins.Add(1)
		if caller != owner || !reflect.DeepEqual(raw, expected) {
			return nil, nil, novelruntime.ErrNotOwned
		}
		return repository, func() { releases.Add(1) }, nil
	})); err != nil {
		t.Fatal(err)
	}
	call := func(name string, prefix ...any) any {
		args := append(prefix, expected["projectPath"], expected)
		return draftResult(t, draftRPC(t, server, cookie, csrf, owner.ClientID, name, args...))
	}
	if got := call("db:get-latest-summary"); got != nil {
		t.Fatal("empty latest must be null", got)
	}
	if got := call("db:continuity-list-before", 2); !reflect.DeepEqual(got, []any{}) {
		t.Fatal("empty list must be array", got)
	}
	for _, chapter := range []int{-1, 0, 4} {
		if got := call("db:save-summary-snapshot", chapter, " 原始状态😀 "); !reflect.DeepEqual(got, map[string]any{"success": true}) {
			t.Fatal(got)
		}
	}
	hash := summaryTransportSeed(t, db)
	if got := call("db:get-latest-summary"); !reflect.DeepEqual(got, map[string]any{"chapterNumber": float64(4), "characterStates": " 原始状态😀 "}) {
		t.Fatal("formal rows must not replace latest legacy snapshot", got)
	}
	listed := call("db:continuity-list-before", 2).([]any)
	if len(listed) != 1 || listed[0].(map[string]any)["sourceStatus"] != "current" || listed[0].(map[string]any)["chapterNotes"] != "连续性笔记" {
		t.Fatal(listed)
	}
	wantSource := map[string]any{"status": "valid", "snapshot": map[string]any{
		"source": map[string]any{"draftId": float64(1), "chapterNumber": float64(1), "finalizationId": "receipt-1", "contentHash": hash},
		"chapterTitle": "第一章", "content": "定稿原文😀", "projectionGeneration": float64(0),
	}}
	if got := call("db:continuity-read-source", 1); !reflect.DeepEqual(got, wantSource) {
		t.Fatal(got)
	}
	if got := call("db:continuity-read-source", 2); !reflect.DeepEqual(got, map[string]any{"status": "legacy", "draftId": float64(2), "chapterNumber": float64(2), "chapterTitle": "旧标题", "content": "旧定稿"}) {
		t.Fatal(got)
	}
	for _, id := range []int{3, 999} {
		if got := call("db:continuity-read-source", id); !reflect.DeepEqual(got, map[string]any{"status": "invalid"}) {
			t.Fatal(got)
		}
	}
	draftExec(t, db, "UPDATE contents SET body='篡改' WHERE id=1")
	if got := call("db:continuity-read-source", 1); !reflect.DeepEqual(got, map[string]any{"status": "invalid"}) {
		t.Fatal(got)
	}
	for _, operation := range summaryTransportOperations[4:] {
		got := call(operation, map[string]any{"draftId": 1, "chapterNotes": "must not write"})
		if !reflect.DeepEqual(got, map[string]any{"success": false, "error": "Error: GENERATION_FINALIZATION_ADMISSION_REQUIRED"}) {
			t.Fatal(got)
		}
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM summary_snapshots").Scan(&count); err != nil || count != 4 {
		t.Fatal("denials wrote formal data", count, err)
	}
	if begins.Load() != releases.Load() || begins.Load() != 14 {
		t.Fatal("borrow/release mismatch", begins.Load(), releases.Load())
	}
}

func TestSummaryBindingAtomicAndNoOtherAuthority(t *testing.T) {
	provider := SummaryProviderFunc(func(context.Context, Caller, any) (*store.Summary, func(), error) { return nil, nil, nil })
	for _, name := range summaryTransportOperations {
		server := fixture(t)
		if err := server.Bind(name, func(context.Context, Caller, []any) (any, error) { return nil, nil }); err != nil {
			t.Fatal(err)
		}
		if err := server.BindSummary(provider); err == nil || err.Error() != "OPERATION_ALREADY_BOUND" || len(server.handlers) != 1 {
			t.Fatal("partial binding", name, err)
		}
		server = fixture(t)
		delete(server.registry.Operations, name)
		if err := server.BindSummary(provider); err == nil || err.Error() != "OPERATION_NOT_EXPOSED" || len(server.handlers) != 0 {
			t.Fatal("partial inventory binding", name, err)
		}
	}
	server := fixture(t)
	if err := server.BindSummary(nil); err == nil || len(server.handlers) != 0 {
		t.Fatal(err)
	}
	if err := server.BindSummary(provider); err != nil || len(server.handlers) != 6 {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-list-all"), 501, "GO_OPERATION_NOT_MIGRATED")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:summary-forged"), 404, "OPERATION_NOT_EXPOSED")
}

func TestSummaryMalformedInputNeverBorrows(t *testing.T) {
	server := fixture(t)
	var begins int
	if err := server.BindSummary(SummaryProviderFunc(func(context.Context, Caller, any) (*store.Summary, func(), error) {
		begins++
		return nil, nil, nil
	})); err != nil {
		t.Fatal(err)
	}
	expected := draftContext("project", "lease", "/synthetic/summary")
	path := expected["projectPath"]
	check := func(name string, code string, args ...any) {
		t.Helper()
		value, err := server.handlers[name](context.Background(), Caller{}, args)
		if strings.Contains(name, "continuity-save-") {
			if err != nil || !reflect.DeepEqual(value, map[string]any{"success": false, "error": "Error: " + code}) {
				t.Fatal(name, value, err)
			}
		} else if value != nil || err == nil || summaryCode(err) != code {
			t.Fatal(name, value, err)
		}
	}
	for _, bad := range []any{nil, true, "1", 1, math.NaN(), math.Inf(1), 1.5, float64(9007199254740992), float64(-9007199254740992), json.Number("1.00000000000000001")} {
		check("db:save-summary-snapshot", "INVALID_ARGUMENTS", bad, "states", path, expected)
	}
	for _, name := range summaryTransportOperations[2:4] {
		for _, bad := range []any{float64(0), float64(-1), nil, "1", json.Number("1.1")} {
			check(name, "INVALID_ARGUMENTS", bad, path, expected)
		}
	}
	check("db:save-summary-snapshot", "INVALID_ARGUMENTS", float64(1), nil, path, expected)
	check("db:save-summary-snapshot", "INVALID_ARGUMENTS", float64(1), string([]byte{0xff}), path, expected)
	check("db:save-summary-snapshot", "SUMMARY_INPUT_UNSUPPORTED", float64(1), strings.Repeat("s", (32<<20)+1), path, expected)
	for _, name := range summaryTransportOperations {
		prefix := []any{}
		switch name {
		case "db:save-summary-snapshot":
			prefix = []any{float64(1), "states"}
		case "db:continuity-list-before", "db:continuity-read-source", "db:continuity-save-finalized", "db:continuity-save-character-state-candidates":
			prefix = []any{float64(1)}
		}
		check(name, "INVALID_ARGUMENTS", prefix...)
		for _, badContext := range []any{nil, map[string]any{}, draftContext("", "lease", path.(string)), draftContext(string([]byte{0xff}), "lease", path.(string)), map[string]any{"projectId": "project", "leaseId": "lease", "projectPath": path, "extra": true}} {
			check(name, "PROJECT_SESSION_REQUIRED", append(append([]any{}, prefix...), path, badContext)...)
		}
		check(name, "PROJECT_CONTEXT_CHANGED", append(append([]any{}, prefix...), "/foreign", expected)...)
		check(name, "PROJECT_CONTEXT_REQUIRED", append(append([]any{}, prefix...), string([]byte{0xff}), expected)...)
	}
	if begins != 0 {
		t.Fatal("malformed input reached provider", begins)
	}
}

func TestSummaryHTTPAuthOwnershipAndStaleContext(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	otherCookie, otherCSRF := bootstrap(t, server)
	_, repository := summaryTransportDB(t)
	expected := draftContext("project", "lease", "/synthetic/summary")
	owner := draftOwner(cookie, "synthetic-tab-0001")
	var begins int
	if err := server.BindSummary(SummaryProviderFunc(func(_ context.Context, caller Caller, raw any) (*store.Summary, func(), error) {
		begins++
		if caller != owner {
			return nil, nil, novelruntime.ErrNotOwned
		}
		if !reflect.DeepEqual(raw, expected) {
			return nil, nil, security.NewHTTPError(409, "PROJECT_SESSION_STALE")
		}
		return repository, func() {}, nil
	})); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"args": []any{expected["projectPath"], expected}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		headers map[string]string
		status int
		code string
	}{
		{map[string]string{"X-Novel-User": ""}, 401, "AUTHENTICATION_REQUIRED"},
		{map[string]string{"X-Client-Id": ""}, 400, "CLIENT_ID_REQUIRED"},
		{map[string]string{"X-CSRF-Token": "bad"}, 403, "CSRF_DENIED"},
		{map[string]string{"Origin": "https://untrusted.invalid"}, 403, "ORIGIN_DENIED"},
		{map[string]string{"Sec-Fetch-Site": "cross-site"}, 403, "CROSS_SITE_DENIED"},
	} {
		requireDraftHTTPError(t, rpc(t, server, cookie, csrf, "db:get-latest-summary", string(body), row.headers), row.status, row.code)
	}
	if begins != 0 {
		t.Fatal("HTTP rejection borrowed", begins)
	}
	for _, operation := range summaryTransportOperations[4:] {
		requireDraftFailure(t, draftRPC(t, server, cookie, csrf, "synthetic-other-tab", operation, nil, expected["projectPath"], expected), "PROJECT_SESSION_NOT_OWNED")
		requireDraftFailure(t, draftRPC(t, server, otherCookie, otherCSRF, owner.ClientID, operation, nil, expected["projectPath"], expected), "PROJECT_SESSION_NOT_OWNED")
		requireDraftFailure(t, draftRPC(t, server, cookie, csrf, owner.ClientID, operation, nil, expected["projectPath"], draftContext("project", "old-lease", expected["projectPath"].(string))), "PROJECT_SESSION_STALE")
	}
}

func TestSummaryProviderErrorsAreFiniteRedactedAndReleased(t *testing.T) {
	expected := draftContext("project", "lease", "/synthetic/summary")
	var nilPublic *security.HTTPError
	for _, row := range []struct {
		err error
		code string
	}{
		{nilPublic, "SUMMARY_OPERATION_FAILED"},
		{errors.New("private /author/book.sqlite SQL token"), "SUMMARY_OPERATION_FAILED"},
		{errors.New("SUMMARY_PRIVATE_SECRET"), "SUMMARY_OPERATION_FAILED"},
		{&security.HTTPError{Status: 409, Code: "SUMMARY_PRIVATE_SECRET", Message: "private message"}, "SUMMARY_OPERATION_FAILED"},
		{&security.HTTPError{Status: 409, Code: "PROJECT_SESSION_NOT_OWNED", Message: "private message"}, "PROJECT_SESSION_NOT_OWNED"},
		{novelruntime.ErrNotOwned, "PROJECT_SESSION_NOT_OWNED"},
		{context.Canceled, "REQUEST_CANCELED"},
		{context.DeadlineExceeded, "REQUEST_DEADLINE_EXCEEDED"},
		{errors.New("SUMMARY_INPUT_UNSUPPORTED"), "SUMMARY_INPUT_UNSUPPORTED"},
		{errors.New("CONTINUITY_TARGET_CHAPTER_INVALID"), "CONTINUITY_TARGET_CHAPTER_INVALID"},
		{errors.New("CONTINUITY_SOURCE_DRAFT_INVALID"), "CONTINUITY_SOURCE_DRAFT_INVALID"},
	} {
		server := fixture(t)
		var releases int
		if err := server.BindSummary(SummaryProviderFunc(func(context.Context, Caller, any) (*store.Summary, func(), error) {
			return nil, func() { releases++ }, row.err
		})); err != nil {
			t.Fatal(err)
		}
		cookie, csrf := bootstrap(t, server)
		for _, name := range summaryTransportOperations {
			args := []any{}
			switch name {
			case "db:save-summary-snapshot":
				args = []any{1, "states"}
			case "db:continuity-list-before", "db:continuity-read-source", "db:continuity-save-finalized", "db:continuity-save-character-state-candidates":
				args = []any{1}
			}
			response := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", name, append(args, expected["projectPath"], expected)...)
			if strings.Contains(response.Body.String(), "private") || strings.Contains(response.Body.String(), "PRIVATE_SECRET") || strings.Contains(response.Body.String(), "book.sqlite") {
				t.Fatal("diagnostic leak", response.Body.String())
			}
			if strings.Contains(name, "continuity-save-") {
				requireDraftFailure(t, response, row.code)
			} else {
				if response.Code == 200 || !strings.Contains(response.Body.String(), `"code":"`+row.code+`"`) || strings.Contains(response.Body.String(), "success") {
					t.Fatal("root failure contract", response.Code, response.Body.String())
				}
			}
		}
		if releases != 6 {
			t.Fatal("hold plus error not released", releases)
		}
	}
}

func TestSummaryCapturedInputClonedContextAndHoldAfterCommit(t *testing.T) {
	server := fixture(t)
	db, repository := summaryTransportDB(t)
	expected := draftContext("frozen-project", "lease", "/synthetic/summary")
	args := []any{json.Number("-9007199254740991"), "captured states", expected["projectPath"], expected}
	var releases int
	if err := server.BindSummary(SummaryProviderFunc(func(_ context.Context, _ Caller, raw any) (*store.Summary, func(), error) {
		expected["projectId"] = "renderer mutation"
		args[0], args[1] = float64(88), "renderer mutation"
		if raw.(map[string]any)["projectId"] != "frozen-project" || len(raw.(map[string]any)) != 3 {
			t.Fatal("context not cloned", raw)
		}
		return repository, func() {
			var chapter int64
			var states string
			if err := db.QueryRow("SELECT chapter_number,character_states FROM summary_snapshots").Scan(&chapter, &states); err != nil || chapter != -9007199254740991 || states != "captured states" {
				t.Error("released before captured write committed", chapter, states, err)
			}
			releases++
		}, nil
	})); err != nil {
		t.Fatal(err)
	}
	value, err := server.handlers["db:save-summary-snapshot"](context.Background(), Caller{}, args)
	if err != nil || !reflect.DeepEqual(value, map[string]any{"success": true}) || releases != 1 {
		t.Fatal(value, err, releases)
	}
}

func TestSummaryClosedDatabaseSaveHasRootErrorAndReleases(t *testing.T) {
	server := fixture(t)
	db, repository := summaryTransportDB(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var releases int
	if err := server.BindSummary(SummaryProviderFunc(func(context.Context, Caller, any) (*store.Summary, func(), error) {
		return repository, func() { releases++ }, nil
	})); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	expected := draftContext("project", "lease", "/synthetic/summary")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:save-summary-snapshot", 1, "states", expected["projectPath"], expected), 500, "SUMMARY_OPERATION_FAILED")
	if releases != 1 {
		t.Fatal("store error leaked hold", releases)
	}
}

func TestSummaryUnavailableProviderFailsClosed(t *testing.T) {
	expected := draftContext("project", "lease", "/synthetic/summary")
	_, repository := summaryTransportDB(t)
	for _, repo := range []*store.Summary{nil, repository} {
		server := fixture(t)
		if err := server.BindSummary(SummaryProviderFunc(func(context.Context, Caller, any) (*store.Summary, func(), error) { return repo, nil, nil })); err != nil {
			t.Fatal(err)
		}
		value, err := server.handlers["db:get-latest-summary"](context.Background(), Caller{}, []any{expected["projectPath"], expected})
		if value != nil || err == nil || err.Error() != "SUMMARY_PROVIDER_UNAVAILABLE" {
			t.Fatal(value, err)
		}
	}
}
