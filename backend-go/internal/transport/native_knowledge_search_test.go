//go:build lance_native && cgo

package transport

import (
	"context"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgequery"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
)

func TestNativeKnowledgeSearchUsesAdmittedSessionSnapshot(t *testing.T) {
	config := nativeKnowledgeHTTPConfig(t)
	server := fixture(t)
	manager := projectsession.NewKnowledgeManager()
	defer manager.Close()
	cookie, csrf := bootstrap(t, server)
	tab := "synthetic-native-kb-search-tab-0001"
	caller := draftOwner(cookie, tab)
	owner := projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}
	opened, err := manager.OpenKnowledge(context.Background(), owner, config)
	if err != nil {
		t.Fatal(err)
	}
	expected := draftContext(opened.ProjectID, opened.LeaseID, opened.ProjectPath)
	provider := KnowledgeSearchProviderFunc(func(ctx context.Context, c Caller, raw any) (*knowledgequery.SearchCapture, func(), error) {
		parsed, err := projectsession.ParseContext(raw)
		if err != nil {
			return nil, nil, err
		}
		return manager.BeginKnowledgeSearch(ctx, projectsession.Owner{SessionID: c.SessionID, ClientID: c.ClientID}, parsed)
	})
	if err := server.BindKnowledgeSearch(provider); err != nil {
		t.Fatal(err)
	}
	got := draftResult(t, draftRPC(t, server, cookie, csrf, tab, "kb:search", "", nil, config.ProjectPath, expected)).([]any)
	if len(got) == 0 {
		t.Fatal("native search returned no captured chunks")
	}
	for _, item := range got {
		row := item.(map[string]any)
		if len(row) != 3 || row["text"] == "" || row["fileName"] == "" || row["score"] != float64(0.5) {
			t.Fatal("unexpected original search DTO", row)
		}
	}
	knowledgeRequireResponse(t, draftRPC(t, server, cookie, csrf, "synthetic-native-kb-search-tab-0002", "kb:search", "", nil, config.ProjectPath, expected), 500, "PROJECT_SESSION_NOT_OWNED")
	if err := manager.Release(owner, opened); err != nil {
		t.Fatal(err)
	}
	knowledgeRequireResponse(t, draftRPC(t, server, cookie, csrf, tab, "kb:search", "", nil, config.ProjectPath, expected), 500, "PROJECT_SESSION_NOT_OWNED")
}
