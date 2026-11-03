package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/importsource"
)

func TestImportSelectionAuthorInspectionUsesTrustedCaptureAndOneUseReceipt(t *testing.T) {
	server := fixture(t)
	store, err := importsource.NewStore([]byte("0123456789abcdef0123456789abcdef"), importsource.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var provided Caller
	provider := ImportSelectionProviderFunc(func(_ context.Context, caller Caller, request ImportSelectionRequest, expected map[string]any) (ImportSelection, func(), error) {
		provided = caller
		if request.RunID != "run-1" || request.Purpose != importsource.AuthorManuscript || request.Locale != "zh-CN" || expected["projectPath"] != "/private/book" {
			t.Fatalf("request changed: %+v %+v", request, expected)
		}
		return ImportSelection{ProjectID: "project-1", LeaseID: "lease-1", Sources: []importsource.CapturedSource{{
			DisplayName: "chapters.txt", Reader: strings.NewReader("第一章 雨夜\n雨落长安。"),
			Identity: importsource.Identity{CanonicalLocation: "/trusted/staged/chapters.txt", FileIdentity: "staged-inode-1"},
		}}}, func() {}, nil
	})
	if err := server.BindImportSelection(store, provider); err != nil {
		t.Fatal(err)
	}
	if server.handlers["db:import-run-author-preview"] != nil || server.handlers["db:import-run-prepare-inspection"] != nil {
		t.Fatal("in-memory selection exposed durable preview or parsing")
	}
	cookie, csrf := bootstrap(t, server)
	expected := draftContext("project-1", "lease-1", "/private/book")
	request := map[string]any{"runId": "run-1", "purpose": "author-manuscript", "locale": "zh-CN", "expectedProjectPath": "/private/book"}
	response := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "dialog:select-novel-files", request, expected)
	if response.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
	}
	if provided.SessionID == "" || provided.ClientID != "synthetic-tab-0001" || provided.Principal != "synthetic-author" {
		t.Fatalf("caller identity lost: %+v", provided)
	}
	var envelope struct {
		Result struct {
			Success    bool `json:"success"`
			Inspection struct {
				InspectionID string `json:"inspectionId"`
				ChapterCount int    `json:"chapterCount"`
			} `json:"inspection"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || !envelope.Result.Success || envelope.Result.Inspection.InspectionID == "" || envelope.Result.Inspection.ChapterCount != 1 {
		t.Fatalf("invalid inspection response: %v %s", err, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "/trusted/staged") || strings.Contains(response.Body.String(), "staged-inode") || strings.Contains(response.Body.String(), "雨落长安") {
		t.Fatal("private capture identity or manuscript body leaked")
	}
	binding := importsource.Binding{Caller: provided.SessionID + ":" + provided.ClientID, Generation: "project-1:lease-1", Purpose: importsource.AuthorManuscript}
	summary, err := store.Peek(envelope.Result.Inspection.InspectionID, binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Peek(summary.InspectionID, importsource.Binding{Caller: provided.SessionID + ":other-tab", Generation: binding.Generation, Purpose: binding.Purpose}); err == nil {
		t.Fatal("foreign tab read inspection")
	}
	if _, err := store.Consume(summary.InspectionID, strings.Repeat("0", 64), binding); err == nil {
		t.Fatal("wrong preview CAS consumed inspection")
	}
	inspection, err := store.Consume(summary.InspectionID, summary.PreviewCAS, binding)
	if err != nil || len(inspection.Chapters) != 1 {
		t.Fatalf("valid internal consume failed: %v %+v", err, inspection)
	}
	if _, err := store.Consume(summary.InspectionID, summary.PreviewCAS, binding); err == nil {
		t.Fatal("inspection replayed")
	}
}

func TestImportSelectionRejectsUnboundAuthorityAndReferenceDurability(t *testing.T) {
	server := fixture(t)
	store, err := importsource.NewStore([]byte("0123456789abcdef0123456789abcdef"), importsource.StoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.BindImportSelection(store, nil); err == nil {
		t.Fatal("bound selection without capture authority")
	}
	calls := 0
	provider := ImportSelectionProviderFunc(func(_ context.Context, _ Caller, _ ImportSelectionRequest, _ map[string]any) (ImportSelection, func(), error) {
		calls++
		return ImportSelection{ProjectID: "foreign-project", LeaseID: "lease-1"}, func() {}, nil
	})
	if err := server.BindImportSelection(store, provider); err != nil {
		t.Fatal(err)
	}
	caller := Caller{SessionID: "cookie", ClientID: "tab"}
	expected := draftContext("project-1", "lease-1", "/private/book")
	request := map[string]any{"runId": "run-1", "purpose": "author-manuscript", "locale": "zh-CN", "expectedProjectPath": "/private/book"}
	for _, args := range [][]any{
		{request},
		{map[string]any{"runId": "run-1", "purpose": "author-manuscript", "locale": "zh-CN", "expectedProjectPath": "/private/book", "filePath": "/etc/passwd"}, expected},
		{request, map[string]any{"projectId": "project-1", "leaseId": "lease-1", "projectPath": "/private/book", "extra": true}},
		{map[string]any{"runId": "run-1", "purpose": "author-manuscript", "locale": "zh-CN", "expectedProjectPath": "/other/book"}, expected},
	} {
		if _, err := server.handlers["dialog:select-novel-files"](context.Background(), caller, args); err == nil {
			t.Fatalf("invalid request accepted: %+v", args)
		}
	}
	if calls != 0 {
		t.Fatal("invalid request reached source provider")
	}
	reference := map[string]any{"runId": "run-1", "purpose": "reference", "locale": "zh-CN", "expectedProjectPath": "/private/book"}
	if _, err := server.handlers["dialog:select-novel-files"](context.Background(), caller, []any{reference, expected}); err == nil || !strings.Contains(err.Error(), "IMPORT_DURABLE_PARSING_REQUIRED") {
		t.Fatalf("reference parsing falsely admitted: %v", err)
	}
	if calls != 0 {
		t.Fatal("reference selection reached source provider")
	}
	if _, err := server.handlers["dialog:select-novel-files"](context.Background(), caller, []any{request, expected}); err == nil || !strings.Contains(err.Error(), "IMPORT_SELECTION_FAILED") {
		t.Fatalf("foreign project selection accepted: %v", err)
	}
	if store.ActiveCount() != 0 {
		t.Fatal("foreign project left an inspection receipt")
	}
}
