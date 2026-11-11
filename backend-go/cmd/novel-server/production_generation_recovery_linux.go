//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the binder for the recovery family. All three inventoried names
// are now served:
//
//	generation:list-recovery             SERVED  production_generation_recovery_serve3_linux.go
//	generation:find-blueprint-recovery   SERVED  production_generation_recovery_serve3_linux.go
//	generation:list-directory-progress   SERVED  production_batch_reads_linux.go (sibling)
//
// The two names this file binds take the frozen project session as their only
// wire argument (request arity zero), and they project only rows this lane can
// read: the batches and editors buckets reuse the served sibling projections,
// the planning bucket carries the directory recovery contexts, and
// find-blueprint-recovery answers the newest unavailable or continued context.
// A proved planning continuation short-circuits to 'continued' before any
// fingerprint compare (main-generation-owner.ts:787); the remaining writeState
// arms still need the rebuildBinding comparison this lane does not port.

const (
	productionGenerationRecoveryList      = "generation:list-recovery"
	productionGenerationRecoveryFind      = "generation:find-blueprint-recovery"
	productionGenerationRecoveryDirectory = "generation:list-directory-progress"
)

// productionGenerationRecoveryLine is one declaration that has to exist for a
// claim in this file to stay true: the path of the file, the line inside it, and
// a marker that must appear in that file. The tests re-read every one of them, so
// a moved or deleted declaration fails this lane's test instead of leaving a note
// that points at nothing.
type productionGenerationRecoveryLine struct {
	Path   string
	Line   int
	Marker string
}

// productionGenerationRecoverySeam is one refused name and the missing piece that
// keeps it unbound. Nothing is refused now, so the table is empty and the type is
// kept for the sub-verifiers that still walk it.
type productionGenerationRecoverySeam struct {
	Operation string
	Missing   []productionGenerationRecoveryLine
}

// productionGenerationRecoveryServed is one name this lane serves plus the
// declarations that make that true.
type productionGenerationRecoveryServed struct {
	Operation string
	Evidence  []productionGenerationRecoveryLine
}

// productionGenerationRecoveryServedOperations is every name of the family this
// finished lane serves, spelled once so the binder and the test cannot drift.
func productionGenerationRecoveryServedOperations() []string {
	return []string{
		productionGenerationRecoveryDirectory,
		productionGenerationRecoveryList,
		productionGenerationRecoveryFind,
	}
}

// productionGenerationRecoveryRefusedOperations is empty: no recovery name stays
// 501. The function is kept because the sub-verifiers and tests still walk it.
func productionGenerationRecoveryRefusedOperations() []string {
	return []string{}
}

// productionGenerationRecoveryServedEvidence is the proof for every served name.
// Each row names a declaration in another file that the test re-reads from disk,
// so "served" cannot quietly become "served by nobody".
func productionGenerationRecoveryServedEvidence() []productionGenerationRecoveryServed {
	return []productionGenerationRecoveryServed{
		{
			Operation: productionGenerationRecoveryDirectory,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "production_batch_reads_linux.go", Line: 99, Marker: `productionBatchDirectoryName = "generation:list-directory-progress"`},
				{Path: "production_batch_reads_linux.go", Line: 203, Marker: "store.ListDirectoryGenerationProgress(ctx, db)"},
				{Path: "production_open_linux.go", Line: 701, Marker: "newProductionBatchReads(open.durable).Bind(server)"},
				{Path: "../../internal/store/batch_progress.go", Line: 280,
					Marker: "func ListDirectoryGenerationProgress(ctx context.Context, db *sql.DB) (*DirectoryGenerationCatalog, error)"},
				{Path: "../../../web/operations.json", Line: 224, Marker: `"generation:list-directory-progress": "project"`},
			},
		},
		{
			Operation: productionGenerationRecoveryList,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "production_generation_recovery_serve3_linux.go", Line: 93, Marker: "server.Bind(productionGenerationRecoveryList, serve.list())"},
				{Path: "../../../web/operations.json", Line: 221, Marker: `"generation:list-recovery": "project"`},
			},
		},
		{
			Operation: productionGenerationRecoveryFind,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "production_generation_recovery_serve3_linux.go", Line: 96, Marker: "server.Bind(productionGenerationRecoveryFind, serve.find())"},
				{Path: "../../internal/store/planning_continuation.go", Line: 63,
					Marker: "func FindDirectoryRecoveryNeed(ctx context.Context, db *sql.DB, projectID string) (*DirectoryRecoveryNeed, error)"},
				{Path: "../../../web/operations.json", Line: 222, Marker: `"generation:find-blueprint-recovery": "project"`},
			},
		},
	}
}

// productionGenerationRecoverySeams is empty: both previously refused names are
// served now, so there is no missing piece left to document.
func productionGenerationRecoverySeams() []productionGenerationRecoverySeam {
	return []productionGenerationRecoverySeam{}
}

// productionGenerationRecoverySeamFor looks one name up in the (empty) seam table.
func productionGenerationRecoverySeamFor(operation string) (productionGenerationRecoverySeam, bool) {
	for _, seam := range productionGenerationRecoverySeams() {
		if seam.Operation == operation {
			return seam, true
		}
	}
	return productionGenerationRecoverySeam{}, false
}

// productionGenerationRecoveryServedEvidenceFor does the same for the served table.
func productionGenerationRecoveryServedEvidenceFor(operation string) (productionGenerationRecoveryServed, bool) {
	for _, served := range productionGenerationRecoveryServedEvidence() {
		if served.Operation == operation {
			return served, true
		}
	}
	return productionGenerationRecoveryServed{}, false
}

// productionGenerationRecoveryServedBy reports whether the finished lane really
// installs every name this file records as served.
func productionGenerationRecoveryServedBy(bound []string) bool {
	for _, operation := range productionGenerationRecoveryServedOperations() {
		found := false
		for _, name := range bound {
			if name == operation {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// bindProductionGenerationRecovery binds the two names this family owns and
// verifies the one served by the sibling batch/directory slice. It is the storeful
// half of the wiring: the editors bucket needs the owner, which is recorded by
// bindProductionGenerationRecoveryOwner inside the owner block of
// production_open_linux.go before this binder runs.
func bindProductionGenerationRecovery(durable *productionDurableOpen, server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	// Install the two names for real; the directory catalog is already installed
	// by the batch/directory slice (production_open_linux.go:779).
	if err := bindProductionGenerationRecoveryServed(durable, server); err != nil {
		return err
	}
	for _, operation := range productionGenerationRecoveryServedOperations() {
		evidence, found := productionGenerationRecoveryServedEvidenceFor(operation)
		if !found || len(evidence.Evidence) == 0 {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_EVIDENCE_INCOMPLETE")
		}
	}
	if !productionGenerationRecoveryServedBy(server.BoundOperations()) {
		return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVED_MISSING")
	}
	for _, operation := range productionGenerationRecoveryRefusedOperations() {
		seam, found := productionGenerationRecoverySeamFor(operation)
		if !found || len(seam.Missing) == 0 {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_SEAM_INCOMPLETE")
		}
	}
	// The bucket table and the serve decisions are verified against the same
	// finished lane, now that both names are bound (see the sub-verifiers).
	if err := bindProductionGenerationRecoveryReads(server); err != nil {
		return err
	}
	if err := bindProductionGenerationRecoveryServe(server); err != nil {
		return err
	}
	if err := bindProductionGenerationRecoveryServe2(server); err != nil {
		return err
	}
	return nil
}
