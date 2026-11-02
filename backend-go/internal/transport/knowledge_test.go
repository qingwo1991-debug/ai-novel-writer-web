package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgequery"
	novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// Synthetic adopted private views only: this provider does not pretend to open or
// durably publish an author project, and every HTTP request still passes real
// bootstrap cookie/CSRF/origin/tab transport checks.
type knowledgeAdoption struct {
	expected  map[string]any
	view      *knowledgequery.View
	finalized map[string]int64
}
type adoptedKnowledgeProvider struct {
	mu               sync.RWMutex
	projects         map[Caller]knowledgeAdoption
	begins, releases atomic.Int64
}

func newAdoptedKnowledgeProvider() *adoptedKnowledgeProvider {
	return &adoptedKnowledgeProvider{projects: make(map[Caller]knowledgeAdoption)}
}
func (p *adoptedKnowledgeProvider) adopt(c Caller, expected map[string]any, view *knowledgequery.View, finalized map[string]int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.projects[c] = knowledgeAdoption{expected, view, finalized}
}
func (p *adoptedKnowledgeProvider) revoke(c Caller) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.projects, c)
}
func (p *adoptedKnowledgeProvider) BeginKnowledge(ctx context.Context, c Caller, raw any) (*knowledgequery.View, map[string]int64, func(), error) {
	p.begins.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	p.mu.RLock()
	entry, exists := p.projects[c]
	if !exists {
		p.mu.RUnlock()
		return nil, nil, nil, novelruntime.ErrNotOwned
	}
	expected, ok := raw.(map[string]any)
	if !ok || expected["projectId"] != entry.expected["projectId"] || expected["leaseId"] != entry.expected["leaseId"] || expected["projectPath"] != entry.expected["projectPath"] {
		p.mu.RUnlock()
		return nil, nil, nil, novelruntime.ErrNotOwned
	}
	return entry.view, entry.finalized, func() { p.releases.Add(1); p.mu.RUnlock() }, nil
}

func knowledgeTransportView(t *testing.T) (*knowledgequery.View, string) {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(path, "knowledge-copies"), 0700); err != nil {
		t.Fatal(err)
	}
	id := "transport-doc-星海🙂"
	content := "合成作者修改副本\r\nΩ🙂"
	hash := sha256.Sum256([]byte(content))
	copyRaw, err := json.Marshal(map[string]any{"content": content, "indexedHash": hex.EncodeToString(hash[:]), "edited": true, "indexDirty": false})
	if err != nil {
		t.Fatal(err)
	}
	idHash := sha256.Sum256([]byte(id))
	if err := os.WriteFile(filepath.Join(path, "knowledge-copies", hex.EncodeToString(idHash[:])+".json"), copyRaw, 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	field := func(name, kind string) knowledgedomain.Field { return knowledgedomain.Field{Name: name, Type: kind} }
	snapshot := knowledgedomain.Snapshot{Tables: []knowledgedomain.Table{{Name: "documents", Version: 1, Schema: []knowledgedomain.Field{field("id", "utf8"), field("fileName", "utf8"), field("importedAt", "utf8"), field("chunkCount", "float64"), field("filePath", "utf8"), field("corpusKind", "utf8")}, Records: []map[string]any{{"id": id, "fileName": "合成🙂.txt", "importedAt": "2026-01-01", "chunkCount": json.Number("0"), "filePath": "knowledge-copy:ignored-suffix", "corpusKind": "reference"}}}}}
	view, err := knowledgequery.New(context.Background(), snapshot, root, knowledgecopy.Budget{MaxMetadataBytes: 1024 * 1024, MaxCopies: 100})
	if err != nil {
		t.Fatal(err)
	}
	return view, path
}
func knowledgeWire(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func knowledgeRequireResponse(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if response.Code != status || !strings.Contains(response.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("HTTP%d %s, want%d %s", response.Code, response.Body.String(), status, code)
	}
}

func TestKnowledgeHTTPFourReadShapesAndFinalizedAnnotation(t *testing.T) {
	server := fixture(t)
	provider := newAdoptedKnowledgeProvider()
	if err := server.BindKnowledge(provider); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	view, _ := knowledgeTransportView(t)
	expected := draftContext("synthetic-kb-project", "synthetic-kb-lease", "/synthetic/kb-book")
	owner := draftOwner(cookie, "synthetic-kb-tab-0001")
	id := "transport-doc-星海🙂"
	provider.adopt(owner, expected, view, map[string]int64{id: 12, "unrelated": 99})
	docs, err := view.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		args []any
		want any
	}{{"kb:list-documents", []any{expected["projectPath"], expected}, nil}, {"kb:stats", []any{expected["projectPath"], expected}, nil}, {"kb:get-vectorless-count", []any{expected["projectPath"], expected}, nil}, {"kb:read-document-copy", []any{id, expected["projectPath"], expected}, nil}} {
		t.Run(test.name, func(t *testing.T) {
			switch test.name {
			case "kb:list-documents":
				rows := make([]map[string]any, len(docs))
				for i, doc := range docs {
					rows[i] = map[string]any{}
					for key, value := range doc {
						rows[i][key] = value
					}
					if doc["id"] == id {
						rows[i]["finalizedChapter"] = int64(12)
					}
				}
				test.want = rows
			case "kb:stats":
				test.want, err = view.Stats(context.Background())
			case "kb:get-vectorless-count":
				test.want, err = view.VectorlessCount(context.Background())
			case "kb:read-document-copy":
				test.want, err = view.ReadCopy(context.Background(), id)
			}
			if err != nil {
				t.Fatal(err)
			}
			got := draftResult(t, draftRPC(t, server, cookie, csrf, owner.ClientID, test.name, test.args...))
			if !reflect.DeepEqual(got, knowledgeWire(t, test.want)) {
				t.Fatal("knowledge shape changed", got, test.want)
			}
		})
	}
	if provider.begins.Load() != 4 || provider.releases.Load() != 4 {
		t.Fatal("hold release mismatch", provider.begins.Load(), provider.releases.Load())
	}
	again, err := view.Documents(context.Background())
	if err != nil || !reflect.DeepEqual(docs, again) {
		t.Fatal("finalized annotation mutated view", err)
	}
	provider.adopt(owner, expected, view, nil)
	result := draftResult(t, draftRPC(t, server, cookie, csrf, owner.ClientID, "kb:list-documents", expected["projectPath"], expected)).([]any)
	if _, exists := result[0].(map[string]any)["finalizedChapter"]; exists {
		t.Fatal("stale finalization annotation")
	}
}

