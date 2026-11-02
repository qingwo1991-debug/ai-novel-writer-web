package transport

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

const recoveryTransportSchema = `
CREATE TABLE blueprints (
 chapter_number INTEGER PRIMARY KEY, title TEXT NOT NULL, role TEXT, purpose TEXT,
 key_events TEXT, characters TEXT, suspense_hook TEXT, user_guidance TEXT,
 notes TEXT, notes_updated_at TEXT, updated_at TEXT
);
CREATE TABLE recovery_candidates (
 candidate_id TEXT PRIMARY KEY, run_id TEXT NOT NULL, step_id TEXT NOT NULL,
 project_id TEXT NOT NULL, chapter_number INTEGER NOT NULL, chapter_title TEXT NOT NULL,
 source_snapshot TEXT NOT NULL, source_hash TEXT NOT NULL, source_draft_id INTEGER,
 source_draft_version INTEGER, source_draft_identity_captured INTEGER NOT NULL DEFAULT 0,
 visible_text TEXT NOT NULL, content_hash TEXT NOT NULL, failure_code TEXT NOT NULL,
 failure_reason TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
 replaces_candidate_id TEXT, created_at TEXT NOT NULL DEFAULT 'synthetic-created', resolved_at TEXT
);`

func recoveryTransportDB(t *testing.T) (*sql.DB, *store.Recovery) {
	t.Helper()
	db, _ := transportDraftDB(t)
	draftExec(t, db, recoveryTransportSchema)
	draftExec(t, db, `INSERT INTO blueprints(chapter_number,title,role,purpose,key_events,characters,suspense_hook,user_guidance)
 VALUES(1,'第一章','开篇','目标','事件','["甲"]','','')`)
	return db, store.NewRecovery(db)
}

func recoveryTransportInput() map[string]any {
	return map[string]any{
		"runId": "run-1", "stepId": "step-1", "chapterNumber": float64(1), "chapterTitle": " 第一章 ",
		"source": map[string]any{"chapterNumber": float64(1), "title": "第一章", "role": "开篇", "purpose": "目标", "keyEvents": "事件", "characters": []any{"甲"}},
		"sourceDraft": nil, "visibleText": " <think>private model reasoning</think>正文😀 ",
		"failureCode": " INTERRUPTED ", "failureReason": " 连接断开 ",
	}
}

func TestRecoveryHTTPRoundTripOriginalEnvelopesAndFiltering(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	db, repository := recoveryTransportDB(t)
	expected := draftContext("frozen-project", "frozen-lease", "/synthetic/recovery")
	owner := draftOwner(cookie, "synthetic-tab-0001")
	var begins, releases atomic.Int64
	provider := RecoveryProviderFunc(func(_ context.Context, caller Caller, raw any) (*store.Recovery, func(), error) {
		begins.Add(1)
		if caller != owner || !reflect.DeepEqual(raw, expected) {
			return nil, nil, novelruntime.ErrNotOwned
		}
		return repository, func() {
			// A successful record must already be committed when the hold is
			// returned, not released immediately after borrowing the repository.
			var count int
			if err := db.QueryRow("SELECT count(*) FROM recovery_candidates").Scan(&count); err != nil || count < 1 {
				t.Errorf("hold released before repository finished: %d %v", count, err)
			}
			releases.Add(1)
		}, nil
	})
	if err := server.BindRecovery(provider); err != nil {
		t.Fatal(err)
	}
	call := func(operation string, args ...any) any {
		return draftResult(t, draftRPC(t, server, cookie, csrf, owner.ClientID, operation, args...))
	}
	input := recoveryTransportInput()
	result := call("db:recovery-candidate-record", input, expected["projectPath"], expected).(map[string]any)
	if result["success"] != true || len(result) != 2 {
		t.Fatal(result)
	}
	candidate := result["candidate"].(map[string]any)
	if candidate["projectId"] != "frozen-project" || candidate["visibleText"] != "正文😀" || candidate["sourceCurrent"] != true || candidate["status"] != "pending" {
		t.Fatal(candidate)
	}
	if len(candidate) != 16 || candidate["source"] != nil || candidate["sourceDraft"] != nil || candidate["source_snapshot"] != nil {
		t.Fatal("private source data or wrong candidate shape", candidate)
	}
	if _, exists := input["projectId"]; exists {
		t.Fatal("renderer-owned map mutated")
	}
	id := candidate["candidateId"].(string)
	listed := call("db:recovery-candidate-list", expected["projectPath"], expected)
	if !reflect.DeepEqual(listed, []any{candidate}) {
		t.Fatal("list must remain bare array", listed)
	}
	updated := call("db:recovery-candidate-update", id, "<THINK>secret</THINK>修改正文", expected["projectPath"], expected).(map[string]any)
	if updated["success"] != true || updated["candidate"].(map[string]any)["visibleText"] != "修改正文" {
		t.Fatal(updated)
	}
	resolved := call("db:recovery-candidate-resolve", id, "continued", expected["projectPath"], expected)
	if !reflect.DeepEqual(resolved, map[string]any{"success": true}) {
		t.Fatal(resolved)
	}
	if got := call("db:recovery-candidate-list", expected["projectPath"], expected); !reflect.DeepEqual(got, []any{}) {
		t.Fatal(got)
	}
	requireDraftFailure(t, draftRPC(t, server, cookie, csrf, owner.ClientID, "db:recovery-candidate-update", id, "not pending", expected["projectPath"], expected), "RECOVERY_CANDIDATE_NOT_PENDING")
	if begins.Load() != releases.Load() || begins.Load() != 6 {
		t.Fatal("whole-call release mismatch", begins.Load(), releases.Load())
	}
}

