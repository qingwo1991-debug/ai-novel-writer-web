//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// This file is the re-test of legacy-roster:stage against the execute serve that
// landed this session. execute now settles attempts and writes generation_artifacts
// rows, so a settled/unknown attempt plus an artifact CAN exist for a run this lane
// opened. This file proves both halves of that claim:
//
//   - the positive half, over real HTTP and a separate mode=ro connection: an
//     executed run owns a settled attempt, a stop finish, a terminal artifact whose
//     text passes the candidate gate, and still no model revision anywhere the proof
//     would read it;
//   - the negative half: the name stays at the fail-closed 501 for the settled run,
//     the run with no attempt, a stale lease and a foreign project, byte for byte,
//     and no proposal row is written.

// TestProductionLegacyRosterStageSeamAfterExecuteLanded re-reads every fact the
// refusal turns on, so a landed model-revision port fails this test instead of
// leaving a refusal that points at nothing.
func TestProductionLegacyRosterStageSeamAfterExecuteLanded(t *testing.T) {
	// The settled half: the artifact row, the settle arm, the terminal-attempt gate
	// and the execute dispatch are what the re-test now counts on.
	for _, fact := range productionLegacyRosterStageSettled() {
		raw, err := os.ReadFile(fact.Path)
		if err != nil {
			t.Fatalf("%s: %s: %v", fact.Term, fact.Path, err)
		}
		if !productionSeamContains(string(raw), fact.Marker) {
			t.Fatalf("%s no longer documents %s in %s (line %d)", fact.Term, fact.Marker, fact.Path, fact.Line)
		}
	}
	// The unreadable half: the manifest writer, the unexported binding field, the
	// digest that folds it, and the proof's model step.
	for _, fact := range productionLegacyRosterStageUnreadable() {
		raw, err := os.ReadFile(fact.Path)
		if err != nil {
			t.Fatalf("%s: %s: %v", fact.Term, fact.Path, err)
		}
		if !productionSeamContains(string(raw), fact.Marker) {
			t.Fatalf("%s no longer documents %s in %s (line %d)", fact.Term, fact.Marker, fact.Path, fact.Line)
		}
	}

	if productionLegacyRosterStageRevisionSymbol != "generationowner.Binding.modelRevision" {
		t.Fatalf("the refusal names %s, want the binding field", productionLegacyRosterStageRevisionSymbol)
	}
	binding, err := os.ReadFile("../../internal/generationowner/binding.go")
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !productionSeamContains(string(binding), "modelRevision string") {
		t.Fatal("the binding field the refusal names is gone")
	}
	if productionSeamContains(string(binding), "ModelRevision()") {
		t.Fatal("the binding now exposes the model revision; the proof can read it, so serve stage")
	}

	// The writer and the effect arm are still missing: no store file names the
	// legacy proposal kind, and none names the sealing effect key.
	if offender, found := productionLegacyRosterStoreLegacyKind(t); found {
		t.Fatalf("%v now names the legacy roster proposal kind; serve stage", offender)
	}
	if offender, found := productionLegacyRosterStageEffectInStore(t); found {
		t.Fatalf("%v now names the legacyRosterProposal effect; serve stage", offender)
	}

	// The name stays in the family's refusal table.
	if gap, found := productionLegacyRosterSeam(productionLegacyRosterStageName); !found || len(gap.Missing) == 0 {
		t.Fatalf("%s is no longer a documented seam: %v", productionLegacyRosterStageName, gap)
	}
}

