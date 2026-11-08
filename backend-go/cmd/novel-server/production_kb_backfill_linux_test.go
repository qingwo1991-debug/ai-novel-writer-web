//go:build linux

package main

import (
	"net/http"
	"strings"
	"testing"
)

// This file proves kb:backfill-vectors is served as an honest no-op over the real
// HTTP adapter and the real frozen project session. The lane stores no vectors -
// its index is SQLite FTS with `registry: null` - so the truthful answer is the
// reference's own zero-work branch: `{ success: true, processed: 0, failed: 0 }`
// (knowledge-base.ts:788-790). The two tests pin both halves of that honesty: the
// success never claims a vector was backfilled, and the foreign-project and
// stale-lease refusals both run the shared fence before any byte of the author's
// store is reached, so a refused or successful backfill leaves the database, the
// canonical snapshot and the copies tree byte-identical.

func init() {
	// kb:backfill-vectors  [expectedProjectPath]  (src/shared/ipc-channels.ts:1257)
	// plus the trailing frozen context.
	productionKnowledgeWireArity[productionKnowledgeBackfillVectorsName] = 1
}

// TestProductionKnowledgeBackfillVectorsAnswersHonestZero proves the success arm:
// one imported document gives the backfill real rows it could have touched, and
// the answer is exactly zero vectors backfilled with zero failures, leaving the
// whole store byte-identical.
func TestProductionKnowledgeBackfillVectorsAnswersHonestZero(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	imported := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
		[]any{productionKnowledgeProse, productionKnowledgeNamedFile, projectPath, frozen})
	if imported["success"] != true {
		t.Fatalf("kb:import-text refused the seed the backfill needs: %v", imported)
	}
	before := productionKnowledgeReadFingerprint(t, projectPath)

	answer := productionKnowledgeCall(t, client, productionKnowledgeBackfillVectorsName,
		[]any{projectPath, frozen})
	if answer["success"] != true {
		t.Fatalf("kb:backfill-vectors refused instead of answering the honest no-op: %v", answer)
	}
	if processed := productionKnowledgeCount(t, answer["processed"]); processed != 0 {
		t.Fatalf("kb:backfill-vectors claimed %d vectors backfilled in an FTS-only store", processed)
	}
	if failed := productionKnowledgeCount(t, answer["failed"]); failed != 0 {
		t.Fatalf("kb:backfill-vectors reported %d failures: %v", failed, answer)
	}
	if after := productionKnowledgeReadFingerprint(t, projectPath); after != before {
		t.Fatal("a successful backfill wrote the author database or the knowledge store")
	}
}

// TestProductionKnowledgeBackfillVectorsRefusesWithoutWriting proves the two
// refusals run the shared fence: a project path that disagrees with the frozen
// context is PROJECT_CONTEXT_CHANGED, and a lease the runtime no longer holds is
// PROJECT_SESSION_NOT_OWNED. Neither writes a byte of the author's store.
func TestProductionKnowledgeBackfillVectorsRefusesWithoutWriting(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	imported := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
		[]any{productionKnowledgeProse, productionKnowledgeNamedFile, projectPath, frozen})
	if imported["success"] != true {
		t.Fatalf("kb:import-text refused the seed the refusals need: %v", imported)
	}
	before := productionKnowledgeReadFingerprint(t, projectPath)

	stale := map[string]any{
		"projectId":   frozen["projectId"],
		"projectPath": frozen["projectPath"],
		"leaseId":     "stale-lease",
	}
	refusals := []struct {
		label string
		args  []any
		code  string
	}{
		{"foreign project path", []any{projectPath + "-elsewhere", frozen}, "PROJECT_CONTEXT_CHANGED"},
		{"stale lease", []any{projectPath, stale}, "PROJECT_SESSION_NOT_OWNED"},
	}
	for _, refusal := range refusals {
		response := client.call(t, productionKnowledgeBackfillVectorsName, refusal.args)
		if response.Code == http.StatusOK {
			t.Fatalf("%s: kb:backfill-vectors answered success instead of a refusal", refusal.label)
		}
		if !strings.Contains(response.Body.String(), refusal.code) {
			t.Fatalf("%s: kb:backfill-vectors answered %d %s, want %s",
				refusal.label, response.Code, response.Body.String(), refusal.code)
		}
	}
	if after := productionKnowledgeReadFingerprint(t, projectPath); after != before {
		t.Fatal("a refused backfill wrote the author database or the knowledge store")
	}
}
