//go:build linux

package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// This file is the re-test of the one legacy-roster name the family still
// refuses. It does two things:
//
//  1. stage: proves the adoption projection is not the proof - the adoption writes
//     no proposal row, no store file knows the legacy proposal kind, and the
//     manifest this lane freezes carries no modelReceipt for the proof's last step;
//  2. stage: the name stays at the fail-closed 501, byte-identically for the
//     in-project request, a stale lease, a foreign project, no arguments and a
//     non-object, and the admitted database is untouched.
//
// execute is no longer in this list: it is served by
// production_legacy_roster_execute_serve_linux.go and its real-HTTP success,
// repair, replacement, replay and refusal tests live beside it.

// TestProductionLegacyRosterStageSeamIsNotTheAdoptionProjection pins stage's refusal
// to the artifact proof, not to the roster projection the adoption landed.
func TestProductionLegacyRosterStageSeamIsNotTheAdoptionProjection(t *testing.T) {
	productionLegacyRosterSeamProof(t, productionLegacyRosterStageName)

	// The port that landed this session is an identity projection plus an approval
	// receipt. It writes no proposal row, so it cannot stand in for the stage write.
	adoption, err := os.ReadFile("../../internal/store/character_roster_adoption.go")
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !productionSeamContains(string(adoption), "func (r *Characters) AdoptLegacyRoster(") {
		t.Fatal("the adoption port the stage refusal cites is gone; the refusal needs re-testing")
	}
	if productionSeamContains(string(adoption), productionLegacyRosterProposalFragment) {
		t.Fatal("the adoption now writes a character_identity_proposals row; stage may be derivable from it, so serve stage")
	}

	// No store file knows the legacy proposal kind, so the writer the reference's
	// legacy arm inserts through does not exist in Go yet. This fails the day one
	// lands, which is the day stage can be built on it.
	if offender, found := productionLegacyRosterStoreLegacyKind(t); found {
		t.Fatalf("%v now names the legacy roster proposal kind; the stage writer may have landed, so serve stage", offender)
	}

	// The proof's last step needs the run's own modelReceipt.modelRevision
	// (legacy-roster-generation-proof.ts:40-41). The manifest this lane freezes does
	// not carry it, so no artifact a browser run could own is provable.
	manifest := productionLegacyRosterManifestText(
		syntheticSelection{Operation: productionLegacyRosterOperation, UIActionNonce: "legacy-roster-nonce", Output: "structured-data"},
		generationowner.Scope{Project: "legacy-roster-project", Epoch: "legacy-roster-epoch"},
		"legacy-roster-lease", "legacy-roster-fingerprint", "legacy-roster-key", "legacy-roster-epoch",
		`{"projectId":"legacy-roster-project"}`, `{"purpose":"legacy-character-roster-repair"}`)
	if productionSeamContains(manifest, `"modelReceipt"`) {
		t.Fatal("the frozen legacy roster manifest now carries modelReceipt; the proof's model step may be satisfiable, so serve stage")
	}
	if !productionSeamContains(manifest, `"legacyRosterContext"`) || !productionSeamContains(manifest, `"legacyRosterTask"`) {
		t.Fatalf("the frozen manifest is not this lane's: %v", manifest)
	}
}

// TestProductionLegacyRosterStageStaysUnbound drives the refused name through the
// real transport on a frozen session. It is not installed, so every argument shape
// - including a stale lease and a foreign project - must answer the same fail-closed
// 501, byte for byte, and leave the admitted database as it was.
func TestProductionLegacyRosterStageStaysUnbound(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatalf("%v", err)
	}
	defer cleanup()
	author := newProductionDurableClient(t, handler, productionDurableTab)
	frozen := productionLegacyRosterOpen(t, author, projectPath)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	before := productionGraphReadsBytes(t, database)

	handle := map[string]any{
		"projectId":    productionDurableProjectID,
		"epoch":        syntheticString(frozen["leaseId"]),
		"rootActionId": "legacy-roster-missing-root",
		"runId":        "legacy-roster-missing-run",
	}
	artifact := map[string]any{
		"artifactId": "legacy-roster-missing-artifact",
		"revision":   int64(0),
		"textHash":   strings.Repeat("a", 64),
	}
	request := map[string]any{"handle": handle, "artifact": artifact}
	// The three contexts a bound name would have to refuse separately: the real one,
	// a lease this session never held, and a project that is not the one admitted.
	stale := map[string]any{"projectId": productionDurableProjectID, "projectPath": projectPath,
		"leaseId": productionLegacyRosterStaleLease}
	foreign := map[string]any{"projectId": "legacy-roster-foreign-project", "projectPath": projectPath,
		"leaseId": syntheticString(frozen["leaseId"])}

	operation := productionLegacyRosterStageName
	bodies := make([][]byte, 0, 5)
	for _, args := range [][]any{
		{request, frozen},
		{request, stale},
		{request, foreign},
		{},
		{"not-an-object", frozen},
	} {
		response := author.call(t, operation, args)
		if response.Code != http.StatusNotImplemented {
			t.Fatalf("%s answered %d %s, want 501 GO_OPERATION_NOT_MIGRATED", operation, response.Code, response.Body.String())
		}
		if !bytes.Contains(response.Body.Bytes(), []byte("GO_OPERATION_NOT_MIGRATED")) {
			t.Fatalf("%s answered %d %s without GO_OPERATION_NOT_MIGRATED", operation, response.Code, response.Body.String())
		}
		bodies = append(bodies, append([]byte(nil), response.Body.Bytes()...))
	}
	for index := 1; index < len(bodies); index++ {
		if !bytes.Equal(bodies[0], bodies[index]) {
			t.Fatalf("%s answered a different body for argument shape %d; an uninstalled name answers one 501 for every shape", operation, index)
		}
	}
	productionGraphReadsUnchanged(t, database, before, operation)
}

// productionLegacyRosterStoreLegacyKind reports the first file in internal/store that
// names the legacy roster proposal kind, so a landed stage writer fails the test that
// says the writer is missing instead of passing silently.
func productionLegacyRosterStoreLegacyKind(t *testing.T) (string, bool) {
	t.Helper()
	entries, err := os.ReadDir("../../internal/store")
	if err != nil {
		t.Fatalf("%v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		path := filepath.Join("../../internal/store", entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v", err)
		}
		if productionSeamContains(string(raw), productionLegacyRosterStageLegacy) {
			return path, true
		}
	}
	return "", false
}
