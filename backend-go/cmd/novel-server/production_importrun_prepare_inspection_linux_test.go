//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/importsource"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// These tests re-test the one refusal this file records. They do not claim a
// serve: the name stays at GO_OPERATION_NOT_MIGRATED, and every test below pins
// WHY that is no longer the reason the old seam gave.

// TestProductionImportRunPrepareInspectionCasIsProducibleFromStagedUploads walks
// the real browser journey up to the refusal: it stages a forty-chapter
// manuscript through the mounted upload route, selects it with the served picker,
// and then proves the preview CAS the old refusal said could never be produced is
// in fact the store's own value for the picker's receipt - peeked and consumed
// server-side without ever crossing the wire. The name itself is still refused
// byte-identically under a live lease, a stale lease and a foreign project.
func TestProductionImportRunPrepareInspectionCasIsProducibleFromStagedUploads(t *testing.T) {
	lane := newProductionImportTestLane(t)
	treeBefore := productionCloudBackupTreeDigest(t, lane.project)
	databaseBefore := productionCloudBackupDatabaseDigest(t, lane.project)

	// Forty chapters in one staged manuscript: the browser DOES produce these
	// bytes, and the picker freezes them into one inspection.
	var manuscript strings.Builder
	for number := 1; number <= 40; number++ {
		fmt.Fprintf(&manuscript, "第%d章 标题%d\n\n这是第%d章的正文内容。\n\n", number, number, number)
	}
	lane.stage(t, lane.author, "全稿.txt", manuscript.String())

	response := lane.author.call(t, "dialog:select-novel-files", []any{lane.request(), lane.frozen})
	if response.Code != http.StatusOK {
		t.Fatalf("the picker refused the staged manuscript: %d %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if strings.Contains(body, "previewCAS") {
		t.Fatal("the picker put the preview CAS on the wire; it is a server-side handoff and the reference never had one")
	}
	var wire productionImportWire
	if err := decodeProductionImportRunPrepareInspection(response, &wire); err != nil {
		t.Fatal(err)
	}
	if !wire.Success || wire.Inspection.InspectionID == "" || wire.Inspection.ChapterCount != 40 {
		t.Fatalf("the picker did not freeze forty chapters: %+v", wire)
	}

	// The caller and generation the binder would use, rebuilt from the served
	// session, so Peek/Consume are exercised exactly as a prepare handler would.
	caller := lane.caller(lane.author)
	binding := importsource.Binding{
		Caller:     caller.SessionID + ":" + caller.ClientID,
		Generation: syntheticString(lane.frozen["projectId"]) + ":" + syntheticString(lane.frozen["leaseId"]),
		Purpose:    importsource.AuthorManuscript,
	}

	// The refusal is still the refusal, independent of the id and the context: a
	// live id under a live lease, a stale lease and a foreign project all answer
	// the same 501 bytes, and none of them writes.
	request := map[string]any{"inspectionId": wire.Inspection.InspectionID,
		"runId": "production-import-run-1", "purpose": "reference", "locale": "zh-CN"}
	var refusal string
	for index, shape := range []struct {
		label string
		path  string
		ctx   map[string]any
	}{
		{"live lease", syntheticString(lane.frozen["projectPath"]), lane.frozen},
		{"stale lease", syntheticString(lane.frozen["projectPath"]), productionImportContext(lane.frozen, "leaseId", "stale-lease-0001")},
		{"foreign project", lane.project + "-foreign", productionImportContext(lane.frozen, "projectPath", lane.project+"-foreign")},
	} {
		answer := lane.author.call(t, "db:import-run-prepare-inspection", []any{request, shape.path, shape.ctx})
		if answer.Code != http.StatusNotImplemented || !strings.Contains(answer.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("%s answered %d %s, want 501 GO_OPERATION_NOT_MIGRATED", shape.label, answer.Code, answer.Body.String())
		}
		if index == 0 {
			refusal = answer.Body.String()
			continue
		}
		if answer.Body.String() != refusal {
			t.Fatalf("%s differs from the live-lease body:\n live:  %s\n other: %s", shape.label, refusal, answer.Body.String())
		}
	}

	// The CAS is the store's own value for the receipt: Peek hands it to a trusted
	// consumer, a wrong CAS is refused without consuming, and the peeked CAS
	// releases all forty chapters and the one source.
	summary, err := lane.selection.store.Peek(wire.Inspection.InspectionID, binding)
	if err != nil {
		t.Fatalf("the picker's receipt is not resolvable by its own store: %v", err)
	}
	if len(summary.PreviewCAS) != 64 || summary.ChapterCount != 40 || len(summary.Preview) != 8 {
		t.Fatalf("the receipt does not carry the frozen CAS and forty chapters: %+v", summary)
	}

	// A reference purpose cannot resolve the author receipt: the reference half of
	// the reference handler can never be fed by this picker, which is the exact
	// structural reason it is dead code in this lane.
	reference := binding
	reference.Purpose = importsource.Reference
	if _, err := lane.selection.store.Peek(wire.Inspection.InspectionID, reference); err == nil || err.Error() != "IMPORT_INSPECTION_EXPIRED" {
		t.Fatalf("a reference binding resolved an author receipt: %v", err)
	}

	// A wrong CAS is refused without consuming; the peeked CAS releases all forty
	// chapters and the one source.
	if _, err := lane.selection.store.Consume(wire.Inspection.InspectionID, strings.Repeat("0", 64), binding); err == nil || err.Error() != "IMPORT_INSPECTION_PREVIEW_CHANGED" {
		t.Fatalf("a wrong CAS was not refused with IMPORT_INSPECTION_PREVIEW_CHANGED: %v", err)
	}
	inspection, err := lane.selection.store.Consume(wire.Inspection.InspectionID, summary.PreviewCAS, binding)
	if err != nil {
		t.Fatalf("consume with the peeked CAS refused: %v", err)
	}
	if len(inspection.Chapters) != 40 || len(inspection.Sources) != 1 {
		t.Fatalf("consume returned %d chapters from %d sources, want the forty staged", len(inspection.Chapters), len(inspection.Sources))
	}
	for _, chapter := range inspection.Chapters {
		if chapter.Title == "" || chapter.Content == "" {
			t.Fatalf("consume returned an empty chapter: %+v", chapter)
		}
	}
	if active := lane.selection.store.ActiveCount(); active != 0 {
		t.Fatalf("consume left %d receipts behind", active)
	}

	// Nothing about staging, selecting or refusing touched the project bytes.
	productionImportUnchanged(t, lane.project, treeBefore, databaseBefore)
}

// TestProductionImportRunPrepareInspectionDeclarationsAreReal re-reads every
// corrected declaration, proves the author prepare has no store method, and
// proves BeginParsing refuses the author purpose with the store's own code.
func TestProductionImportRunPrepareInspectionDeclarationsAreReal(t *testing.T) {
	for _, line := range append(productionImportRunPrepareInspectionRefuted(), productionImportRunPrepareInspectionMissing()...) {
		raw, err := os.ReadFile(line.Path)
		if err != nil {
			t.Fatalf("%v: %v", line.Path, err)
		}
		if !productionSeamContains(string(raw), line.Marker) {
			t.Fatalf("%v no longer contains %v (recorded at line %d): %v",
				line.Path, line.Marker, line.Line, productionSeamDiagnosis(string(raw), line.Marker, line.Line))
		}
	}

	// No store method implements the author half of ImportRunRepository.prepare:
	// the whole internal/store package must not declare ImportRuns.Prepare.
	matches, err := filepath.Glob("../../internal/store/*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if productionSeamContains(string(raw), "func (r *ImportRuns) Prepare(") {
			t.Fatalf("%v now declares the author prepare; prepare-inspection has to be re-argued", path)
		}
	}

	// BeginParsing refuses every purpose but reference, before it ever opens a
	// database, so the reference half cannot adopt the author inspection the
	// picker mints.
	runs := store.NewImportRuns(nil)
	_, err = runs.BeginParsing(context.Background(), store.ImportRunBeginParsingRequest{
		RunID:             "production-prepare-inspection-run",
		Purpose:           "author-manuscript",
		Locale:            "zh-CN",
		SourceFingerprint: strings.Repeat("a", 64),
	})
	if err == nil || err.Error() != "IMPORT_RUN_PURPOSE_INVALID" {
		t.Fatalf("BeginParsing did not refuse the author purpose: %v", err)
	}
}

// TestProductionImportRunPrepareInspectionBinderValidatesTheRecord proves the
// no-op binder installs nothing, refuses a nil adapter/server and an
// already-bound name, and that the record validator rejects a fabricated bad
// gate, bad shape, bad name and already-bound set.
func TestProductionImportRunPrepareInspectionBinderValidatesTheRecord(t *testing.T) {
	lane := newProductionImportTestLane(t)
	adapter := newProductionImportRunPrepareInspection(lane.open.durable)
	before := len(lane.server.BoundOperations())
	if err := adapter.Bind(lane.server); err != nil {
		t.Fatalf("the no-op binder failed against the live lane: %v", err)
	}
	if after := len(lane.server.BoundOperations()); after != before {
		t.Fatalf("the binder installed %d operations, want none", after-before)
	}
	if err := newProductionImportRunPrepareInspection(nil).Bind(lane.server); err == nil {
		t.Fatal("a nil adapter was accepted")
	}
	if err := adapter.Bind(nil); err == nil {
		t.Fatal("a nil server was accepted")
	}

	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
	if err := server.Bind(productionImportRunPrepareInspectionOperation, func(context.Context, transport.Caller, []any) (any, error) {
		return nil, nil
	}); err != nil {
		t.Fatalf("the name is not inventoried, so its unbound answer would not be 501: %v", err)
	}
	if err := adapter.Bind(server); err == nil || err.Error() != "IMPORT_RUN_PREPARE_INSPECTION_ALREADY_BOUND" {
		t.Fatalf("an already-bound name was accepted: %v", err)
	}

	line := productionImportRunPrepareInspectionLineOf("x", 0, "y")
	refuted := []productionImportRunPrepareInspectionLine{line}
	missing := []productionImportRunPrepareInspectionLine{line}
	valid := productionImportRunPrepareInspectionValidate(productionImportRunPrepareInspectionOperation,
		productionImportRunPrepareInspectionArity, productionImportRunPrepareInspectionPathIndex,
		productionImportRunPrepareInspectionGate, productionImportRunPrepareInspectionPort,
		refuted, missing, nil)
	if valid != nil {
		t.Fatalf("the real record failed its own validator: %v", valid)
	}
	cases := []struct {
		label    string
		got      error
		expected string
	}{
		{"already bound", productionImportRunPrepareInspectionValidate(
			productionImportRunPrepareInspectionOperation, 3, 1, productionImportRunPrepareInspectionGate,
			productionImportRunPrepareInspectionPort, refuted, missing,
			map[string]bool{productionImportRunPrepareInspectionOperation: true}), "IMPORT_RUN_PREPARE_INSPECTION_ALREADY_BOUND"},
		{"foreign name", productionImportRunPrepareInspectionValidate(
			"db:not-a-real-name", 3, 1, productionImportRunPrepareInspectionGate,
			productionImportRunPrepareInspectionPort, refuted, missing, nil), "IMPORT_RUN_PREPARE_INSPECTION_NAME_INVALID"},
		{"bad gate", productionImportRunPrepareInspectionValidate(
			productionImportRunPrepareInspectionOperation, 3, 1, "not symbolic",
			productionImportRunPrepareInspectionPort, refuted, missing, nil), "IMPORT_RUN_PREPARE_INSPECTION_GATE_INVALID"},
		{"bad shape", productionImportRunPrepareInspectionValidate(
			productionImportRunPrepareInspectionOperation, 1, 0, productionImportRunPrepareInspectionGate,
			productionImportRunPrepareInspectionPort, refuted, missing, nil), "IMPORT_RUN_PREPARE_INSPECTION_SHAPE_INVALID"},
		{"empty port", productionImportRunPrepareInspectionValidate(
			productionImportRunPrepareInspectionOperation, 3, 1, productionImportRunPrepareInspectionGate,
			"", refuted, missing, nil), "IMPORT_RUN_PREPARE_INSPECTION_SEAM_INCOMPLETE"},
		{"blank line", productionImportRunPrepareInspectionValidate(
			productionImportRunPrepareInspectionOperation, 3, 1, productionImportRunPrepareInspectionGate,
			productionImportRunPrepareInspectionPort, []productionImportRunPrepareInspectionLine{{}},
			missing, nil), "IMPORT_RUN_PREPARE_INSPECTION_LINE_INVALID"},
	}
	for _, item := range cases {
		if item.got == nil || item.got.Error() != item.expected {
			t.Fatalf("%s: %v, want %v", item.label, item.got, item.expected)
		}
	}
}

// decodeProductionImportRunPrepareInspection decodes the picker's answer without
// naming a CAS field, so the wire type and the projection cannot drift.
func decodeProductionImportRunPrepareInspection(response *httptest.ResponseRecorder, into any) error {
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		return err
	}
	if len(envelope.Result) == 0 {
		return errors.New("empty result")
	}
	return json.Unmarshal(envelope.Result, into)
}
