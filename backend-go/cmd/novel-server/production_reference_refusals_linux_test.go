//go:build linux

package main

import (
	"encoding/json"
	"testing"
)

// TestProductionReferenceRefusalsMirrorTheDesktop pins the two names whose only
// correct answer is the desktop controller's own refusal. They are bound now, so a
// browser can call them and receive the reference's code - never a 501, which would
// be indistinguishable from "not migrated".
func TestProductionReferenceRefusalsMirrorTheDesktop(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, nil)
	before := lane.bytes(t)

	for _, arm := range []struct {
		operation string
		args      func() []any
		want      string
	}{
		{
			operation: productionReferenceRefusalPlotTree,
			args: func() []any {
				return []any{map[string]any{"nodes": []any{}}, map[string]any{"edges": []any{}},
					lane.projectPath, lane.frozen}
			},
			want: "GENERATION_GRAPH_ADMISSION_REQUIRED",
		},
		{
			operation: productionReferenceRefusalBlueprint,
			args: func() []any {
				return []any{int64(1), "chapter plot notes", lane.projectPath, lane.frozen}
			},
			want: "GENERATION_FINALIZATION_ADMISSION_REQUIRED",
		},
	} {
		response := lane.author.call(t, arm.operation, arm.args())
		if response.Code != 200 {
			t.Fatalf("%s answered HTTP %v, want a 200 envelope: %s", arm.operation,
				response.Code, response.Body.String())
		}
		var body struct {
			Result struct {
				Success bool   `json:"success"`
				Error   string `json:"error"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v %s", arm.operation, err, response.Body.String())
		}
		if body.Result.Success {
			t.Fatalf("%s answered success for a write the desktop refuses", arm.operation)
		}
		if body.Result.Error != "Error: "+arm.want {
			t.Fatalf("%s refused with %q, want %q", arm.operation, body.Result.Error, "Error: "+arm.want)
		}
	}

	// Both are the reference's own refusals: nothing was borrowed to write, so the
	// database is byte-identical.
	if after := lane.bytes(t); string(after) != string(before) {
		t.Fatal("a reference refusal changed the database bytes")
	}

	// The neighbour checks still apply: a foreign path is a context change, a wrong
	// arity is an argument error, and a body without a live lease is refused by the
	// borrow fence before the refusal code.
	foreign := []any{map[string]any{}, map[string]any{}, "/elsewhere", lane.frozen}
	if response := lane.author.call(t, productionReferenceRefusalPlotTree, foreign); response.Code != 409 {
		t.Fatalf("a foreign path answered %v, want 409: %s", response.Code, response.Body.String())
	}
	if response := lane.author.call(t, productionReferenceRefusalBlueprint, []any{int64(1), "notes", lane.projectPath}); response.Code != 400 {
		t.Fatalf("a wrong arity answered %v, want 400: %s", response.Code, response.Body.String())
	}
}