func TestRecoveryBindingAtomicAndNoOtherAuthority(t *testing.T) {
	provider := RecoveryProviderFunc(func(context.Context, Caller, any) (*store.Recovery, func(), error) { return nil, nil, nil })
	for _, collision := range []string{"db:recovery-candidate-record", "db:recovery-candidate-list", "db:recovery-candidate-update", "db:recovery-candidate-resolve"} {
		server := fixture(t)
		if err := server.Bind(collision, func(context.Context, Caller, []any) (any, error) { return nil, nil }); err != nil {
			t.Fatal(err)
		}
		if err := server.BindRecovery(provider); err == nil || err.Error() != "OPERATION_ALREADY_BOUND" || len(server.handlers) != 1 {
			t.Fatal("partial binding on collision", collision, err, server.handlers)
		}
	}
	server := fixture(t)
	delete(server.registry.Operations, "db:recovery-candidate-resolve")
	if err := server.BindRecovery(provider); err == nil || err.Error() != "OPERATION_NOT_EXPOSED" || len(server.handlers) != 0 {
		t.Fatal("partial binding outside inventory", err, server.handlers)
	}
	server = fixture(t)
	if err := server.BindRecovery(nil); err == nil || len(server.handlers) != 0 {
		t.Fatal("nil provider bound", err)
	}
	if err := server.BindRecovery(provider); err != nil || len(server.handlers) != 4 {
		t.Fatal(err, server.handlers)
	}
	cookie, csrf := bootstrap(t, server)
	for _, operation := range []string{"db:recovery-candidate-promote", "web:recovery-candidate-record"} {
		requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", operation), 404, "OPERATION_NOT_EXPOSED")
	}
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-list-all"), 501, "GO_OPERATION_NOT_MIGRATED")
}

