//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// This file holds the two halves of production_finalization_generation_write_linux.go's
// answer.
//
// TestProductionLaneRefusesFinalizationGenerationWrite drives the three names
// over the real HTTP handler and the frozen project session: every envelope gets
// the fail-closed 501, byte for byte, and the admitted database is unchanged. The
// envelope set is the one the reference's own flow produces - the request with
// the live lease, the same request replayed, the same request under another
// project, the same request under a lease this session never held - plus the bare
// session with no request at all. The point of comparing the bodies is that a
// name this lane cannot serve must not answer differently for a request it could
// not read.
//
// TestProductionFinalizationGenerationWriteLedgerProbe is the re-test the family
// file's ledger seam did not survive: the lane's own ledger, on a copy of an
// admitted file, opens a `finalized-chapter-notes` run and a
// `finalized-character-state` run with every key the reference freezes beside the
// slot. The control open at the top proves the probe's request shape is one the
// ledger accepts, and the two negative opens prove the acceptance is the family
// allowlist's own rule rather than a blanket widening: the same keys under an
// ordinary operation are refused, and a planning name is refused by the name
// gate the family file used to blame.
const (
	// productionFinalizationGenerationWriteForeignProject is a project this lane
	// never opened. The envelope is refused before the project is looked at, which
	// is exactly what the byte-identity assertion below records.
	productionFinalizationGenerationWriteForeignProject = "5d0c6c46-2f4f-4a8e-9a44-ffffffffffff"
	// productionFinalizationGenerationWriteProbeProject and its "other" twin are
	// the probe's own project ids: the ledger does not check them against a project
	// table, and using ids no seeded row carries keeps every probe open a fresh
	// root instead of replaying the fixture's.
	productionFinalizationGenerationWriteProbeProject = "finalization-generation-write-probe"
	productionFinalizationGenerationWriteProbeOther   = "finalization-generation-write-probe-other"
)

func TestProductionFinalizationGenerationWriteLedgerProbe(t *testing.T) {
	_, _, database, _ := productionGraphReadsRoot(t)
	productionGraphReadsSeedLedger(t, database)
	copied := productionFinalizationGenerationWriteProbeCopy(t, database)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(copied)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	ordinal := 0
	ledger := store.NewGenerationLedger(db,
		func() int64 { return 1700000000000 },
		func() string {
			ordinal++
			return "finalization-generation-write-probe-" + strconv.Itoa(ordinal)
		})
	ctx := context.Background()

	// The control: the probe's request shape is one the ledger accepts, so a
	// refusal below is the finalization binding and not a malformed request.
	control := productionGraphReadsSeedRequest(t, "review-chapter", productionFinalizationGenerationWriteProbeProject,
		productionGraphReadsFrozenEpoch, map[string]any{"operation": "review-chapter"})
	if err := productionFinalizationGenerationWriteOpen(ctx, ledger, control); err != nil {
		t.Fatalf("the probe request shape was refused for an ordinary operation: %v", err)
	}
	// The re-test: both finalization operations, with every key the reference
	// freezes beside the slot (electron/services/generation-source-binding.ts:587-591).
	for _, operation := range []string{"finalized-chapter-notes", "finalized-character-state"} {
		request := productionGraphReadsSeedRequest(t, operation, productionFinalizationGenerationWriteProbeProject,
			productionGraphReadsFrozenEpoch,
			productionFinalizationGenerationWriteProbeManifest(t, operation, productionGraphReadsFrozenEpoch))
		if err := productionFinalizationGenerationWriteOpen(ctx, ledger, request); err != nil {
			t.Fatalf("the ledger refused a %s run with %v, so the ledger is still the seam and the family row this change dropped has to come back",
				operation, err)
		}
	}
	// The allowlist is a family allowlist, not a blanket widening: the same keys
	// under an ordinary operation are refused. A distinct project keeps the open
	// key fresh, so the family key is the only thing that can refuse this request.
	stolen := productionGraphReadsSeedRequest(t, "review-chapter", productionFinalizationGenerationWriteProbeOther,
		productionGraphReadsFrozenEpoch,
		productionFinalizationGenerationWriteProbeManifest(t, "review-chapter", productionGraphReadsFrozenEpoch))
	if err := productionFinalizationGenerationWriteOpen(ctx, ledger, stolen); err == nil {
		t.Fatal("the ledger accepted a finalization slot under an ordinary operation")
	} else if !strings.Contains(err.Error(), "GENERATION_OPERATION_UNSUPPORTED") {
		t.Fatalf("the ledger refused the stolen slot with %v, want GENERATION_OPERATION_UNSUPPORTED", err)
	}
	// The planning gate the family file used to blame is real, and it is a name
	// test - which is why it does not reach this family's two operation names.
	planning := productionGraphReadsSeedRequest(t, "import-blueprints", productionFinalizationGenerationWriteProbeOther,
		productionGraphReadsFrozenEpoch, map[string]any{"operation": "import-blueprints"})
	if err := productionFinalizationGenerationWriteOpen(ctx, ledger, planning); err == nil {
		t.Fatal("the ledger accepted a planning operation")
	} else if !strings.Contains(err.Error(), "GENERATION_OPERATION_UNSUPPORTED") {
		t.Fatalf("the ledger refused a planning operation with %v, want GENERATION_OPERATION_UNSUPPORTED", err)
	}
}

