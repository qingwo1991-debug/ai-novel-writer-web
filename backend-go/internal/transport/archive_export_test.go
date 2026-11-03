package transport

import (
	"context"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/archiveformat"
)

func TestArchiveExportRejectsMalformedContextAndGrantBeforeProvider(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	called := 0
	provider := ArchiveExportProviderFunc(func(context.Context, Caller, map[string]string, string) (*ArchiveExportAttempt, error) {
		called++
		return nil, nil
	})
	if err := server.BindArchiveExport(provider, archiveformat.Limits{}); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"args":[]}`,
		`{"args":[{"targetArchiveGrantId":"grant","projectSession":{"projectId":"p","leaseId":"l","projectPath":"/p","extra":"x"}}]}`,
		`{"args":[{"targetArchiveGrantId":"","projectSession":{"projectId":"p","leaseId":"l","projectPath":"/p"}}]}`,
	} {
		response := rpc(t, server, cookie, csrf, "project:archive-export", body, nil)
		if response.Code != 200 || !strings.Contains(response.Body.String(), `"success":false`) {
			t.Fatalf("%d %s", response.Code, response.Body.String())
		}
	}
	if called != 0 {
		t.Fatalf("provider called %d times", called)
	}
}

func TestArchiveExportCannotSucceedWithProviderWithoutPrivateStage(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	provider := ArchiveExportProviderFunc(func(_ context.Context, caller Caller, expected map[string]string, grant string) (*ArchiveExportAttempt, error) {
		if caller.SessionID == "" || caller.ClientID == "" || grant != "grant" || expected["projectPath"] != "/p" {
			t.Fatalf("owner/context/grant: %+v %+v %s", caller, expected, grant)
		}
		return &ArchiveExportAttempt{}, nil
	})
	if err := server.BindArchiveExport(provider, archiveformat.Limits{}); err != nil {
		t.Fatal(err)
	}
	response := rpc(t, server, cookie, csrf, "project:archive-export", `{"args":[{"targetArchiveGrantId":"grant","projectSession":{"projectId":"p","leaseId":"l","projectPath":"/p"}}]}`, nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "PORTABLE_EXPORT_AUTHORITY_REQUIRED") {
		t.Fatalf("%d %s", response.Code, response.Body.String())
	}
}
