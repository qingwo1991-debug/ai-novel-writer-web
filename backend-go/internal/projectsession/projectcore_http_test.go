package projectsession

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

func TestProjectCoreBrowserWireOnAdmittedPrivateCopy(t *testing.T) {
	project, contract, source := sessionFixture(t)
	// Synthetic fixture initialization before acquiring any source capability: the
	// admitted session itself never creates or writes author source rows.
	dsn, err := store.ExistingDSN(source)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT OR IGNORE INTO project_core(id,project_name) VALUES('main','synthetic')"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(source)
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
	provider := transport.ProjectCoreProviderFunc(func(ctx context.Context, caller transport.Caller, raw any) (*store.ProjectCore, func(), error) {
		expected, err := ParseContext(raw)
		if err != nil {
			return nil, nil, err
		}
		return manager.BeginProjectCore(ctx, Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}, expected)
	})
	if err := server.BindProjectCore(provider); err != nil {
		t.Fatal(err)
	}
	if err := server.BindProjectCore(provider); err == nil {
		t.Fatal("duplicate binding replaced handlers")
	}
	var caller transport.Caller
	if err := server.Bind("config:get", func(_ context.Context, value transport.Caller, _ []any) (any, error) { caller = value; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	boot := httptest.NewRequest("GET", "/api/bootstrap", nil)
	boot.Header.Set("X-Novel-User", "synthetic-author")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, boot)
	if response.Code != 200 {
		t.Fatal(response.Code)
	}
	cookie := response.Result().Cookies()[0]
	var bootstrap struct {
		CSRFToken string `json:"csrfToken"`
	}
	if json.Unmarshal(response.Body.Bytes(), &bootstrap) != nil {
		t.Fatal("bootstrap")
	}
	invoke := func(op, tab string, args []any) (int, map[string]any) {
		body, err := json.Marshal(map[string]any{"args": args})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/operations/"+op, bytes.NewReader(body))
		request.AddCookie(cookie)
		request.Header.Set("X-Novel-User", "synthetic-author")
		request.Header.Set("X-Client-Id", tab)
		request.Header.Set("X-CSRF-Token", bootstrap.CSRFToken)
		request.Header.Set("Origin", "http://synthetic.test")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		var result map[string]any
		if json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatal(response.Body.String())
		}
		return response.Code, result
	}
	const tab = "synthetic-browser-tab-0001"
	if status, _ := invoke("config:get", tab, []any{}); status != 200 {
		t.Fatal(status)
	}
	owner := Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}
	expected, err := manager.OpenScratchCopy(context.Background(), owner, project, contract, t.TempDir(), 32*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	status, result := invoke("db:project-core-get", tab, []any{project.Path(), expected})
	if status != 200 {
		t.Fatal(status, result)
	}
	core, ok := result["result"].(map[string]any)
	if !ok {
		t.Fatal("fixture missing core row", result)
	}
	if status, _ := invoke("db:project-core-get", "synthetic-browser-tab-0002", []any{project.Path(), expected}); status == 200 {
		t.Fatal("cross tab core leaked")
	}
	update := map[string]any{"synopsis": "initial public synopsis", "genre": "synthetic genre", "premise": "synthetic premise"}
	status, result = invoke("db:project-core-update", tab, []any{update, project.Path(), expected})
	if status != 200 || result["result"].(map[string]any)["success"] != true {
		t.Fatal(status, result)
	}
	_, result = invoke("db:project-core-get", tab, []any{project.Path(), expected})
	core = result["result"].(map[string]any)
	baseline := map[string]any{}
	for _, key := range []string{"synopsis", "premise", "charactersArch", "worldbuilding", "genre", "totalChapters", "wordsPerChapter", "writingLanguage", "plotStructure", "narrativePov", "globalGuidance"} {
		baseline[key] = core[key]
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for _, body := range []string{"candidate one 中文", "candidate two 😀"} {
		wg.Go(func() {
			status, result := invoke("db:project-core-synopsis-commit", tab, []any{map[string]any{"synopsis": body, "expected": baseline}, project.Path(), expected})
			if status != 200 {
				t.Error(status, result)
				return
			}
			r := result["result"].(map[string]any)
			mu.Lock()
			defer mu.Unlock()
			if r["success"] == true {
				wins++
			} else if r["error"] == "PROJECT_CORE_SYNOPSIS_CHANGED" {
				conflicts++
			} else {
				t.Error(r)
			}
		})
	}
	wg.Wait()
	if wins != 1 || conflicts != 1 {
		t.Fatal("source/target CAS lost update", wins, conflicts)
	}
	for _, key := range []string{"generationRunHandle", "authorRecovery"} {
		request := map[string]any{"synopsis": "must not save", "expected": baseline, key: nil}
		status, _ := invoke("db:project-core-synopsis-commit", tab, []any{request, project.Path(), expected})
		if status == 200 {
			t.Fatal("unmigrated owner bypass", key)
		}
	}
	status, result = invoke("db:project-core-update", tab, []any{map[string]any{"charactersArch": nil}, project.Path(), expected})
	if status != 200 || result["result"].(map[string]any)["error"] != "Error: PROJECT_CORE_CHARACTERS_ARCH_READ_ONLY" {
		t.Fatal(status, result)
	}
	repository, release, err := manager.BeginProjectCore(context.Background(), owner, expected)
	if err != nil || repository == nil {
		t.Fatal(err)
	}
	if err := manager.Release(owner, expected); err == nil {
		t.Fatal("active core hold released")
	}
	release()
	if err := manager.Release(owner, expected); err != nil {
		t.Fatal(err)
	}
	if status, _ := invoke("db:project-core-get", tab, []any{project.Path(), expected}); status == 200 {
		t.Fatal("stale core context reused")
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("author source changed", err)
	}
}
