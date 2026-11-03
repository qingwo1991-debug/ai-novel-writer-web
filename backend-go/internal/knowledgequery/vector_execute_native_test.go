//go:build lance_native && cgo

package knowledgequery

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/lancenative"
)

func TestNativeVectorExecutorPinnedFixture(t *testing.T) {
	contractRoot := os.Getenv("GO_LANCE_CONTRACTROOT")
	if contractRoot == "" {
		t.Skip("optional GitHub synthetic native fixture is not supplied")
	}
	path := filepath.Join(contractRoot, "copy", "lancedb")
	output := t.TempDir()
	if err := os.Chmod(output, 0700); err != nil {
		t.Fatal(err)
	}
	manifest, err := lancenative.Export(context.Background(), path, output)
	if err != nil {
		t.Fatal(err)
	}
	var version uint64
	for _, table := range manifest.Tables {
		if table.Name == "chunks__space_1" {
			version = table.Version
		}
	}
	if version == 0 {
		t.Fatal("missing native vector fixture")
	}
	executor, err := NewNativeLanceVectorExecutor(path)
	if err != nil {
		t.Fatal(err)
	}
	response, err := executor.QueryVector(context.Background(), VectorPlan{
		TableName: "chunks__space_1", TableVersion: version,
		Generation: 1, Dimension: 2, ModelFingerprint: "synthetic:active-🧭",
		DistanceMetric: "l2", TopK: 3, Query: []float64{1, 0},
	})
	if err != nil || response.TableVersion != version || len(response.Hits) == 0 || len(response.Hits) > 3 {
		t.Fatalf("native adapter lost rows or identity: %+v %v", response, err)
	}
	for _, hit := range response.Hits {
		if hit.ID == "" || hit.DocumentID == "" || hit.FileName == "" {
			t.Fatalf("native adapter lost row identity: %+v", hit)
		}
	}
}
