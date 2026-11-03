//go:build lance_native && cgo

package lancenative

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestVectorQueryRejectsInvalidRequestBeforeNativeDispatch(t *testing.T) {
	base := VectorRequest{Table: "chunks__space_1", Version: 1, Generation: 1,
		Dimension: 2, Metric: "l2", ModelFingerprint: "synthetic:active", TopK: 2,
		Vector: []float32{1, 0}}
	for name, mutate := range map[string]func(*VectorRequest){
		"table":       func(r *VectorRequest) { r.Table = "chunks__space_2" },
		"metric":      func(r *VectorRequest) { r.Metric = "arbitrary" },
		"dimension":   func(r *VectorRequest) { r.Dimension = 3 },
		"limit":       func(r *VectorRequest) { r.TopK = 101 },
		"nonfinite":   func(r *VectorRequest) { r.Vector[0] = float32(math.NaN()) },
		"zero-cosine": func(r *VectorRequest) { r.Metric = "cosine"; r.Vector = []float32{0, 0} },
	} {
		t.Run(name, func(t *testing.T) {
			request := base
			request.Vector = append([]float32(nil), base.Vector...)
			mutate(&request)
			if _, err := QueryVector(context.Background(), "/nonexistent", request); !errors.Is(err, ErrInput) {
				t.Fatalf("invalid request reached native dispatch: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := QueryVector(ctx, "/nonexistent", base); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestVectorQueryPinnedPrivateFixture(t *testing.T) {
	root := os.Getenv("GO_LANCE_CONTRACTROOT")
	if root == "" {
		t.Skip("optional GitHub synthetic fixture is not supplied")
	}
	path := filepath.Join(root, "copy", "lancedb")
	manifest, err := Export(context.Background(), path, private(t))
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
		t.Fatal("fixture lacks active vector table")
	}
	request := VectorRequest{Table: "chunks__space_1", Version: version, Generation: 1,
		Dimension: 2, Metric: "l2", ModelFingerprint: "synthetic:active-🧭", TopK: 3,
		Vector: []float32{1, 0}}
	result, err := QueryVector(context.Background(), path, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Table != request.Table || result.Version != version || len(result.Records) == 0 || len(result.Records) > 3 {
		t.Fatal("vector query lost pinned identity or bounds")
	}
	previous := math.Inf(-1)
	for _, record := range result.Records {
		distance, ok := record["_distance"].(float64)
		if !ok || math.IsNaN(distance) || math.IsInf(distance, 0) || distance < previous {
			t.Fatal("vector query returned invalid ordering or distance")
		}
		previous = distance
	}
	request.Version++
	if _, err := QueryVector(context.Background(), path, request); err == nil {
		t.Fatal("stale table version accepted")
	}
}