func TestRecoveryRejectsMalformedBodiesBeforeBorrowing(t *testing.T) {
	server := fixture(t)
	var begins atomic.Int64
	if err := server.BindRecovery(RecoveryProviderFunc(func(context.Context, Caller, any) (*store.Recovery, func(), error) {
		begins.Add(1)
		return nil, nil, nil
	})); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	expected := draftContext("project", "lease", "/synthetic/recovery")
	path := expected["projectPath"]
	for _, mutation := range []func(map[string]any){
		func(m map[string]any) { m["projectId"] = "forged" },
		func(m map[string]any) { m["status"] = "continued" },
		func(m map[string]any) { m["sourceHash"] = strings.Repeat("a", 64) },
		func(m map[string]any) { m["visibleText"] = nil },
		func(m map[string]any) { m["source"].(map[string]any)["private"] = true },
		func(m map[string]any) { m["source"].(map[string]any)["characters"] = []any{1} },
	} {
		input := recoveryTransportInput()
		mutation(input)
		requireDraftFailure(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:recovery-candidate-record", input, path, expected), "INVALID_ARGUMENTS")
	}
	for _, row := range []struct { name string; args []any; code string }{
		{"db:recovery-candidate-record", []any{recoveryTransportInput(), path}, "INVALID_ARGUMENTS"},
		{"db:recovery-candidate-record", []any{recoveryTransportInput(), path, nil}, "PROJECT_SESSION_REQUIRED"},
		{"db:recovery-candidate-record", []any{recoveryTransportInput(), path, map[string]any{"projectId": "project", "leaseId": "lease", "projectPath": path, "extra": true}}, "PROJECT_SESSION_REQUIRED"},
		{"db:recovery-candidate-update", []any{"id", "body", "/foreign", expected}, "PROJECT_CONTEXT_CHANGED"},
		{"db:recovery-candidate-resolve", []any{"id", "pending", path, expected}, "RECOVERY_CANDIDATE_ACTION_INVALID"},
		{"db:recovery-candidate-update", []any{float64(1), "body", path, expected}, "INVALID_ARGUMENTS"},
		{"db:recovery-candidate-resolve", []any{"id", "discarded", nil, expected}, "PROJECT_CONTEXT_REQUIRED"},
	} {
		requireDraftFailure(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", row.name, row.args...), row.code)
	}
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:recovery-candidate-list", path, nil), 400, "PROJECT_SESSION_REQUIRED")
	// Direct-handler calls cover non-JSON native values and invalid UTF-8 that
	// encoding/json would sanitize before HTTP dispatch.
	for _, bad := range []any{math.NaN(), math.Inf(1), float64(1.5), float64(9007199254740992), "1", true, nil, json.Number("1.00000000000000001")} {
		input := recoveryTransportInput()
		input["chapterNumber"] = bad
		value, err := server.handlers["db:recovery-candidate-record"](context.Background(), Caller{}, []any{input, path, expected})
		if err != nil || value.(map[string]any)["success"] != false {
			t.Fatal(bad, value, err)
		}
	}
	input := recoveryTransportInput()
	input["visibleText"] = string([]byte{0xff})
	value, err := server.handlers["db:recovery-candidate-record"](context.Background(), Caller{}, []any{input, path, expected})
	if err != nil || value.(map[string]any)["error"] != "Error: INVALID_ARGUMENTS" || begins.Load() != 0 {
		t.Fatal(value, err, begins.Load())
	}
}

func TestRecoveryHTTPAuthOwnershipAndStaleSource(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	otherCookie, otherCSRF := bootstrap(t, server)
	db, repository := recoveryTransportDB(t)
	expected := draftContext("project", "lease", "/synthetic/recovery")
	owner := draftOwner(cookie, "synthetic-tab-0001")
	var begins atomic.Int64
	if err := server.BindRecovery(RecoveryProviderFunc(func(_ context.Context, caller Caller, raw any) (*store.Recovery, func(), error) {
		begins.Add(1)
		if caller != owner || !reflect.DeepEqual(raw, expected) {
			return nil, nil, novelruntime.ErrNotOwned
		}
		return repository, func() {}, nil
	})); err != nil {
		t.Fatal(err)
	}
	args := []any{expected["projectPath"], expected}
	body, err := json.Marshal(map[string]any{"args": args})
	if err != nil { t.Fatal(err) }
	for _, row := range []struct { headers map[string]string; status int; code string }{
		{map[string]string{"X-Novel-User": ""}, 401, "AUTHENTICATION_REQUIRED"},
		{map[string]string{"X-Client-Id": ""}, 400, "CLIENT_ID_REQUIRED"},
		{map[string]string{"X-CSRF-Token": "bad"}, 403, "CSRF_DENIED"},
		{map[string]string{"Origin": "https://untrusted.invalid"}, 403, "ORIGIN_DENIED"},
		{map[string]string{"Sec-Fetch-Site": "cross-site"}, 403, "CROSS_SITE_DENIED"},
	} {
		requireDraftHTTPError(t, rpc(t, server, cookie, csrf, "db:recovery-candidate-list", string(body), row.headers), row.status, row.code)
	}
	if begins.Load() != 0 { t.Fatal("HTTP denial reached provider") }
	for _, row := range []struct { cookie *http.Cookie; csrf, tab string; context map[string]any }{
		{cookie, csrf, "synthetic-tab-other", expected},
		{otherCookie, otherCSRF, owner.ClientID, expected},
		{cookie, csrf, owner.ClientID, draftContext("project", "old-lease", "/synthetic/recovery")},
		{cookie, csrf, owner.ClientID, draftContext("foreign-project", "lease", "/synthetic/recovery")},
	} {
		response := draftRPC(t, server, row.cookie, row.csrf, row.tab, "db:recovery-candidate-record", recoveryTransportInput(), expected["projectPath"], row.context)
		requireDraftFailure(t, response, "PROJECT_SESSION_NOT_OWNED")
	}
	created := draftResult(t, draftRPC(t, server, cookie, csrf, owner.ClientID, "db:recovery-candidate-record", recoveryTransportInput(), expected["projectPath"], expected)).(map[string]any)
	id := created["candidate"].(map[string]any)["candidateId"]
	draftExec(t, db, "UPDATE blueprints SET purpose='changed' WHERE chapter_number=1")
	for _, row := range []struct { operation, text string }{{"db:recovery-candidate-update", "edited"}, {"db:recovery-candidate-resolve", "continued"}} {
		requireDraftFailure(t, draftRPC(t, server, cookie, csrf, owner.ClientID, row.operation, id, row.text, expected["projectPath"], expected), "RECOVERY_CANDIDATE_SOURCE_CHANGED")
	}
	listed := draftResult(t, draftRPC(t, server, cookie, csrf, owner.ClientID, "db:recovery-candidate-list", args...)).([]any)
	if len(listed) != 1 || listed[0].(map[string]any)["sourceCurrent"] != false { t.Fatal(listed) }
	resolved := draftResult(t, draftRPC(t, server, cookie, csrf, owner.ClientID, "db:recovery-candidate-resolve", id, "discarded", expected["projectPath"], expected))
	if !reflect.DeepEqual(resolved, map[string]any{"success": true}) { t.Fatal(resolved) }
}

func TestRecoveryProviderErrorsReleaseAndRedact(t *testing.T) {
	expected := draftContext("project", "lease", "/synthetic/recovery")
	for _, failure := range []error{
		errors.New("private /author/book.sqlite SQL token"),
		errors.New("RECOVERY_CANDIDATE_PRIVATE_SECRET"),
		&security.HTTPError{Status: 409, Code: "RECOVERY_CANDIDATE_PRIVATE_SECRET", Message: "private provider message"},
		&security.HTTPError{Status: 409, Code: "PROJECT_SESSION_NOT_OWNED", Message: "private provider message"},
		novelruntime.ErrNotOwned, context.Canceled, context.DeadlineExceeded,
	} {
		server := fixture(t)
		var releases int
		if err := server.BindRecovery(RecoveryProviderFunc(func(context.Context, Caller, any) (*store.Recovery, func(), error) {
			return nil, func() { releases++ }, failure
		})); err != nil { t.Fatal(err) }
		cookie, csrf := bootstrap(t, server)
		for _, operation := range []string{"db:recovery-candidate-list", "db:recovery-candidate-record"} {
			args := []any{expected["projectPath"], expected}
			if operation == "db:recovery-candidate-record" { args = append([]any{recoveryTransportInput()}, args...) }
			response := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", operation, args...)
			if strings.Contains(response.Body.String(), "private") || strings.Contains(response.Body.String(), "PRIVATE_SECRET") || strings.Contains(response.Body.String(), "book.sqlite") {
				t.Fatal("provider diagnostic leaked", response.Body.String())
			}
			if operation == "db:recovery-candidate-record" { requireDraftFailure(t, response, recoveryCode(failure)) }
		}
		if releases != 2 { t.Fatal("provider returned hold plus error was not released", releases) }
	}
}

func TestRecoveryRecordCloneAndFrozenIdentity(t *testing.T) {
	server := fixture(t)
	_, repository := recoveryTransportDB(t)
	input := recoveryTransportInput()
	expected := draftContext("frozen-project", "lease", "/synthetic/recovery")
	if err := server.BindRecovery(RecoveryProviderFunc(func(_ context.Context, _ Caller, raw any) (*store.Recovery, func(), error) {
		// Provider receives a context clone; renderer mutations after parsing
		// cannot change the identity already injected into the record clone.
		expected["projectId"] = "renderer-mutated"
		input["runId"] = "renderer-mutated"
		input["source"].(map[string]any)["title"] = "renderer-mutated"
		if raw.(map[string]any)["projectId"] != "frozen-project" { t.Fatal(raw) }
		return repository, func() {}, nil
	})); err != nil { t.Fatal(err) }
	value, err := server.handlers["db:recovery-candidate-record"](context.Background(), Caller{}, []any{input, expected["projectPath"], expected})
	if err != nil { t.Fatal(err) }
	result := value.(map[string]any)
	if result["success"] != true { t.Fatal(result) }
	candidate := result["candidate"].(map[string]any)
	if candidate["projectId"] != "frozen-project" || candidate["runId"] != "run-1" || candidate["sourceCurrent"] != true { t.Fatal(candidate) }
}
