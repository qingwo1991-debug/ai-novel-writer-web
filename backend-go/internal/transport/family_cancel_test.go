package transport

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The graph and finalization families do not share the `generation:cancel` wire
// shape. The renderer sends `{ handle: view.handle }` for those channels and the
// Node owner rejects any request carrying a key other than `handle`. Passing the
// envelope straight through to the provider would leave both bound operations
// broken for every real call while a "not 501" assertion still passed, so the
// unwrap is pinned here rather than inferred from the HTTP test.
func TestFamilyCancelsUnwrapTheHandleEnvelope(t *testing.T) {
	cases := []struct{ operation, invalidCode string }{
		{"graph-generation:cancel", "GENERATION_GRAPH_REQUEST_INVALID"},
		{"finalization-generation:cancel", "GENERATION_FINALIZATION_REQUEST_INVALID"},
	}
	expected := map[string]any{"projectId": "project", "leaseId": "lease", "projectPath": "/project"}
	for _, testCase := range cases {
		var received any
		provider := &GenerationProvider{
			Cancel: func(_ context.Context, _ Caller, _ map[string]any, arg any) (any, error) {
				received = arg
				return map[string]any{"success": true}, nil
			},
		}
		// fixture loads the real operations inventory: Bind refuses anything the
		// registry does not expose, so a zero Server would panic instead of telling
		// us the operation is not inventoriued.
		server := fixture(t)
		if err := server.BindGeneration(provider); err != nil {
			t.Fatal(err)
		}
		handler := server.handlers[testCase.operation]
		if handler == nil {
			t.Fatalf("%s is not bound", testCase.operation)
		}
		inner := map[string]any{"runId": "run-1", "rootActionId": "root-1"}
		if _, err := handler(context.Background(), Caller{}, []any{map[string]any{"handle": inner}, expected}); err != nil {
			t.Fatalf("%s: %v", testCase.operation, err)
		}
		if !reflect.DeepEqual(received, inner) {
			t.Fatalf("%s passed %#v to the provider, want the unwrapped handle", testCase.operation, received)
		}

		rejected := []struct {
			name     string
			envelope any
		}{
			{"an extra key", map[string]any{"handle": inner, "runId": "run-1"}},
			{"no handle", map[string]any{"run": inner}},
			{"a bare handle", inner},
			{"a null request", nil},
		}
		for _, rejection := range rejected {
			name, envelope := rejection.name, rejection.envelope
			value, err := handler(context.Background(), Caller{}, []any{envelope, expected})
			if err != nil {
				t.Fatalf("%s with %s returned a bare error %v; mutating ops carry the code in the DTO", testCase.operation, name, err)
			}
			encoded, marshalErr := json.Marshal(value)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if !strings.Contains(string(encoded), testCase.invalidCode) {
				t.Fatalf("%s with %s = %s, want %s", testCase.operation, name, encoded, testCase.invalidCode)
			}
		}
	}
}