func TestKnowledgeHTTPFrozenContextAndArgumentGatesBeforeProvider(t *testing.T) {
	server := fixture(t)
	provider := newAdoptedKnowledgeProvider()
	if err := server.BindKnowledge(provider); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	expected := draftContext("project", "lease", "/synthetic/kb")
	for _, test := range []struct {
		name   string
		args   []any
		status int
		code   string
	}{{"missing-context", []any{"/synthetic/kb"}, 400, "INVALID_ARGUMENTS"}, {"nil-context", []any{"/synthetic/kb", nil}, 400, "PROJECT_SESSION_REQUIRED"}, {"partial-context", []any{"/synthetic/kb", map[string]any{"projectId": "project", "projectPath": "/synthetic/kb"}}, 400, "PROJECT_SESSION_REQUIRED"}, {"wrong-path", []any{"/synthetic/other", expected}, 409, "PROJECT_CONTEXT_CHANGED"}, {"path-type", []any{true, expected}, 400, "PROJECT_CONTEXT_REQUIRED"}, {"empty-path", []any{"", expected}, 400, "PROJECT_CONTEXT_REQUIRED"}, {"too-many", []any{"/synthetic/kb", expected, expected}, 400, "INVALID_ARGUMENTS"}} {
		t.Run(test.name, func(t *testing.T) {
			knowledgeRequireResponse(t, draftRPC(t, server, cookie, csrf, "synthetic-kb-tab-0001", "kb:stats", test.args...), test.status, test.code)
		})
	}
	for _, id := range []any{"", nil, 12, true, map[string]any{}, []any{}} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			knowledgeRequireResponse(t, draftRPC(t, server, cookie, csrf, "synthetic-kb-tab-0001", "kb:read-document-copy", id, "/synthetic/kb", expected), 400, "INVALID_ARGUMENTS")
		})
	}
	if provider.begins.Load() != 0 {
		t.Fatal("bad request borrowed capability")
	}
}

