//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file re-tests the bucket claim behind generation:list-recovery against
// the code this session added. The previous pass refused the whole name because
// three of its five buckets have no port; graph-generation:execute/:confirm,
// generation:compose-visible and the ledger effect writer landed none of those
// three, so the refusal still holds. The two buckets that DO have a port are
// named here so the refusal is not read as "no bucket has a port".
//
//	editors   served by editor-inline:read-recovery (production_editor_inline_linux.go)
//	batches   served by generation:list-batches      (production_batch_reads_linux.go)
//	planning  REFUSED: readPlanningContinuation and rebuildBinding have no Go port
//	drafts    REFUSED: read-context refuses the short-outline retry state
//	reviews   REFUSED: review-revision:read-recovery is not bound
//
// generation:list-recovery therefore stays unbound. The recovery binder is wired
// with only the server (production_open_linux.go:733) and cannot reach the owner
// the editors bucket projects through, and a list that dropped the three refused
// buckets would read as "there is nothing to resume", which is worse than a 501.

// productionGenerationRecoveryBucket is one bucket of GenerationRecoveryList and
// whether this lane can read it. Evidence holds one declaration per claim: for a
// served bucket the sibling name that serves it, for a refused bucket the exact
// missing port.
type productionGenerationRecoveryBucket struct {
	Name     string
	Served   bool
	Evidence []productionGenerationRecoveryLine
}

// productionGenerationRecoveryBuckets is the whole bucket table of
// generation:list-recovery, spelled once so the binder and the test cannot drift.
func productionGenerationRecoveryBuckets() []productionGenerationRecoveryBucket {
	return []productionGenerationRecoveryBucket{
		{
			Name:   "editors",
			Served: true,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "production_editor_inline_linux.go", Line: 227, Marker: "server.Bind(productionEditorInlineReadRecoveryName, read)"},
			},
		},
		{
			Name:   "batches",
			Served: true,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "production_batch_reads_linux.go", Line: 118, Marker: "{productionBatchListName, productionBatchListArity},"},
			},
		},
		{
			Name:   "planning",
			Served: false,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "../../../electron/repositories/generation-run-repository.ts", Line: 179, Marker: "readPlanningContinuation(runId: string): PlanningContinuationReceipt | null"},
				{Path: "../../internal/store/draft_commit_tx.go", Line: 56, Marker: "rebuildBinding has no Go port in this"},
			},
		},
		{
			Name:   "drafts",
			Served: false,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "production_draft_context_linux.go", Line: 720, Marker: "!= productionDraftContextOperation"},
			},
		},
		{
			Name:   "reviews",
			Served: false,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "production_review_revision_linux.go", Line: 32, Marker: "review-revision:read-recovery    NOT bound (see below)"},
			},
		},
	}
}

// bindProductionGenerationRecoveryReads verifies the bucket table against the
// lane that is actually built. It installs nothing: generation:list-recovery and
// generation:find-blueprint-recovery stay 501, and the two served buckets stay
// served by their sibling names rather than being claimed twice here.
func bindProductionGenerationRecoveryReads(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	for _, bucket := range productionGenerationRecoveryBuckets() {
		if bucket.Name == "" || len(bucket.Evidence) == 0 {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_BUCKET_INCOMPLETE")
		}
	}
	bound := server.BoundOperations()
	for _, name := range []string{"editor-inline:read-recovery", "generation:list-batches"} {
		if !productionRecoveryBoundContains(bound, name) {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_BUCKET_SERVED_MISSING")
		}
	}
	for _, name := range []string{productionGenerationRecoveryList, productionGenerationRecoveryFind} {
		if productionRecoveryBoundContains(bound, name) {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_NAME_SERVED")
		}
	}
	return nil
}

// productionRecoveryBoundContains reports whether a finished lane's bound set
// holds one operation name.
func productionRecoveryBoundContains(bound []string, name string) bool {
	for _, boundName := range bound {
		if boundName == name {
			return true
		}
	}
	return false
}
