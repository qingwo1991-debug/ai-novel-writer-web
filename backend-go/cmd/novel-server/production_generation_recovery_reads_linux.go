//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the bucket table of generation:list-recovery. The name is served
// now (production_generation_recovery_serve3_linux.go), and each of its five
// buckets has one of two fates:
//
//	editors   REAL  projected by editor-inline:read-recovery (the served sibling)
//	batches   REAL  projected by generation:list-batches (the served sibling)
//	planning  REAL  projected by this family's readContext port (continued and
//	                unavailable directory contexts)
//	drafts    []    its receipt is absent in this lane
//	reviews   []    its receipt is absent in this lane
//
// drafts/reviews are [] because the ports the reference reads are unbound, not
// because the reads failed, so the list's own partial flag stays false for them.

// The three reasons a bucket can carry. They are this file's own vocabulary, in
// the symbolic shape the lane publishes, and they never reach a renderer.
const (
	// productionRecoveryBucketReasonServed is a bucket a sibling name already
	// answers: the list copies that projection, not an invented one.
	productionRecoveryBucketReasonServed = "SERVED_BY_SIBLING_NAME"
	// productionRecoveryBucketReasonAbsent is a bucket answered as an empty array
	// because the receipt it would project is absent in this lane.
	productionRecoveryBucketReasonAbsent = "EMPTY_BY_ABSENCE"
	// productionRecoveryBucketReasonContext is a bucket projected by this family's
	// own recovery-context reader rather than by a sibling operation.
	productionRecoveryBucketReasonContext = "SERVED_BY_RECOVERY_CONTEXT"
)

// productionGenerationRecoveryBucket is one bucket of GenerationRecoveryList,
// whether the list serves it, and which kind of answer it is. Evidence holds one
// declaration per claim.
type productionGenerationRecoveryBucket struct {
	Name     string
	Served   bool
	Reason   string
	Evidence []productionGenerationRecoveryLine
}

// productionRecoveryBucketReasonKnown reports whether one bucket reason is one
// this file publishes. A typo therefore fails startup instead of being recorded
// as a new kind of answer.
func productionRecoveryBucketReasonKnown(reason string) bool {
	switch reason {
	case productionRecoveryBucketReasonServed, productionRecoveryBucketReasonAbsent,
		productionRecoveryBucketReasonContext:
		return true
	}
	return false
}

// productionGenerationRecoveryBuckets is the whole bucket table of
// generation:list-recovery, spelled once so the binder and the test cannot drift.
func productionGenerationRecoveryBuckets() []productionGenerationRecoveryBucket {
	return []productionGenerationRecoveryBucket{
		{
			Name:   "editors",
			Served: true,
			Reason: productionRecoveryBucketReasonServed,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "production_editor_inline_linux.go", Line: 227, Marker: "server.Bind(productionEditorInlineReadRecoveryName, read)"},
			},
		},
		{
			Name:   "batches",
			Served: true,
			Reason: productionRecoveryBucketReasonServed,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "production_batch_reads_linux.go", Line: 118, Marker: "{productionBatchListName, productionBatchListArity},"},
			},
		},
		{
			Name:   "planning",
			Served: true,
			Reason: productionRecoveryBucketReasonContext,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "production_generation_recovery_serve3_linux.go", Line: 118, Marker: "planning, partial, err := r.planning(ctx, caller, args[0], expected.ProjectID, expected.LeaseID)"},
				{Path: "../../internal/store/planning_continuation.go", Line: 63,
					Marker: "func ListDirectoryRecoveryNeeds(ctx context.Context, db *sql.DB, projectID string) ([]*DirectoryRecoveryNeed, bool, error)"},
			},
		},
		{
			Name:   "drafts",
			Served: false,
			Reason: productionRecoveryBucketReasonAbsent,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "production_generation_recovery_serve3_linux.go", Line: 82, Marker: "Drafts:   []map[string]any{}"},
			},
		},
		{
			Name:   "reviews",
			Served: false,
			Reason: productionRecoveryBucketReasonAbsent,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "production_generation_recovery_serve3_linux.go", Line: 79, Marker: "Reviews:  []map[string]any{}"},
			},
		},
	}
}

// bindProductionGenerationRecoveryReads verifies the bucket table against the
// lane that is actually built. It installs nothing: the two names are bound by
// bindProductionGenerationRecoveryServed, editors and batches stay served by
// their sibling names, and planning is projected by the recovery context reader.
func bindProductionGenerationRecoveryReads(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	for _, bucket := range productionGenerationRecoveryBuckets() {
		if bucket.Name == "" || len(bucket.Evidence) == 0 ||
			!productionRecoveryBucketReasonKnown(bucket.Reason) {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_BUCKET_INCOMPLETE")
		}
	}
	bound := server.BoundOperations()
	for _, name := range []string{"editor-inline:read-recovery", "generation:list-batches"} {
		if !productionRecoveryBoundContains(bound, name) {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_BUCKET_SERVED_MISSING")
		}
	}
	// Both names are served now, so a lane that does not install them is not the
	// lane this table describes.
	for _, name := range []string{productionGenerationRecoveryList, productionGenerationRecoveryFind} {
		if !productionRecoveryBoundContains(bound, name) {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_NAME_MISSING")
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