func TestKnowledgeHTTPOwnershipIncludesSessionTabLeaseAndRevocation(t *testing.T) {
	server := fixture(t)
	provider := newAdoptedKnowledgeProvider()
	if err := server.BindKnowledge(provider); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	otherCookie, otherCSRF := bootstrap(t, server)
	view, _ := knowledgeTransportView(t)
	expected := draftContext("project", "lease", "/synthetic/kb")
	owner := draftOwner(cookie, "synthetic-kb-tab-0001")
	provider.adopt(owner, expected, view, nil)
	wrongLease := draftContext("project", "different-lease", "/synthetic/kb")
	wrongID := draftContext("different-project", "lease", "/synthetic/kb")
	for _, test := range []struct {
		name      string
		cookie    *http.Cookie
		csrf, tab string
		expected  map[string]any
	}{{"same-user-other-tab", cookie, csrf, "synthetic-kb-tab-0002", expected}, {"same-user-new-session", otherCookie, otherCSRF, owner.ClientID, expected}, {"wrong-lease", cookie, csrf, owner.ClientID, wrongLease}, {"wrong-project", cookie, csrf, owner.ClientID, wrongID}} {
		t.Run(test.name, func(t *testing.T) {
			knowledgeRequireResponse(t, draftRPC(t, server, test.cookie, test.csrf, test.tab, "kb:stats", "/synthetic/kb", test.expected), 500, "PROJECT_SESSION_NOT_OWNED")
		})
	}
	wrongRoot := draftContext("project", "lease", "/synthetic/other-kb")
	knowledgeRequireResponse(t, draftRPC(t, server, cookie, csrf, owner.ClientID, "kb:stats", "/synthetic/other-kb", wrongRoot), 500, "PROJECT_SESSION_NOT_OWNED")
	provider.revoke(owner)
	knowledgeRequireResponse(t, draftRPC(t, server, cookie, csrf, owner.ClientID, "kb:stats", "/synthetic/kb", expected), 500, "PROJECT_SESSION_NOT_OWNED")
	if provider.releases.Load() != 0 {
		t.Fatal("nonowners obtained release")
	}
}

func TestKnowledgeHTTPAuthenticationAndUnportedOperations(t *testing.T) {
	server := fixture(t)
	provider := newAdoptedKnowledgeProvider()
	if err := server.BindKnowledge(provider); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	body := `{"args":["/synthetic/kb",{"projectId":"project","leaseId":"lease","projectPath":"/synthetic/kb"}]}`
	for _, test := range []struct {
		name    string
		headers map[string]string
		status  int
		code    string
	}{{"csrf", map[string]string{"X-CSRF-Token": "invalid"}, 403, "CSRF_DENIED"}, {"origin", map[string]string{"Origin": "https://untrusted.invalid"}, 403, "ORIGIN_DENIED"}, {"tab", map[string]string{"X-Client-Id": "bad"}, 400, "CLIENT_ID_REQUIRED"}, {"auth", map[string]string{"X-Novel-User": ""}, 401, "AUTHENTICATION_REQUIRED"}} {
		t.Run(test.name, func(t *testing.T) {
			knowledgeRequireResponse(t, rpc(t, server, cookie, csrf, "kb:stats", body, test.headers), test.status, test.code)
		})
	}
	knowledgeRequireResponse(t, rpc(t, server, cookie, csrf, "kb:save-document-copy", `{"args":[]}`, nil), 501, "GO_OPERATION_NOT_MIGRATED")
	knowledgeRequireResponse(t, rpc(t, server, cookie, csrf, "kb:search", `{"args":[]}`, nil), 501, "GO_OPERATION_NOT_MIGRATED")
	knowledgeRequireResponse(t, rpc(t, server, cookie, csrf, "kb:unknown-private-operation", `{"args":[]}`, nil), 404, "OPERATION_NOT_EXPOSED")
	if provider.begins.Load() != 0 {
		t.Fatal("security or unbound operation borrowed view")
	}
}

