package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type projectLifecycleProbe struct {
	opens   int
	saves   int
	closes  int
	context int
	target  any
	patch   map[string]any
	expect  any
	caller  Caller
	err     error
}

func (p *projectLifecycleProbe) OpenProject(_ context.Context, caller Caller, target any, token string, rendererPath any) (any, error) {
	p.opens++
	p.caller = caller
	p.target = target
	if p.err != nil {
		return nil, p.err
	}
	return map[string]any{"success": true, "requestToken": token, "activeProjectPath": rendererPath, "dbReady": true}, nil
}

func (p *projectLifecycleProbe) CloseProject(_ context.Context, caller Caller, path string, expected any) (any, error) {
	p.closes++
	p.caller = caller
	p.expect = expected
	return map[string]any{"success": true, "closedPath": path}, nil
}

func (p *projectLifecycleProbe) ProjectRuntimeContext(_ context.Context, caller Caller) (any, error) {
	p.context++
	p.caller = caller
	return map[string]any{"activeProjectPath": nil, "dbReady": false}, nil
}

func (p *projectLifecycleProbe) SaveProject(_ context.Context, caller Caller, id string, patch map[string]any, expected any) (any, error) {
	p.saves++
	p.caller = caller
	p.patch = patch
	p.expect = expected
	if p.err != nil {
		return nil, p.err
	}
	return map[string]any{"success": true, "id": id}, nil
}

func TestProjectLifecycleHTTPRequiresFrozenContextAndProjectsOnlyCoreFields(t *testing.T) {
	server := fixture(t)
	probe := new(projectLifecycleProbe)
	if err := server.BindProjects(probe); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	caller := draftOwner(cookie, "synthetic-tab-0001")
	expected := draftContext("project-a", "lease-a", "/synthetic/project-a")
	bad := draftRPC(t, server, cookie, csrf, caller.ClientID, "project:save", "project-a", map[string]any{"path": "/synthetic/project-b"}, "/synthetic/project-a", expected)
	if got := draftResult(t, bad); !reflect.DeepEqual(got, map[string]any{"success": false, "error": "Error: PROJECT_SESSION_ROOT_MISMATCH"}) {
		t.Fatal(got)
	}
	if probe.saves != 0 {
		t.Fatal("mismatched renderer path reached project provider")
	}
	bad = draftRPC(t, server, cookie, csrf, caller.ClientID, "project:update-config", "project-a", map[string]any{"novelConfig": map[string]any{"totalChapters": map[string]any{"nested": true}}}, "/synthetic/project-a", expected)
	if got := draftResult(t, bad); !reflect.DeepEqual(got, map[string]any{"success": false, "error": "Error: PROJECT_CORE_INVALID_BINDING"}) {
		t.Fatal(got)
	}
	if probe.saves != 0 {
		t.Fatal("nested SQL binding reached project provider")
	}
	data := map[string]any{"id": "project-a", "sessionLease": "lease-a", "path": "/synthetic/project-a", "name": "Novel", "rawSecret": "must-not-forward", "novelConfig": map[string]any{"genre": "fantasy", "narrativePOV": "third", "writingLanguage": "en-US", "writingStyle": nil, "referenceWorks": nil, "rawSecret": "must-not-forward"}}
	response := draftRPC(t, server, cookie, csrf, caller.ClientID, "project:save", "project-a", data, "/synthetic/project-a", expected)
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	if probe.saves != 1 || probe.caller != caller {
		t.Fatal("caller ownership was not forwarded", probe.saves, probe.caller)
	}
	want := map[string]any{"projectName": "Novel", "genre": "fantasy", "narrativePov": "third", "writingLanguage": "en-US", "writingStyle": "", "referenceWorks": ""}
	if !reflect.DeepEqual(probe.patch, want) || !reflect.DeepEqual(probe.expect, expected) {
		t.Fatal("unsafe or incomplete core projection", probe.patch, probe.expect)
	}
	if err := server.BindProjects(probe); err == nil {
		t.Fatal("duplicate domain binding replaced live handlers")
	}
}

func TestProjectLifecycleHTTPDelegatesAdmissionWithoutInventingReadyState(t *testing.T) {
	server := fixture(t)
	probe := new(projectLifecycleProbe)
	if err := server.BindProjects(probe); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	tab := "synthetic-tab-0001"
	bad := draftRPC(t, server, cookie, csrf, tab, "project:open", map[string]any{"path": "/arbitrary/author-project"}, "token", nil)
	requireDraftHTTPError(t, bad, http.StatusBadRequest, "INVALID_ARGUMENTS")
	if probe.opens != 0 {
		t.Fatal("request path reached admission provider")
	}
	probe.err = errors.New("PRIVATE /author/project.db SQLITE failure")
	failed := draftRPC(t, server, cookie, csrf, tab, "project:open", "opaque-catalog-key", "token", nil)
	var failure map[string]map[string]any
	if err := json.Unmarshal(failed.Body.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	if failed.Code != http.StatusInternalServerError || failure["error"]["code"] != "REQUEST_FAILED" || strings.Contains(failed.Body.String(), "/author/project.db") {
		t.Fatal("private provider diagnostic leaked", failed.Code, failed.Body.String())
	}
	if probe.opens != 1 || probe.target != "opaque-catalog-key" {
		t.Fatal("catalog key was not delegated", probe.opens, probe.target)
	}
	probe.err = nil
	opened := draftRPC(t, server, cookie, csrf, tab, "project:open", map[string]any{"grantId": "caller-owned-grant", "displayName": "Chosen", "path": "/untrusted"}, "token-2", nil)
	if opened.Code != http.StatusOK || probe.opens != 2 {
		t.Fatal(opened.Code, opened.Body.String())
	}
	if !reflect.DeepEqual(probe.target, map[string]any{"grantId": "caller-owned-grant", "displayName": "Chosen"}) {
		t.Fatal("untrusted path was forwarded", probe.target)
	}
	expected := draftContext("project-a", "lease-a", "/synthetic/project-a")
	closed := draftRPC(t, server, cookie, csrf, tab, "db:close", "/synthetic/project-b", expected)
	requireDraftHTTPError(t, closed, http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
	if probe.closes != 0 {
		t.Fatal("stale close reached provider")
	}
	context := draftRPC(t, server, cookie, csrf, tab, "project:get-runtime-context")
	if context.Code != http.StatusOK || !reflect.DeepEqual(draftResult(t, context), map[string]any{"activeProjectPath": nil, "dbReady": false}) || probe.context != 1 {
		t.Fatal("runtime context was fabricated", context.Body.String())
	}
}