// TestProductionLegacyRosterStageRefusesTheExecutedRun drives the refused name
// through the real HTTP handler on the run execute just settled, then reads the
// run's facts back over a separate mode=ro connection.
func TestProductionLegacyRosterStageRefusesTheExecutedRun(t *testing.T) {
	lane := productionLegacyRosterExecuteServeOpen(t,
		[]string{productionLegacyRosterExecuteRoster}, nil, "legacy-roster-stage-executed")
	executed := lane.handle(t)
	lane.answer(t, executed) // settle one attempt and write its artifact

	// A second run with no settled attempt: begin only.
	var second struct {
		View map[string]any `json:"view"`
	}
	lane.author.decode(t, productionLegacyRosterBeginName,
		[]any{map[string]any{"modelId": productionTestModelID, "uiActionNonce": "legacy-roster-stage-empty"}, lane.frozen}, &second)
	empty := syntheticMap(second.View["handle"])
	if empty == nil || syntheticString(empty["runId"]) == "" {
		t.Fatalf("the second begin returned no handle: %v", second.View)
	}

	db := productionBatchWriteReadOnly(t, lane.database)

	// The settled attempt: settled, a stop finish, and no failure code.
	var status, usageJSON string
	if err := db.QueryRow(`SELECT status, usage_receipt_json FROM generation_attempts WHERE run_id=?`, lane.runID).Scan(&status, &usageJSON); err != nil {
		t.Fatalf("read the settled attempt: %v", err)
	}
	if status != "settled" {
		t.Fatalf("the executed attempt is %s, want settled", status)
	}
	var usage struct {
		Result struct {
			FinishReason string `json:"finishReason"`
			FailureCode  string `json:"failureCode"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(usageJSON), &usage); err != nil {
		t.Fatalf("decode the attempt usage: %v", err)
	}
	if usage.Result.FinishReason != productionLegacyRosterFinishStop || usage.Result.FailureCode != "" {
		t.Fatalf("the settled attempt answers finish %s failure %s, want a clean stop", usage.Result.FinishReason, usage.Result.FailureCode)
	}

	// The artifact row: owned by the run, not discarded, self-hashing, and its text
	// passes the candidate gate the proof's parse step requires.
	var artifactID, artifactStatus, artifactJSON, artifactRunID string
	if err := db.QueryRow(`SELECT artifact_id,status,artifact_json,run_id FROM generation_artifacts WHERE run_id=?`, lane.runID).Scan(&artifactID, &artifactStatus, &artifactJSON, &artifactRunID); err != nil {
		t.Fatalf("read the artifact row: %v", err)
	}
	if artifactRunID != lane.runID || artifactStatus == "discarded" {
		t.Fatalf("the artifact %s belongs to %s with status %s", artifactID, artifactRunID, artifactStatus)
	}
	var artifact struct {
		Text     string `json:"text"`
		TextHash string `json:"textHash"`
	}
	if err := json.Unmarshal([]byte(artifactJSON), &artifact); err != nil {
		t.Fatalf("decode the artifact: %v", err)
	}
	if productionLegacyRosterTextHash(artifact.Text) != artifact.TextHash {
		t.Fatal("the artifact text does not hash to its own textHash")
	}
	if err := productionLegacyRosterCandidateFromText(artifact.Text); err != nil {
		t.Fatalf("the settled artifact does not pass the candidate gate: %v", err)
	}

	// The model revision is absent from every document the proof would read.
	var bindingJSON string
	if err := db.QueryRow(`SELECT binding_json FROM generation_runs WHERE run_id=?`, lane.runID).Scan(&bindingJSON); err != nil {
		t.Fatalf("read the run binding: %v", err)
	}
	for _, document := range []struct {
		name string
		text string
	}{
		{"manifest", bindingJSON},
		{"usage receipt", usageJSON},
		{"artifact", artifactJSON},
	} {
		if strings.Contains(document.text, "modelRevision") || strings.Contains(document.text, "modelReceipt") {
			t.Fatalf("the %s carries a model revision the proof could read", document.name)
		}
	}

	// Nothing was staged.
	var proposals int
	if err := db.QueryRow(`SELECT COUNT(*) FROM character_identity_proposals`).Scan(&proposals); err != nil {
		t.Fatalf("count proposals: %v", err)
	}
	if proposals != 0 {
		t.Fatalf("staging wrote %d proposal rows, want none", proposals)
	}

	// The name answers one 501 for every shape - the settled run, the run with no
	// attempt, a stale lease and a foreign project - byte for byte.
	request := func(handle map[string]any) map[string]any {
		return map[string]any{"handle": handle, "artifact": map[string]any{
			"artifactId": "legacy-roster-stage-artifact", "revision": int64(0), "textHash": strings.Repeat("a", 64)}}
	}
	stale := map[string]any{"projectId": syntheticString(lane.frozen["projectId"]),
		"projectPath": lane.projectPath, "leaseId": productionLegacyRosterExecuteStaleLease}
	foreign := map[string]any{
		"projectId":    productionLegacyRosterExecuteForeignProject,
		"epoch":        syntheticString(executed["epoch"]),
		"rootActionId": syntheticString(executed["rootActionId"]),
		"runId":        syntheticString(executed["runId"]),
	}
	bodies := make([][]byte, 0, 4)
	for _, entry := range []struct {
		name string
		args []any
	}{
		{"settled run", []any{request(executed), lane.frozen}},
		{"no attempt", []any{request(empty), lane.frozen}},
		{"stale lease", []any{request(executed), stale}},
		{"foreign project", []any{request(foreign), lane.frozen}},
	} {
		response := lane.author.call(t, productionLegacyRosterStageName, entry.args)
		if response.Code != http.StatusNotImplemented || !bytes.Contains(response.Body.Bytes(), []byte("GO_OPERATION_NOT_MIGRATED")) {
			t.Fatalf("%s: stage answered %d %s, want 501 GO_OPERATION_NOT_MIGRATED", entry.name, response.Code, response.Body.String())
		}
		bodies = append(bodies, append([]byte(nil), response.Body.Bytes()...))
	}
	for index := 1; index < len(bodies); index++ {
		if !bytes.Equal(bodies[0], bodies[index]) {
			t.Fatalf("stage answered different bodies for shape %d; an uninstalled name answers one 501", index)
		}
	}
}

// productionLegacyRosterStageEffectInStore reports the first file in internal/store
// that names the sealing effect key, so a landed ledger arm fails the test that says
// the arm is missing instead of passing silently.
func productionLegacyRosterStageEffectInStore(t *testing.T) (string, bool) {
	t.Helper()
	entries, err := os.ReadDir("../../internal/store")
	if err != nil {
		t.Fatalf("%v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		path := "../../internal/store/" + entry.Name()
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v", err)
		}
		if productionSeamContains(string(raw), "legacyRosterProposal") {
			return path, true
		}
	}
	return "", false
}
