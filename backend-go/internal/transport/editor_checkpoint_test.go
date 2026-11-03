package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestEditorCheckpointBindsOnlyFixedProjectMarker(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	defer server.Close()
	const path = "/synthetic/project-a"
	expected := draftContext("project-a", "lease-a", path)
	called := 0
	provider := EditorCheckpointProviderFunc(func(_ context.Context, _ Caller, _ any) (json.RawMessage, bool, error) {
		called++
		return json.RawMessage(`{"synopsis_incomplete":true}`), true, nil
	})
	if err := server.BindEditorCheckpoint(provider); err != nil {
		t.Fatal(err)
	}
	result := draftResult(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "fs:read-json", path+"/.ai-novel/partial_arch.json", path, expected)).(map[string]any)
	if result["success"] != true || result["data"].(map[string]any)["synopsis_incomplete"] != true || called != 1 {
		t.Fatalf("checkpoint DTO: %#v, calls %d", result, called)
	}
	for _, bad := range []string{path + "/.ai-novel/manifest.json", path + "/.ai-novel/../partial_arch.json", "/synthetic/other/.ai-novel/partial_arch.json"} {
		response := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "fs:read-json", bad, path, expected)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("accepted %q: %d %s", bad, response.Code, response.Body.String())
		}
	}
	if called != 1 {
		t.Fatal("invalid paths reached provider")
	}
	if err := server.BindEditorCheckpoint(provider); err == nil {
		t.Fatal("duplicate binding accepted")
	}
	if response := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "fs:write-json"); response.Code != http.StatusNotImplemented {
		t.Fatal("write JSON bound", response.Code)
	}
}

func TestEditorCheckpointMissingReturnsOriginalFailureShape(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	defer server.Close()
	if err := server.BindEditorCheckpoint(EditorCheckpointProviderFunc(func(context.Context, Caller, any) (json.RawMessage, bool, error) {
		return nil, false, nil
	})); err != nil {
		t.Fatal(err)
	}
	const path = "/synthetic/project-a"
	result := draftResult(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "fs:read-json", path+"/.ai-novel/partial_arch.json", path, draftContext("project-a", "lease-a", path))).(map[string]any)
	if result["success"] != false || result["data"] != nil {
		t.Fatalf("missing checkpoint: %#v", result)
	}
}