// productionFinalizationGenerationWriteEvidence re-reads every declaration the
// decision table names. The marker is searched across the whole file rather than
// only at the recorded line, so a seam that is still documented but shifted by an
// unrelated edit is not a regression while a deleted one is.
func productionFinalizationGenerationWriteEvidence(t *testing.T, operation string, lines []productionFinalizationLine) {
	t.Helper()
	for _, line := range lines {
		raw, err := os.ReadFile(line.Path)
		if err != nil {
			t.Fatalf("%s: %s: %v", operation, line.Path, err)
		}
		if !productionSeamContains(string(raw), line.Marker) {
			t.Fatalf("%s: %s no longer documents the marker recorded at line %d", operation, line.Path, line.Line)
		}
	}
}

// productionFinalizationGenerationWriteTemplateAbsent fails the day a Go copy of
// either template this family renders appears under internal/prompttemplate: that
// is the day begin's refused field gains a producer, so the decision table and
// the family seam have to be re-tested with it.
func productionFinalizationGenerationWriteTemplateAbsent(t *testing.T) {
	t.Helper()
	patterns := []string{"../../internal/prompttemplate/*.go", "../../internal/prompttemplate/*.json"}
	visited := 0
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("read the prompt template package with %s: %v", pattern, err)
		}
		for _, path := range matches {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			visited++
			for _, key := range []string{"generate_chapter_notes", "update_character_cards"} {
				if productionSeamContains(string(raw), key) {
					t.Fatalf("%s now names %s, so the template this family needs has a Go producer and the refused field must be re-tested",
						path, key)
				}
			}
		}
	}
	if visited == 0 {
		t.Fatal("the prompt template package matched no asset, so this check would pass by looking nowhere")
	}
}

// productionFinalizationGenerationWriteProbeCopy copies the admitted database into
// the test's own temporary tree, taken while nothing holds the file open so the
// copy is one consistent page set.
func productionFinalizationGenerationWriteProbeCopy(t *testing.T, database string) string {
	t.Helper()
	raw, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(t.TempDir(), "finalization-generation-write-probe.sqlite3")
	if err := os.WriteFile(copied, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return copied
}

// productionFinalizationGenerationWriteProbeManifest is the manifest half the
// reference freezes for a finalization run: the slot, its key, the captured
// context and its hash, the origin epoch, the rendered task and its hash, beside
// the operation the step names.
//
// The slot key is derived from the slot the way the reference derives it, so the
// probe freezes a consistent pair. The context and task values are shaped like
// the reference's and are not the reference's: the ledger validates key names and
// the operation, never the prose a context carries
// (internal/store/generation_ledger.go:426-447). The slot keeps the fixture's
// step key for both operations for the same reason - the ledger does not
// cross-check it, which is exactly why this probe proves the ledger and claims
// nothing about the run.
func productionFinalizationGenerationWriteProbeManifest(t *testing.T, operation, epoch string) map[string]any {
	t.Helper()
	slot := productionGraphReadsSlot(productionGraphReadsOwnedFinalizationID)
	manifest := map[string]any{}
	manifest["operation"] = operation
	manifest["finalizationGenerationSlot"] = slot
	manifest["finalizationGenerationSlotKey"] = productionGraphReadsSlotKey(t, slot)
	manifest["finalizationGenerationContext"] = map[string]any{
		"slot":         slot,
		"chapterTitle": "第三章 门后有人",
	}
	manifest["finalizationGenerationContextHash"] = strings.Repeat("c", 64)
	manifest["finalizationGenerationOriginEpoch"] = epoch
	manifest["finalizationGenerationTask"] = map[string]any{
		"purpose":        operation,
		"reasoningStage": "review",
		"output":         "visible-text",
	}
	manifest["finalizationGenerationTaskHash"] = strings.Repeat("e", 64)
	return manifest
}

// productionFinalizationGenerationWriteOpen is the ledger's own open call with
// one request map, encoded the way the fixture encodes it so the store decodes
// the binding in JSON key order.
func productionFinalizationGenerationWriteOpen(ctx context.Context, ledger *store.GenerationLedger, request map[string]any) error {
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	_, err = ledger.CallJSON(ctx, "open", json.RawMessage("["+string(encoded)+"]"))
	return err
}
