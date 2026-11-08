//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file proves legacy-roster:stage over the real HTTP handler, through the
// frozen project session, with a real admitted author database, a real model
// lease, and the model stubbed at the lane's own seam. The lane is built by this
// file rather than by productionHandlerWithContract because the binder line that
// installs the name into the product lane lives in production_open_linux.go's
// owner block and that file does not belong to this change; the binder itself is
// the one under test. Every success is read back through a mode=ro connection of
// the test's own, so the staged row is proved by a reader that cannot write it.

// productionLegacyRosterStageServeOpen builds the lane with begin/read/execute
// and stage all bound. It fails rather than skips when the owner cannot be
// built: the fixture already skips without the shipped schema oracle.
func productionLegacyRosterStageServeOpen(t *testing.T, answers []string, nonce string) *productionLegacyRosterExecuteServeLane {
	t.Helper()
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	productionLegacyRosterSeed(t, database, []productionLegacyRosterStatement{
		{Query: "INSERT OR REPLACE INTO character_identity_meta (id, revision, legacy_count) VALUES ('main', 0, 0)"},
		{Query: `INSERT OR REPLACE INTO character_roster_meta
 (id, schema_version, revision, migration_state, legacy_markdown, projection_hash, fact_hash)
 VALUES ('main', 1, 0, 'legacy_markdown_pending', ?, '', '')`,
			Args: []any{productionLegacyRosterExecuteMarkdown}},
		{Query: "DELETE FROM characters"},
		{Query: "UPDATE project_core SET writing_language='zh-CN' WHERE id='main'"},
	})
	listener, origin := productionGenerationWriteListener(t)
	productionGenerationWriteModel(t, root, origin)
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatalf("%v", err)
	}
	open, err := openProduction(context.Background(), layout, productionConfig{
		Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatalf("the lane could not open the admitted project: %v", err)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "go-production-legacy-roster-stage")
	if err := server.BindNavigation(); err != nil {
		_ = open.Close()
		t.Fatalf("the lane could not bind navigation: %v", err)
	}
	if err := server.BindProjects(open.durable); err != nil {
		_ = open.Close()
		t.Fatalf("the lane could not bind the project lifecycle: %v", err)
	}
	seam := &productionLegacyRosterExecuteSeam{answers: answers, finishes: nil}
	owner, err := newProductionGeneration(open, server, seam.dispatch)
	if err != nil {
		_ = open.Close()
		t.Fatalf("the generation owner: %v", err)
	}
	if err := newProductionLegacyRosterServe(owner).Bind(server); err != nil {
		_ = open.Close()
		t.Fatalf("the legacy-roster serve failed to bind: %v", err)
	}
	if err := newProductionLegacyRosterExecuteServe(owner).Bind(server); err != nil {
		_ = open.Close()
		t.Fatalf("the legacy-roster execute serve failed to bind: %v", err)
	}
	if err := newProductionLegacyRosterStageServe(owner).Bind(server); err != nil {
		_ = open.Close()
		t.Fatalf("the legacy-roster stage serve failed to bind: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", server)
	mux.Handle("/api", server)
	t.Cleanup(func() {
		server.Close()
		_ = open.Close()
	})
	author, frozen := productionExecuteSession(t, mux, projectPath, nonce+"-open")
	var recovery struct {
		View    map[string]any `json:"view"`
		ModelID string         `json:"modelId"`
	}
	author.decode(t, productionLegacyRosterBeginName,
		[]any{map[string]any{"modelId": productionTestModelID, "uiActionNonce": nonce}, frozen}, &recovery)
	handle := syntheticMap(recovery.View["handle"])
	if handle == nil || syntheticString(handle["runId"]) == "" || syntheticString(handle["rootActionId"]) == "" {
		t.Fatalf("legacy-roster:begin returned no run handle: %v", recovery.View)
	}
	return &productionLegacyRosterExecuteServeLane{
		projectPath: projectPath,
		database:    database,
		listener:    listener,
		origin:      origin,
		author:      author,
		frozen:      frozen,
		runID:       syntheticString(handle["runId"]),
		seam:        seam,
	}
}

// productionLegacyRosterStageArtifact reads the settled artifact's three-key
// reference back out of the database, so the request under test is the run's own
// artifact, not this test's expectation.
func productionLegacyRosterStageArtifact(t *testing.T, db *sql.DB, runID string) map[string]any {
	t.Helper()
	var artifactID, artifactJSON string
	if err := db.QueryRow(`SELECT artifact_id, artifact_json FROM generation_artifacts WHERE run_id=?`, runID).Scan(&artifactID, &artifactJSON); err != nil {
		t.Fatalf("read the artifact row: %v", err)
	}
	var artifact struct {
		Revision int64  `json:"revision"`
		TextHash string `json:"textHash"`
	}
	if err := json.Unmarshal([]byte(artifactJSON), &artifact); err != nil {
		t.Fatalf("decode the artifact: %v", err)
	}
	return map[string]any{"artifactId": artifactID, "revision": artifact.Revision, "textHash": artifact.TextHash}
}

// TestProductionLegacyRosterStageServesTheExecutedRun drives an executed run
// through stage, reads the staged proposal back over a mode=ro connection, and
// asserts the row is the same pending proposal the pending-finalized reads page:
// the family's source key, a self-hashing envelope, a pending batch whose source
// kind is the legacy roster generation, and a provenance whose model revision is
// the run's own frozen modelReceipt.modelRevision.
func TestProductionLegacyRosterStageServesTheExecutedRun(t *testing.T) {
	lane := productionLegacyRosterStageServeOpen(t,
		[]string{productionLegacyRosterExecuteRoster}, "legacy-roster-stage-success")
	handle := lane.handle(t)
	lane.answer(t, handle) // settle one attempt and write its artifact

	db := productionBatchWriteReadOnly(t, lane.database)
	artifact := productionLegacyRosterStageArtifact(t, db, lane.runID)

	var stage struct {
		ProposalBatchID string         `json:"proposalBatchId"`
		Status          string         `json:"status"`
		Source          map[string]any `json:"source"`
	}
	lane.author.decode(t, productionLegacyRosterStageName,
		[]any{map[string]any{"handle": handle, "artifact": artifact}, lane.frozen}, &stage)
	if stage.ProposalBatchID == "" || stage.Status != productionCharacterProposalStatusPending {
		t.Fatalf("stage answered a non-pending batch: %+v", stage)
	}
	if syntheticString(stage.Source["kind"]) != productionCharacterProposalLegacyKind {
		t.Fatalf("stage answered a batch whose source is not the legacy kind: %+v", stage.Source)
	}

	// The row the pending-finalized reads page: the family source key, and the
	// stored bytes still hash to their recorded hash.
	var sourceKey, sourceHash, rawValue string
	if err := db.QueryRow(`SELECT source_key, source_hash, raw_value FROM character_identity_proposals WHERE proposal_id=?`, stage.ProposalBatchID).
		Scan(&sourceKey, &sourceHash, &rawValue); err != nil {
		t.Fatalf("read the staged proposal row: %v", err)
	}
	if sourceKey != productionCharacterProposalSourceKeyPrefix+syntheticString(lane.frozen["projectId"]) {
		t.Fatalf("the staged row source key is %s, want the project's proposal prefix", sourceKey)
	}
	if sourceHash != productionLegacyRosterTextHash(rawValue) {
		t.Fatal("the staged envelope no longer hashes to its recorded source_hash")
	}
	var envelope struct {
		Version   int64          `json:"version"`
		ProjectID string         `json:"projectId"`
		Proof     map[string]any `json:"proof"`
		Batch     map[string]any `json:"batch"`
	}
	if err := json.Unmarshal([]byte(rawValue), &envelope); err != nil {
		t.Fatalf("decode the staged envelope: %v", err)
	}
	if envelope.Version != 1 || envelope.ProjectID != syntheticString(lane.frozen["projectId"]) ||
		syntheticString(envelope.Batch["proposalBatchId"]) != stage.ProposalBatchID ||
		syntheticString(envelope.Batch["status"]) != productionCharacterProposalStatusPending {
		t.Fatalf("the staged envelope is not the pending proposal this project staged: %+v", envelope)
	}
	provenance := syntheticMap(envelope.Proof["provenance"])
	if provenance == nil || syntheticString(provenance["kind"]) != "generated" {
		t.Fatalf("the staged proof carries no generated provenance: %+v", envelope.Proof)
	}
	modelRevision := syntheticString(provenance["modelRevision"])
	if len(modelRevision) != 64 {
		t.Fatalf("the staged provenance modelRevision is not a 64-hex digest: %s", modelRevision)
	}

	// The provenance modelRevision is the run's own frozen modelReceipt.modelRevision,
	// read from the manifest, not a value the caller guessed.
	var bindingJSON string
	if err := db.QueryRow(`SELECT binding_json FROM generation_runs WHERE run_id=?`, lane.runID).Scan(&bindingJSON); err != nil {
		t.Fatalf("read the run binding: %v", err)
	}
	var binding struct {
		SourceManifest struct {
			ModelReceipt struct {
				ModelRevision string `json:"modelRevision"`
			} `json:"modelReceipt"`
		} `json:"sourceManifest"`
	}
	if err := json.Unmarshal([]byte(bindingJSON), &binding); err != nil {
		t.Fatalf("decode the run binding: %v", err)
	}
	if binding.SourceManifest.ModelReceipt.ModelRevision == "" ||
		binding.SourceManifest.ModelReceipt.ModelRevision != modelRevision {
		t.Fatalf("the staged modelRevision %s does not name the frozen modelReceipt %s",
			modelRevision, binding.SourceManifest.ModelReceipt.ModelRevision)
	}

	// The effect was stamped on the settled attempt, so the run is sealed against
	// re-dispatch.
	var usageJSON string
	if err := db.QueryRow(`SELECT usage_receipt_json FROM generation_attempts WHERE run_id=?`, lane.runID).Scan(&usageJSON); err != nil {
		t.Fatalf("read the settled attempt: %v", err)
	}
	if !strings.Contains(usageJSON, `"legacyRosterProposal"`) {
		t.Fatal("the settled attempt carries no legacyRosterProposal effect")
	}
}

// TestProductionLegacyRosterStageRefusesARunWithoutASettledAttempt drives a run
// that was begun but never executed: the store's artifactFor finds no attempt and
// refuses by the artifact code, and nothing is staged.
func TestProductionLegacyRosterStageRefusesARunWithoutASettledAttempt(t *testing.T) {
	lane := productionLegacyRosterStageServeOpen(t, nil, "legacy-roster-stage-no-attempt")
	handle := lane.handle(t)
	artifact := map[string]any{"artifactId": "legacy-roster-stage-no-artifact", "revision": int64(0), "textHash": strings.Repeat("a", 64)}
	response := lane.author.call(t, productionLegacyRosterStageName,
		[]any{map[string]any{"handle": handle, "artifact": artifact}, lane.frozen})
	if response.Code == http.StatusOK {
		t.Fatalf("%s answered 200 for a run with no attempt: %s", productionLegacyRosterStageName, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), productionLegacyRosterArtifactInvalid) {
		t.Fatalf("%s refused with %s, want %s", productionLegacyRosterStageName, response.Body.String(), productionLegacyRosterArtifactInvalid)
	}
	db := productionBatchWriteReadOnly(t, lane.database)
	var proposals int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM character_identity_proposals`).Scan(&proposals); err != nil {
		t.Fatalf("count proposals: %v", err)
	}
	if proposals != 0 {
		t.Fatalf("a refused stage wrote %d proposal rows, want none", proposals)
	}
}

// TestProductionLegacyRosterStageRefusesStaleLeaseAndForeignProject pins the two
// context refusals: the same request bytes under a lease this session never
// held, and a handle that names another project, answer exactly what the
// family's served read answers for them - byte for byte - and neither writes.
func TestProductionLegacyRosterStageRefusesStaleLeaseAndForeignProject(t *testing.T) {
	lane := productionLegacyRosterStageServeOpen(t, nil, "legacy-roster-stage-refusals")
	handle := lane.handle(t)
	artifact := map[string]any{"artifactId": "legacy-roster-stage-artifact", "revision": int64(0), "textHash": strings.Repeat("a", 64)}
	stale := map[string]any{"projectId": syntheticString(lane.frozen["projectId"]),
		"projectPath": lane.projectPath, "leaseId": productionLegacyRosterExecuteStaleLease}
	foreignHandle := map[string]any{
		"projectId":    productionLegacyRosterExecuteForeignProject,
		"epoch":        syntheticString(handle["epoch"]),
		"rootActionId": syntheticString(handle["rootActionId"]),
		"runId":        syntheticString(handle["runId"]),
	}
	for _, refusal := range []struct {
		label  string
		handle map[string]any
		ctx    map[string]any
		code   string
	}{
		{"stale lease", handle, stale, ""},
		{"foreign project", foreignHandle, lane.frozen, productionLegacyRosterIdentityMismatch},
	} {
		refusal := refusal
		t.Run(refusal.label, func(t *testing.T) {
			stage := lane.author.call(t, productionLegacyRosterStageName,
				[]any{map[string]any{"handle": refusal.handle, "artifact": artifact}, refusal.ctx})
			read := lane.author.call(t, productionLegacyRosterReadName,
				[]any{map[string]any{"handle": refusal.handle}, refusal.ctx})
			if stage.Code == http.StatusOK {
				t.Fatalf("%s was served for a %s: %s", productionLegacyRosterStageName, refusal.label, stage.Body.String())
			}
			if stage.Code != read.Code || !bytes.Equal(stage.Body.Bytes(), read.Body.Bytes()) {
				t.Fatalf("a %s answered differently per name: stage %d %s, read %d %s",
					refusal.label, stage.Code, stage.Body.String(), read.Code, read.Body.String())
			}
			if refusal.code != "" && !strings.Contains(stage.Body.String(), refusal.code) {
				t.Fatalf("a %s answered %s, want %s", refusal.label, stage.Body.String(), refusal.code)
			}
		})
	}
}

// TestProductionLegacyRosterStageBinder pins the fail-closed shape: no owner, no
// name, and a nil server is refused.
func TestProductionLegacyRosterStageBinder(t *testing.T) {
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
	if err := newProductionLegacyRosterStageServe(nil).Bind(server); err != nil {
		t.Fatalf("the stage binder failed with a nil owner: %v", err)
	}
	if names := server.BoundOperations(); len(names) != 0 {
		t.Fatalf("the stage binder installed %v with a nil owner, want nothing", names)
	}
	if err := newProductionLegacyRosterStageServe(nil).Bind(nil); err == nil {
		t.Fatal("a nil server was accepted")
	}
}
