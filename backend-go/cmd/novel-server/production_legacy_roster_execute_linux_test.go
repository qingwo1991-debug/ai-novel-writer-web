//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// This file is the re-test of the two legacy-roster names the family still
// refuses. It does three things:
//
//  1. execute: proves the term still missing is prose (the repair and replacement
//     prompts), by pinning the committed prompt asset's key set and deriving the
//     unshipped fragments from the loader. When that prose lands, the asset pin
//     fails first, which is the signal to serve execute instead of refusing it;
//  2. stage: proves the adoption projection is not the proof - the adoption writes
//     no proposal row, no store file knows the legacy proposal kind, and the
//     manifest this lane freezes carries no modelReceipt for the proof's last step;
//  3. both: the names stay at the fail-closed 501, byte-identically for the
//     in-project request, a stale lease, a foreign project, no arguments and a
//     non-object, and the admitted database is untouched.

const (
	// productionLegacyRosterPromptAssetPath is the committed asset the loader embeds
	// (internal/prompttemplate/legacyroster.go:36).
	productionLegacyRosterPromptAssetPath = "../../internal/prompttemplate/legacy-roster-v1.json"
	// productionLegacyRosterPromptAssetKeys is that asset's whole key set, sorted.
	// Two of the five are the shipped prose fragments; a sixth key is a prompt the
	// six-step loop needs, which is why the pin fails rather than shrugs.
	productionLegacyRosterPromptAssetKeys = "contract,formatVersion,key,kind,systemPrompt"
	// productionLegacyRosterRetryPattern is the worked dispatch/readback this lane
	// would build execute on (production_retry_draft_outline_linux.go:145).
	productionLegacyRosterRetryPattern = "service.Execute(ctx, frozen, handle.RunID,"
)

// TestProductionLegacyRosterExecuteSeamIsTheUnshippedPrompts pins execute's refusal
// to the one thing that is actually missing: the repair and replacement prompts.
func TestProductionLegacyRosterExecuteSeamIsTheUnshippedPrompts(t *testing.T) {
	// Every term the seam table names is still authored at the line it claims.
	productionLegacyRosterSeamProof(t, productionLegacyRosterExecuteName)

	// The dispatch/readback the loop would reuse still exists, so "buildable" in the
	// refusal is a fact about this module and not a hope.
	pattern, err := os.ReadFile("production_retry_draft_outline_linux.go")
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !productionSeamContains(string(pattern), productionLegacyRosterRetryPattern) {
		t.Fatal("the frozen-task dispatch pattern the execute refusal cites is gone; the refusal needs a new dispatch term")
	}

	// What the loader answers: the two shipped fragments, in the asset's order.
	shipped, err := productionLegacyRosterShippedPrompts()
	if err != nil {
		t.Fatalf("%v", err)
	}
	if joined := strings.Join(shipped, ","); joined != productionLegacyRosterSystemFragment+","+productionLegacyRosterContractFragment {
		t.Fatalf("the loader answers %s, want the two shipped roster fragments only", joined)
	}

	// What the committed asset carries: the authority the loader cannot widen
	// without this test moving with it.
	raw, err := os.ReadFile(productionLegacyRosterPromptAssetPath)
	if err != nil {
		t.Fatalf("%v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("%v", err)
	}
	keys := make([]string, 0, len(document))
	for key := range document {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if joined := strings.Join(keys, ","); joined != productionLegacyRosterPromptAssetKeys {
		t.Fatalf("the legacy roster prompt asset now carries %s; a prompt fragment the six-step loop composes may have landed, so execute must be served instead of refused", joined)
	}

	// And the derivation the refusal is written from agrees.
	unshipped := productionLegacyRosterUnshippedPrompts()
	if len(unshipped) != 3 {
		t.Fatalf("execute derives %d unshipped prompt fragments, want the repair system prompt, its wrapper and the replacement prompt", len(unshipped))
	}
	for _, name := range productionLegacyRosterLoopPromptFragments() {
		found := false
		for _, candidate := range unshipped {
			if candidate == name {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s is a prompt fragment execute needs and the derivation lost it", name)
		}
	}
}

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

// TestProductionLegacyRosterExecuteAndStageStayUnbound drives both names through the
// real transport on a frozen session. Neither is installed, so every argument shape
// - including a stale lease and a foreign project - must answer the same fail-closed
// 501, byte for byte, and leave the admitted database as it was.
func TestProductionLegacyRosterExecuteAndStageStayUnbound(t *testing.T) {
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
	// The three contexts a bound name would have to refuse separately: the real one,
	// a lease this session never held, and a project that is not the one admitted.
	stale := map[string]any{"projectId": productionDurableProjectID, "projectPath": projectPath,
		"leaseId": productionLegacyRosterStaleLease}
	foreign := map[string]any{"projectId": "legacy-roster-foreign-project", "projectPath": projectPath,
		"leaseId": syntheticString(frozen["leaseId"])}

	requests := map[string]any{
		productionLegacyRosterExecuteName: map[string]any{"handle": handle},
		productionLegacyRosterStageName:   map[string]any{"handle": handle, "artifact": artifact},
	}
	for operation, request := range requests {
		t.Run(operation, func(t *testing.T) {
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
		})
	}
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
