//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// The two names this file used to refuse with the fail-closed 501 are served now,
// by production_import_generation_serve_linux.go. What is left here is the two
// records that change rests on.
//
// TestProductionImportGenerationLedgerAdmission is the re-test the family file's
// ledger seam did not survive. Before this change the ledger refused every import
// binding, which is why the two names could only answer 501. It now admits the
// three import members for exactly the two stage operations this lane freezes, and
// still refuses them for every other operation - the control open at the top
// proves the probe's request shape is one the ledger accepts, so the refusals
// below are the allowlist's own rule and never a malformed request.
//
// TestProductionImportGenerationBinderRecord proves the server-only binder the
// lane's chain already calls still holds: it installs nothing (the two names are
// installed by production_import_generation_serve_linux.go, which needs the
// generation owner), it refuses a nil server, and its seam table is empty, so a
// name that slipped back to refused without a documented reason fails here.
//
// The names themselves are driven over the real transport in
// production_import_generation_serve_linux_test.go. The served-count test owns the
// count.
const (
	// The two leases below are ones this session never holds. They are not secrets
	// and not derived from the live lease: the point is that they are refused.
	productionImportGenerationStaleLease = "import-generation-stale-lease"
	productionImportGenerationSlotRunID  = "import-generation-read-run"
	// productionImportGenerationLedgerProject is the probe's own project id: the
	// ledger does not check it against a project table, and using an id no seeded
	// row carries keeps every probe open a fresh root instead of replaying the
	// fixture's.
	productionImportGenerationLedgerProject = "import-generation-admission-probe"
)

// TestProductionImportGenerationLedgerAdmission asks the lane's own ledger to open
// one run per import operation on a copy of the admitted file: the two stages this
// lane freezes are accepted with all three import members beside the operation, and
// every other operation keeps the refusal it answered before.
func TestProductionImportGenerationLedgerAdmission(t *testing.T) {
	_, _, database, _ := productionGraphReadsRoot(t)
	productionGraphReadsSeedLedger(t, database)
	// The probe runs against a copy taken here, before the lane opens the file: the
	// seeding helper closed its own connection, so this copy is the admitted
	// database in one consistent piece, and an experiment on it cannot touch the
	// bytes the fixture's digest arms are about.
	probe := productionImportGenerationProbeCopy(t, database)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(probe)}
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
			return "import-generation-probe-" + strconv.Itoa(ordinal)
		})
	ctx := context.Background()

	// The control: the probe's request shape is one the ledger accepts, so a
	// refusal below is the binding's own rule and not a malformed request.
	if err := productionImportGenerationLedgerOpen(t, ctx, ledger, productionGraphReadsSeedRequest(t,
		"review-chapter", productionImportGenerationLedgerProject, productionGraphReadsFrozenEpoch,
		map[string]any{"operation": "review-chapter"})); err != nil {
		t.Fatalf("the probe's own request shape was refused for an ordinary operation: %v", err)
	}
	// The import manifest is the one the reference freezes for an import run
	// (electron/services/generation-source-binding.ts:599): the slot, its key and
	// the captured context, beside the stage operation.
	importManifest := func(operation string) map[string]any {
		manifest := map[string]any{}
		manifest["operation"] = operation
		manifest["importSlot"] = map[string]any{"runId": productionImportGenerationSlotRunID,
			"stage": "global", "batchId": "done"}
		manifest["importSlotKey"] = strings.Repeat("b", 64)
		manifest["importContext"] = map[string]any{"manifestFingerprint": strings.Repeat("c", 64)}
		return manifest
	}
	for _, operation := range []string{productionImportGlobalFactsOperation, productionImportWritingStyleOperation} {
		if err := productionImportGenerationLedgerOpen(t, ctx, ledger, productionGraphReadsSeedRequest(t,
			operation, productionImportGenerationLedgerProject, productionGraphReadsFrozenEpoch,
			importManifest(operation))); err != nil {
			t.Fatalf("the ledger refused %s, the operation that owns an import slot: %v", operation, err)
		}
	}
	// Every other operation keeps its refusal. The blueprint stage is the third
	// import name and is refused by the planning gate, not by the allowlist; the
	// ordinary names prove the allowlist itself is operation-scoped.
	for _, operation := range []string{"chapter-draft", "agent-round", "import-blueprints",
		"chapter-blueprint-directory"} {
		err := productionImportGenerationLedgerOpen(t, ctx, ledger, productionGraphReadsSeedRequest(t,
			operation, productionImportGenerationLedgerProject, productionGraphReadsFrozenEpoch,
			importManifest(operation)))
		if err == nil {
			t.Fatalf("the ledger opened %s with an import slot, so any family can carry one", operation)
		}
		if !strings.Contains(err.Error(), "GENERATION_OPERATION_UNSUPPORTED") {
			t.Fatalf("the ledger refused %s with %v, want GENERATION_OPERATION_UNSUPPORTED", operation, err)
		}
	}
}

// TestProductionImportGenerationBinderRecord pins the server-only binder that
// production_open_linux.go's chain already calls. It installs nothing, so the two
// names keep failing closed until the owner-bound binder in
// production_import_generation_serve_linux.go is wired beside it.
func TestProductionImportGenerationBinderRecord(t *testing.T) {
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
	if err := bindProductionImportGeneration(server); err != nil {
		t.Fatalf("the import binder record failed: %v", err)
	}
	if names := server.BoundOperations(); len(names) != 0 {
		t.Fatalf("the import binder record installed %v, want nothing", names)
	}
	if err := bindProductionImportGeneration(nil); err == nil {
		t.Fatal("a nil server was accepted by the import binder record")
	}
	if gaps := productionImportGenerationGaps(); len(gaps) != 0 {
		t.Fatalf("the import seam table still refuses %v, but both names are served now", gaps)
	}
	if productionModelExecutingRefusalCode != "GO_OPERATION_NOT_MIGRATED" {
		t.Fatalf("the fail-closed code is %v, want GO_OPERATION_NOT_MIGRATED", productionModelExecutingRefusalCode)
	}
	for _, operation := range productionImportGenerationOperations() {
		if _, refused := productionImportGenerationGap(operation); refused {
			t.Fatalf("%v is still documented as refused", operation)
		}
		// Each name is inventoried, so an absent handler is exactly 501
		// GO_OPERATION_NOT_MIGRATED and never 404 OPERATION_NOT_EXPOSED.
		fresh := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
		if err := fresh.Bind(operation, func(context.Context, transport.Caller, []any) (any, error) {
			return nil, nil
		}); err != nil {
			t.Fatalf("%s is not inventoried: %v", operation, err)
		}
	}
}

// productionImportGenerationProbeCopy copies the admitted database into the
// test's own temporary tree. The copy is taken while nothing holds the file
// open, so it is one consistent page set; the probe writes only to the copy.
func productionImportGenerationProbeCopy(t *testing.T, database string) string {
	t.Helper()
	raw, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(t.TempDir(), "import-generation-ledger-probe.sqlite3")
	if err := os.WriteFile(copied, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return copied
}

// productionImportGenerationLedgerOpen is the ledger's own open call with one
// request map, encoded the way the fixture encodes it so the store decodes the
// binding in JSON key order.
func productionImportGenerationLedgerOpen(t *testing.T, ctx context.Context, ledger *store.GenerationLedger, request map[string]any) error {
	t.Helper()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ledger.CallJSON(ctx, "open", json.RawMessage("["+string(encoded)+"]"))
	return err
}