func TestKnowledgeBindingAtomicAndRequiresProvider(t *testing.T) {
	server := fixture(t)
	if err := server.BindKnowledge(nil); err == nil {
		t.Fatal("nil provider accepted")
	}
	var function KnowledgeProviderFunc
	if _, _, _, err := function.BeginKnowledge(context.Background(), Caller{}, nil); err == nil {
		t.Fatal("nil provider function accepted")
	}
	provider := KnowledgeProviderFunc(func(context.Context, Caller, any) (*knowledgequery.View, map[string]int64, func(), error) {
		return nil, nil, nil, errors.New("unused")
	})
	if err := server.Bind("kb:read-document-copy", func(context.Context, Caller, []any) (any, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := server.BindKnowledge(provider); err == nil {
		t.Fatal("collision accepted")
	}
	for _, name := range []string{"kb:list-documents", "kb:stats", "kb:get-vectorless-count"} {
		if _, exists := server.handlers[name]; exists {
			t.Fatal("partial knowledge binding", name)
		}
	}
}

func TestKnowledgeProviderErrorsReleaseOnceAndNeverEchoPrivateText(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		code string
	}{{"raw", errors.New("private /author/project.db API_KEY secret prose"), "KNOWLEDGE_READ_FAILED"}, {"uppercase", errors.New("PRIVATE_SECRET_TOKEN"), "KNOWLEDGE_READ_FAILED"}, {"joined-context", errors.Join(context.Canceled, errors.New("private /author/body")), "REQUEST_CANCELED"}, {"deadline", errors.Join(context.DeadlineExceeded, errors.New("private /author/body")), "REQUEST_DEADLINE_EXCEEDED"}, {"owner", novelruntime.ErrNotOwned, "PROJECT_SESSION_NOT_OWNED"}, {"public", security.NewHTTPError(409, "PROJECT_SESSION_STALE"), "PROJECT_SESSION_STALE"}} {
		t.Run(test.name, func(t *testing.T) {
			server := fixture(t)
			var release atomic.Int64
			provider := KnowledgeProviderFunc(func(context.Context, Caller, any) (*knowledgequery.View, map[string]int64, func(), error) {
				return nil, nil, func() { release.Add(1) }, test.err
			})
			if err := server.BindKnowledge(provider); err != nil {
				t.Fatal(err)
			}
			cookie, csrf := bootstrap(t, server)
			expected := draftContext("project", "lease", "/synthetic/kb")
			status := 500
			if test.name == "public" {
				status = 409
			}
			response := draftRPC(t, server, cookie, csrf, "synthetic-kb-tab-0001", "kb:stats", "/synthetic/kb", expected)
			knowledgeRequireResponse(t, response, status, test.code)
			if release.Load() != 1 || strings.Contains(response.Body.String(), "private") || strings.Contains(response.Body.String(), "/author") || strings.Contains(response.Body.String(), "SECRET_TOKEN") {
				t.Fatal("release or redaction failure", response.Body.String(), release.Load())
			}
		})
	}
}

func TestKnowledgeProviderMustReturnViewAndRelease(t *testing.T) {
	view, _ := knowledgeTransportView(t)
	for _, kind := range []string{"no-view", "no-release"} {
		t.Run(kind, func(t *testing.T) {
			server := fixture(t)
			var released atomic.Int64
			provider := KnowledgeProviderFunc(func(context.Context, Caller, any) (*knowledgequery.View, map[string]int64, func(), error) {
				if kind == "no-view" {
					return nil, nil, func() { released.Add(1) }, nil
				}
				return view, nil, nil, nil
			})
			if err := server.BindKnowledge(provider); err != nil {
				t.Fatal(err)
			}
			cookie, csrf := bootstrap(t, server)
			expected := draftContext("project", "lease", "/synthetic/kb")
			knowledgeRequireResponse(t, draftRPC(t, server, cookie, csrf, "synthetic-kb-tab-0001", "kb:stats", "/synthetic/kb", expected), 500, "KNOWLEDGE_PROVIDER_UNAVAILABLE")
			want := int64(0)
			if kind == "no-view" {
				want = 1
			}
			if released.Load() != want {
				t.Fatal("invalid provider release mismatch")
			}
		})
	}
}

func TestKnowledgeReadCopyUnavailableRetainsOriginalResult(t *testing.T) {
	server := fixture(t)
	provider := newAdoptedKnowledgeProvider()
	if err := server.BindKnowledge(provider); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	view, _ := knowledgeTransportView(t)
	expected := draftContext("project", "lease", "/synthetic/kb")
	owner := draftOwner(cookie, "synthetic-kb-tab-0001")
	provider.adopt(owner, expected, view, nil)
	got := draftResult(t, draftRPC(t, server, cookie, csrf, owner.ClientID, "kb:read-document-copy", "missing-doc", "/synthetic/kb", expected))
	want := map[string]any{"available": false, "indexStatus": "unavailable"}
	if !reflect.DeepEqual(got, want) || provider.releases.Load() != 1 {
		t.Fatal("unavailable copy shape or release", got)
	}
}

func TestKnowledgeCancellationBeforeBorrowAndDuringReadReleases(t *testing.T) {
	view, _ := knowledgeTransportView(t)
	for _, afterBorrow := range []bool{false, true} {
		t.Run(fmt.Sprint(afterBorrow), func(t *testing.T) {
			server := fixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var begins, releases atomic.Int64
			provider := KnowledgeProviderFunc(func(context.Context, Caller, any) (*knowledgequery.View, map[string]int64, func(), error) {
				begins.Add(1)
				cancel()
				return view, nil, func() { releases.Add(1) }, nil
			})
			if err := server.BindKnowledge(provider); err != nil {
				t.Fatal(err)
			}
			if !afterBorrow {
				cancel()
			}
			expected := draftContext("project", "lease", "/synthetic/kb")
			_, err := server.handlers["kb:stats"](ctx, Caller{}, []any{"/synthetic/kb", expected})
			if err == nil || err.Error() != "REQUEST_CANCELED" {
				t.Fatal("cancellation not sanitized", err)
			}
			want := int64(0)
			if afterBorrow {
				want = 1
			}
			if begins.Load() != want || releases.Load() != want {
				t.Fatal("canceled borrow leaked", begins.Load(), releases.Load())
			}
		})
	}
}

func TestKnowledgeDirectInvalidUTF8AndOversizedStringsNeverBorrow(t *testing.T) {
	server := fixture(t)
	var begins atomic.Int64
	provider := KnowledgeProviderFunc(func(context.Context, Caller, any) (*knowledgequery.View, map[string]int64, func(), error) {
		begins.Add(1)
		return nil, nil, nil, nil
	})
	if err := server.BindKnowledge(provider); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{string([]byte{0xff}), strings.Repeat("x", int(security.DefaultBodyLimit)+1)} {
		expected := draftContext("project", "lease", text)
		if _, err := server.handlers["kb:stats"](context.Background(), Caller{}, []any{text, expected}); err == nil {
			t.Fatal("invalid path accepted")
		}
		expected = draftContext("project", "lease", "/synthetic/kb")
		if _, err := server.handlers["kb:read-document-copy"](context.Background(), Caller{}, []any{text, "/synthetic/kb", expected}); err == nil {
			t.Fatal("invalid ID accepted")
		}
	}
	if begins.Load() != 0 {
		t.Fatal("invalid string borrowed view")
	}
}

type knowledgeHoldContext struct {
	context.Context
	inspect func()
}

func (c knowledgeHoldContext) Err() error {
	if c.inspect != nil {
		c.inspect()
	}
	return c.Context.Err()
}
func TestKnowledgeProviderReleaseIsAfterViewRead(t *testing.T) {
	server := fixture(t)
	view, _ := knowledgeTransportView(t)
	var borrowed, readWhileHeld atomic.Bool
	var released atomic.Int64
	ctx := knowledgeHoldContext{Context: context.Background(), inspect: func() {
		if borrowed.Load() {
			if released.Load() != 0 {
				t.Fatal("lease released before read")
			}
			readWhileHeld.Store(true)
		}
	}}
	provider := KnowledgeProviderFunc(func(context.Context, Caller, any) (*knowledgequery.View, map[string]int64, func(), error) {
		borrowed.Store(true)
		return view, nil, func() { borrowed.Store(false); released.Add(1) }, nil
	})
	if err := server.BindKnowledge(provider); err != nil {
		t.Fatal(err)
	}
	expected := draftContext("project", "lease", "/synthetic/kb")
	if _, err := server.handlers["kb:stats"](ctx, Caller{}, []any{"/synthetic/kb", expected}); err != nil {
		t.Fatal(err)
	}
	if !readWhileHeld.Load() || released.Load() != 1 {
		t.Fatal("view read not held or release missing")
	}
}

func TestKnowledgeReadErrorIsFiniteAllowlist(t *testing.T) {
	var nilPublic *security.HTTPError
	for _, err := range []error{nil, nilPublic, errors.New("KNOWLEDGE_QUERY_PRIVATE_UNKNOWN"), errors.New("ALL_UPPERCASE_SECRET"), fmt.Errorf("private %w", novelruntime.ErrNotOwned)} {
		if got := knowledgeReadError(err).Error(); got != "KNOWLEDGE_READ_FAILED" {
			t.Fatal("unknown error exposed", got)
		}
	}
	public := security.NewHTTPError(409, "PROJECT_CONTEXT_CHANGED")
	if knowledgeReadError(public) != public {
		t.Fatal("public error not preserved")
	}
}
