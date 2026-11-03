//go:build lance_native && cgo

package projectaccess

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgelogical"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

func TestNativeKnowledgeOriginalLogicalHashGeneratedIDs(t *testing.T) {
	root := os.Getenv("GO_LANCE_CONTRACTROOT")
	if root == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("native logical fixture required")
		}
		t.Skip("GitHub native logical fixture required")
	}
	var oracle struct {
		Format  int    `json:"format"`
		Oracle  string `json:"oracle"`
		Summary struct {
			Documents   int    `json:"documents"`
			Chunks      int    `json:"chunks"`
			TableCount  int    `json:"tableCount"`
			LogicalHash string `json:"logicalHash"`
		} `json:"summary"`
		Tables []knowledgelogical.TableHash `json:"tables"`
	}
	raw, err := os.ReadFile(filepath.Join(root, "logical-contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &oracle); err != nil {
		t.Fatal(err)
	}
	if oracle.Format != 1 || oracle.Oracle != "actual-original-vector-migration-export-generated-id-lane" || len(oracle.Tables) != 6 {
		t.Fatal("unrecognized original logical oracle")
	}
	c := accessFixture(t)
	storage := filepath.Join(c.ProjectPath, ".ai-novel")
	if err := os.CopyFS(storage, os.DirFS(filepath.Join(root, "logical-copy"))); err != nil {
		t.Fatal(err)
	}
	c.Knowledge = &KnowledgeConfig{
		Copy:     projectfs.KnowledgeBudget{MaxBytes: 128 * 1024 * 1024, MaxEntries: 10000, MaxDepth: 32},
		Metadata: knowledgecopy.Budget{MaxMetadataBytes: 8 * 1024 * 1024, MaxCopies: 100},
		Native:   true, Logical: true,
	}
	before := nativeSourceManifest(t, c)
	report, err := Preflight(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if report.Knowledge == nil || report.Knowledge.Logical == nil || report.Knowledge.Native == nil || report.Knowledge.CopyLinks == nil {
		t.Fatal("logical evidence escaped independent native/copy proof", report)
	}
	actual := report.Knowledge.Logical
	if actual.Documents != oracle.Summary.Documents || actual.Chunks != oracle.Summary.Chunks ||
		actual.TableCount != oracle.Summary.TableCount || actual.LogicalHash != oracle.Summary.LogicalHash ||
		!reflect.DeepEqual(actual.Tables, oracle.Tables) {
		t.Fatalf("native typed Go hash differs from original TS: %+v %+v", actual, oracle)
	}
	if !reflect.DeepEqual(before, nativeSourceManifest(t, c)) {
		t.Fatal("logical preflight changed author source")
	}
	accessClean(t, c)
	result := struct {
		Summary any                          `json:"summary"`
		Tables  []knowledgelogical.TableHash `json:"tables"`
	}{Summary: map[string]any{"documents": actual.Documents, "chunks": actual.Chunks, "tableCount": actual.TableCount, "logicalHash": actual.LogicalHash}, Tables: actual.Tables}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "native-logical-summary.json"), append(encoded, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestNativeKnowledgeLogicalHashUnsupportedUnicodeFailsClosed(t *testing.T) {
	c := nativeAccessFixture(t)
	c.Knowledge.Logical = true
	report, err := Preflight(context.Background(), c)
	if err == nil || report.Knowledge != nil {
		t.Fatal("unproved multiple Unicode ID collation admitted", report, err)
	}
	accessClean(t, c)
}
