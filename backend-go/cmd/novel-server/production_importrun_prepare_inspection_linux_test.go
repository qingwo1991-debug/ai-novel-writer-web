//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// These tests re-read the record this file keeps. The name is served now
// (production_importrun_prepare_inspection_serve_linux_test.go walks the whole
// journey), so what is left here is the proof that each of the three pinned gaps is
// closed by a declaration that exists - and that the parts of the store the record
// relies on still behave the way it says they do.

// TestProductionImportRunPrepareInspectionRecordIsReal re-reads every declaration
// the closed record points at. A marker that moved is a diagnosis, and a deleted
// declaration is a failure: the record would otherwise be prose about a store that
// no longer looks like it.
func TestProductionImportRunPrepareInspectionRecordIsReal(t *testing.T) {
	lines := productionImportRunPrepareInspectionClosed()
	if len(lines) == 0 {
		t.Fatal("the record carries no declaration, so nothing proves the gaps closed")
	}
	for _, line := range lines {
		if line.Path == "" || line.Marker == "" {
			t.Fatalf("a record row is not a line: %v", line.Path)
		}
		raw, err := os.ReadFile(line.Path)
		if err != nil {
			t.Fatalf("%v: %v", line.Path, err)
		}
		if !productionSeamContains(string(raw), line.Marker) {
			t.Fatalf("%v no longer contains %v (recorded at line %d): %v",
				line.Path, line.Marker, line.Line,
				productionSeamDiagnosis(string(raw), line.Marker, line.Line))
		}
	}
}

// TestProductionImportRunPrepareInspectionStoreHalfIsClosed proves the two facts
// about the store the record states: the author prepare is declared exactly once,
// in the file the record names, and it validates before it opens anything. The
// parsing lane's own guard is asserted beside it, because the record leans on it:
// the author arm is not the parsing lane, so BeginParsing refusing every purpose but
// reference is still correct.
func TestProductionImportRunPrepareInspectionStoreHalfIsClosed(t *testing.T) {
	matches, err := filepath.Glob("../../internal/store/*.go")
	if err != nil {
		t.Fatal(err)
	}
	declarations := make([]string, 0, 1)
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if productionSeamContains(string(raw), "func (r *ImportRuns) PrepareAuthor(") {
			declarations = append(declarations, filepath.Base(path))
		}
	}
	if len(declarations) != 1 || declarations[0] != "importrun_author_prepare.go" {
		t.Fatalf("the author prepare is declared in %v, want exactly importrun_author_prepare.go", declarations)
	}

	// A nil database answers the request's own first refusal, never a panic: the
	// method validates the id and the fingerprints before it asks for a
	// transaction, exactly like BeginParsing.
	runs := store.NewImportRuns(nil)
	_, err = runs.PrepareAuthor(context.Background(), store.ImportRunPrepareAuthorRequest{
		RunID: "production-prepare-inspection-run",
	})
	if err == nil || err.Error() != "IMPORT_RUN_ID_INVALID" {
		t.Fatalf("PrepareAuthor did not validate before opening a database: %v", err)
	}
	_, err = runs.BeginParsing(context.Background(), store.ImportRunBeginParsingRequest{
		RunID:             "production-prepare-inspection-run",
		Purpose:           "author-manuscript",
		Locale:            "zh-CN",
		SourceFingerprint: strings.Repeat("a", 64),
	})
	if err == nil || err.Error() != "IMPORT_RUN_PURPOSE_INVALID" {
		t.Fatalf("BeginParsing no longer refuses the author purpose: %v", err)
	}
}
